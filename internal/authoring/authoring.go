// Package authoring turns workflow definitions into persisted rows and
// compiled engine DAGs. It owns the canonical DAG-name convention
// (envID-triggerIdentifier) that trigger and delivery agree on.
package authoring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/secretbox"
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
	// box encrypts declared secret control fields (e.g. http_request headers)
	// before a step is persisted. nil disables encryption -- store-only or
	// legacy wiring keeps working, storing plaintext exactly as before.
	box *secretbox.Box
}

// New constructs a Service over the given store and engine. eng may be nil
// for store-only wiring (e.g. the operator console in tests); passing a
// non-nil *engine.Engine (or any other registerer) enables DAG registration.
func New(s *store.Store, eng registerer, opts ...Option) *Service {
	svc := &Service{store: s, eng: eng}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Option configures a Service at construction.
type Option func(*Service)

// WithSecretBox enables at-rest encryption of declared secret control fields.
// Omitting it preserves the previous plaintext behaviour, so every existing
// New(store, eng) caller is unchanged.
func WithSecretBox(box *secretbox.Box) Option {
	return func(s *Service) { s.box = box }
}

// sealSteps encrypts each step's declared secret fields before persistence.
// prior maps step id -> the step as currently stored, so an unchanged (masked)
// secret is preserved rather than re-sealed; pass nil on create.
func (s *Service) sealSteps(steps []domain.Step, prior map[string]domain.Step) ([]domain.Step, error) {
	if s.box == nil {
		return steps, nil
	}
	out := make([]domain.Step, len(steps))
	for i, step := range steps {
		sealed, err := stepTypes.EncryptSecrets(s.box, step, prior[step.StepID])
		if err != nil {
			return nil, fmt.Errorf("authoring: seal step %q: %w", step.StepID, err)
		}
		out[i] = sealed
	}
	return out, nil
}

// DAGName is the canonical engine workflow name: envID-scoped trigger
// identifier joined by '-'. trigger and delivery compute the same value.
// NOTE: dagnats stores workflows under this name as a NATS KV key, which
// rejects ':' (`nats: invalid key`); '-' is KV- and subject-token-safe and is
// proven end-to-end by the engine e2e (workflow "foundation-smoke").
func DAGName(scope domain.Scope, triggerIdentifier string) string {
	return names.Workflow(scope.EnvID, triggerIdentifier)
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
	// PayloadSchema is optional and, unlike Critical below, is not set by
	// postWorkflow (POST /v1/workflows never accepted one) -- only Ensure's
	// create-when-absent path populates it, so it can carry the full PUT
	// body's payload schema into a brand-new row.
	PayloadSchema json.RawMessage
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
	sealedSteps, err := s.sealSteps(in.Steps, nil)
	if err != nil {
		return domain.Workflow{}, err
	}
	steps, err := json.Marshal(sealedSteps)
	if err != nil {
		return domain.Workflow{}, fmt.Errorf("authoring: marshal steps: %w", err)
	}
	wf := domain.Workflow{
		Name:              in.Name,
		TriggerIdentifier: in.TriggerIdentifier,
		Active:            true,
		Steps:             steps,
		PayloadSchema:     in.PayloadSchema,
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
	// Empty bytes and JSON null are both "no steps yet" (a config-only draft),
	// not malformed input.
	if err := json.Unmarshal(wf.Steps, &steps); len(wf.Steps) > 0 && err != nil {
		return engine.WorkflowDef{}, fmt.Errorf("authoring: unmarshal steps: %w", err)
	}
	def := engine.WorkflowDef{
		Name:           names.Workflow(wf.EnvID, wf.TriggerIdentifier),
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
			sd = engine.StepDef{ID: step.StepID, Task: names.Send(step.Type), Type: dag.StepTypeNormal}
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
	upgradeDef(&def)

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

// republishConcurrency bounds RepublishAll's in-flight registrations.
const republishConcurrency = 8

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
//
// Registrations overlap, republishConcurrency at a time: on a multi-node
// dagnats each one waits on replication round trips (about 1.6 s on the
// mesh), so a serial boot of a few dozen workflows would outlast a deploy's
// health window.
func (s *Service) RepublishAll(ctx context.Context) (int, error) {
	wfs, err := s.store.Workflows().ListAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("authoring: list all workflows: %w", err)
	}
	if s.eng == nil {
		return 0, nil
	}
	var (
		wg         sync.WaitGroup
		registered atomic.Int32
		slots      = make(chan struct{}, republishConcurrency)
	)
	for _, wf := range wfs {
		def, ok := s.bootDef(ctx, wf)
		if !ok {
			continue
		}
		if len(def.Steps) == 0 {
			// A config-only draft (see ErrNoSteps): dagnats rejects a 0-step
			// registration, and Update already declines to attempt one. It is
			// an expected state, not a boot failure, so do not log it as one.
			slog.Default().Info("authoring: republish skip (draft has no steps)",
				"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
				"org_id", wf.OrgID, "env_id", wf.EnvID)
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			if rErr := s.eng.RegisterWorkflow(ctx, def); rErr != nil {
				slog.Default().Error("authoring: republish skip (register failed)",
					"workflow_id", wf.ID, "trigger_identifier", wf.TriggerIdentifier,
					"org_id", wf.OrgID, "env_id", wf.EnvID, "err", rErr)
				return
			}
			registered.Add(1)
		}()
	}
	wg.Wait()
	return int(registered.Load()), nil
}

// upgradeDef gives a definition persisted before the notify. prefix (N4) the
// names it would have today: its workflow name and every step's task type.
// Stored compiled_def bytes are left as written; reading them through here is
// what lets a rollback or a boot re-register an old version under names that
// match what trigger starts and what the workers handle. Idempotent.
func upgradeDef(def *engine.WorkflowDef) {
	def.Name = names.Upgrade(def.Name)
	for i := range def.Steps {
		def.Steps[i].Task = names.Upgrade(def.Steps[i].Task)
	}
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
		upgradeDef(&def)
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
// recompiles the DAG, re-registers it with the engine so edits take effect on
// the next trigger, and — on successful registration — appends the exact
// registered bytes (plus the full editable-row snapshot) to the workflow's
// version stack, exactly as Publish does. Update is the plain step-editor Save
// path (and the AI-refine Keep path): it changes what the engine runs LIVE, so
// that change must be captured in Version History too, or history would keep
// showing a stale Publish as "current" while the engine already runs the edit
// (an H8-class "history lies about what's live" bug). A step configuration that
// fails Compile (e.g. invalid delay amount) is rejected with an
// ErrCompileFailure before any write occurs. Returns store.ErrNotFound when the
// workflow does not exist in this scope. Returns ErrNoSteps when in.Steps is
// empty: the config write still happens, but engine re-registration — and
// therefore versioning — is skipped (see ErrNoSteps).
func (s *Service) Update(ctx context.Context, scope domain.Scope, slug string, in UpdateInput) (domain.Workflow, error) {
	// Load the existing row to obtain its ID (required for the UPDATE predicate).
	existing, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
	if err != nil {
		return domain.Workflow{}, err
	}

	// Seal secret fields, passing the currently-stored steps as prior so a
	// secret the operator left masked is preserved rather than re-sealed as the
	// mask (see EncryptSecrets).
	prior, err := priorStepsByID(existing.Steps)
	if err != nil {
		return domain.Workflow{}, err
	}
	sealedSteps, err := s.sealSteps(in.Steps, prior)
	if err != nil {
		return domain.Workflow{}, err
	}
	steps, err := json.Marshal(sealedSteps)
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
	//
	// Divergence window (deliberate, not an oversight): the store write above
	// has already committed, so if RegisterWorkflow fails here the row and the
	// engine's compiled DAG disagree until the next successful Update. The
	// store describes the new definition; the engine still runs the old one.
	//
	// This is left uncompensated on purpose. A rollback write would have to
	// succeed at exactly the moment the process is already failing to reach a
	// dependency, and if IT failed the state would be worse and harder to
	// reason about than the single-writer divergence it was meant to undo.
	// RegisterWorkflow is idempotent, so any later Update — or a RepublishAll —
	// reconciles the two, and the error is returned to the caller rather than
	// swallowed, so the failure is visible at the point it happens.
	if s.eng != nil {
		def, _ := Compile(candidate)
		if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
			return domain.Workflow{}, fmt.Errorf("authoring: re-register workflow: %w", err)
		}
		// Capture the just-registered def in Version History so "current"
		// reflects what Update delivered — mirrors Publish. Guarded by the same
		// nil-engine check: store-only wiring records nothing (consistent with
		// Create), keeping every nil-engine caller's version count unchanged.
		if err := s.recordVersion(ctx, scope, candidate.ID, candidate.Name, candidate.TriggerIdentifier, candidate.Steps, candidate.PayloadSchema, candidate.Critical, def); err != nil {
			return domain.Workflow{}, err
		}
	}

	return candidate, nil
}

// EnsureInput is Ensure's desired-state request: the full PUT body plus the
// slug it targets.
type EnsureInput struct {
	Name              string
	TriggerIdentifier string
	Steps             []domain.Step
	PayloadSchema     json.RawMessage
	// Critical, when set, is part of the desired state: a different value is
	// a change, and an update writes it. Nil keeps whatever the workflow
	// already has, so a caller that never sends it cannot clear a flag set
	// elsewhere (danmestas/notify#2).
	Critical *bool
}

// EnsureOutcome reports what Ensure actually did, so the HTTP handler can
// pick the right status code (201 for EnsureCreated, 200 otherwise).
type EnsureOutcome int

const (
	// EnsureCreated means no row existed for the slug; one was created.
	EnsureCreated EnsureOutcome = iota
	// EnsureUnchanged means a row existed and its definition already
	// matched the submitted one; nothing was written.
	EnsureUnchanged
	// EnsureUpdated means a row existed with a different definition and was
	// overwritten (OnConflictUpdate).
	EnsureUpdated
)

// OnConflict selects Ensure's behavior when the workflow already exists with
// a definition different from the one submitted.
type OnConflict string

const (
	// OnConflictUpdate overwrites the stored definition -- today's PUT
	// behavior, kept as the default so existing callers see no change.
	OnConflictUpdate OnConflict = "update"
	// OnConflictKeep rejects the write with ErrDefinitionConflict instead of
	// touching the stored row.
	OnConflictKeep OnConflict = "keep"
)

// ErrDefinitionConflict is returned by Ensure when onConflict is
// OnConflictKeep and the stored definition differs from the submitted one.
// Existing is the stored workflow, so the caller can summarize it in a 409
// response body for the client to diff against.
type ErrDefinitionConflict struct {
	Existing domain.Workflow
}

func (e *ErrDefinitionConflict) Error() string {
	return "authoring: workflow exists with a different definition"
}

// Ensure makes the workflow identified by slug match in, treating PUT
// /v1/workflows/{slug} as an idempotent upsert rather than an unconditional
// overwrite:
//
//   - absent -> created (EnsureCreated). The caller is still responsible for
//     Publish, exactly as postWorkflow (POST) calls Create then Publish
//     separately -- Ensure only performs the store write.
//   - present and the submitted definition (name, steps, payload schema)
//     canonically equals the stored one -> EnsureUnchanged: no write, no new
//     workflow_version row, engine left alone. TriggerIdentifier and Active
//     are not part of the comparison: TriggerIdentifier is the lookup key
//     (slug) so it can never differ, and PUT has never accepted an Active
//     field (SetActive is the only mutator), so it likewise never differs.
//   - present and different -> onConflict decides. OnConflictUpdate (the
//     default, preserving today's PUT behavior) overwrites via Update, which
//     registers the recompiled DAG and records a new version, exactly as a
//     direct Update call does. OnConflictKeep returns ErrDefinitionConflict
//     without writing.
//
// Concurrent identical Ensure calls racing on an absent slug are safe: the
// store's unique index on (environment_id, trigger_identifier) lets exactly
// one Create through, and every loser's Create returns store.ErrConflict.
// Ensure treats that as "someone just created it" and re-reads the row the
// winner wrote, falling into the equal/different check above instead of
// surfacing the race as an error -- so N concurrent identical PUTs all
// resolve to 2xx (one 201, the rest 200 EnsureUnchanged) against exactly one
// stored row.
func (s *Service) Ensure(ctx context.Context, scope domain.Scope, slug string, in EnsureInput, onConflict OnConflict) (domain.Workflow, EnsureOutcome, error) {
	existing, err := s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		wf, cErr := s.Create(ctx, scope, WorkflowInput{
			Name:              in.Name,
			TriggerIdentifier: in.TriggerIdentifier,
			Steps:             in.Steps,
			PayloadSchema:     in.PayloadSchema,
			Critical:          in.Critical != nil && *in.Critical,
		})
		switch {
		case cErr == nil:
			return wf, EnsureCreated, nil
		case errors.Is(cErr, store.ErrConflict):
			// Lost the create race: re-read what the winner wrote and fall
			// through to the equal/different check below.
			existing, err = s.store.Workflows().GetByTriggerIdentifier(ctx, scope, slug)
			if err != nil {
				return domain.Workflow{}, 0, err
			}
		default:
			return domain.Workflow{}, 0, cErr
		}
	case err != nil:
		return domain.Workflow{}, 0, err
	}

	equal, err := s.definitionEqual(existing, in)
	if err != nil {
		return domain.Workflow{}, 0, err
	}
	if equal {
		return existing, EnsureUnchanged, nil
	}
	if onConflict == OnConflictKeep {
		return domain.Workflow{}, 0, &ErrDefinitionConflict{Existing: existing}
	}
	critical := existing.Critical
	if in.Critical != nil {
		critical = *in.Critical
	}
	wf, err := s.Update(ctx, scope, slug, UpdateInput{
		Name:          in.Name,
		Steps:         in.Steps,
		PayloadSchema: in.PayloadSchema,
		Critical:      critical,
	})
	if err != nil {
		return domain.Workflow{}, 0, err
	}
	return wf, EnsureUpdated, nil
}

// definitionEqual reports whether in's definition already matches existing's
// stored one. Steps are sealed exactly as Update would seal them (using
// existing's steps as the "prior" map, so a resubmitted SecretMask resolves
// to the already-stored ciphertext instead of comparing a mask against real
// secret bytes), then both the sealed candidate and the stored steps are
// decrypted back to plaintext before the canonical-JSON comparison.
//
// Comparing plaintext rather than ciphertext matters because secretbox.Seal
// uses a fresh random nonce on every call: an IaC-style caller that resubmits
// the literal plaintext secret (not the SecretMask -- it never GETs first)
// would otherwise seal to different ciphertext bytes on every Ensure call and
// never compare equal, turning every boot into a spurious update (or, under
// onConflict=keep, a spurious 409). Decrypting is deterministic regardless of
// nonce, so this still recognizes an unchanged secret while catching a
// genuinely changed one.
func (s *Service) definitionEqual(existing domain.Workflow, in EnsureInput) (bool, error) {
	if existing.Name != in.Name {
		return false, nil
	}
	if in.Critical != nil && *in.Critical != existing.Critical {
		return false, nil
	}
	existingSteps, err := unmarshalSteps(existing.Steps)
	if err != nil {
		return false, err
	}
	prior, err := priorStepsByID(existing.Steps)
	if err != nil {
		return false, err
	}
	sealedSteps, err := s.sealSteps(in.Steps, prior)
	if err != nil {
		return false, err
	}

	existingPlain, err := s.decryptSteps(existingSteps)
	if err != nil {
		return false, err
	}
	candidatePlain, err := s.decryptSteps(sealedSteps)
	if err != nil {
		return false, err
	}

	existingJSON, err := json.Marshal(existingPlain)
	if err != nil {
		return false, fmt.Errorf("authoring: marshal steps: %w", err)
	}
	candidateJSON, err := json.Marshal(candidatePlain)
	if err != nil {
		return false, fmt.Errorf("authoring: marshal steps: %w", err)
	}
	stepsEqual, err := canonicalJSONEqual(existingJSON, candidateJSON)
	if err != nil {
		return false, err
	}
	if !stepsEqual {
		return false, nil
	}
	return canonicalJSONEqual(existing.PayloadSchema, in.PayloadSchema)
}

// decryptSteps opens each step's declared secret fields, undoing sealSteps.
// It is used only for definitionEqual's plaintext comparison, never for
// persistence: the stored row and any candidate written via Create/Update
// always keep secrets sealed. A nil box (encryption disabled) is a no-op,
// matching sealSteps.
func (s *Service) decryptSteps(steps []domain.Step) ([]domain.Step, error) {
	if s.box == nil {
		return steps, nil
	}
	out := make([]domain.Step, len(steps))
	for i, step := range steps {
		opened, err := stepTypes.DecryptSecrets(s.box, step)
		if err != nil {
			return nil, fmt.Errorf("authoring: decrypt step %q: %w", step.StepID, err)
		}
		out[i] = opened
	}
	return out, nil
}

// unmarshalSteps decodes a persisted workflow's steps JSON into a slice,
// preserving order. An empty or absent steps blob yields nil, not an error.
func unmarshalSteps(raw json.RawMessage) ([]domain.Step, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var steps []domain.Step
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("authoring: unmarshal steps: %w", err)
	}
	return steps, nil
}

// canonicalJSONEqual reports whether a and b encode the same JSON value,
// ignoring key order and whitespace. An empty/absent raw value is treated as
// JSON null, so an unset payload schema compares equal to another unset one.
func canonicalJSONEqual(a, b json.RawMessage) (bool, error) {
	ca, err := canonicalJSON(a)
	if err != nil {
		return false, fmt.Errorf("authoring: canonicalize: %w", err)
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		return false, fmt.Errorf("authoring: canonicalize: %w", err)
	}
	return ca == cb, nil
}

// canonicalJSON re-marshals raw so object key order and insignificant
// whitespace don't affect comparison (encoding/json sorts map keys on
// marshal). Array element order is preserved, which is correct here: step
// order is meaningful.
func canonicalJSON(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "null", nil
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
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

// priorStepsByID unmarshals a persisted workflow's steps JSON into a
// stepID->step map, used to preserve unchanged (masked) secrets on Update. An
// empty or absent steps blob yields an empty map, not an error.
func priorStepsByID(raw json.RawMessage) (map[string]domain.Step, error) {
	if len(raw) == 0 {
		return map[string]domain.Step{}, nil
	}
	var steps []domain.Step
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("authoring: unmarshal prior steps: %w", err)
	}
	m := make(map[string]domain.Step, len(steps))
	for _, st := range steps {
		m[st.StepID] = st
	}
	return m, nil
}
