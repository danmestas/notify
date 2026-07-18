package engine_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

var traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

// TestRequestInjectsTraceparent pins the NATS control-plane seam: every
// api.runs.* request carries a W3C traceparent header so dagnats can stitch
// the run trace onto notify's trigger trace. It fails against the headerless
// nc.RequestWithContext this used to use.
func TestRequestInjectsTraceparent(t *testing.T) {
	natsURL, bridgeURL := dagtest.StartSidecar(t)

	// Independent connection that watches the control subject alongside
	// dagnats; whichever replies first satisfies StartRun, and we only assert
	// on what went out on the wire.
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	got := make(chan nats.Header, 1)
	sub, err := nc.Subscribe("api.runs.start", func(msg *nats.Msg) {
		select {
		case got <- msg.Header:
		default:
		}
		_ = msg.Respond([]byte(`{"run_id":"r1"}`))
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	e, err := engine.New(natsURL, bridgeURL)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Stop()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "root")
	defer span.End()

	// The reply may come from dagnats (an error envelope for an unknown
	// workflow) or from our subscriber; either is fine — the header is what
	// this test pins.
	_, _ = e.StartRun(ctx, "no-such-workflow", nil)

	select {
	case h := <-got:
		// Canonical casing is the wire contract — see observe's
		// TestInjectNATSHeaderCarriesTraceparent.
		tpHdr := h.Get("Traceparent")
		if tpHdr == "" {
			t.Fatalf("api.runs.start carried no traceparent; header=%v", h)
		}
		if !traceparentRe.MatchString(tpHdr) {
			t.Fatalf("traceparent %q does not match W3C format", tpHdr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for api.runs.start")
	}
}
