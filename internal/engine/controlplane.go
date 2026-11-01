package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/observe"
)

// Control-plane subjects: dagnats' routes scoped to the notify program
// (api.p.<program>.*). The server checks that every workflow name, step task
// type and event type is under notify., and answers a run of another
// program's workflow like an unknown run. Bodies and replies match the global
// api.workflows.register / api.runs.* routes, which a program's restricted
// NATS user may not publish. Every reply is JSON; a failure is an
// {"error": "..."} envelope.
const (
	subjRegister = "api.p.notify.workflows.register"
	subjStartRun = "api.p.notify.runs.start"
	subjGetRun   = "api.p.notify.runs.get"
)

// RegisterWorkflow registers def with the engine. Returns an error if the
// engine rejects the definition.
func (e *Engine) RegisterWorkflow(ctx context.Context, def WorkflowDef) error {
	body, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("marshal workflow def: %w", err)
	}
	reply, err := e.request(ctx, subjRegister, body)
	if err != nil {
		return err
	}
	return decodeError(reply)
}

// StartRun triggers a run of the named workflow with the given input and
// returns the new run ID.
func (e *Engine) StartRun(ctx context.Context, workflow string, input json.RawMessage) (string, error) {
	body, err := json.Marshal(struct {
		Workflow string          `json:"workflow"`
		Input    json.RawMessage `json:"input,omitempty"`
	}{Workflow: workflow, Input: input})
	if err != nil {
		return "", fmt.Errorf("marshal start-run request: %w", err)
	}
	reply, err := e.request(ctx, subjStartRun, body)
	if err != nil {
		return "", err
	}
	if err := decodeError(reply); err != nil {
		return "", err
	}
	var res struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(reply, &res); err != nil {
		return "", fmt.Errorf("decode start-run reply: %w", err)
	}
	return res.RunID, nil
}

// GetRun fetches the current snapshot for a run. The request body is the plain
// run-id bytes (not JSON), per the dagnats api.runs.get contract.
func (e *Engine) GetRun(ctx context.Context, runID string) (RunSnapshot, error) {
	reply, err := e.request(ctx, subjGetRun, []byte(runID))
	if err != nil {
		return RunSnapshot{}, err
	}
	if err := decodeError(reply); err != nil {
		return RunSnapshot{}, err
	}
	var snap RunSnapshot
	if err := json.Unmarshal(reply, &snap); err != nil {
		return RunSnapshot{}, fmt.Errorf("decode run snapshot: %w", err)
	}
	return snap, nil
}

// request issues a NATS request/reply, honoring ctx cancellation.
func (e *Engine) request(ctx context.Context, subject string, body []byte) ([]byte, error) {
	ctx, span := observe.Tracer().Start(
		ctx, "notify.engine.request",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("nats.subject", subject)),
	)
	defer span.End()

	// RequestMsgWithContext (not RequestWithContext) so the request carries a
	// W3C traceparent: dagnats stitches the run it starts onto notify's trace.
	req := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	observe.InjectNATSHeader(ctx, req.Header)

	msg, err := e.nc.RequestMsgWithContext(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("nats request %q: %w", subject, err)
	}
	return msg.Data, nil
}

// decodeError returns a Go error if reply is a {"error":...} envelope.
func decodeError(reply []byte) error {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(reply, &env); err == nil && env.Error != "" {
		return fmt.Errorf("engine: %s", env.Error)
	}
	return nil
}
