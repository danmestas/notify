package delivery

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	webpushprovider "github.com/danmestas/notify/internal/providers/webpush"
)

const (
	// webPushRetryDelay spaces the one in-send retry of devices that failed
	// transiently while others may have succeeded (#360).
	webPushRetryDelay = 2 * time.Second
	// webPushRetireAfter is how many consecutive permanent refusals retire a
	// subscription, the way a 404/410 already does (#360).
	webPushRetireAfter = 3
)

// webPushDevice is one device's outcome on a Web Push message, recorded in
// the message's extras.devices (#359). Host names the push service; the full
// endpoint URL identifies the device and is not echoed.
type webPushDevice struct {
	SubscriptionID string `json:"subscriptionId"`
	Host           string `json:"host"`
	// Outcome is "delivered"; "failed"; "retired", refused too many times
	// in a row and deleted; or "gone", reported dead by the push service
	// (404/410) and deleted.
	Outcome string `json:"outcome"`
	Status  int    `json:"status,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// fanOutWebPush sends msg to every subscription and returns each device's
// outcome, how many delivered, how many failed, and the last failure. A
// device that fails transiently gets one retry after webPushRetryDelay, so a
// timeout on one device is not lost because another device succeeded and
// the step will not be retried.
func (h *PushHandler) fanOutWebPush(ctx context.Context, scope domain.Scope, subscriberID string, sender webPushSender, subs []domain.WebPushSubscription, msg providers.PushMessage) (outcomes []webPushDevice, sent, failed int, lastErr error) {
	outcomes = make([]webPushDevice, len(subs))
	var retry []int
	send := func(i int) error {
		sub := subs[i]
		_, err := sender.SendToSubscription(ctx, webpushprovider.Subscription{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		}, msg)
		return err
	}
	settle := func(i int, err error) {
		sub := subs[i]
		d := webPushDevice{SubscriptionID: sub.ID, Host: endpointHost(sub.Endpoint)}
		switch {
		case err == nil:
			d.Outcome = "delivered"
			sent++
			// Best-effort: a store error must not fail a delivered push.
			_ = h.store.WebPushSubscriptions().ResetFailures(ctx, scope, subscriberID, sub.Endpoint)
		case errors.Is(err, webpushprovider.ErrSubscriptionGone):
			d.Outcome = "gone"
			_ = h.store.WebPushSubscriptions().DeleteByEndpoint(ctx, scope, subscriberID, sub.Endpoint)
		default:
			d.Outcome = "failed"
			failed++
			lastErr = err
			var se *webpushprovider.StatusError
			if errors.As(err, &se) {
				d.Status, d.Reason = se.Code, se.Reason
			} else {
				d.Reason = err.Error()
			}
			if countsAgainstDevice(err) && h.retire(ctx, scope, subscriberID, sub.Endpoint) {
				d.Outcome = "retired"
			}
		}
		outcomes[i] = d
	}

	for i := range subs {
		err := send(i)
		if err != nil && providers.IsTransient(err) {
			retry = append(retry, i)
			continue
		}
		settle(i, err)
	}
	if len(retry) > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(h.webPushRetryDelay):
		}
		for _, i := range retry {
			settle(i, send(i))
		}
	}
	return outcomes, sent, failed, lastErr
}

// retire counts a permanent refusal against the subscription and deletes it
// once it has been refused webPushRetireAfter times in a row. It reports
// whether the subscription was retired.
func (h *PushHandler) retire(ctx context.Context, scope domain.Scope, subscriberID, endpoint string) bool {
	n, err := h.store.WebPushSubscriptions().RecordFailure(ctx, scope, subscriberID, endpoint)
	if err != nil || n < webPushRetireAfter {
		return false
	}
	return h.store.WebPushSubscriptions().DeleteByEndpoint(ctx, scope, subscriberID, endpoint) == nil
}

// countsAgainstDevice reports whether a failure says the subscription itself
// is bad. Transient faults do not, and neither does 413: a payload too large
// is the message's fault, and would fail on every device alike.
func countsAgainstDevice(err error) bool {
	if providers.IsTransient(err) {
		return false
	}
	var se *webpushprovider.StatusError
	if errors.As(err, &se) && se.Code == http.StatusRequestEntityTooLarge {
		return false
	}
	return true
}

// endpointHost returns the push service host of a subscription endpoint.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}
