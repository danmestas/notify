// Package fcm implements providers.PushProvider against Firebase Cloud
// Messaging's HTTP v1 API
// (POST https://fcm.googleapis.com/v1/projects/{projectId}/messages:send,
// mirrors the reference fcm.provider which uses firebase-admin). the reference platform relies on the
// Admin SDK; we replicate the same end state — an OAuth2-bearer-authenticated
// v1 send — directly:
//
//   - the OAuth2 access token is minted from the service-account JSON via a
//     JWT-bearer grant (scope https://www.googleapis.com/auth/firebase.messaging),
//     cached and auto-refreshed by golang.org/x/oauth2 (~1h tokens);
//   - the message body is {"message":{token,notification:{title,body},data}}.
//
// A 2xx is a successful send; 4xx (bad token, unregistered, malformed) is a
// permanent fault carrying the response body; 5xx, token-mint failures, and
// transport faults are wrapped in providers.TransientError so the delivery
// handler can retry per 0001 §10.
package fcm

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
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// messagingScope is the OAuth2 scope an FCM v1 access token must carry.
const messagingScope = "https://www.googleapis.com/auth/firebase.messaging"

// defaultBaseURL is the FCM v1 endpoint host. Overridable via BaseURL in tests.
const defaultBaseURL = "https://fcm.googleapis.com"

// TokenSource yields a bearer access token for the FCM v1 API. *oauth2.Token
// sources (and the value returned by google.JWTConfigFromJSON(...).TokenSource)
// satisfy this; tests inject a stub so they never reach Google.
type TokenSource interface {
	Token() (*oauth2.Token, error)
}

// Client posts push notifications to FCM's HTTP v1 API for one Firebase
// project. ProjectID selects the messages:send path; Tokens mints the OAuth2
// bearer. A nil HTTPClient gets a 10s-timeout default per Send. BaseURL is
// empty in production (defaultBaseURL) and points at an httptest server in
// unit tests.
type Client struct {
	ProjectID  string
	Tokens     TokenSource
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient builds a Client from the raw service-account JSON and project id.
// It constructs a JWT-bearer token source over the firebase.messaging scope;
// the source caches and refreshes the access token internally. An invalid
// service-account JSON is a permanent configuration fault.
func NewClient(ctx context.Context, serviceAccountJSON []byte, projectID string) (*Client, error) {
	cfg, err := google.JWTConfigFromJSON(serviceAccountJSON, messagingScope)
	if err != nil {
		return nil, fmt.Errorf("fcm: parse service account: %w", err)
	}
	return &Client{
		ProjectID: projectID,
		Tokens:    cfg.TokenSource(ctx),
	}, nil
}

// fcmNotification is the notification block of an FCM v1 message.
type fcmNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

// fcmMessage is the "message" object of an FCM v1 send request.
type fcmMessage struct {
	Token        string            `json:"token"`
	Notification fcmNotification   `json:"notification"`
	Data         map[string]string `json:"data,omitempty"`
}

// fcmSendRequest is the FCM v1 messages:send body.
type fcmSendRequest struct {
	Message fcmMessage `json:"message"`
}

// fcmSendResponse is the v1 success envelope: {"name":"projects/{p}/messages/{id}"}.
type fcmSendResponse struct {
	Name string `json:"name"`
}

// Send posts a single message to
// {BaseURL or defaultBaseURL}/v1/projects/{ProjectID}/messages:send. The device
// (registration) token is msg.Topic, stored in the subscriber_channels row. The
// OAuth2 bearer is minted from the configured token source; a mint failure is
// transient (retry-safe). The provider message id is the "name" field of the
// success response.
func (c *Client) Send(ctx context.Context, msg providers.PushMessage) (providers.Result, error) {
	if strings.TrimSpace(msg.Topic) == "" {
		// Fail closed: an empty registration token is a permanent addressing fault.
		return providers.Result{}, fmt.Errorf("fcm: empty device token")
	}
	if c.ProjectID == "" {
		return providers.Result{}, fmt.Errorf("fcm: empty project id")
	}
	if c.Tokens == nil {
		return providers.Result{}, fmt.Errorf("fcm: no token source configured")
	}

	tok, err := c.Tokens.Token()
	if err != nil {
		// A token-mint failure (network to oauth2.googleapis.com, transient
		// signing issue) is retry-safe.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("fcm: mint access token: %w", err))
	}

	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	endpoint := fmt.Sprintf("%s/v1/projects/%s/messages:send", base, c.ProjectID)

	payload := fcmSendRequest{Message: fcmMessage{
		Token: msg.Topic,
		Notification: fcmNotification{
			Title: msg.Title,
			Body:  msg.Body,
		},
		Data: msg.Data,
	}}
	body, err := json.Marshal(payload)
	if err != nil {
		return providers.Result{}, fmt.Errorf("fcm: marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return providers.Result{}, fmt.Errorf("fcm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)

	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Transport fault or timeout: retry-safe.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("fcm: post %s: %w", endpoint, err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var env fcmSendResponse
		_ = json.Unmarshal(respBody, &env)
		return providers.Result{ProviderMessageID: env.Name}, nil
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("fcm: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx (bad token, unregistered, malformed): permanent.
		return providers.Result{}, fmt.Errorf("fcm: %s: status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}
