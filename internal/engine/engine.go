// Package engine is notify's client to the dagnats execution engine, which
// runs as a separate process. It hides dagnats' NATS control plane and its
// worker SDK behind a small, deep API: callers register workflows, start runs,
// poll run snapshots, and register task handlers without touching NATS.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/danmestas/dagnats/dag"
	"github.com/danmestas/dagnats/worker"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/observe"
)

// Workflow definitions are dagnats' own types, so notify registers exactly
// what dagnats validates.
type (
	WorkflowDef     = dag.WorkflowDef
	StepDef         = dag.StepDef
	RetryPolicy     = dag.RetryPolicy
	SingletonConfig = dag.SingletonConfig
)

// TaskPayload is what a handler learns about the task it runs.
type TaskPayload struct {
	// TaskID is {runID}.{stepID}, dagnats' key for the task.
	TaskID string
	RunID  string
	StepID string
	// Attempt is the 1-based dispatch count: 1 first, 2 on the first retry.
	Attempt int
	Input   json.RawMessage
}

// RunSnapshot mirrors the run fields of dagnats's api.runs.get reply
// (RunResponse embeds dag.WorkflowRun).
type RunSnapshot struct {
	RunID  string          `json:"run_id"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output,omitempty"`
}

// IsTerminal reports whether the run has reached a finished state.
func (r RunSnapshot) IsTerminal() bool {
	switch r.Status {
	case "completed", "failed", "cancelled", "compensated", "compensate_failed":
		return true
	default:
		return false
	}
}

// Succeeded reports whether the run completed successfully.
func (r RunSnapshot) Succeeded() bool {
	return r.Status == "completed"
}

// HandlerFunc executes a single task and returns its output or an error.
type HandlerFunc func(ctx context.Context, t TaskPayload) (json.RawMessage, error)

// RetryableError marks a handler failure as transient: the engine retries the
// step per its RetryPolicy before dead-lettering. Unmarked errors stay
// permanent (non-retriable), preserving the prior behaviour of every handler.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// NewRetryableError wraps err so the engine reports the failure as retriable.
func NewRetryableError(err error) error { return &RetryableError{Err: err} }

// NonRetryableError marks a handler failure as permanent. Plain errors are
// already treated as permanent; the explicit wrapper documents a deliberate
// classification at the call site.
type NonRetryableError struct{ Err error }

func (e *NonRetryableError) Error() string { return e.Err.Error() }
func (e *NonRetryableError) Unwrap() error { return e.Err }

// NewNonRetryableError wraps err so the failure is explicitly non-retriable.
func NewNonRetryableError(err error) error { return &NonRetryableError{Err: err} }

// handlerConcurrency is how many tasks of one type run at once. The HTTP bridge
// this replaces started a goroutine per polled task with no bound of its own,
// so the consumer's MaxAckPending was the only limit; the worker SDK's ceiling
// is the closest match, and it keeps a slow digest.wait or agent turn from
// holding up the rest of its type.
const handlerConcurrency = worker.ConcurrencyMax

// defaultAckWait is how long a claim lasts without a heartbeat; it matches
// dagnats' default. Keeping a long wait's claim alive is the worker SDK's job.
const defaultAckWait = 5 * time.Minute

// Engine talks to dagnats over one NATS connection: request/reply for the
// control plane, and dagnats' worker SDK for tasks. The zero value is not
// usable; construct with New.
type Engine struct {
	nc  *nats.Conn
	log *slog.Logger

	handlers      map[string]HandlerFunc
	agentHandlers map[string]AgentLoopFunc
	started       bool
	ackWait       time.Duration

	w *worker.Worker
	// life ends every in-flight handler's context on Stop, so a handler that
	// waits (digest.wait, a delay) returns and requeues instead of holding
	// shutdown for the worker's drain timeout.
	life     context.Context
	endLife  context.CancelFunc
	stopOnce sync.Once
}

// Option configures an Engine at construction.
type Option func(*Engine)

// WithLogger sets the logger used for handler faults. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// withAckWait shortens the claim lease, for tests that outlast it.
func withAckWait(d time.Duration) Option {
	return func(e *Engine) { e.ackWait = d }
}

// New dials dagnats' NATS endpoint.
func New(natsURL string, opts ...Option) (*Engine, error) {
	nc, err := eventbus.DialNATS(natsURL, "notify-engine", "dagnats sidecar (-nats-url)")
	if err != nil {
		return nil, err
	}
	e := &Engine{
		nc:            nc,
		log:           slog.Default(),
		handlers:      make(map[string]HandlerFunc),
		agentHandlers: make(map[string]AgentLoopFunc),
		ackWait:       defaultAckWait,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// Handle registers a handler for a task type. It must be called before
// StartWorkers; calling it afterward panics.
func (e *Engine) Handle(taskType string, fn HandlerFunc) {
	if e.started {
		panic("engine: Handle called after StartWorkers")
	}
	if _, dup := e.agentHandlers[taskType]; dup {
		panic("engine: Handle conflicts with HandleAgentLoop for task type " + taskType)
	}
	e.handlers[taskType] = fn
}

// Handles reports whether a handler is registered for taskType. It is a
// read-only introspection used by wiring smoke tests (e.g. asserting the
// digest.wait task handler was registered before StartWorkers).
func (e *Engine) Handles(taskType string) bool {
	_, ok := e.handlers[taskType]
	return ok
}

// StartWorkers starts claiming tasks for every registered type. Claiming stops
// when ctx is cancelled or Stop is called.
func (e *Engine) StartWorkers(ctx context.Context) {
	if e.started {
		panic("engine: StartWorkers called twice")
	}
	e.started = true
	e.life, e.endLife = context.WithCancel(context.WithoutCancel(ctx))
	if len(e.handlers)+len(e.agentHandlers) == 0 {
		return
	}
	e.w = worker.NewWorker(e.nc)
	opts := []worker.HandlerOption{worker.WithConcurrency(handlerConcurrency), worker.WithAckWait(e.ackWait)}
	for taskType, fn := range e.handlers {
		e.w.Handle(taskType, e.taskHandler(taskType, fn), opts...)
	}
	for taskType, fn := range e.agentHandlers {
		e.w.Handle(taskType, e.loopHandler(taskType, fn), opts...)
	}
	e.w.Start()
	context.AfterFunc(ctx, e.stopWorker)
}

// Ready reports whether the engine can reach dagnats and, once started, is
// claiming tasks. It backs /readyz.
func (e *Engine) Ready() error {
	if !e.nc.IsConnected() {
		return fmt.Errorf("dagnats NATS connection is %s", e.nc.Status())
	}
	if e.started && e.w != nil && e.life.Err() != nil {
		return errors.New("task worker stopped")
	}
	return nil
}

// Stop ends in-flight handlers' contexts, waits for them to settle their
// tasks, and closes the NATS connection. It never calls dagnats' control plane.
func (e *Engine) Stop() error {
	e.stopWorker()
	e.nc.Close()
	return nil
}

// stopWorker cancels handler contexts first so a waiting handler returns, then
// lets the worker drain: every in-flight task publishes its outcome while the
// connection is still open.
func (e *Engine) stopWorker() {
	e.stopOnce.Do(func() {
		if e.endLife != nil {
			e.endLife()
		}
		if e.w != nil {
			e.w.Stop()
		}
	})
}

// taskHandler adapts a HandlerFunc to the worker SDK: the handler's output
// completes the step, a RetryableError hands it back to the retry policy, and
// any other error or a panic fails the step permanently.
func (e *Engine) taskHandler(taskType string, fn HandlerFunc) worker.HandlerFunc {
	return func(tc worker.TaskContext) error {
		ctx, end := e.taskContext(tc, taskType)
		defer end()
		t := payloadOf(tc)
		var out json.RawMessage
		err := e.guard(taskType, t.TaskID, func() (herr error) {
			out, herr = fn(ctx, t)
			return herr
		})
		if err != nil {
			return failure(ctx, err)
		}
		return tc.Complete(out)
	}
}

// taskContext derives the handler's context: the worker's (trace from the
// dispatch, cancelled with the run) also ends on Stop, and carries one span
// named for what the task is so deliveries aggregate apart from other steps.
func (e *Engine) taskContext(tc worker.TaskContext, taskType string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(tc.Context())
	stopLife := context.AfterFunc(e.life, cancel)
	name := "notify.task.run"
	attrs := []attribute.KeyValue{
		attribute.String("task.type", taskType),
		attribute.String("task.id", tc.RunID()+"."+tc.StepID()),
		attribute.String("run.id", tc.RunID()),
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
	return ctx, func() {
		span.End()
		stopLife()
		cancel()
	}
}

// guard runs fn and turns a panic into an error, so a panicking handler fails
// only its own step.
func (e *Engine) guard(taskType, taskID string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("engine: task handler panicked",
				"task_type", taskType, "task_id", taskID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return fn()
}

// failure maps a handler error onto the worker's classification: only a
// RetryableError is retried; everything else is permanent, as it always was.
func failure(ctx context.Context, err error) error {
	var re *RetryableError
	if errors.As(err, &re) {
		recordError(ctx, err)
		return err
	}
	return permanent(ctx, err)
}

// permanent fails the step with no retry, whatever err wraps.
func permanent(ctx context.Context, err error) error {
	recordError(ctx, err)
	return worker.NewNonRetryableError(err)
}

func recordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// payloadOf reads the handler's view of the task from the worker's context.
func payloadOf(tc worker.TaskContext) TaskPayload {
	// RetryCount is 0 on the first dispatch and the attempt number after.
	attempt := tc.RetryCount()
	if attempt < 1 {
		attempt = 1
	}
	return TaskPayload{
		TaskID:  tc.RunID() + "." + tc.StepID(),
		RunID:   tc.RunID(),
		StepID:  tc.StepID(),
		Attempt: attempt,
		Input:   tc.Input(),
	}
}
