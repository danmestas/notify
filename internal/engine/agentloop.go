package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/observe"
)

// This file extends the engine with dagnats AGENT-LOOP support over the
// existing HTTP bridge. dagnats already implements the durable loop on the wire
// (poll carries Iteration; resolve supports continue/complete; a step.continue
// event re-enqueues the step for the next iteration). The engine maps that onto
// a small handler API without the caller ever touching NATS or HTTP.
//
// Checkpoint durability mirrors dagnats's native worker exactly: state is
// stored in the "checkpoints" KV bucket at key {runID}.{stepID} and read back
// on the next iteration. The bridge exposes no checkpoint-read endpoint and the
// poll payload omits the checkpoint, so reads go straight to KV over NATS — the
// same connection the control plane already uses. Streaming likewise mirrors
// the native PutStream: a core-NATS publish to stream.{runID}.{stepID}.

// AgentLoopConfig bounds an agent-loop step. MaxIterations/MaxDuration/LoopDelay
// mirror dagnats's dag.AgentLoopConfig (the JSON config blob; durations marshal
// as nanoseconds). Timeout bounds a single iteration's task dispatch and is
// emitted on the StepDef itself, not inside the config blob.
type AgentLoopConfig struct {
	MaxIterations int           `json:"max_iterations"`
	MaxDuration   time.Duration `json:"max_duration,omitempty"`
	LoopDelay     time.Duration `json:"loop_delay,omitempty"`
	Timeout       time.Duration `json:"-"`
}

// AgentLoopStep builds a StepDef for an agent-loop step that the dagnats engine
// will drive iteratively. Type is the dagnats wire string "agent_loop"; the
// MaxIterations/MaxDuration/LoopDelay bounds ride StepDef.Config and Timeout
// rides StepDef.Timeout, matching the JSON emitted by dag.WorkflowBuilder's
// AgentLoop(...).With* chain.
func AgentLoopStep(id, task string, cfg AgentLoopConfig) StepDef {
	// Timeout is excluded from the config blob via its `json:"-"` tag; this
	// marshal cannot fail for AgentLoopConfig's fixed shape.
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic("engine: marshal AgentLoopConfig: " + err.Error())
	}
	return StepDef{
		ID:      id,
		Task:    task,
		Type:    "agent_loop",
		Config:  raw,
		Timeout: cfg.Timeout,
	}
}

// AgentLoopFunc handles one iteration of an agent-loop task. It reads the prior
// checkpoint and current iteration from lc, does its work, and must end the
// iteration with exactly one terminal action: lc.Continue (run again),
// lc.Complete (finish the step), or by returning an error (fail the step). A
// handler that returns without Continue or Complete fails the task to keep it
// from hanging in-flight.
type AgentLoopFunc func(ctx context.Context, lc *AgentLoopContext) error

// HandleAgentLoop registers an agent-loop handler for a task type. Like Handle,
// it must be called before StartWorkers. A task type cannot be registered as
// both a simple Handle and an agent-loop handler.
func (e *Engine) HandleAgentLoop(taskType string, fn AgentLoopFunc) {
	if e.started {
		panic("engine: HandleAgentLoop called after StartWorkers")
	}
	if _, dup := e.handlers[taskType]; dup {
		panic("engine: HandleAgentLoop conflicts with Handle for task type " + taskType)
	}
	e.agentHandlers[taskType] = fn
}

// AgentLoopContext is the per-iteration handle passed to an AgentLoopFunc. It
// exposes the polled task plus the prior checkpoint, and the terminal actions
// that resolve the iteration against the bridge.
type AgentLoopContext struct {
	e          *Engine
	ctx        context.Context
	task       TaskPayload
	checkpoint []byte
	resolved   bool
}

// Iteration is the 0-based loop iteration: 0 on first dispatch, advancing by one
// per Continue.
func (lc *AgentLoopContext) Iteration() int { return lc.task.Iteration }

// Checkpoint returns the durable state persisted by the previous iteration's
// Continue, or nil on iteration 0 (no prior checkpoint).
func (lc *AgentLoopContext) Checkpoint() []byte { return lc.checkpoint }

// Input returns the step input resolved by the engine for this iteration.
func (lc *AgentLoopContext) Input() json.RawMessage { return lc.task.Input }

// RunID returns the run this task belongs to.
func (lc *AgentLoopContext) RunID() string { return lc.task.RunID }

// StepID returns the agent-loop step's id.
func (lc *AgentLoopContext) StepID() string { return lc.task.StepID }

// errAlreadyResolved guards against a handler issuing a second terminal action.
var errAlreadyResolved = errors.New("engine: agent-loop iteration already resolved")

// Continue persists checkpoint (when non-nil) for the next iteration and
// resolves the task with outcome "continue", so the dagnats engine re-enqueues
// the step with Iteration+1. The checkpoint is stored in the "checkpoints" KV
// at key {runID}.{stepID} and surfaces as Checkpoint() on the next iteration.
func (lc *AgentLoopContext) Continue(checkpoint []byte) error {
	if lc.resolved {
		return errAlreadyResolved
	}
	if checkpoint != nil {
		if err := lc.e.putCheckpoint(lc.ctx, lc.task.TaskID, checkpoint); err != nil {
			return err
		}
	}
	lc.resolved = true
	return lc.e.resolve(lc.ctx, lc.task.TaskID, resolveRequest{Action: "continue"})
}

// Complete resolves the task with outcome "complete", finishing the agent-loop
// step (and, for a terminal step, the run) with output.
func (lc *AgentLoopContext) Complete(output json.RawMessage) error {
	if lc.resolved {
		return errAlreadyResolved
	}
	lc.resolved = true
	return lc.e.resolve(lc.ctx, lc.task.TaskID, resolveRequest{Action: "complete", Output: output})
}

// Stream publishes data to the run's streaming subject stream.{runID}.{stepID}
// for real-time consumers. It mirrors the native worker's PutStream: a
// core-NATS publish carrying the raw bytes verbatim. Stream is not terminal —
// the handler must still Continue or Complete to resolve the iteration.
func (lc *AgentLoopContext) Stream(data []byte) error {
	subject := "stream." + lc.task.RunID + "." + lc.task.StepID
	if err := lc.e.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publish stream %q: %w", subject, err)
	}
	// Flush so the bytes are on the wire before the handler proceeds.
	return lc.e.nc.Flush()
}

// dispatch routes a polled task to its agent-loop handler when one is
// registered, else to the simple Handle path. The simple path is unchanged.
func (e *Engine) dispatch(ctx context.Context, taskType string, t TaskPayload) {
	// One span per dispatched task, named for what the task is: deliveries get
	// their own name and channel attribute so they aggregate separately from
	// ordinary workflow steps.
	//
	// The task carries the trace context dagnats stamped on it at dispatch
	// (dagnats#534), so this span continues the trigger's trace rather than
	// rooting a new one. Tasks without it (older dagnats, or a run started
	// outside a trace) simply root here, as before.
	ctx = observe.ContextWithRemoteTrace(ctx, t.TraceParent, t.TraceState)
	name := "notify.task.run"
	attrs := []attribute.KeyValue{
		attribute.String("task.type", taskType),
		attribute.String("task.id", t.TaskID),
		attribute.String("run.id", t.RunID),
	}
	if channel, ok := strings.CutPrefix(taskType, "send."); ok {
		name = "notify.delivery.send"
		attrs = append(attrs, attribute.String("channel", channel))
	}
	ctx, span := observe.Tracer().Start(
		ctx, name,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	if fn, ok := e.agentHandlers[taskType]; ok {
		e.runAgentLoopTask(ctx, fn, t)
		return
	}
	e.runTask(ctx, e.handlers[taskType], t)
}

// runAgentLoopTask loads the prior checkpoint, runs the handler, and ensures the
// task is resolved. The handler owns resolution via the context's terminal
// actions; if it returns without one, the task is failed so it cannot hang.
func (e *Engine) runAgentLoopTask(ctx context.Context, fn AgentLoopFunc, t TaskPayload) {
	cp, err := e.loadCheckpoint(ctx, t.TaskID)
	if err != nil {
		e.failTask(t.TaskID, fmt.Sprintf("load checkpoint: %v", err))
		return
	}
	lc := &AgentLoopContext{e: e, ctx: ctx, task: t, checkpoint: cp}
	herr := fn(ctx, lc)
	if lc.resolved {
		return
	}
	reason := "agent-loop handler returned without Continue or Complete"
	if herr != nil {
		reason = herr.Error()
	}
	e.failTask(t.TaskID, reason)
}

// failTask resolves a task with outcome "fail" (non-retriable) using an
// independent timeout, matching runTask's resolution discipline.
func (e *Engine) failTask(taskID, reason string) {
	rctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	_ = e.resolve(rctx, taskID, resolveRequest{
		Action:      "fail",
		Error:       reason,
		FailureType: "non_retriable",
	})
}

// resolve POSTs a resolve request for taskID to the bridge.
func (e *Engine) resolve(ctx context.Context, taskID string, req resolveRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal resolve request: %w", err)
	}
	_, err = e.post(ctx, e.bridgeURL+"/v1/tasks/"+taskID+"/resolve", body)
	return err
}

// checkpointsKV lazily opens the dagnats "checkpoints" KV bucket over the
// engine's NATS connection. The bucket is created by the dagnats server at
// startup; opening it read/write lets agent-loop handlers round-trip durable
// state the bridge does not expose over HTTP.
func (e *Engine) checkpointsKV(ctx context.Context) (jetstream.KeyValue, error) {
	e.kvOnce.Do(func() {
		js, err := jetstream.New(e.nc)
		if err != nil {
			e.kvErr = fmt.Errorf("init jetstream: %w", err)
			return
		}
		kv, err := js.KeyValue(ctx, "checkpoints")
		if err != nil {
			e.kvErr = fmt.Errorf("open checkpoints KV: %w", err)
			return
		}
		e.checkpoints = kv
	})
	return e.checkpoints, e.kvErr
}

// loadCheckpoint reads the durable checkpoint for taskID ({runID}.{stepID}),
// returning nil when none was written (e.g. iteration 0).
func (e *Engine) loadCheckpoint(ctx context.Context, taskID string) ([]byte, error) {
	kv, err := e.checkpointsKV(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(ctx, taskID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get checkpoint %q: %w", taskID, err)
	}
	return entry.Value(), nil
}

// putCheckpoint persists data as the checkpoint for taskID ({runID}.{stepID}),
// overwriting any prior iteration's checkpoint.
func (e *Engine) putCheckpoint(ctx context.Context, taskID string, data []byte) error {
	kv, err := e.checkpointsKV(ctx)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, taskID, data); err != nil {
		return fmt.Errorf("put checkpoint %q: %w", taskID, err)
	}
	return nil
}
