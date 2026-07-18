package observe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/danmestas/notify/internal/observe"
)

// TestLogHandlerStampsTraceID pins both halves of the correlation contract:
// inside a recording span the record gains matching trace_id/span_id; outside
// one, output is byte-identical to the wrapped handler's (no keys added).
func TestLogHandlerStampsTraceID(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{})
	log := slog.New(observe.NewLogHandler(inner))

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("test").Start(context.Background(), "root")
	log.InfoContext(ctx, "inside")
	sc := span.SpanContext()
	span.End()

	rec := decodeLast(t, &buf)
	if got := rec["trace_id"]; got != sc.TraceID().String() {
		t.Fatalf("trace_id = %v, want %s", got, sc.TraceID())
	}
	if got := rec["span_id"]; got != sc.SpanID().String() {
		t.Fatalf("span_id = %v, want %s", got, sc.SpanID())
	}

	buf.Reset()
	log.InfoContext(context.Background(), "outside")
	rec = decodeLast(t, &buf)
	if _, ok := rec["trace_id"]; ok {
		t.Fatal("trace_id stamped outside a span")
	}
	if _, ok := rec["span_id"]; ok {
		t.Fatal("span_id stamped outside a span")
	}
}

func decodeLast(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", buf.String(), err)
	}
	return rec
}
