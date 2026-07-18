package observe

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler stamps trace_id/span_id onto records whose context carries a
// valid span, and otherwise forwards untouched.
//
// This is a wrapper rather than the otelslog bridge on purpose: no log export
// is in scope, so the bridge's exporter machinery would be dead weight. The
// wrapper costs one otel trace API import and, crucially, is inert when
// tracing is disabled — no span means no fields, so stderr stays byte-identical
// to the unwrapped handler's output.
type logHandler struct{ inner slog.Handler }

// NewLogHandler wraps inner so every record logged inside a span carries the
// trace_id and span_id needed to join logs to traces.
func NewLogHandler(inner slog.Handler) slog.Handler { return &logHandler{inner: inner} }

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{inner: h.inner.WithGroup(name)}
}
