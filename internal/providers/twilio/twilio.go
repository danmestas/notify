// Package twilio implements providers.SMSProvider against the Twilio
// Programmable Messaging REST API
// (POST https://api.twilio.com/2010-04-01/Accounts/{AccountSID}/Messages.json).
// mirroring the reference twilio.provider, it sends an x-www-form-urlencoded body
// From=<from>&To=<to>&Body=<text> with HTTP Basic auth (AccountSID:AuthToken)
// and reads the returned JSON "sid" as the provider message id. 2xx is success;
// 4xx is a permanent (plain) error; 5xx and transport faults are wrapped in
// providers.TransientError so the delivery handler can ask the engine to retry
// per 0001 §10.
package twilio

import (
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
)

var _ providers.SMSProvider = (*Client)(nil)

// defaultBaseURL is the Twilio REST API host. Overridable in tests via BaseURL.
const defaultBaseURL = "https://api.twilio.com"

// Client sends SMS through one Twilio account. AccountSID and AuthToken are the
// integration credentials (HTTP Basic auth); From is the sender number used
// when SMSMessage.From is empty. A nil HTTPClient gets a 10s-timeout default
// per Send.
type Client struct {
	AccountSID string
	AuthToken  string
	From       string
	BaseURL    string // defaults to https://api.twilio.com
	HTTPClient *http.Client
}

// Send POSTs an x-www-form-urlencoded body (From, To, Body) to
// {BaseURL}/2010-04-01/Accounts/{AccountSID}/Messages.json with HTTP Basic auth
// (AccountSID:AuthToken), mirroring the reference twilio.provider. msg.From overrides
// the configured From when non-empty. An empty AccountSID, AuthToken, or
// recipient (msg.To) fails closed (permanent). The provider message id is the
// "sid" of Twilio's JSON acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.SMSMessage) (providers.Result, error) {
	if c.AccountSID == "" || c.AuthToken == "" {
		return providers.Result{}, fmt.Errorf("twilio: missing credentials")
	}
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("twilio: empty recipient")
	}
	from := msg.From
	if from == "" {
		from = c.From
	}
	if from == "" {
		return providers.Result{}, fmt.Errorf("twilio: empty from")
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/2010-04-01/Accounts/" + c.AccountSID + "/Messages.json"

	form := url.Values{}
	form.Set("From", from)
	form.Set("To", msg.To)
	form.Set("Body", msg.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return providers.Result{}, fmt.Errorf("twilio: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.AccountSID, c.AuthToken)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer
		// so the endpoint URL (which carries the AccountSID in its path) does
		// not leak into error text persisted as Message.ErrorText or logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("twilio: post messages: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			SID string `json:"sid"`
		}
		_ = json.Unmarshal(respBody, &ack)
		return providers.Result{ProviderMessageID: ack.SID}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("twilio: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad credentials, invalid number, payload): permanent.
		return providers.Result{}, fmt.Errorf("twilio: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
