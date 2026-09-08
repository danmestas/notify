// Package engine is notify's control-plane and worker client to the dagnats
// execution engine, which runs as a sidecar process. It hides the NATS
// request/reply control plane and the HTTP worker bridge behind a small, deep
// API: callers register workflows, start runs, poll run snapshots, and
// register task handlers without ever touching NATS or HTTP directly.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/observe"
)

// Control-plane subjects exposed by the dagnats NATS API.
const (
	subjRegister = "api.workflows.register"
	subjStartRun = "api.runs.start"
	subjGetRun   = "api.runs.get"

	controlTimeout = 5 * time.Second
)

// WorkflowDef mirrors dagnats's dag.WorkflowDef JSON (only the fields notify
// needs). dagnats requires at least one step and rejects unknown workflows.
type WorkflowDef struct {
	Name  string    `json:"name"`
	Steps []StepDef `json:"steps"`
	// IdempotencyKey is a dot-path evaluated against the run input; a StartRun
	// whose key value was already seen returns the existing run id instead of
	// starting a duplicate run.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Singleton constrains runs to one at a time per key (dagnats admission).
	Singleton *SingletonConfig `json:"singleton,omitempty"`
}

// SingletonConfig mirrors dagnats's dag.SingletonConfig: at most one active
// run per key. Key is a dot-path evaluated against the run input; empty keys
// the lock by workflow name alone.
type SingletonConfig struct {
	Mode string `json:"mode"` // "skip" drops the duplicate; "cancel" cancels the active run
	Key  string `json:"key,omitempty"`
}

// RetryPolicy mirrors dagnats's dag.RetryPolicy. Durations marshal as
// nanoseconds (Go time.Duration JSON), matching the dagnats wire.
type RetryPolicy struct {
	MaxAttempts  int           `json:"max_attempts"`
	Strategy     string        `json:"strategy"` // "fixed" | "linear" | "exponential"
	InitialDelay time.Duration `json:"initial_delay"`
	MaxDelay     time.Duration `json:"max_delay"`
}

// StepDef mirrors dagnats's dag.StepDef JSON. Type marshals as a string such as
// "normal" or "agent_loop"; set it explicitly to be safe. Timeout bounds a
// single task dispatch (per agent-loop iteration) and marshals as nanoseconds
// (Go time.Duration JSON), matching dagnats's dag.StepDef.Timeout.
type StepDef struct {
	ID        string          `json:"id"`
	Task      string          `json:"task"`
	Type      string          `json:"type,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	DependsOn []string        `json:"depends_on,omitempty"`
	Retry     *RetryPolicy    `json:"retry,omitempty"`
	Timeout   time.Duration   `json:"timeout,omitempty"`
}

// TaskPayload mirrors dagnats's protocol.TaskPayload / bridge pollResponse JSON.
// Attempt is the 1-based dispatch count; always ≥1 since danmestas/dagnats#384
// fixed first-dispatch propagation (previously omitted, unmarshaling to 0).
type TaskPayload struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id"`
	StepID string `json:"step_id"`
	// Iteration is the 0-based agent-loop iteration count: 0 on first dispatch,
	// incremented by the engine on each Continue. Omitted (0) for normal steps.
	Iteration int             `json:"iteration,omitempty"`
	Attempt   int             `json:"attempt"`
	Input     json.RawMessage `json:"input,omitempty"`
	// TraceParent/TraceState carry the W3C trace context dagnats stamps on each
	// dispatched task (danmestas/dagnats#534), letting this task's spans continue
	// the trace that started the run instead of rooting a new one.
	//
	// The JSON keys are the un-underscored W3C wire names and must match
	// dagnats's tags byte-for-byte — JSON field matching is case-insensitive but
	// name-exact, and a silent mismatch here reproduces the orphaned-trace bug
	// that a canonicalised header key caused on the NATS seam (#250).
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
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

// Engine talks to a dagnats sidecar over NATS (control) and HTTP (worker
// bridge). The zero value is not usable; construct with New.
type Engine struct {
	nc        *nats.Conn
	bridgeURL string
	http      *http.Client
	log       *slog.Logger

	// bridgeToken authenticates every worker-bridge request. dagnats gates
	// the bridge on DAGNATS_BRIDGE_TOKEN: with it set, a request without a
	// matching bearer is answered 401 and no task is ever handed over, so a
	// notify that does not send it accepts triggers and executes nothing
	// (#325). Empty means the bridge is in open dev mode, which is how the
	// test sidecar and the mini both run it.
	bridgeToken string

	handlers      map[string]HandlerFunc
	agentHandlers map[string]AgentLoopFunc
	started       bool

	// checkpoints is the dagnats "checkpoints" KV bucket, opened lazily on the
	// first agent-loop dispatch. Agent-loop handlers read/write durable state
	// here at key {runID}.{stepID} (the bridge has no checkpoint-read endpoint).
	kvOnce      sync.Once
	checkpoints jetstream.KeyValue
	kvErr       error

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Option configures an Engine at construction. Options are variadic so the
// many existing New(natsURL, bridgeURL) call sites stay untouched.
type Option func(*Engine)

// WithBridgeToken sets the bearer token sent on every worker-bridge request.
// Empty is the zero value and sends no Authorization header, matching a
// dagnats bridge running in open dev mode.
func WithBridgeToken(token string) Option {
	return func(e *Engine) { e.bridgeToken = token }
}

// WithLogger sets the logger used for bridge faults. Defaults to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// New dials the sidecar's NATS endpoint and prepares an HTTP client for the
// worker bridge mounted at bridgeURL.
func New(natsURL, bridgeURL string, opts ...Option) (*Engine, error) {
	nc, err := eventbus.DialNATS(natsURL, "notify-engine", "dagnats sidecar (-nats-url)")
	if err != nil {
		return nil, err
	}
	e := &Engine{
		nc:            nc,
		bridgeURL:     bridgeURL,
		http:          &http.Client{Timeout: 30 * time.Second},
		log:           slog.Default(),
		handlers:      make(map[string]HandlerFunc),
		agentHandlers: make(map[string]AgentLoopFunc),
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
	e.handlers[taskType] = fn
}

// Handles reports whether a handler is registered for taskType. It is a
// read-only introspection used by wiring smoke tests (e.g. asserting the
// digest.wait task handler was registered before StartWorkers).
func (e *Engine) Handles(taskType string) bool {
	_, ok := e.handlers[taskType]
	return ok
}

// StartWorkers launches one long-poll loop per registered task type. Loops run
// until ctx is cancelled or Stop is called.
func (e *Engine) StartWorkers(ctx context.Context) {
	e.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	for taskType := range e.handlers {
		e.wg.Add(1)
		go e.pollLoop(loopCtx, taskType)
	}
	for taskType := range e.agentHandlers {
		e.wg.Add(1)
		go e.pollLoop(loopCtx, taskType)
	}
}

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

// Stop cancels all worker loops, waits for them to drain, and closes the NATS
// connection.
func (e *Engine) Stop() error {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	e.nc.Close()
	return nil
}

// request issues a NATS request/reply with the control timeout, honoring ctx
// cancellation.
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

// pollRequest is the JSON body for POST /v1/tasks/poll.
type pollRequest struct {
	TaskTypes []string `json:"task_types"`
	MaxTasks  int      `json:"max_tasks"`
	TimeoutMs int64    `json:"timeout_ms"`
}

// resolveRequest is the JSON body for POST /v1/tasks/{id}/resolve.
type resolveRequest struct {
	Action      string          `json:"action"`
	Output      json.RawMessage `json:"output,omitempty"`
	Error       string          `json:"error,omitempty"`
	FailureType string          `json:"failure_type,omitempty"`
}

// Long-poll timeouts for the worker bridge. Agent-loop steps re-enqueue their
// next iteration the instant a handler Continues, and the re-enqueued task is
// only picked up on a fresh poll cycle — so a short long-poll keeps
// iteration-to-iteration latency low. Simple one-shot handlers favour a longer
// poll to minimise idle request churn.
const (
	pollTimeoutMs          = 5000
	pollTimeoutAgentLoopMs = 1000
)

// pollFaultReportInterval rate-limits repeated poll-failure reports for one
// task type. A poll long-polls for at most pollTimeoutMs and fails instantly
// when the bridge rejects it, so a persistent fault (a 401 from a bridge whose
// DAGNATS_BRIDGE_TOKEN we do not carry) recurs several times a second per task
// type. Reporting every occurrence would bury the log; reporting none is what
// let #325 run for four days with no log line at all.
const pollFaultReportInterval = 30 * time.Second

// pollLoop long-polls the bridge for one task type and dispatches each task to
// its handler in its own goroutine.
func (e *Engine) pollLoop(ctx context.Context, taskType string) {
	defer e.wg.Done()
	timeoutMs := int64(pollTimeoutMs)
	if _, ok := e.agentHandlers[taskType]; ok {
		timeoutMs = pollTimeoutAgentLoopMs
	}
	// Fault state is per-loop, and each loop owns exactly one task type, so
	// these need no synchronisation.
	var (
		consecutiveFailures int
		lastReported        time.Time
	)
	for {
		if ctx.Err() != nil {
			return
		}
		tasks, err := e.poll(ctx, taskType, timeoutMs)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A poll failure is reported on its first occurrence and then at
			// most once per pollFaultReportInterval while it persists. The
			// error is never silent: a bridge that rejects or cannot serve
			// this task type means the step never runs, and nothing else in
			// the system will say so.
			consecutiveFailures++
			if lastReported.IsZero() || time.Since(lastReported) >= pollFaultReportInterval {
				e.log.Warn("engine: bridge poll failed, tasks for this type are not being executed",
					"task_type", taskType,
					"bridge_url", e.bridgeURL,
					"consecutive_failures", consecutiveFailures,
					"err", err)
				lastReported = time.Now()
			}
			// Transient bridge/network error; back off briefly and retry.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if consecutiveFailures > 0 {
			// Clearing lastReported as well as the counter is what makes the
			// next fault report immediately instead of waiting out a timer
			// left over from the previous one.
			e.log.Info("engine: bridge poll recovered",
				"task_type", taskType, "after_failures", consecutiveFailures)
			consecutiveFailures = 0
			lastReported = time.Time{}
		}
		if len(tasks) == 0 {
			// The bridge's long-poll sometimes returns immediately with no
			// tasks (e.g. after the backing consumer drains); a short sleep
			// keeps the loop from busy-spinning against the bridge.
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		for _, task := range tasks {
			e.wg.Add(1)
			go func(t TaskPayload) {
				defer e.wg.Done()
				e.dispatch(ctx, taskType, t)
			}(task)
		}
	}
}

// poll issues one long-poll request for taskType and returns the tasks fetched.
func (e *Engine) poll(ctx context.Context, taskType string, timeoutMs int64) ([]TaskPayload, error) {
	body, err := json.Marshal(pollRequest{
		TaskTypes: []string{taskType},
		MaxTasks:  10,
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		return nil, err
	}
	resp, err := e.post(ctx, e.bridgeURL+"/v1/tasks/poll", body)
	if err != nil {
		return nil, err
	}
	var tasks []TaskPayload
	if err := json.Unmarshal(resp, &tasks); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	return tasks, nil
}

// runTask executes fn for one task and reports the outcome to the bridge.
func (e *Engine) runTask(ctx context.Context, fn HandlerFunc, t TaskPayload) {
	out, err := fn(ctx, t)
	var req resolveRequest
	if err != nil {
		failureType := "non_retriable"
		var re *RetryableError
		if errors.As(err, &re) {
			failureType = "retriable"
		}
		req = resolveRequest{
			Action:      "fail",
			Error:       err.Error(),
			FailureType: failureType,
		}
	} else {
		req = resolveRequest{Action: "complete", Output: out}
	}
	body, mErr := json.Marshal(req)
	if mErr != nil {
		return
	}
	// Resolution must not be skipped just because the loop ctx was cancelled
	// mid-flight; use a short independent timeout. WithoutCancel rather than
	// Background so the span (and any other ctx values) survive the loop's
	// cancellation while controlTimeout still bounds the call.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
	defer cancel()
	rctx, span := observe.Tracer().Start(
		rctx, "notify.bridge.resolve",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("task.id", t.TaskID),
			attribute.String("resolve.action", req.Action),
		),
	)
	defer span.End()
	if _, perr := e.post(rctx, e.bridgeURL+"/v1/tasks/"+t.TaskID+"/resolve", body); perr != nil {
		span.RecordError(perr)
		span.SetStatus(codes.Error, perr.Error())
	}
}

// post performs a JSON POST and returns the response body, erroring on non-2xx.
func (e *Engine) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.bridgeToken != "" {
		req.Header.Set("Authorization", "Bearer "+e.bridgeToken)
	}
	// Every bridge POST carries traceparent — dagnats's bridge already
	// extracts it, so the resolve leg stitches with no dagnats change.
	observe.InjectHTTPHeader(ctx, req.Header)
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, data)
	}
	return data, nil
}
