package eventbus

import "testing"

// TestDialNATSOptions pins the resilience policy DialNATS must apply to every
// notify NATS connection: infinite reconnects, jittered backoff, and all three
// lifecycle handlers wired. Without these, a dagnats outage longer than the
// nats.go default cap (60 attempts × ~2s) permanently kills the connection and
// leaves a zombie process (HTTP up, poll loop dead).
func TestDialNATSOptions(t *testing.T) {
	natsURL := startSidecar(t)

	nc, err := DialNATS(natsURL, "notify-test", "dagnats sidecar (-nats-url)")
	if err != nil {
		t.Fatalf("DialNATS: %v", err)
	}
	t.Cleanup(nc.Close)

	if got := nc.Opts.MaxReconnect; got != -1 {
		t.Errorf("MaxReconnect = %d, want -1 (infinite)", got)
	}
	if got := nc.Opts.ReconnectJitter; got <= 0 {
		t.Errorf("ReconnectJitter = %v, want > 0", got)
	}
	if nc.Opts.DisconnectedErrCB == nil {
		t.Error("DisconnectedErrCB handler is nil, want set")
	}
	if nc.Opts.ReconnectedCB == nil {
		t.Error("ReconnectedCB handler is nil, want set")
	}
	if nc.Opts.ClosedCB == nil {
		t.Error("ClosedCB handler is nil, want set")
	}
	if got := nc.Opts.Name; got != "notify-test" {
		t.Errorf("Name = %q, want %q", got, "notify-test")
	}
}
