// Package authoring turns workflow definitions into persisted rows and
// compiled engine DAGs. It owns the canonical DAG-name convention
// (envID-triggerIdentifier) that trigger and delivery agree on.
package authoring

import (
	"context"
	"encoding/json"
	"errors"
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
		if step.Type == "delay" || step.Type == "digest" {
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

// GetBySlug loads a single workflow by its trigger identifier (slug) within
// the Scope. Returns store.ErrNotFound when no matching row exists.
func (s *Service) GetBySlug(ctx context.Context, scope domain.Scope, slug string) (domain.Workflow, error) {
	return s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
}

// List returns all workflows for the Scope, ordered by insertion.
func (s *Service) List(ctx context.Context, scope domain.Scope) ([]domain.Workflow, error) {
	return s.store.Workflows().List(ctx, scope)
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

// UpdateInput carries the mutable fields for an Update call.
type UpdateInput struct {
	Name          string
	Steps         []domain.Step
	PayloadSchema json.RawMessage
}

// ErrCompileFailure wraps a Compile error so HTTP handlers can map it to 422.
type ErrCompileFailure struct{ Cause error }

func (e *ErrCompileFailure) Error() string { return "compile failure: " + e.Cause.Error() }
func (e *ErrCompileFailure) Unwrap() error { return e.Cause }

// Update persists a new name/steps/payload_schema for the identified workflow,
// recompiles the DAG, and re-registers it with the engine so edits take effect
// on the next trigger. A step configuration that fails Compile (e.g. invalid
// delay amount) is rejected with an ErrCompileFailure before any write occurs.
// Returns store.ErrNotFound when the workflow does not exist in this scope.
func (s *Service) Update(ctx context.Context, scope domain.Scope, slug string, in UpdateInput) (domain.Workflow, error) {
	// Load the existing row to obtain its ID (required for the UPDATE predicate).
	existing, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
	if err != nil {
		return domain.Workflow{}, err
	}

	steps, err := json.Marshal(in.Steps)
	if err != nil {
		return domain.Workflow{}, fmt.Errorf("authoring: marshal steps: %w", err)
	}

	// Build the candidate workflow and compile it before writing; reject on
	// compile failure with an ErrCompileFailure (maps to HTTP 422).
	candidate := domain.Workflow{
		ID:                existing.ID,
		OrgID:             existing.OrgID,
		EnvID:             existing.EnvID,
		TriggerIdentifier: existing.TriggerIdentifier,
		Active:            existing.Active,
		Origin:            existing.Origin,
		Name:              in.Name,
		Steps:             steps,
		PayloadSchema:     in.PayloadSchema,
	}
	if _, err := Compile(candidate); err != nil {
		return domain.Workflow{}, &ErrCompileFailure{Cause: err}
	}

	// Persist.
	if err := s.store.Workflows().Update(ctx, scope, candidate); err != nil {
		return domain.Workflow{}, err
	}

	// Re-register the compiled DAG so in-flight triggers use the new definition.
	// Compile was already validated above; cannot fail here.
	def, _ := Compile(candidate)
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return domain.Workflow{}, fmt.Errorf("authoring: re-register workflow: %w", err)
	}

	return candidate, nil
}

// Delete removes the workflow row. The engine does not expose an explicit
// deregister API (no KV delete on NATS subject, and registering an empty-step
// DAG would corrupt the registry). Hard-deleting the store row is the correct
// decision: in-flight runs hold their own snapshot inside the engine and will
// complete normally; notification history rows reference workflow_id as a plain
// string (no FK), so they survive and remain queryable for audit purposes.
//
// Returns store.ErrNotFound when the workflow does not exist in this scope.
func (s *Service) Delete(ctx context.Context, scope domain.Scope, slug string) error {
	return s.store.Workflows().Delete(ctx, scope, slug)
}

// SetActive flips the active flag for the identified workflow. Inactive
// workflows are skipped at trigger.Ingest time (acknowledged=false, no-op).
// The engine DAG is not deregistered — re-activating is instant, no
// re-publish needed.
// Returns store.ErrNotFound when the workflow does not exist in this scope.
func (s *Service) SetActive(ctx context.Context, scope domain.Scope, slug string, active bool) (domain.Workflow, error) {
	if err := s.store.Workflows().SetActive(ctx, scope, slug, active); err != nil {
		return domain.Workflow{}, err
	}
	return s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
}

// DuplicateInput carries the desired name and identifier for the new workflow.
type DuplicateInput struct {
	Name              string
	TriggerIdentifier string
}

// ErrIdentifierConflict is returned by Duplicate when the desired
// TriggerIdentifier already exists in this scope.
var ErrIdentifierConflict = fmt.Errorf("authoring: trigger identifier already exists")

// Duplicate copies an existing workflow under a new name and identifier,
// inheriting the source's steps and payload schema, then publishes the new
// DAG. Returns ErrIdentifierConflict (maps to 409) when the new identifier is
// already taken. Returns store.ErrNotFound when the source slug does not exist.
func (s *Service) Duplicate(ctx context.Context, scope domain.Scope, srcSlug string, in DuplicateInput) (domain.Workflow, error) {
	src, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, srcSlug)
	if err != nil {
		return domain.Workflow{}, err
	}

	// Check for identifier collision before writing.
	if _, checkErr := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, in.TriggerIdentifier); checkErr == nil {
		return domain.Workflow{}, ErrIdentifierConflict
	} else if !isNotFound(checkErr) {
		return domain.Workflow{}, fmt.Errorf("authoring: check identifier: %w", checkErr)
	}

	newWF := domain.Workflow{
		Name:              in.Name,
		TriggerIdentifier: in.TriggerIdentifier,
		Active:            true,
		Steps:             src.Steps,
		PayloadSchema:     src.PayloadSchema,
	}
	created, err := s.store.Workflows().Create(ctx, scope, newWF)
	if err != nil {
		return domain.Workflow{}, err
	}

	if err := s.Publish(ctx, scope, in.TriggerIdentifier); err != nil {
		return domain.Workflow{}, err
	}

	return created, nil
}

// isNotFound reports whether err is a store.ErrNotFound sentinel.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
