package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
)

// TestRoot_RedirectsToConsole guards that the bare host root ("/") sends the
// visitor to the console instead of 404ing. notify serves no landing page at
// "/", so a plain visit via the Cloudflare tunnel or the Tailscale name would
// otherwise show a raw "404 page not found".
func TestRoot_RedirectsToConsole(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	id := identity.New(st, make([]byte, 32))
	if _, _, err := id.Bootstrap(ctx, "Acme", "dev"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	inbox := inboxapi.New(st)
	handler := httpapi.New(id, nil, nil, inbox, nil, nil, nil, nil, nil, nil, nil, st, nil, nil, nil)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusFound {
		t.Fatalf("GET /: status = %d, want %d (redirect)", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/console/" {
		t.Fatalf("GET / Location = %q, want %q", loc, "/console/")
	}
}
