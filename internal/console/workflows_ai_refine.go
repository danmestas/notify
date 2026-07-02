package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// handleWorkflowAIRefine handles POST /console/api/workflows/{slug}/ai-refine. It
// loads the scope-checked workflow, seeds a refine-mode builder session from its
// current shape (name + trigger identifier + steps) plus the requested change,
// dispatches a durable agent-loop run, and holds the SSE connection open relaying
// the run's live Events exactly like ai-generate — reasoning appends, the canvas
// re-renders, and the terminal done event renders the refined proposal with an
// Apply/Discard action pair. A missing/cross-scope workflow 404s; an unconfigured
// builder returns a graceful message.
func (c *Console) handleWorkflowAIRefine(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIRefine: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIRefine: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIRefine: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		c.serveNotFound(w, r)
		return
	}
	if c.aiBuilder == nil || c.buildStream == nil || c.deps.BuilderSessions == nil {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Mode: store.BuilderSessionModeRefine,
			Slug: slug,
			Error: "AI builder not configured. Set NOTIFY_AI_API_KEY (or ANTHROPIC_API_KEY) to enable " +
				"Refine with AI.",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			Mode:  store.BuilderSessionModeRefine,
			Slug:  slug,
			Error: "Describe the change you want before refining.",
		})
		return
	}

	// Load the workflow under scope; a missing or cross-tenant slug 404s (no seed,
	// no session leaks into the caller's scope).
	wf, err := c.deps.Authoring.GetBySlug(r.Context(), scope, slug)
	if err != nil {
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai refine load workflow", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	seed, err := draftFromWorkflow(wf)
	if err != nil {
		c.deps.Logger.Error("console: ai refine decode workflow steps", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}
	seedJSON, err := json.Marshal(seed)
	if err != nil {
		c.deps.Logger.Error("console: ai refine marshal seed", "slug", slug, "err", err)
		c.serveError(w, r)
		return
	}

	sess, err := c.deps.BuilderSessions.CreateRefine(r.Context(), scope, prompt, slug, seedJSON)
	if err != nil {
		c.deps.Logger.Error("console: ai refine create session", "err", err)
		c.serveError(w, r)
		return
	}

	runID, err := c.aiBuilder.Dispatch(r.Context(), scope, sess.ID)
	if err != nil {
		c.deps.Logger.Error("console: ai refine dispatch run", "err", err)
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Mode:      store.BuilderSessionModeRefine,
			Slug:      slug,
			Error:     "Could not start the builder. Please try again.",
		})
		return
	}

	c.streamAIBuild(w, r, scope, sess.ID, runID, store.BuilderSessionModeRefine, slug)
}

// handleWorkflowAIApply handles POST /console/api/workflows/{slug}/ai-apply/{sessionID}.
// It loads the scope-checked refined draft and pushes it through
// authoring.Update, recompiling and republishing the workflow, then redirects to
// the (updated) workflow detail page. It applies only when the session targets
// THIS slug (no cross-workflow clobber) and the durable run has finished (Ready,
// not a mid-run draft). Idempotency + scope checks mirror ai-confirm.
func (c *Console) handleWorkflowAIApply(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("handleWorkflowAIApply: w is nil")
	}
	if r == nil {
		panic("handleWorkflowAIApply: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleWorkflowAIApply: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	if c.deps.BuilderSessions == nil {
		c.serveNotFound(w, r)
		return
	}
	slug := chi.URLParam(r, "slug")
	sessionID := chi.URLParam(r, "sessionID")
	if slug == "" || sessionID == "" {
		http.Error(w, "missing slug or session", http.StatusBadRequest)
		return
	}

	sess, err := c.deps.BuilderSessions.Get(r.Context(), scope, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai apply load session", "err", err)
		c.serveError(w, r)
		return
	}

	// Target guard: the session must have been created to refine THIS workflow.
	// Scope alone is not enough — a session refining workflow A must not be
	// applicable to workflow B in the same environment, which would clobber B with
	// A's draft. A mismatch is indistinguishable from a wrong session id, so 404.
	if sess.TargetSlug != slug {
		c.serveNotFound(w, r)
		return
	}

	// Readiness + idempotency guard: only a session the durable run has finished
	// (Ready) may be applied. A still-running (Draft) session would apply a
	// partial/original draft to the live workflow; an already-applied or discarded
	// one must not re-run Update. Every non-Ready status is rejected inline.
	if sess.Status != store.BuilderSessionReady {
		msg := "These changes are still being generated — try again in a moment."
		switch sess.Status {
		case store.BuilderSessionConfirmed:
			msg = "These changes were already applied."
		case store.BuilderSessionDiscarded:
			msg = "These changes were discarded."
		}
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Mode:      store.BuilderSessionModeRefine,
			Slug:      slug,
			Error:     msg,
		})
		return
	}

	var draft aibuilder.WorkflowDraft
	if err := json.Unmarshal(sess.DraftWorkflow, &draft); err != nil {
		c.deps.Logger.Error("console: ai apply decode draft", "err", err)
		c.patchAIPreview(w, r, aiBuilderPreviewView{
			SessionID: sess.ID,
			Mode:      store.BuilderSessionModeRefine,
			Slug:      slug,
			Error:     "The refined workflow could not be read. Refine again.",
		})
		return
	}

	if _, err := c.deps.Authoring.Update(r.Context(), scope, slug, authoring.UpdateInput{
		Name:          draft.Name,
		Steps:         draft.Steps,
		PayloadSchema: draft.PayloadSchema,
	}); err != nil {
		var compileErr *authoring.ErrCompileFailure
		if errors.As(err, &compileErr) {
			c.patchAIPreview(w, r, c.aiRefinePreview(sess.ID, slug, &draft, "Step configuration is invalid: "+compileErr.Error()))
			return
		}
		if isNotFound(err) {
			c.serveNotFound(w, r)
			return
		}
		c.deps.Logger.Error("console: ai apply update", "err", err)
		c.patchAIPreview(w, r, c.aiRefinePreview(sess.ID, slug, &draft, "Applying the changes failed."))
		return
	}
	if err := c.deps.Authoring.Publish(r.Context(), scope, slug); err != nil {
		c.deps.Logger.Error("console: ai apply publish", "err", err)
		// The row is updated; the editor reflects the change. Continue to it.
	}
	if err := c.deps.BuilderSessions.SetStatus(r.Context(), scope, sess.ID, store.BuilderSessionConfirmed); err != nil {
		c.deps.Logger.Error("console: ai apply set status", "err", err)
	}
	http.Redirect(w, r, "/console/workflows/"+slug, http.StatusSeeOther)
}

// aiRefinePreview builds a refine-mode preview view (Apply/Discard action pair)
// with an optional inline error banner.
func (c *Console) aiRefinePreview(sessionID, slug string, draft *aibuilder.WorkflowDraft, errMsg ...string) aiBuilderPreviewView {
	v := c.aiPreviewFromDraft(sessionID, draft, nil, errMsg...)
	v.Mode = store.BuilderSessionModeRefine
	v.Slug = slug
	return v
}

// draftFromWorkflow converts a persisted workflow into a WorkflowDraft the
// AgentLoop can seed a refine run from: the name + trigger identifier carry over,
// and the JSON steps column is decoded into the draft's []domain.Step. The
// trigger identifier is immutable, so seeding it lets setWorkflowMetadata (which
// only writes an empty identifier) leave it untouched.
func draftFromWorkflow(wf domain.Workflow) (aibuilder.WorkflowDraft, error) {
	draft := aibuilder.WorkflowDraft{
		Name:              wf.Name,
		TriggerIdentifier: wf.TriggerIdentifier,
		PayloadSchema:     wf.PayloadSchema,
	}
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &draft.Steps); err != nil {
			return aibuilder.WorkflowDraft{}, err
		}
	}
	return draft, nil
}
