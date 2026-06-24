// Package sns implements providers.SMSProvider against the Amazon SNS query API
// (POST https://sns.<region>.amazonaws.com/ with an
// application/x-www-form-urlencoded Action=Publish form). It SigV4-signs each
// request with the awssigv4 signer (no aws-sdk-go dependency) and parses the
// MessageId out of the XML PublishResult. 2xx is success; 4xx is a permanent
// (plain) error; 5xx and transport faults are wrapped in
// providers.TransientError so the delivery handler can ask the engine to retry
// per 0001 §10.
package sns

import (
	"context"
	"encoding/xml"
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

var _ providers.SMSProvider = (*Client)(nil)

const (
	snsService = "sns"
	snsVersion = "2010-03-31"
)

// Client sends SMS through the Amazon SNS Publish action for one set of IAM
// credentials. AccessKeyID/SecretAccessKey are the signing credentials; Region
// selects the regional endpoint. BaseURL overrides the derived endpoint in
// tests. A nil HTTPClient gets a 10s-timeout default per Send. now is injected
// for deterministic SigV4 dates in tests; it defaults to time.Now.
type Client struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	BaseURL         string // defaults to https://sns.<region>.amazonaws.com
	HTTPClient      *http.Client
	now             func() time.Time
}

// publishResult models the subset of the SNS XML acknowledgement we read.
type publishResult struct {
	XMLName   xml.Name `xml:"PublishResponse"`
	MessageID string   `xml:"PublishResult>MessageId"`
}

// Send SigV4-signs and POSTs an SNS Publish form. The recipient is
// SMSMessage.To (E.164) and the text is SMSMessage.Body. Empty credentials,
// region, or recipient fail closed (permanent). The provider message id is the
// MessageId of SNS's XML acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.SMSMessage) (providers.Result, error) {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return providers.Result{}, fmt.Errorf("sns: missing credentials")
	}
	if c.Region == "" {
		return providers.Result{}, fmt.Errorf("sns: missing region")
	}
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("sns: empty recipient")
	}

	base := c.BaseURL
	if base == "" {
		base = "https://sns." + c.Region + ".amazonaws.com"
	}
	endpoint := strings.TrimRight(base, "/") + "/"

	form := url.Values{}
	form.Set("Action", "Publish")
	form.Set("PhoneNumber", msg.To)
	form.Set("Message", msg.Body)
	form.Set("Version", snsVersion)
	body := []byte(form.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return providers.Result{}, fmt.Errorf("sns: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	now := time.Now
	if c.now != nil {
		now = c.now
	}
	if err := awssigv4.Sign(req, body, snsService, c.Region, c.AccessKeyID, c.SecretAccessKey, now()); err != nil {
		return providers.Result{}, fmt.Errorf("sns: sign request: %w", err)
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
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("sns: post publish: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack publishResult
		_ = xml.Unmarshal(respBody, &ack)
		return providers.Result{ProviderMessageID: ack.MessageID}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("sns: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad credentials, invalid number): permanent.
		return providers.Result{}, fmt.Errorf("sns: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
