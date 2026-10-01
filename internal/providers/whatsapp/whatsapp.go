// Package whatsapp implements providers.ChatProvider against the WhatsApp
// Business (Meta Cloud) API
// (POST https://graph.facebook.com/v22.0/<phone_number_id>/messages).
// It POSTs a JSON body
// {messaging_product:"whatsapp", recipient_type:"individual", to:<MSISDN>,
// type:"text", text:{preview_url:false, body:<content>}} with a Bearer access
// token and reads the returned JSON messages[0].id as the provider message id.
// 2xx is success; 4xx is a permanent (plain) error; 5xx and transport faults
// are wrapped in providers.TransientError so the delivery handler can ask the
// engine to retry per 0001 §10.
package whatsapp

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
)

var _ providers.ChatProvider = (*Client)(nil)

// defaultBaseURL is the Meta Graph API host. Overridable in tests via BaseURL.
const defaultBaseURL = "https://graph.facebook.com/v22.0"

// Client posts chat messages through one WhatsApp Business phone number.
// AccessToken is the integration credential (Bearer auth); PhoneNumberID is the
// sending number's id, which forms the request path. A nil HTTPClient gets a
// 10s-timeout default per Send.
type Client struct {
	AccessToken   string
	PhoneNumberID string
	BaseURL       string // defaults to https://graph.facebook.com/v22.0
	HTTPClient    *http.Client
}

// textBody is the nested "text" object of a WhatsApp text message.
type textBody struct {
	PreviewURL bool   `json:"preview_url"`
	Body       string `json:"body"`
}

// messagePayload is the wire JSON shape POSTed to /<phone_number_id>/messages.
type messagePayload struct {
	MessagingProduct string   `json:"messaging_product"`
	RecipientType    string   `json:"recipient_type"`
	To               string   `json:"to"`
	Type             string   `json:"type"`
	Text             textBody `json:"text"`
}

// Send POSTs a WhatsApp text message to
// {BaseURL}/{PhoneNumberID}/messages with Bearer auth (AccessToken). The
// recipient MSISDN is ChatMessage.Address and the body is ChatMessage.Content.
// An empty AccessToken, PhoneNumberID, or Address fails closed (permanent). The
// provider message id is messages[0].id of Meta's JSON acknowledgement.
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	if c.AccessToken == "" {
		return providers.Result{}, fmt.Errorf("whatsapp: missing access token")
	}
	if c.PhoneNumberID == "" {
		return providers.Result{}, fmt.Errorf("whatsapp: missing phone number id")
	}
	if msg.Address == "" {
		return providers.Result{}, fmt.Errorf("whatsapp: empty recipient")
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := strings.TrimRight(base, "/") + "/" + c.PhoneNumberID + "/messages"

	body, err := json.Marshal(messagePayload{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		To:               msg.Address,
		Type:             "text",
		Text:             textBody{PreviewURL: false, Body: msg.Content},
	})
	if err != nil {
		return providers.Result{}, fmt.Errorf("whatsapp: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

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
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("whatsapp: post messages: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ack struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(respBody, &ack)
		var id string
		if len(ack.Messages) > 0 {
			id = ack.Messages[0].ID
		}
		return providers.Result{ProviderMessageID: id}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("whatsapp: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad token, invalid recipient, payload): permanent.
		return providers.Result{}, fmt.Errorf("whatsapp: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
