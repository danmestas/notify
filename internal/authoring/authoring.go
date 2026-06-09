// Package authoring turns workflow definitions into persisted rows and
// compiled engine DAGs. It owns the canonical DAG-name convention
// (envID-triggerIdentifier) that trigger and delivery agree on.
package authoring

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// Service persists workflows and registers their compiled DAGs with the engine.
type Service struct {
	store *store.Store
	eng   *engine.Engine
}

// New constructs a Service over the given store and engine.
func New(s *store.Store, eng *engine.Engine) *Service {
	return &Service{store: s, eng: eng}
}

// DAGName is the canonical engine workflow name: envID-scoped trigger
// identifier joined by '-'. trigger and delivery compute the same value.
// NOTE: dagnats stores workflows under this name as a NATS KV key, which
// rejects ':' (`nats: invalid key`); '-' is KV- and subject-token-safe and is
// proven end-to-end by the engine e2e (workflow "foundation-smoke").
func DAGName(scope domain.Scope, triggerIdentifier string) string {
	return scope.EnvID + "-" + triggerIdentifier
}

// WorkflowInput is the authoring request: a named workflow with an ordered
// list of channel steps keyed by TriggerIdentifier.
type WorkflowInput struct {
	Name              string
	TriggerIdentifier string
	Steps             []domain.Step
}

// Create persists a workflow row. Steps are marshalled into the row's JSON
// steps column and the workflow is active on creation.
func (s *Service) Create(ctx context.Context, scope domain.Scope, in WorkflowInput) (domain.Workflow, error) {
	steps, err := json.Marshal(in.Steps)
	if err != nil {
		return domain.Workflow{}, fmt.Errorf("authoring: marshal steps: %w", err)
	}
	wf := domain.Workflow{
		Name:              in.Name,
		TriggerIdentifier: in.TriggerIdentifier,
		Active:            true,
		Steps:             steps,
	}
	return s.store.Workflows().Create(ctx, scope, wf)
}

// Compile turns a persisted workflow into an engine DAG: one StepDef per step,
// chained sequentially via DependsOn to preserve order. A "delay" step compiles
// to an engine Sleep step the engine times itself; channel steps become normal
// "send.<type>" tasks.
func Compile(wf domain.Workflow) (engine.WorkflowDef, error) {
	var steps []domain.Step
	if err := json.Unmarshal(wf.Steps, &steps); err != nil {
		return engine.WorkflowDef{}, fmt.Errorf("authoring: unmarshal steps: %w", err)
	}
	def := engine.WorkflowDef{
		Name:  wf.EnvID + "-" + wf.TriggerIdentifier,
		Steps: make([]engine.StepDef, 0, len(steps)),
	}
	for i, step := range steps {
		var sd engine.StepDef
		if step.Type == "delay" {
			cfg, err := delayConfig(step.ControlValues)
			if err != nil {
				return engine.WorkflowDef{}, fmt.Errorf("authoring: step %q: %w", step.StepID, err)
			}
			sd = engine.StepDef{ID: step.StepID, Type: "sleep", Config: cfg}
		} else {
			sd = engine.StepDef{ID: step.StepID, Task: "send." + step.Type, Type: "normal"}
		}
		if i > 0 {
			sd.DependsOn = []string{steps[i-1].StepID}
		}
		def.Steps = append(def.Steps, sd)
	}
	return def, nil
}

// delayConfig parses a delay step's controlValues ({"amount":<int>,"unit":...})
// into a dag.SleepConfig JSON ({"duration":<int64 ns>}). Unit defaults to
// seconds; amount must be positive.
func delayConfig(controlValues json.RawMessage) (json.RawMessage, error) {
	var cv struct {
		Amount int64  `json:"amount"`
		Unit   string `json:"unit"`
	}
	if len(controlValues) > 0 {
		if err := json.Unmarshal(controlValues, &cv); err != nil {
			return nil, fmt.Errorf("unmarshal delay controlValues: %w", err)
		}
	}
	if cv.Amount <= 0 {
		return nil, fmt.Errorf("delay amount must be positive, got %d", cv.Amount)
	}
	var unit time.Duration
	switch cv.Unit {
	case "", "seconds":
		unit = time.Second
	case "minutes":
		unit = time.Minute
	case "hours":
		unit = time.Hour
	case "days":
		unit = 24 * time.Hour
	default:
		return nil, fmt.Errorf("unknown delay unit %q", cv.Unit)
	}
	cfg, err := json.Marshal(struct {
		Duration time.Duration `json:"duration"`
	}{Duration: time.Duration(cv.Amount) * unit})
	if err != nil {
		return nil, fmt.Errorf("marshal sleep config: %w", err)
	}
	return cfg, nil
}

// Publish loads the workflow by trigger identifier, compiles it, and registers
// the DAG with the engine.
func (s *Service) Publish(ctx context.Context, scope domain.Scope, triggerIdentifier string) error {
	wf, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, triggerIdentifier)
	if err != nil {
		return err
	}
	def, err := Compile(wf)
	if err != nil {
		return err
	}
	return s.eng.RegisterWorkflow(ctx, def)
}
