// Package webpush sends browser Web Push notifications (RFC8030/RFC8291,
// VAPID/RFC8292) via github.com/SherClockHolmes/webpush-go. Unlike every
// other push provider a single subscriber may hold many live device
// subscriptions (one per browser/tab), so Client exposes SendToSubscription —
// one call per stored webpush_subscriptions row — rather than the single
// per-subscriber Send the providers.PushProvider interface expects; the
// delivery handler (internal/delivery/push.go) does the per-subscriber
// fan-out. Client still implements providers.PushProvider (Send always fails)
// purely so it type-checks as the return value of
// connectivity.PushProviderFromIntegration's switch.
package webpush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	vapidpush "github.com/SherClockHolmes/webpush-go"

	"github.com/danmestas/notify/internal/providers"
)

// defaultTTL is the push service TTL applied when the step's control values
// (msg.Data["ttl"], seconds) are absent or unparsable.
const defaultTTL = 24 * time.Hour

// defaultSubject is the VAPID JWT subject (RFC8292) used when the org has no
// configured contact.
const defaultSubject = "mailto:notify@localhost"

// ErrSubscriptionGone marks a subscription the push service reports as
// permanently dead (404 or 410): the caller must delete the
// webpush_subscriptions row for it and stop retrying.
var ErrSubscriptionGone = errors.New("webpush: subscription gone")

// Subscription is one browser PushSubscription target: the endpoint URL and
// the two keys from PushSubscription.getKey(). Mirrors
// domain.WebPushSubscription without importing the domain package (this
// package must stay import-light like every other providers/* package).
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Client sends Web Push messages using one org/environment's minted VAPID
// keypair.
type Client struct {
	PublicKey  string
	PrivateKey string
	// Subject identifies the sender in the VAPID JWT: a mailto: or https: URI
	// the push service may use to contact the sender if it needs to. Empty
	// defaults to "mailto:notify@localhost".
	Subject    string
	HTTPClient *http.Client
}

// wirePayload is the JSON body decrypted client-side and handed to the
// browser's push event listener.
type wirePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Send implements providers.PushProvider for compile-time compatibility only.
// Web Push has no single per-subscriber address (a subscriber may hold many
// device subscriptions), so real delivery always goes through
// SendToSubscription — see the delivery handler's webpush fan-out.
func (c *Client) Send(context.Context, providers.PushMessage) (providers.Result, error) {
	return providers.Result{}, errors.New("webpush: Send is not supported; use SendToSubscription for per-device delivery")
}

// SendToSubscription renders msg as {"title","body","url","tag"}, encrypts it
// for sub per RFC8291, and POSTs it to sub.Endpoint with a VAPID
// Authorization header. url/tag come from msg.Data["url"]/msg.Data["tag"]
// (the same channel-agnostic Data map FCM/Expo read); TTL comes from
// msg.Data["ttl"] (seconds), defaulting to 24h.
//
// Classification: 2xx is success. 404/410 mean the push service has
// permanently dropped the subscription — returns ErrSubscriptionGone so the
// caller prunes the row. Other 4xx are permanent (a plain error). 5xx and
// transport faults are wrapped in providers.TransientError. No returned error
// ever includes the VAPID private key or the subscription's auth secret —
// only wire status codes and the (non-secret) response body.
func (c *Client) SendToSubscription(ctx context.Context, sub Subscription, msg providers.PushMessage) (providers.Result, error) {
	body, err := json.Marshal(wirePayload{
		Title: msg.Title,
		Body:  msg.Body,
		URL:   msg.Data["url"],
		Tag:   msg.Data["tag"],
	})
	if err != nil {
		return providers.Result{}, fmt.Errorf("webpush: marshal payload: %w", err)
	}

	subject := c.Subject
	if subject == "" {
		subject = defaultSubject
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := vapidpush.SendNotificationWithContext(ctx, body, &vapidpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     vapidpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &vapidpush.Options{
		HTTPClient:      hc,
		Subscriber:      subject,
		TTL:             int(ttl(msg.Data).Seconds()),
		VAPIDPublicKey:  c.PublicKey,
		VAPIDPrivateKey: c.PrivateKey,
	})
	if err != nil {
		// Every failure surfacing here is either an encryption/build fault
		// (malformed subscription keys — permanent) or the underlying
		// http.Client's transport error (timeout, connection refused —
		// transient). webpush-go returns no sentinel to tell them apart, and a
		// malformed subscription is rare next to transport faults, so this
		// mirrors the ntfy client's "transport error => transient" default.
		return providers.Result{}, providers.NewTransientError(fmt.Errorf("webpush: send: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return providers.Result{}, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return providers.Result{}, ErrSubscriptionGone
	case resp.StatusCode >= 500:
		return providers.Result{}, providers.NewTransientError(
			fmt.Errorf("webpush: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		// 4xx other than 404/410 (bad request, auth): permanent.
		return providers.Result{}, fmt.Errorf("webpush: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// ttl reads the "ttl" key (seconds) from data, falling back to defaultTTL
// when absent, empty, or unparsable.
func ttl(data map[string]string) time.Duration {
	if data == nil {
		return defaultTTL
	}
	raw, ok := data["ttl"]
	if !ok || raw == "" {
		return defaultTTL
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultTTL
	}
	return time.Duration(secs) * time.Second
}
