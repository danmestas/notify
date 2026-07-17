package metrics

import (
	"expvar"
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
