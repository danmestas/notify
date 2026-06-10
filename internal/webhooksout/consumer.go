// Package webhooksout consumes notify's delivery-status events off the event
// bus and POSTs each raw event to the active outbound webhook endpoints
// configured for the event's tenancy scope (OrgID/EnvID).
package webhooksout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/store"
)

const (
	maxAttempts = 3
	retryDelay  = 50 * time.Millisecond
)

// Consumer fans delivery-status events out to per-scope webhook endpoints.
type Consumer struct {
	bus    *eventbus.Bus
	store  *store.Store
	client *http.Client
}

// New builds a Consumer bound to bus and st with a short-timeout HTTP client.
func New(bus *eventbus.Bus, st *store.Store) *Consumer {
	return &Consumer{
		bus:   bus,
		store: st,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// Start subscribes to every environment's delivery-status events. For each
// event it parses the tenancy scope from the message JSON, looks up the active
// webhook endpoints for that scope, and POSTs the raw event bytes to each
// endpoint URL with a small bounded retry on error/non-2xx. The returned stop
// func unsubscribes.
func (c *Consumer) Start(ctx context.Context) (stop func() error, err error) {
	unsub, err := c.bus.SubscribeAllDelivery(func(data []byte) {
		c.dispatch(ctx, data)
	})
	if err != nil {
		return nil, fmt.Errorf("webhooksout: Start: %w", err)
	}
	return unsub, nil
}

// dispatch resolves the scope from the event and POSTs to each active endpoint.
func (c *Consumer) dispatch(ctx context.Context, data []byte) {
	var hdr struct {
		OrgID string
		EnvID string
	}
	if err := json.Unmarshal(data, &hdr); err != nil {
		return
	}
	scope := domain.Scope{OrgID: hdr.OrgID, EnvID: hdr.EnvID}

	endpoints, err := c.store.WebhookEndpoints().ListActiveByEnv(ctx, scope)
	if err != nil {
		return
	}
	for _, e := range endpoints {
		c.post(ctx, e.URL, data)
	}
}

// post sends data as application/json to url, retrying up to maxAttempts on
// transport error or non-2xx response.
func (c *Consumer) post(ctx context.Context, url string, data []byte) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
	}
}
