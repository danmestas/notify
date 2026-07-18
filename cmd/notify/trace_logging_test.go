package main

import (
	"log/slog"
	"testing"
)

// TestInstallTraceLogging_DisabledLeavesSlogUntouched is the regression guard for
// the OTel slice's "byte-identical logs when disabled" criterion. Installing the
// trace-stamping handler unconditionally would switch every operator's log output
// from the log package's default format to slog text format — including operators
// who never configured an OTLP endpoint. An earlier revision did exactly that; if
// a refactor drops the endpoint gate again, this fails instead of shipping.
func TestInstallTraceLogging_DisabledLeavesSlogUntouched(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })

	if installTraceLogging("") {
		t.Fatal("installTraceLogging(\"\") = true, want false: the disabled path must not install a handler")
	}
	if slog.Default() != before {
		t.Fatal("disabled path replaced slog.Default(); log output must stay byte-identical to pre-OTel notify")
	}
}

// TestInstallTraceLogging_EnabledInstalls pins the other half: with an endpoint
// configured the operator has opted in, so the handler must actually be installed
// (otherwise logs carry no trace_id and the correlation half of the slice is dead).
func TestInstallTraceLogging_EnabledInstalls(t *testing.T) {
	before := slog.Default()
	t.Cleanup(func() { slog.SetDefault(before) })

	if !installTraceLogging("http://127.0.0.1:4318") {
		t.Fatal("installTraceLogging(endpoint) = false, want true: the enabled path must install the handler")
	}
	if slog.Default() == before {
		t.Fatal("enabled path did not replace slog.Default(); trace_id/span_id would never be stamped")
	}
}
