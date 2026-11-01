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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danmestas/dagnats/dag"
	"github.com/danmestas/dagnats/program"
	"github.com/danmestas/dagnats/worker"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/observe"
	"github.com/danmestas/notify/internal/timestamp"
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
	// ResumeAt is the deadline an earlier dispatch of this step parked it
	// until with WaitUntil; zero when the step has never parked. A handler
	// resumes toward it rather than recomputing a wait from now.
	ResumeAt time.Time
}

// WaitError is WaitUntil's result: it asks the engine to park the task.
type WaitError struct{ At time.Time }

func (e *WaitError) Error() string { return "engine: wait until " + e.At.Format(time.RFC3339) }

// WaitUntil returns the error a handler returns to wait until at without
// holding a worker: the engine parks the task in dagnats (a durable,
// delayed redelivery) and dispatches it again with ResumeAt set to at. It
// survives restarts, a cancelled run is never redelivered, and a wait of any
// length holds no slot and no claim. The resumed dispatch returns output once
// at has passed, or WaitUntil again.
func WaitUntil(at time.Time) error { return &WaitError{At: at} }

// maxPark bounds one park. A longer wait parks again on resume, which also
// rewrites the marker before dagnats' 30-day checkpoint TTL could drop it.
const maxPark = time.Hour

// parkPrefix tags the pause name that carries a park's deadline.
const parkPrefix = "until:"

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
	nc *nats.Conn
	// ownsConn is set when New dialed nc; a connection passed to FromConn
	// belongs to the caller and Stop leaves it open.
	ownsConn bool
	log      *slog.Logger

	handlers      map[string]HandlerFunc
	agentHandlers map[string]AgentLoopFunc
	started       bool
	ackWait       time.Duration
	concurrency   int
	// maxRunTime holds the WithMaxRunTime of each type that set one.
	maxRunTime map[string]time.Duration

	w *worker.Worker
	// register, when set, takes every handler instead of w: the host runs
	// the worker (program.Host.Handle) and owns its drain.
	register Registrar
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

// withConcurrency lowers the per-lane task limit, for tests that need a lane
// to fill.
func withConcurrency(n int) Option {
	return func(e *Engine) { e.concurrency = n }
}

// Registrar registers a task handler on a worker the host runs. It is
// program.Host.Handle's shape.
type Registrar func(taskType string, fn worker.HandlerFunc, opts ...worker.HandlerOption)

// WithRegistrar makes StartWorkers register every handler through register
// instead of starting a worker of the engine's own. The host then claims the
// tasks, recovers panics and drains on its stop; the engine still classifies
// errors, parks waits and ends handler contexts when Start's ctx is cancelled
// or Stop is called.
func WithRegistrar(register Registrar) Option {
	if register == nil {
		panic("engine: WithRegistrar: register must not be nil")
	}
	return func(e *Engine) { e.register = register }
}

// inboxPrefix is the reply inbox the engine's connection must dial with:
// dagnats answers the api.p.notify.* control plane only under the notify
// program's own inbox and drops any other reply unanswered.
var inboxPrefix = program.InboxPrefix(strings.TrimSuffix(names.Prefix, "."))

// InboxOption dials a connection with the reply inbox FromConn requires.
// program.Run already dials notify's program connection so.
func InboxOption() nats.Option { return nats.CustomInboxPrefix(inboxPrefix) }

// New dials dagnats' NATS endpoint. The engine owns that connection and Stop
// closes it.
func New(natsURL string, opts ...Option) (*Engine, error) {
	nc, err := eventbus.DialNATS(natsURL, "notify-engine", "dagnats sidecar (-nats-url)", InboxOption())
	if err != nil {
		return nil, err
	}
	e := FromConn(nc, opts...)
	e.ownsConn = true
	return e, nil
}

// FromConn builds an engine on an existing connection to dagnats, which must
// have been dialed with InboxOption. The connection stays the caller's: Stop
// drains the worker but does not close it.
func FromConn(nc *nats.Conn, opts ...Option) *Engine {
	if nc != nil && nc.Opts.InboxPrefix != inboxPrefix {
		panic(fmt.Sprintf("engine.FromConn: connection inbox is %q; dial with engine.InboxOption() (%s)",
			nc.Opts.InboxPrefix, inboxPrefix))
	}
	e := &Engine{
		nc:            nc,
		log:           slog.Default(),
		handlers:      make(map[string]HandlerFunc),
		agentHandlers: make(map[string]AgentLoopFunc),
		ackWait:       defaultAckWait,
		concurrency:   handlerConcurrency,
		maxRunTime:    make(map[string]time.Duration),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Handle registers a handler for a task type. It must be called before
// StartWorkers; calling it afterward panics.
func (e *Engine) Handle(taskType string, fn HandlerFunc, opts ...HandleOption) {
	if e.started {
		panic("engine: Handle called after StartWorkers")
	}
	if _, dup := e.agentHandlers[taskType]; dup {
		panic("engine: Handle conflicts with HandleAgentLoop for task type " + taskType)
	}
	e.handlers[taskType] = fn
	e.applyHandleOptions(taskType, opts)
}

// HandleOption tunes one task type at registration.
type HandleOption func(e *Engine, taskType string)

// WithMaxRunTime is how long dagnats keeps a running task of this type
// claimed (its worker heartbeats while the handler runs). Past it the task
// redelivers, so set it above the type's own worst case. Types without it get
// dagnats' default of an hour.
func WithMaxRunTime(d time.Duration) HandleOption {
	if d <= 0 || d > worker.MaxRunTimeMax {
		panic("engine: WithMaxRunTime out of range")
	}
	return func(e *Engine, taskType string) { e.maxRunTime[taskType] = d }
}

func (e *Engine) applyHandleOptions(taskType string, opts []HandleOption) {
	for _, opt := range opts {
		opt(e, taskType)
	}
}

// workerOptions are the SDK options every type gets, plus its own max run time.
func (e *Engine) workerOptions(taskType string) []worker.HandlerOption {
	opts := []worker.HandlerOption{worker.WithConcurrency(e.concurrency), worker.WithAckWait(e.ackWait)}
	if d, ok := e.maxRunTime[taskType]; ok {
		opts = append(opts, worker.WithMaxRunTime(d))
	}
	return opts
}

// Handles reports whether a handler is registered for taskType. It is a
// read-only introspection used by wiring smoke tests (e.g. asserting the
// digest.wait task handler was registered before StartWorkers).
func (e *Engine) Handles(taskType string) bool {
	_, ok := e.handlers[taskType]
	return ok
}

// TaskTypes returns every task type with a registered handler, simple or
// agent-loop, sorted. Wiring tests compare it with the manifest.
func (e *Engine) TaskTypes() []string {
	out := make([]string, 0, len(e.handlers)+len(e.agentHandlers))
	for taskType := range e.handlers {
		out = append(out, taskType)
	}
	for taskType := range e.agentHandlers {
		out = append(out, taskType)
	}
	sort.Strings(out)
	return out
}

// StartWorkers starts claiming tasks for every registered type. Claiming stops
// when ctx is cancelled or Stop is called. It fails when dagnats will not let
// the worker subscribe (its task stream is not ready, or permission is
// denied); the engine then claims nothing, and Ready says so.
func (e *Engine) StartWorkers(ctx context.Context) error {
	if e.started {
		panic("engine: StartWorkers called twice")
	}
	e.started = true
	e.life, e.endLife = context.WithCancel(context.WithoutCancel(ctx))
	if e.register != nil {
		e.registerAll(e.register)
		context.AfterFunc(ctx, e.endLife)
		return nil
	}
	if len(e.handlers)+len(e.agentHandlers) == 0 {
		return nil
	}
	w := worker.NewWorker(e.nc)
	e.registerAll(w.Handle)
	if err := w.Start(); err != nil {
		e.endLife()
		return fmt.Errorf("engine: start task worker: %w", err)
	}
	e.w = w
	context.AfterFunc(ctx, e.stopWorker)
	return nil
}

// registerAll hands every registered handler, adapted to the worker SDK, to
// register.
func (e *Engine) registerAll(register Registrar) {
	for taskType, fn := range e.handlers {
		register(taskType, e.taskHandler(taskType, fn), e.workerOptions(taskType)...)
	}
	for taskType, fn := range e.agentHandlers {
		register(taskType, e.loopHandler(taskType, fn), e.workerOptions(taskType)...)
	}
}

// Ready reports whether the engine can reach dagnats and, once started, is
// claiming tasks. It backs /readyz.
func (e *Engine) Ready() error {
	if !e.nc.IsConnected() {
		return fmt.Errorf("dagnats NATS connection is %s", e.nc.Status())
	}
	if e.started && e.life.Err() != nil {
		return errors.New("task worker stopped")
	}
	return nil
}

// Stop ends in-flight handlers' contexts, waits for them to settle their
// tasks, and closes the NATS connection. It never calls dagnats' control plane.
func (e *Engine) Stop() error {
	e.stopWorker()
	if e.ownsConn {
		e.nc.Close()
	}
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
// completes the step, a WaitError parks it, a RetryableError hands it back to
// the retry policy, and any other error or a panic fails the step permanently.
func (e *Engine) taskHandler(taskType string, fn HandlerFunc) worker.HandlerFunc {
	return func(tc worker.TaskContext) error {
		ctx, end := e.taskContext(tc, taskType)
		defer end()
		t := payloadOf(tc)
		t.ResumeAt = parkedUntil(tc)
		var out json.RawMessage
		err := e.guard(taskType, t.TaskID, func() (herr error) {
			out, herr = fn(ctx, t)
			return herr
		})
		var we *WaitError
		if errors.As(err, &we) {
			return park(ctx, tc, we.At)
		}
		if err != nil {
			return failure(ctx, err)
		}
		return tc.Complete(out)
	}
}

// park parks tc until at with dagnats' Pause: a checkpointed marker naming the
// deadline, then a delayed redelivery. A wait longer than maxPark parks again
// on resume.
func park(ctx context.Context, tc worker.TaskContext, at time.Time) error {
	d := min(time.Until(at), maxPark)
	if d <= 0 {
		d = time.Millisecond
	}
	if err := tc.Pause(parkPrefix+timestamp.Format(at), d); err != nil {
		return failure(ctx, NewRetryableError(fmt.Errorf("park until %s: %w", at.Format(time.RFC3339), err)))
	}
	return nil
}

// parkedUntil reads the deadline a previous park left for this step, or zero.
// A read error is treated as no park: the handler then computes its wait
// afresh, which is what it did before parks existed.
func parkedUntil(tc worker.TaskContext) time.Time {
	cp, err := tc.LoadCheckpoint()
	if err != nil || len(cp) == 0 {
		return time.Time{}
	}
	var marker struct {
		Name string `json:"pause_resume"`
	}
	if json.Unmarshal(cp, &marker) != nil {
		return time.Time{}
	}
	ts, ok := strings.CutPrefix(marker.Name, parkPrefix)
	if !ok {
		return time.Time{}
	}
	at, err := timestamp.Parse(ts)
	if err != nil {
		return time.Time{}
	}
	return at
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
	if channel, ok := names.Channel(taskType); ok {
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
