// Package ses implements providers.EmailProvider against the Amazon SES v2 API
// (POST https://email.<region>.amazonaws.com/v2/email/outbound-emails). It
// SigV4-signs each request with the awssigv4 signer (no aws-sdk-go dependency)
// and reads the returned JSON "MessageId" as the provider message id. 2xx is
// success; 4xx is a permanent (plain) error; 5xx and transport faults are
// wrapped in providers.TransientError so the delivery handler can ask the
// engine to retry per 0001 §10.
package ses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/awssigv4"
)

var _ providers.EmailProvider = (*Client)(nil)

const sesService = "ses"

// Client sends email through Amazon SES v2 for one set of IAM credentials.
// AccessKeyID/SecretAccessKey are the signing credentials; Region selects the
// regional endpoint; From is the default sender. BaseURL overrides the derived
// endpoint in tests. A nil HTTPClient gets a 10s-timeout default per Send. now
// is injected for deterministic SigV4 dates in tests; it defaults to time.Now.
type Client struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	From            string
	BaseURL         string // defaults to https://email.<region>.amazonaws.com
	HTTPClient      *http.Client
	now             func() time.Time
}

// simpleSubject / simpleBodyPart / simpleBody / simpleContent / emailContent /
// destination / sendPayload model the SESv2 outbound-emails request JSON.
type sesData struct {
	Data string `json:"Data"`
}

type sesBody struct {
	Text sesData `json:"Text"`
	HTML sesData `json:"Html"`
}

type sesSimple struct {
	Subject sesData `json:"Subject"`
	Body    sesBody `json:"Body"`
}

type sesContent struct {
	Simple sesSimple `json:"Simple"`
}

type sesDestination struct {
	ToAddresses []string `json:"ToAddresses"`
}

type sesPayload struct {
	FromEmailAddress string         `json:"FromEmailAddress"`
	Destination      sesDestination `json:"Destination"`
	Content          sesContent     `json:"Content"`
}

// Send SigV4-signs and POSTs an SESv2 outbound-emails request. msg.From
// overrides the configured From when non-empty. Empty credentials, recipient
// (msg.To), or resolved sender fail closed (permanent). The provider message id
// is the "MessageId" of SES's JSON acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.EmailMessage) (providers.Result, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return providers.Result{}, fmt.Errorf("ses: missing credentials")
	}
	if c.Region == "" {
		return providers.Result{}, fmt.Errorf("ses: missing region")
	}
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("ses: empty recipient")
	}
	from := msg.From
	if from == "" {
		from = c.From
	}
	if from == "" {
		return providers.Result{}, fmt.Errorf("ses: empty from")
	}

	base := c.BaseURL
	if base == "" {
		base = "https://email." + c.Region + ".amazonaws.com"
	}
	endpoint := strings.TrimRight(base, "/") + "/v2/email/outbound-emails"

	body, err := json.Marshal(sesPayload{
		FromEmailAddress: from,
		Destination:      sesDestination{ToAddresses: []string{msg.To}},
		Content: sesContent{Simple: sesSimple{
			Subject: sesData{Data: msg.Subject},
			Body: sesBody{
				Text: sesData{Data: msg.Text},
				HTML: sesData{Data: msg.HTML},
			},
		}},
	})
	if err != nil {
		return providers.Result{}, fmt.Errorf("ses: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("ses: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	now := time.Now
	if c.now != nil {
		now = c.now
	}
	if err := awssigv4.Sign(req, body, sesService, c.Region, c.AccessKeyID, c.SecretAccessKey, now()); err != nil {
		return providers.Result{}, fmt.Errorf("ses: sign request: %w", err)
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer so
		// the endpoint does not leak into error text persisted/logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("ses: post outbound-emails: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			MessageID string `json:"MessageId"`
		}
		_ = json.Unmarshal(respBody, &ack)
		return providers.Result{ProviderMessageID: ack.MessageID}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("ses: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad credentials, invalid payload): permanent.
		return providers.Result{}, fmt.Errorf("ses: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
