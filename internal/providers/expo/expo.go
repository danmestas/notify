// Package expo implements providers.PushProvider against Expo's push service
// (https://exp.host/--/api/v2/push/send, mirrors the reference expo.provider): POST a
// JSON array of one message {to, title, body, data} and read back a
// {"data":[{status,id,message}]} envelope. A per-ticket status of "ok" is a
// successful send; "error" is a permanent fault carrying the Expo message
// (e.g. "DeviceNotRegistered"). A 2xx response with a malformed envelope, a
// 4xx, or an explicit ticket error is permanent; 5xx and transport faults are
// wrapped in providers.TransientError so the delivery handler can retry per
// 0001 §10.
package expo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

// defaultSendURL is Expo's push-send endpoint. Overridable via BaseURL in tests.
const defaultSendURL = "https://exp.host/--/api/v2/push/send"

// Client posts push notifications to Expo's push service with one optional
// access token. Fields mirror connectivity.ExpoConfig. A nil HTTPClient gets a
// 10s-timeout default per Send. BaseURL is empty in production (defaultSendURL
// is used) and points at an httptest server in unit tests.
type Client struct {
	AccessToken string
	BaseURL     string
	HTTPClient  *http.Client
}

// expoMessage is one element of the request array Expo's send endpoint expects.
type expoMessage struct {
	To    string         `json:"to"`
	Title string         `json:"title,omitempty"`
	Body  string         `json:"body,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// expoResponse is the {"data":[{status,id,message}]} envelope Expo returns.
type expoResponse struct {
	Data []struct {
		Status  string `json:"status"`
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"data"`
}

// Send posts a single-message array to {BaseURL or defaultSendURL}. The device
// token is msg.Topic (the ExponentPushToken stored in the subscriber_channels
// row). When an access token is configured it is sent as Authorization: Bearer.
// The provider message id is the ticket id from a status:"ok" reply.
func (c *Client) Send(ctx context.Context, msg providers.PushMessage) (providers.Result, error) {
	endpoint := c.BaseURL
	if endpoint == "" {
		endpoint = defaultSendURL
	}
	if strings.TrimSpace(msg.Topic) == "" {
		// Fail closed: an empty device token is a permanent addressing fault.
		return providers.Result{}, fmt.Errorf("expo: empty device token")
	}

	var expoData map[string]any
	if len(msg.Data) > 0 {
		expoData = make(map[string]any, len(msg.Data))
		for k, v := range msg.Data {
			expoData[k] = v
		}
	}
	payload := []expoMessage{{
		To:    msg.Topic,
		Title: msg.Title,
		Body:  msg.Body,
		Data:  expoData,
	}}
	body, err := json.Marshal(payload)
	if err != nil {
		return providers.Result{}, fmt.Errorf("expo: marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("expo: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("expo: post %s: %w", endpoint, err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	switch {
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("expo: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// 4xx (auth, bad request, payload): permanent.
		return providers.Result{}, fmt.Errorf("expo: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var env expoResponse
	if err := json.Unmarshal(respBody, &env); err != nil {
		return providers.Result{}, fmt.Errorf("expo: decode response: %w", err)
	}
	if len(env.Data) == 0 {
		return providers.Result{}, fmt.Errorf("expo: empty ticket data in response")
	}
	ticket := env.Data[0]
	switch ticket.Status {
	case "ok":
		return providers.Result{ProviderMessageID: ticket.ID}, nil
	case "error":
		// Permanent: surface the Expo message (e.g. DeviceNotRegistered).
		return providers.Result{}, fmt.Errorf("expo: %s", strings.TrimSpace(ticket.Message))
	default:
		return providers.Result{}, fmt.Errorf("expo: unexpected ticket status %q", ticket.Status)
	}
}
