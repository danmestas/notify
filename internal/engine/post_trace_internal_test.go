package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var traceparentFormat = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`)

// TestPostInjectsTraceparent pins the HTTP-bridge seam: when the ctx handed to
// post() carries a span, the POST goes out with a traceparent header, so
// dagnats's bridge extractor stitches the resolve leg onto notify's trace with
// no dagnats change. This is the resolve path's contract — the poll loop
// deliberately runs without a span (see the plan), so poll POSTs inject
// nothing, which is the correct behaviour and not what this test covers.
func TestPostInjectsTraceparent(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("traceparent"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	e := &Engine{bridgeURL: srv.URL, http: &http.Client{Timeout: 5 * time.Second}}

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "root")
	defer span.End()

	if _, err := e.post(ctx, e.bridgeURL+"/v1/tasks/poll", []byte(`{}`)); err != nil {
		t.Fatalf("post: %v", err)
	}

	select {
	case v := <-got:
		if v == "" {
			t.Fatal("bridge POST carried no traceparent")
		}
		if !traceparentFormat.MatchString(v) {
			t.Fatalf("traceparent %q does not match W3C format", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bridge POST")
	}
}
