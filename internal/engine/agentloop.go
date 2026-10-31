package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/dagnats/dag"
	"github.com/danmestas/dagnats/worker"
)

// Agent-loop steps run on dagnats' worker.LoopTask. The state a handler
// checkpoints rides the step.continue event and comes back in the next
// iteration's task payload, so it is bounded by dagnats' loop_state_max_bytes
// (256 KiB by default, never more than NATS' max_payload). Streaming publishes
// to stream.{runID}.{stepID}.

// AgentLoopConfig bounds an agent-loop step (MaxIterations, MaxDuration,
// LoopDelay); durations marshal as nanoseconds.
type AgentLoopConfig = dag.AgentLoopConfig

// AgentLoopStep builds a StepDef for an agent-loop step that the dagnats engine
// will drive iteratively. cfg rides StepDef.Config and timeout, which bounds a
// single iteration's dispatch (0 for dagnats' default), rides StepDef.Timeout,
// matching the JSON emitted by dag.WorkflowBuilder's AgentLoop(...).With* chain.
func AgentLoopStep(id, task string, cfg AgentLoopConfig, timeout time.Duration) StepDef {
	// AgentLoopConfig is a fixed shape of ints and durations; this marshal
	// cannot fail.
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic("engine: marshal AgentLoopConfig: " + err.Error())
	}
	return StepDef{
		ID:      id,
		Task:    task,
		Type:    dag.StepTypeAgentLoop,
		Config:  raw,
		Timeout: timeout,
	}
}

// AgentLoopFunc handles one iteration of an agent-loop task. It reads the prior
// checkpoint from lc, does its work, and must end the iteration with exactly
// one terminal action: lc.Continue (run again), lc.Complete (finish the step),
// or by returning an error (fail the step). A handler that returns without
// Continue or Complete fails the task to keep it from hanging in-flight.
type AgentLoopFunc func(ctx context.Context, lc *AgentLoopContext) error

// HandleAgentLoop registers an agent-loop handler for a task type. Like Handle,
// it must be called before StartWorkers. A task type cannot be registered as
// both a simple Handle and an agent-loop handler.
func (e *Engine) HandleAgentLoop(taskType string, fn AgentLoopFunc, opts ...HandleOption) {
	if e.started {
		panic("engine: HandleAgentLoop called after StartWorkers")
	}
	if _, dup := e.handlers[taskType]; dup {
		panic("engine: HandleAgentLoop conflicts with Handle for task type " + taskType)
	}
	e.agentHandlers[taskType] = fn
	e.applyHandleOptions(taskType, opts)
}

// AgentLoopContext is the per-iteration handle passed to an AgentLoopFunc. It
// exposes the task plus the prior checkpoint, and the terminal actions that
// resolve the iteration.
type AgentLoopContext struct {
	tc         worker.LoopTask
	checkpoint []byte
	resolved   bool
}

// Checkpoint returns the state the previous iteration passed to Continue, or
// nil on the first iteration.
func (lc *AgentLoopContext) Checkpoint() []byte { return lc.checkpoint }

// Input returns the step input resolved by the engine for this iteration.
func (lc *AgentLoopContext) Input() json.RawMessage { return lc.tc.Input() }

// RunID returns the run this task belongs to.
func (lc *AgentLoopContext) RunID() string { return lc.tc.RunID() }

// StepID returns the agent-loop step's id.
func (lc *AgentLoopContext) StepID() string { return lc.tc.StepID() }

// errAlreadyResolved guards against a handler issuing a second terminal action.
var errAlreadyResolved = errors.New("engine: agent-loop iteration already resolved")

// Continue asks for another iteration. A non-nil checkpoint becomes the next
// iteration's Checkpoint(); nil carries the current one forward unchanged.
func (lc *AgentLoopContext) Continue(checkpoint []byte) error {
	if lc.resolved {
		return errAlreadyResolved
	}
	if checkpoint != nil {
		if err := lc.tc.Checkpoint(checkpoint); err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
	}
	if err := lc.tc.Continue(nil); err != nil {
		return err
	}
	lc.resolved = true
	return nil
}

// Complete finishes the agent-loop step (and, for a terminal step, the run)
// with output.
func (lc *AgentLoopContext) Complete(output json.RawMessage) error {
	if lc.resolved {
		return errAlreadyResolved
	}
	if err := lc.tc.Complete(output); err != nil {
		return err
	}
	lc.resolved = true
	return nil
}

// Stream publishes data verbatim to stream.{runID}.{stepID} for real-time
// consumers. It is not terminal: the handler must still Continue or Complete.
func (lc *AgentLoopContext) Stream(data []byte) error {
	return lc.tc.PutStream(data)
}

// loopHandler adapts an AgentLoopFunc to the worker SDK. The handler owns
// resolution through its context's terminal actions; one that returns, errs or
// panics without resolving fails the step permanently so it cannot hang.
func (e *Engine) loopHandler(taskType string, fn AgentLoopFunc) worker.HandlerFunc {
	return func(tc worker.TaskContext) error {
		ctx, end := e.taskContext(tc, taskType)
		defer end()
		cp, err := tc.LoadCheckpoint()
		if err != nil {
			return permanent(ctx, fmt.Errorf("load checkpoint: %w", err))
		}
		lc := &AgentLoopContext{tc: tc, checkpoint: cp}
		herr := e.guard(taskType, tc.RunID()+"."+tc.StepID(), func() error { return fn(ctx, lc) })
		if lc.resolved {
			return nil
		}
		if herr == nil {
			herr = errors.New("agent-loop handler returned without Continue or Complete")
		}
		return permanent(ctx, herr)
	}
}
