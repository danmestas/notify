package main

import (
	"log/slog"
	"os"

	"github.com/danmestas/notify/internal/observe"
)

// installTraceLogging stamps trace_id/span_id onto every record logged inside a
// span — but ONLY when tracing is enabled. It reports whether it installed, so
// the gate itself is testable (see trace_logging_test.go); silently losing this
// gate would regress every disabled operator's log format.
//
// Doing this unconditionally would switch notify's log output from the log
// package's default format to slog text format — and reroute stdlib log through
// slog — for every operator, including those who never opted into OTel. Gating
// on the endpoint keeps the disabled path byte-identical to pre-OTel notify.
//
// When enabled, the format change is an accepted deviation: correlating logs
// with traces requires the structured handler, and configuring an OTLP endpoint
// is the opt-in.
//
// The wrapped handler is an explicit TextHandler, NOT slog.Default()'s. Until
// the first SetDefault, slog's default handler emits through the log package,
// and SetDefault points log's output back at the slog default — so wrapping it
// would route log -> wrapper -> inner default -> log and recurse forever,
// hanging the process before it ever listens.
func installTraceLogging(otelEndpoint string) bool {
	if otelEndpoint == "" {
		return false
	}
	slog.SetDefault(slog.New(observe.NewLogHandler(slog.NewTextHandler(os.Stderr, nil))))
	return true
}
