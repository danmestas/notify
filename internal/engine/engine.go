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
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
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
// "normal"; set it explicitly to be safe.
type StepDef struct {
	ID        string          `json:"id"`
	Task      string          `json:"task"`
	Type      string          `json:"type,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	DependsOn []string        `json:"depends_on,omitempty"`
	Retry     *RetryPolicy    `json:"retry,omitempty"`
}

// TaskPayload mirrors dagnats's protocol.TaskPayload / bridge pollResponse JSON.
// Attempt is the 1-based dispatch count; always ≥1 since danmestas/dagnats#384
// fixed first-dispatch propagation (previously omitted, unmarshaling to 0).
type TaskPayload struct {
	TaskID  string          `json:"task_id"`
	RunID   string          `json:"run_id"`
	StepID  string          `json:"step_id"`
	Attempt int             `json:"attempt"`
	Input   json.RawMessage `json:"input,omitempty"`
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

	handlers map[string]HandlerFunc
	started  bool

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New dials the sidecar's NATS endpoint and prepares an HTTP client for the
// worker bridge mounted at bridgeURL.
func New(natsURL, bridgeURL string) (*Engine, error) {
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, fmt.Errorf("connect nats %q: %w", natsURL, err)
	}
	return &Engine{
		nc:        nc,
		bridgeURL: bridgeURL,
		http:      &http.Client{Timeout: 30 * time.Second},
		handlers:  make(map[string]HandlerFunc),
	}, nil
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
	msg, err := e.nc.RequestWithContext(ctx, subject, body)
	if err != nil {
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

// pollLoop long-polls the bridge for one task type and dispatches each task to
// its handler in its own goroutine.
func (e *Engine) pollLoop(ctx context.Context, taskType string) {
	defer e.wg.Done()
	fn := e.handlers[taskType]
	for {
		if ctx.Err() != nil {
			return
		}
		tasks, err := e.poll(ctx, taskType)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Transient bridge/network error; back off briefly and retry.
			time.Sleep(200 * time.Millisecond)
			continue
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
				e.runTask(ctx, fn, t)
			}(task)
		}
	}
}

// poll issues one long-poll request for taskType and returns the tasks fetched.
func (e *Engine) poll(ctx context.Context, taskType string) ([]TaskPayload, error) {
	body, err := json.Marshal(pollRequest{
		TaskTypes: []string{taskType},
		MaxTasks:  10,
		TimeoutMs: 5000,
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
	// mid-flight; use a short independent timeout.
	rctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	_, _ = e.post(rctx, e.bridgeURL+"/v1/tasks/"+t.TaskID+"/resolve", body)
}

// post performs a JSON POST and returns the response body, erroring on non-2xx.
func (e *Engine) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
