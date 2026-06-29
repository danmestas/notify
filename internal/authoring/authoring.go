// Package authoring turns workflow definitions into persisted rows and
// compiled engine DAGs. It owns the canonical DAG-name convention
// (envID-triggerIdentifier) that trigger and delivery agree on.
package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// stepTypes is the canonical step-type registry: the single source of truth for
// how each step type compiles, defaults, parses, and validates. Compile drives
// every step's StepDef from it.
var stepTypes = steptype.Default()

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
	// Validate by compiling before any write — mirrors Update. An invalid step
	// configuration (e.g. delay amount <= 0) must be rejected with an
	// ErrCompileFailure (HTTP 422) and must NOT leak a persisted workflow row.
	if _, err := Compile(wf); err != nil {
		return domain.Workflow{}, &ErrCompileFailure{Cause: err}
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
		if st := stepTypes.Get(step.Type); st != nil {
			compiled, cErr := st.Compile(step)
			if cErr != nil {
				return engine.WorkflowDef{}, fmt.Errorf("authoring: step %q: %w", step.StepID, cErr)
			}
			sd = compiled
		} else {
			// Unknown/legacy type: preserve historical behavior — an
			// unregistered type compiled to a channel "send.<type>" task.
			sd = engine.StepDef{ID: step.StepID, Task: "send." + step.Type, Type: "normal"}
		}
		if i > 0 {
			sd.DependsOn = []string{steps[i-1].StepID}
		}
		def.Steps = append(def.Steps, sd)
	}
	return def, nil
}

// GetBySlug loads a single workflow by its trigger identifier (slug) within
// the Scope. Returns store.ErrNotFound when no matching row exists.
func (s *Service) GetBySlug(ctx context.Context, scope domain.Scope, slug string) (domain.Workflow, error) {
	return s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
}

// GetByID loads a single workflow by its internal ULID id within the Scope.
// Returns store.ErrNotFound when no matching row exists. Used by the console
// resend action (W4 #55), which has a notification's WorkflowID and needs the
// workflow's trigger identifier to re-fire it.
func (s *Service) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Workflow, error) {
	return s.store.Workflows().GetByID(ctx, scope, id)
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
	// A nil engine means the service is wired for store-only operation
	// (the operator console seeds and edits workflow rows without a live
	// engine connection in tests). Registration is skipped; Create already
	// persists rows without touching the engine, so this keeps the two
	// paths consistent.
	if s.eng == nil {
		return nil
	}
	return s.eng.RegisterWorkflow(ctx, def)
}

// RepublishAll re-registers every persisted workflow with the engine. It is
// called once at boot: the dagnats engine holds its workflow registry in
// memory and loses it on restart, so a workflow that was only registered at
// create/update time silently stops firing (the engine runs it with
// task_count=0) until the row is saved again. RepublishAll closes that gap by
// compiling and re-registering each stored workflow on startup.
//
// It is resilient by design: a workflow that fails to compile (e.g. a
// malformed delay step persisted by an older version) is logged and skipped
// rather than aborting boot. The count of successfully re-registered workflows
// is returned for the caller to log. A nil engine (store-only wiring) makes
// this a no-op returning 0.
func (s *Service) RepublishAll(ctx context.Context) (int, error) {
	wfs, err := s.store.Workflows().ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("authoring: list all workflows: %w", err)
	}
	if s.eng == nil {
		return 0, nil
	}
	registered := 0
	for _, wf := range wfs {
		def, cErr := Compile(wf)
		if cErr != nil {
			slog.Default().Error("authoring: republish skip (compile failed)",
				"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
				"org_id", wf.OrgID, "env_id", wf.EnvID, "err", cErr)
			continue
		}
		if rErr := s.eng.RegisterWorkflow(ctx, def); rErr != nil {
			slog.Default().Error("authoring: republish skip (register failed)",
				"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
				"org_id", wf.OrgID, "env_id", wf.EnvID, "err", rErr)
			continue
		}
		registered++
	}
	return registered, nil
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
	// Compile was already validated above; cannot fail here. A nil engine
	// (store-only wiring, e.g. the console in tests) skips registration —
	// the row is already persisted, consistent with Create.
	if s.eng != nil {
		def, _ := Compile(candidate)
		if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
			return domain.Workflow{}, fmt.Errorf("authoring: re-register workflow: %w", err)
		}
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
		// The workflow row is already persisted; Publish failure is non-fatal
		// (matches the Create path). Log and return the created workflow so
		// callers can redirect to the editor instead of surfacing a false error.
		slog.Default().Error("authoring: publish after duplicate (non-fatal)", "trigger", in.TriggerIdentifier, "err", err)
	}

	return created, nil
}

// isNotFound reports whether err is a store.ErrNotFound sentinel.
func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
