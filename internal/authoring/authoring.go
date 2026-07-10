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
	"regexp"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// stepTypes is the canonical step-type registry: the single source of truth for
// how each step type compiles, defaults, parses, and validates. Compile drives
// every step's StepDef from it.
var stepTypes = steptype.Default()

// idempotencyDotPath is the dagnats StartRun admission key every compiled DAG
// dedupes on: a top-level dot-path evaluated against the run input. It targets
// runenv.InAppEnvelope.IdempotencyKey (json:"idempotencyKey", a top-level
// sibling of Payload, NOT nested under it) — the envelope shape shared by
// every channel DAG, so one constant is correct for all compiled workflows.
// trigger.go composes that field from (TransactionID, SubscriberID) rather
// than using TransactionID alone: a multicast trigger (to.topicKey) mints ONE
// TransactionID shared by every fan-out recipient, so a bare "transactionId"
// dot-path would collide every recipient after the first onto one dagnats
// run. A retried trigger reuses TransactionID (trigger.go mints a ULID only
// when the client omits one) against the SAME recipient, so the composed key
// still dedupes a genuine retry while leaving distinct multicast recipients
// distinct.
const idempotencyDotPath = "idempotencyKey"

// registerer is the subset of *engine.Engine authoring depends on to publish
// a compiled DAG. Publish, Update, RepublishAll, and Rollback register
// through it; tests substitute a fake implementation to assert exactly what
// gets registered without booting a real dagnats sidecar (engine.Engine has
// no RegisterWorkflow introspection API — a live sidecar can prove
// RegisterWorkflow didn't error, but not what def it was called with).
type registerer interface {
	RegisterWorkflow(ctx context.Context, def engine.WorkflowDef) error
}

// Service persists workflows and registers their compiled DAGs with the engine.
type Service struct {
	store *store.Store
	eng   registerer
}

// New constructs a Service over the given store and engine. eng may be nil
// for store-only wiring (e.g. the operator console in tests); passing a
// non-nil *engine.Engine (or any other registerer) enables DAG registration.
func New(s *store.Store, eng registerer) *Service {
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

// triggerIdentifierPattern is the URL-safe slug allowlist for
// WorkflowInput.TriggerIdentifier: lowercase letters, digits, hyphens, and
// underscores, starting with a letter or digit. TriggerIdentifier is used
// verbatim as a console URL path segment (view/edit/delete routes) and as
// the DAGName suffix registered with the engine; an id containing '/' (path
// separator) or other reserved/URL-unsafe characters produces a workflow
// every console route 404s on, permanently orphaning it (see H2).
var triggerIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ErrInvalidTriggerIdentifier is returned by Create when
// WorkflowInput.TriggerIdentifier fails triggerIdentifierPattern. Checked
// before any store write, mirroring ErrCompileFailure's before-write
// validation.
var ErrInvalidTriggerIdentifier = fmt.Errorf("authoring: trigger identifier may only contain lowercase letters, numbers, hyphens, and underscores")

// WorkflowInput is the authoring request: a named workflow with an ordered
// list of channel steps keyed by TriggerIdentifier.
type WorkflowInput struct {
	Name              string
	TriggerIdentifier string
	Steps             []domain.Step
	// Critical marks the workflow no-opt-out: subscriber channel/workflow
	// preferences cannot suppress its delivery (see domain.Workflow.Critical).
	Critical bool
}

// Create persists a workflow row. Steps are marshalled into the row's JSON
// steps column and the workflow is active on creation.
func (s *Service) Create(ctx context.Context, scope domain.Scope, in WorkflowInput) (domain.Workflow, error) {
	if !triggerIdentifierPattern.MatchString(in.TriggerIdentifier) {
		return domain.Workflow{}, ErrInvalidTriggerIdentifier
	}
	steps, err := json.Marshal(in.Steps)
	if err != nil {
		return domain.Workflow{}, fmt.Errorf("authoring: marshal steps: %w", err)
	}
	wf := domain.Workflow{
		Name:              in.Name,
		TriggerIdentifier: in.TriggerIdentifier,
		Active:            true,
		Steps:             steps,
		Critical:          in.Critical,
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
		Name:           wf.EnvID + "-" + wf.TriggerIdentifier,
		Steps:          make([]engine.StepDef, 0, len(steps)),
		IdempotencyKey: idempotencyDotPath,
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

// Publish loads the workflow by trigger identifier, compiles it, registers
// the DAG with the engine, and — on successful registration — appends the
// exact registered bytes to the workflow's version stack (store.WorkflowVersionRepo)
// so a bad publish is a Rollback rather than a rebuild.
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
	// engine connection in tests). Registration — and therefore versioning,
	// which only captures what was actually registered — is skipped; Create
	// already persists rows without touching the engine, so this keeps the
	// two paths consistent.
	if s.eng == nil {
		return nil
	}
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return err
	}
	return s.recordVersion(ctx, scope, wf.ID, wf.Name, wf.TriggerIdentifier, wf.Steps, wf.PayloadSchema, wf.Critical, def)
}

// recordVersion marshals def (the exact bytes just registered with the
// engine) and appends it, together with the full editable-row snapshot
// (steps/payloadSchema/critical — migration 0028), to the workflow's version
// stack. Shared by Publish and Rollback — rollback is itself a publish, so it
// too grows the append-only stack rather than mutating history.
// name/triggerIdentifier are the metadata stored alongside the def: Rollback
// passes the ROLLED-BACK version's name/trigger (not the current live row's)
// so the audit row describes the def it actually carries. steps/
// payloadSchema/critical make each version a complete snapshot of the
// editable workflow row at the moment it was published, not just its
// compiled DAG — this is what lets Rollback restore the whole row (see
// Rollback) instead of only re-registering the engine def.
func (s *Service) recordVersion(ctx context.Context, scope domain.Scope, workflowID, name, triggerIdentifier string, steps, payloadSchema json.RawMessage, critical bool, def engine.WorkflowDef) error {
	defJSON, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("authoring: marshal compiled def: %w", err)
	}
	if _, err := s.store.WorkflowVersions().Create(ctx, scope, workflowID, name, triggerIdentifier, defJSON, string(steps), string(payloadSchema), critical); err != nil {
		return fmt.Errorf("authoring: record version: %w", err)
	}
	return nil
}

// Rollback restores an earlier published version's FULL editable snapshot —
// steps, name, payload_schema, critical (migration 0028) — and re-registers
// its compiled DAG byte-for-byte (skipping Compile — the persisted
// compiled_def IS the def that was registered), then appends a new version
// row copying that snapshot, keeping the version stack append-only and
// auditable: a rollback is itself a publish.
//
// Restoring the whole row (not just re-registering the def) fixes a
// correctness bug, not just a UI-honesty gap: delivery reads step CONTENT
// from the live workflows.steps row at send time
// (internal/delivery/rebuild.go), not from the compiled def. A rollback that
// only re-registered compiled_def would run the ROLLED-BACK version's
// TOPOLOGY (step ids, ordering) against the CURRENT row's CONTENT — a hybrid
// that errors outright at delivery when a later edit renamed or removed a
// step id the older def still references (H8).
//
// A version captured before migration 0028 (ver.Steps == "", the column's
// ADD COLUMN default) carries no snapshot to restore from: Rollback falls
// back to the original engine-only behavior — register the compiled def,
// leave the editable row untouched — rather than blank workflows.steps.
//
// versionID is scope-checked via a workflow_id -> workflows join
// (store.WorkflowVersionRepo.Get) and, additionally, checked against the
// workflow resolved from triggerIdentifier — a version id that exists in this
// scope but belongs to a different workflow returns store.ErrNotFound rather
// than cross-wiring one workflow's DAG onto another's trigger.
func (s *Service) Rollback(ctx context.Context, scope domain.Scope, triggerIdentifier, versionID string) error {
	wf, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, triggerIdentifier)
	if err != nil {
		return err
	}
	ver, err := s.store.WorkflowVersions().Get(ctx, scope, versionID)
	if err != nil {
		return err
	}
	if ver.WorkflowID != wf.ID {
		return store.ErrNotFound
	}
	var def engine.WorkflowDef
	if err := json.Unmarshal(ver.CompiledDef, &def); err != nil {
		return fmt.Errorf("authoring: unmarshal version %s: %w", versionID, err)
	}

	// steps/payloadSchema/critical default to the LIVE row's current values
	// and are only overwritten below when ver carries a full-row snapshot;
	// this makes the pre-0028 fallback record its new version entry against
	// the (untouched) live row rather than blank values.
	steps, payloadSchema, critical := wf.Steps, wf.PayloadSchema, wf.Critical

	if ver.Steps != "" {
		restored := wf
		restored.Name = ver.Name
		restored.Steps = json.RawMessage(ver.Steps)
		restored.PayloadSchema = json.RawMessage(ver.PayloadSchema)
		restored.Critical = ver.Critical
		if err := s.store.Workflows().Update(ctx, scope, restored); err != nil {
			return fmt.Errorf("authoring: restore editable row for rollback: %w", err)
		}
		steps, payloadSchema, critical = restored.Steps, restored.PayloadSchema, restored.Critical
	} else {
		slog.Default().Warn("authoring: rollback target predates full-row snapshots (migration 0028); registering compiled def only, editable row left unchanged",
			"workflow_id", wf.ID, "trigger_identifier", triggerIdentifier, "version_id", versionID)
	}

	// Mirrors Publish's nil-engine store-only wiring: no live engine means
	// there is nothing to re-register (the row restore above, when it
	// happened, still stands — it is a pure store operation independent of
	// engine wiring).
	if s.eng == nil {
		return nil
	}
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return fmt.Errorf("authoring: rollback register: %w", err)
	}
	// Append the rolled-back def (and, when available, its full-row snapshot)
	// as the new latest version, tagged with the version it restored
	// (ver.Name/TriggerIdentifier) so the audit row and RepublishAll's
	// boot-time re-registration both track v1's def, not the current live
	// row's.
	return s.recordVersion(ctx, scope, wf.ID, ver.Name, ver.TriggerIdentifier, steps, payloadSchema, critical, def)
}

// RepublishAll re-registers every persisted workflow with the engine. It is
// called once at boot: the dagnats engine holds its workflow registry in
// memory and loses it on restart, so a workflow that was only registered at
// create/update time silently stops firing (the engine runs it with
// task_count=0) until the row is saved again. RepublishAll closes that gap by
// re-registering each stored workflow's last-published compiled DAG on startup.
//
// workflow_versions is the durable source of truth: for a workflow that has
// at least one version row, RepublishAll re-registers the LATEST version's
// compiled_def BYTE-FOR-BYTE (json.Unmarshal → RegisterWorkflow, skipping
// Compile — identical to Rollback). This makes rollback survive a restart (a
// rollback appends a new latest version, so boot re-registers that) AND fixes
// the original log-and-skip failure: a workflow whose live steps no longer
// compile still boots from its last-good published version instead of being
// silently dropped.
//
// Only a workflow with NO version rows (pre-migration rows, or a row created
// but never published) falls back to Compile(wf.steps); its next publish
// captures a version and moves it onto the durable path.
//
// It is resilient by design: a workflow that fails to unmarshal/compile or
// register is logged and skipped rather than aborting boot. The count of
// successfully re-registered workflows is returned for the caller to log. A
// nil engine (store-only wiring) makes this a no-op returning 0.
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
		def, ok := s.bootDef(ctx, wf)
		if !ok {
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

// bootDef resolves the WorkflowDef RepublishAll should register for wf at
// boot. It prefers the latest published version's compiled_def (durable
// source of truth, unmarshaled byte-for-byte); only a workflow with no
// version rows falls back to compiling its live steps. ok=false means the
// workflow was logged-and-skipped (bad version bytes or an uncompilable
// pre-version row) and must not be registered.
func (s *Service) bootDef(ctx context.Context, wf domain.Workflow) (engine.WorkflowDef, bool) {
	ver, vErr := s.store.WorkflowVersions().LatestByWorkflow(ctx, wf.ID)
	switch {
	case vErr == nil:
		var def engine.WorkflowDef
		if uErr := json.Unmarshal(ver.CompiledDef, &def); uErr != nil {
			slog.Default().Error("authoring: republish skip (version unmarshal failed)",
				"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
				"org_id", wf.OrgID, "env_id", wf.EnvID, "version_id", ver.ID, "err", uErr)
			return engine.WorkflowDef{}, false
		}
		return def, true
	case errors.Is(vErr, store.ErrNotFound):
		// No published version yet (pre-migration / never-published): fall
		// back to compiling the live steps, preserving prior behavior.
		def, cErr := Compile(wf)
		if cErr != nil {
			slog.Default().Error("authoring: republish skip (compile failed)",
				"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
				"org_id", wf.OrgID, "env_id", wf.EnvID, "err", cErr)
			return engine.WorkflowDef{}, false
		}
		return def, true
	default:
		slog.Default().Error("authoring: republish skip (load version failed)",
			"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
			"org_id", wf.OrgID, "env_id", wf.EnvID, "err", vErr)
		return engine.WorkflowDef{}, false
	}
}

// UpdateInput carries the mutable fields for an Update call.
type UpdateInput struct {
	Name          string
	Steps         []domain.Step
	PayloadSchema json.RawMessage
	// Critical is editable post-create (unlike Active/Origin, which Update
	// deliberately carries forward from existing rather than accepting here).
	Critical bool
}

// ErrCompileFailure wraps a Compile error so HTTP handlers can map it to 422.
type ErrCompileFailure struct{ Cause error }

func (e *ErrCompileFailure) Error() string { return "compile failure: " + e.Cause.Error() }
func (e *ErrCompileFailure) Unwrap() error { return e.Cause }

// ErrNoSteps signals that Update persisted the workflow's config (name,
// steps, payload schema, critical) but skipped re-registering it with the
// engine because the candidate has zero steps — dagnats rejects a 0-step
// registration outright, so attempting it would either error or corrupt
// state. The store write already happened by the time this is returned:
// callers must not treat it as a failed save (M6). It is informational —
// the workflow will not run until at least one step is added and saved
// again — not a rejection of the write.
var ErrNoSteps = errors.New("authoring: workflow has no steps to publish")

// Update persists a new name/steps/payload_schema for the identified workflow,
// recompiles the DAG, and re-registers it with the engine so edits take effect
// on the next trigger. A step configuration that fails Compile (e.g. invalid
// delay amount) is rejected with an ErrCompileFailure before any write occurs.
// Returns store.ErrNotFound when the workflow does not exist in this scope.
// Returns ErrNoSteps when in.Steps is empty: the config write still happens,
// but engine re-registration is skipped (see ErrNoSteps).
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
		Critical:          in.Critical,
	}
	if _, err := Compile(candidate); err != nil {
		return domain.Workflow{}, &ErrCompileFailure{Cause: err}
	}

	// Persist.
	if err := s.store.Workflows().Update(ctx, scope, candidate); err != nil {
		return domain.Workflow{}, err
	}

	// A 0-step candidate cannot be registered (dagnats requires at least one
	// step): skip registration rather than let the engine reject it, and
	// signal ErrNoSteps so callers know the config write above succeeded.
	if len(in.Steps) == 0 {
		return candidate, ErrNoSteps
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
		// Preserve the source's no-opt-out status: cloning a critical workflow
		// (e.g. duplicating "password-reset" to iterate on it) must yield a
		// critical copy, or the guarantee is silently lost on the new row.
		Critical: src.Critical,
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
