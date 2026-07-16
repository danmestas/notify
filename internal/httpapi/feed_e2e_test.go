package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
)

// TestFeedStream_RequiresApiKey proves GET /v1/feed/stream is mounted
// inside httpapi's authenticated group (alongside the realtime gateway):
// a request with no Authorization header is rejected before it ever
// reaches the feedapi gateway.
func TestFeedStream_RequiresApiKey(t *testing.T) {
	natsURL, _ := dagtest.StartSidecar(t)
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	bus, err := eventbus.Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("eventbus.Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	inbox := inboxapi.New(st)
	feed := feedapi.New(bus)

	if _, _, err := id.Bootstrap(ctx, "Acme", "dev"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srv := httptest.NewServer(httpapi.New(id, nil, nil, inbox, nil, feed, nil, nil, nil, nil, nil, nil, nil, nil, nil))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/feed/stream?topic=orders")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route must be inside the auth group)", resp.StatusCode)
	}
}
