package observe_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"testing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/observe"
)

// TestInjectNATSHeader_UsesLowercaseW3CKey pins the header-key casing on the NATS
// seam, which is not a style question but a correctness one.
//
// nats.Header.Get is a plain, case-SENSITIVE map lookup (h[key]) — unlike
// http.Header.Get, which canonicalises. dagnats extracts with
// hdr.Get("traceparent"), the lowercase W3C name. Injecting through
// propagation.HeaderCarrier routes to http.Header.Set, which canonicalises the
// key to "Traceparent" — so dagnats silently finds nothing and every run it
// starts becomes an orphan trace, disconnected from the notify trigger that
// caused it.
//
// That failure is invisible to both services: notify reports a healthy injected
// span, dagnats reports a healthy root span, and only an end-to-end trace query
// shows the two are unrelated. This was observed in production before it was
// caught in a test, so the assertion below deliberately checks BOTH that the
// lowercase key is present and that the canonical form is absent.
func TestInjectNATSHeader_UsesLowercaseW3CKey(t *testing.T) {
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	ctx := oteltrace.ContextWithSpanContext(context.Background(), oteltrace.NewSpanContext(
		oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: oteltrace.FlagsSampled,
			Remote:     true,
		},
	))

	h := nats.Header{}
	observe.InjectNATSHeader(ctx, h)

	// The lowercase W3C key must be the one on the wire — this is the exact
	// lookup dagnats performs.
	if got := h.Get("traceparent"); got == "" {
		t.Fatalf("no lowercase \"traceparent\" on the NATS header; dagnats cannot stitch. header=%v", h)
	} else if !traceparentRe.MatchString(got) {
		t.Fatalf("traceparent = %q, want W3C form", got)
	}
	// The canonicalised form must NOT be what we rely on: if injection ever
	// routes back through http.Header.Set, this catches it.
	if _, canonical := h["Traceparent"]; canonical {
		t.Fatalf("header carries canonicalised \"Traceparent\"; nats.Header.Get is case-sensitive so dagnats would miss it. header=%v", h)
	}
	if len(h["traceparent"]) != 1 {
		t.Fatalf("expected exactly one lowercase traceparent value, got %v", h["traceparent"])
	}
}

// traceparentRe is the W3C traceparent format dagnats's extractor accepts.
var traceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

// TestSetupUnsetNoop pins the central gating invariant: with no endpoint
// configured, Setup constructs nothing at all — no exporter, no provider, no
// goroutines — and leaves the otel globals untouched, so spans stay noop.
func TestSetupUnsetNoop(t *testing.T) {
	before := otel.GetTracerProvider()
	goBefore := runtime.NumGoroutine()

	shutdown, err := observe.Setup(context.Background(), "", "notify", "test")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown func")
	}
	if after := otel.GetTracerProvider(); after != before {
		t.Fatalf("Setup mutated the global TracerProvider: %T -> %T", before, after)
	}
	if delta := runtime.NumGoroutine() - goBefore; delta != 0 {
		t.Fatalf("Setup started %d goroutine(s) while disabled", delta)
	}

	_, span := observe.Tracer().Start(context.Background(), "notify.test")
	defer span.End()
	if span.SpanContext().IsValid() {
		t.Fatal("expected a noop (invalid) span context while disabled")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestSetupEnabledInstallsSDKProvider pins the other half of the gate: with an
// endpoint configured, Setup must actually succeed and install the SDK provider.
// It exists because a semconv/SDK schema-URL mismatch inside resource.Merge is
// invisible to the disabled path yet makes the binary refuse to boot whenever
// OTEL_EXPORTER_OTLP_ENDPOINT is set.
//
// It mutates process-wide otel state, which is acceptable in this package's
// tests only because Setup is the thing under test; nothing here asserts on the
// globals afterwards.
func TestSetupEnabledInstallsSDKProvider(t *testing.T) {
	// Never contacted: the batch processor only exports on flush, and shutdown
	// happens after Close. The server exists to give a real, reachable URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	shutdown, err := observe.Setup(context.Background(), srv.URL, "notify", "test")
	if err != nil {
		t.Fatalf("Setup with endpoint %q: %v", srv.URL, err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil shutdown func")
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("global TracerProvider is %T, want *sdktrace.TracerProvider", otel.GetTracerProvider())
	}
	if tp == nil {
		t.Fatal("global TracerProvider is a nil *sdktrace.TracerProvider")
	}

	ctx, span := observe.Tracer().Start(context.Background(), "notify.test")
	span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("expected a recording span context while enabled")
	}

	h := nats.Header{}
	observe.InjectNATSHeader(ctx, h)
	if got := h.Get("traceparent"); !traceparentRe.MatchString(got) {
		t.Fatalf("traceparent %q does not match W3C format (header=%v)", got, h)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestInjectNATSHeaderCarriesTraceparent pins the wire contract with dagnats:
// the exact header key casing ("traceparent") and the W3C value format its
// propagation.HeaderCarrier-based extractor expects.
func TestInjectNATSHeaderCarriesTraceparent(t *testing.T) {
	// Test-local provider only — never the otel global (Setup owns that).
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "root")
	defer span.End()

	h := nats.Header{}
	observe.InjectNATSHeader(ctx, h)

	// Casing contract with dagnats (#527): dagnats does NOT extract through a
	// canonicalising carrier — it gates on hdr.Get("traceparent"), and
	// nats.Header.Get is a case-SENSITIVE map lookup. So the key on the wire
	// must be the lowercase W3C name; a canonicalised "Traceparent" is silently
	// invisible to it. (This assertion previously read "Traceparent" on the
	// mistaken belief that HeaderCarrier canonicalised on both ends, which let a
	// permanently-orphaned trace seam ship green.)
	got := h.Get("traceparent")
	if got == "" {
		t.Fatalf("no traceparent injected under key %q; header=%v", "traceparent", h)
	}
	if !traceparentRe.MatchString(got) {
		t.Fatalf("traceparent %q does not match W3C format", got)
	}
}
