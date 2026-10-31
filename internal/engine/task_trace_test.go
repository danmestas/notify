package engine_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danmestas/dagnats/dag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/engine"
)

// TestTaskSpanContinuesTheTriggersTrace proves a delivery's span hangs off the
// trace that started its run: StartRun carries the caller's traceparent to
// dagnats, dagnats stamps it on the dispatched task, the worker restores it
// into tc.Context(), and the engine's notify.delivery.send span is its child.
// Losing any link orphans every delivery trace.
func TestTaskSpanContinuesTheTriggersTrace(t *testing.T) {
	// The in-process dagnats installs its own global provider at start, so
	// the recording one goes in after it.
	e, _ := startEngine(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})

	got := make(chan trace.SpanContext, 1)
	e.Handle("send.in_app", func(ctx context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
		got <- trace.SpanContextFromContext(ctx)
		return json.RawMessage(`{}`), nil
	})
	if err := e.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}

	ctx, root := tp.Tracer("test").Start(context.Background(), "trigger")
	if err := e.RegisterWorkflow(ctx, engine.WorkflowDef{
		Name:  "wf-traced",
		Steps: []engine.StepDef{{ID: "only", Task: "send.in_app", Type: dag.StepTypeNormal}},
	}); err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	if _, err := e.StartRun(ctx, "wf-traced", nil); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	root.End()

	var sc trace.SpanContext
	select {
	case sc = <-got:
	case <-time.After(15 * time.Second):
		t.Fatal("task never ran")
	}
	if want := root.SpanContext().TraceID(); sc.TraceID() != want {
		t.Fatalf("handler span trace = %s, want the trigger's %s", sc.TraceID(), want)
	}

	// The handler's span is the engine's own, named for the delivery, and its
	// parent is a span of the same trace (the worker's dispatch span).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range rec.Ended() {
			if s.SpanContext().SpanID() != sc.SpanID() {
				continue
			}
			if s.Name() != "notify.delivery.send" {
				t.Fatalf("handler span name = %q, want notify.delivery.send", s.Name())
			}
			if !s.Parent().IsValid() || s.Parent().TraceID() != sc.TraceID() {
				t.Fatalf("handler span parent = %v, want a span of trace %s", s.Parent(), sc.TraceID())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name()+"/"+s.SpanContext().SpanID().String())
	}
	t.Fatalf("the handler's span %s never ended; ended: %v", sc.SpanID(), names)
}
