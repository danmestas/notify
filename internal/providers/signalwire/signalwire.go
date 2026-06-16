// Package signalwire implements providers.SMSProvider against the SignalWire
// LaML (Twilio-compatible) Messaging API
// (POST https://{SpaceURL}/api/laml/2010-04-01/Accounts/{ProjectID}/Messages.json).
// SignalWire is Twilio-LaML-compatible: same x-www-form-urlencoded body
// From=<from>&To=<to>&Body=<text>, HTTP Basic auth (ProjectID:Token), and a JSON
// response carrying "sid". It differs only in the base URL (a per-account Space
// host) and the credential names. 2xx is success; 4xx is a permanent (plain)
// error; 5xx and transport faults are wrapped in providers.TransientError so the
// delivery handler can ask the engine to retry per 0001 §10.
package signalwire

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

// Client sends SMS through one SignalWire Space. ProjectID and Token are the
// integration credentials (HTTP Basic auth); SpaceURL is the per-account host
// (e.g. "example.signalwire.com"); From is the sender number used when
// SMSMessage.From is empty. A nil HTTPClient gets a 10s-timeout default per
// Send.
type Client struct {
	ProjectID  string
	Token      string
	SpaceURL   string
	From       string
	HTTPClient *http.Client
}

// Send POSTs an x-www-form-urlencoded body (From, To, Body) to
// https://{SpaceURL}/api/laml/2010-04-01/Accounts/{ProjectID}/Messages.json with
// HTTP Basic auth (ProjectID:Token) — the Twilio-LaML wire shape. msg.From
// overrides the configured From when non-empty. An empty ProjectID, Token,
// SpaceURL, or recipient (msg.To) fails closed (permanent). The provider message
// id is the "sid" of SignalWire's JSON acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.SMSMessage) (providers.Result, error) {
	if c.ProjectID == "" || c.Token == "" {
		return providers.Result{}, fmt.Errorf("signalwire: missing credentials")
	}
	if c.SpaceURL == "" {
		return providers.Result{}, fmt.Errorf("signalwire: empty space url")
	}
	if msg.To == "" {
		return providers.Result{}, fmt.Errorf("signalwire: empty recipient")
	}
	from := msg.From
	if from == "" {
		from = c.From
	}
	if from == "" {
		return providers.Result{}, fmt.Errorf("signalwire: empty from")
	}

	endpoint := spaceEndpoint(c.SpaceURL, c.ProjectID)

	form := url.Values{}
	form.Set("From", from)
	form.Set("To", msg.To)
	form.Set("Body", msg.Body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return providers.Result{}, fmt.Errorf("signalwire: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.ProjectID, c.Token)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe. Unwrap the *url.Error layer
		// so the endpoint URL (which carries the ProjectID in its path) does
		// not leak into error text persisted as Message.ErrorText or logged.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("signalwire: post messages: %w", err))
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
			fmt.Errorf("signalwire: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad credentials, invalid number, payload): permanent.
		return providers.Result{}, fmt.Errorf("signalwire: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// spaceEndpoint builds the LaML Messages.json URL from a Space host and project
// id. The Space may be supplied bare ("example.signalwire.com") or with a
// scheme ("https://example.signalwire.com[/...]") — either form resolves to the
// canonical https LaML endpoint. A scheme-less host is assumed https.
func spaceEndpoint(space, projectID string) string {
	host := strings.TrimRight(space, "/")
	if strings.Contains(host, "://") {
		// Keep the supplied scheme/host (e.g. http:// for an httptest server),
		// dropping any trailing path the caller may have included.
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Scheme + "://" + u.Host
		}
	} else {
		host = "https://" + host
	}
	return host + "/api/laml/2010-04-01/Accounts/" + projectID + "/Messages.json"
}
