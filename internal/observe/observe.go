// Package observe is notify's single point of contact with OpenTelemetry.
// It hides SDK construction, W3C context propagation, and the enable/disable
// gate behind four functions so call sites gain one line each and never
// import the SDK.
//
// The package is a complete no-op unless an OTLP endpoint is configured:
// nothing is constructed, no goroutine runs, the otel globals are untouched,
// and every span is the noop implementation.
package observe

import (
	"context"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope every notify span is created under.
const tracerName = "notify"

// wire is the propagator used for outbound injection. It is deliberately a
// fixed W3C TraceContext rather than otel.GetTextMapPropagator(): notify only
// ever speaks W3C on the wire, and pinning it here means injection does not
// depend on global state that Setup may or may not have configured. When
// tracing is disabled the span context is invalid, so this injects nothing.
var wire = propagation.TraceContext{}

// Setup initialises the global OpenTelemetry trace pipeline and returns a
// shutdown func that flushes pending spans.
//
// When endpoint is empty, tracing is fully disabled: no exporter, no provider,
// no goroutines, and the otel globals are left alone so Tracer() hands back the
// noop tracer and log output stays byte-identical. The returned shutdown is a
// no-op.
//
// endpoint is a base URL, as OTEL_EXPORTER_OTLP_ENDPOINT defines it: spans
// are sent to <endpoint>/v1/traces.
//
// Only the endpoint, service name, and version passed here are honored. The
// broader OTEL_* autoconfiguration environment (samplers, headers, protocol
// selection, resource attributes) is deliberately NOT read — cmd/notify is the
// one place notify interprets its environment.
//
// Setup mutates process-wide otel state and must be called exactly once, from
// the server entrypoint only — never from library code or tests.
func Setup(ctx context.Context, endpoint, serviceName, version string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	// endpoint is OTLP's base URL (OTEL_EXPORTER_OTLP_ENDPOINT), to which the
	// traces signal path is appended; WithEndpointURL takes a full URL as is.
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimSuffix(endpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	// The overlay's schema URL is taken from resource.Default() rather than
	// from semconv.SchemaURL: resource.Merge rejects two operands carrying
	// different schema URLs, and the SDK's default resource advances its schema
	// with every SDK release. Pinning the overlay to whatever the linked SDK
	// already uses makes this merge correct across SDK bumps without having to
	// step the semconv import in lockstep. The semconv package is still the
	// source of the attribute *keys*, which are stable across those versions.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		resource.Default().SchemaURL(),
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// Tracer returns notify's tracer. It is the noop tracer until Setup has
// installed a real provider, so call sites need no gating of their own.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// InjectNATSHeader writes the W3C traceparent of ctx's span into h.
//
// The carrier is NATS-specific on purpose. nats.Header and http.Header are both
// map[string][]string, but their Get/Set differ in a way that silently breaks
// this seam: http.Header canonicalises keys ("traceparent" -> "Traceparent"),
// while nats.Header.Get is a plain case-SENSITIVE map lookup. dagnats extracts
// with hdr.Get("traceparent") — the lowercase W3C name — so injecting through
// propagation.HeaderCarrier writes a key dagnats can never find, and every run
// it starts becomes an orphan trace. Both sides still look healthy in isolation,
// which is why this went unnoticed until an end-to-end trace query in prod.
//
// Injecting an invalid (noop) span context writes nothing.
func InjectNATSHeader(ctx context.Context, h nats.Header) {
	wire.Inject(ctx, natsHeaderCarrier(h))
}

// natsHeaderCarrier adapts nats.Header to propagation.TextMapCarrier without the
// MIME canonicalisation http.Header applies, preserving the exact lowercase keys
// the W3C propagator emits.
type natsHeaderCarrier nats.Header

// Get returns the first value for key, matching case-sensitively — the same
// lookup semantics the receiving side uses.
func (c natsHeaderCarrier) Get(key string) string {
	if v := c[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Set stores value under the literal key, with no canonicalisation.
func (c natsHeaderCarrier) Set(key, value string) {
	c[key] = []string{value}
}

// Keys lists the carrier's keys as stored.
func (c natsHeaderCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}
