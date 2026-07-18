package metrics

import (
	"encoding/json"
	"expvar"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// counterValue reads the flattened "<channel>.<outcome>" key out of the
// notify_delivery_total expvar map, returning 0 when the key is absent.
func counterValue(t *testing.T, key string) int64 {
	t.Helper()
	v := expvar.Get(deliveryMapName)
	if v == nil {
		t.Fatalf("expvar %q not published", deliveryMapName)
	}
	m, ok := v.(*expvar.Map)
	if !ok {
		t.Fatalf("expvar %q is %T, want *expvar.Map", deliveryMapName, v)
	}
	iv, ok := m.Get(key).(*expvar.Int)
	if !ok || iv == nil {
		return 0
	}
	return iv.Value()
}

func TestDeliveryCounter(t *testing.T) {
	before := counterValue(t, "email.delivered")
	Delivery("email", OutcomeDelivered)
	Delivery("email", OutcomeDelivered)
	if got := counterValue(t, "email.delivered") - before; got != 2 {
		t.Fatalf("email.delivered delta = %d, want 2", got)
	}
}

func TestNATSCounters(t *testing.T) {
	d0, r0 := natsDisconnects.Value(), natsReconnects.Value()
	NATSDisconnect()
	NATSReconnect()
	NATSReconnect()
	if got := natsDisconnects.Value() - d0; got != 1 {
		t.Fatalf("nats disconnect delta = %d, want 1", got)
	}
	if got := natsReconnects.Value() - r0; got != 2 {
		t.Fatalf("nats reconnect delta = %d, want 2", got)
	}
}

// TestHandler_OnlyNotifyVars guards the security-relevant property that the
// /metrics handler exposes only notify_ prefixed vars and never expvar's default
// "cmdline" (os.Args — would leak a -master-key-hex flag through the public
// tunnel) or "memstats". Body must be valid JSON carrying the notify_ keys.
func TestHandler_OnlyNotifyVars(t *testing.T) {
	// Ensure at least one notify_ var carries a value so the key is present.
	Delivery("email", OutcomeDelivered)

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(rr.Body)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body is not valid JSON: %v (body=%s)", err, body)
	}
	if _, ok := top[deliveryMapName]; !ok {
		t.Fatalf("body missing %q; got keys %v", deliveryMapName, keysOf(top))
	}
	for _, banned := range []string{"cmdline", "memstats"} {
		if _, ok := top[banned]; ok {
			t.Fatalf("body exposes default expvar var %q; want only notify_ keys", banned)
		}
	}
	for k := range top {
		if !strings.HasPrefix(k, "notify_") {
			t.Fatalf("body exposes non-notify_ key %q", k)
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNATSStatusFunc(t *testing.T) {
	SetNATSStatus("notify-test", func() string { return "CONNECTED" })
	v := expvar.Get(natsStatusName)
	if v == nil {
		t.Fatalf("expvar %q not published", natsStatusName)
	}
	got := v.(expvar.Func).Value().(map[string]string)
	if got["notify-test"] != "CONNECTED" {
		t.Fatalf("status[notify-test] = %q, want CONNECTED", got["notify-test"])
	}
}
