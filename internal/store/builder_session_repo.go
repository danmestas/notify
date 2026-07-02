package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// Builder session lifecycle statuses. A session starts (and rides each mid-run
// iteration) in Draft; the durable run's terminal finish step flips it to Ready
// once the final validated draft is persisted. Only a Ready session may be
// confirmed (create) or applied (refine) — Draft is NOT a terminal state, so
// guarding on it would let a caller confirm/apply an original or partial draft
// mid-run. Confirmed/Discarded are the post-action terminal states.
const (
	BuilderSessionDraft     = "draft"
	BuilderSessionReady     = "ready"
	BuilderSessionConfirmed = "confirmed"
	BuilderSessionDiscarded = "discarded"
)

// Builder session modes. A create session builds a brand-new workflow from an
// empty draft; a refine session edits an existing workflow, seeded with that
// workflow's current shape and the slug it targets.
const (
	BuilderSessionModeCreate = "create"
	BuilderSessionModeRefine = "refine"
)

// BuilderSession is one AI workflow-builder run: the originating prompt, the
// proposed draft workflow (a WorkflowDraft JSON snapshot) and its lifecycle
// status. Every row is Scope-keyed (org_id + environment_id).
type BuilderSession struct {
	ID            string
	OrgID         string
	EnvID         string
	Status        string
	DraftWorkflow json.RawMessage
	Prompt        string
	// Mode is BuilderSessionModeCreate (build a new workflow) or
	// BuilderSessionModeRefine (edit TargetSlug's workflow).
	Mode string
	// TargetSlug is the trigger identifier of the workflow a refine session
	// edits; empty for create sessions.
	TargetSlug string
	CreatedAt  string
	UpdatedAt  string
}

// BuilderMessage is one entry of a session's collected message history (the
// builder's reasoning / tool events flattened for audit + replay).
type BuilderMessage struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	ToolCalls string
	CreatedAt string
}

// BuilderSessionRepo persists AI workflow-builder sessions and their message
// history. Every method is Scope-first; a session is reachable only within the
// org + environment that created it.
type BuilderSessionRepo interface {
	// Create inserts a new session seeded with prompt and an empty draft. The
	// caller supplies nothing but the prompt; id, status and timestamps are set
	// here. The session is create-mode with no target slug.
	Create(ctx context.Context, scope domain.Scope, prompt string) (BuilderSession, error)
	// CreateRefine inserts a refine-mode session that edits an existing workflow:
	// prompt is the requested change, targetSlug is the workflow's trigger
	// identifier, and draft seeds the session with the workflow's current shape
	// (a WorkflowDraft JSON snapshot) so the AgentLoop edits rather than builds.
	CreateRefine(ctx context.Context, scope domain.Scope, prompt, targetSlug string, draft json.RawMessage) (BuilderSession, error)
	// Get returns the session by id, filtered by scope. A session from another
	// org/env returns ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, id string) (BuilderSession, error)
	// UpdateDraft stores the proposed draft and moves the session to status.
	UpdateDraft(ctx context.Context, scope domain.Scope, id string, draft json.RawMessage, status string) error
	// SetStatus moves the session to status (e.g. confirmed/discarded).
	SetStatus(ctx context.Context, scope domain.Scope, id, status string) error
	// AppendMessage records one message-history entry for the session. The
	// session is scope-checked before the insert.
	AppendMessage(ctx context.Context, scope domain.Scope, sessionID, role, content, toolCalls string) error
	// Messages returns the session's message history in insertion order.
	Messages(ctx context.Context, scope domain.Scope, sessionID string) ([]BuilderMessage, error)
}

type builderSessionRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *builderSessionRepo) Create(ctx context.Context, scope domain.Scope, prompt string) (BuilderSession, error) {
	return repo.insert(ctx, scope, prompt, BuilderSessionModeCreate, "", json.RawMessage(`{}`))
}

func (repo *builderSessionRepo) CreateRefine(ctx context.Context, scope domain.Scope, prompt, targetSlug string, draft json.RawMessage) (BuilderSession, error) {
	if len(draft) == 0 {
		draft = json.RawMessage(`{}`)
	}
	return repo.insert(ctx, scope, prompt, BuilderSessionModeRefine, targetSlug, draft)
}

func (repo *builderSessionRepo) insert(ctx context.Context, scope domain.Scope, prompt, mode, targetSlug string, draft json.RawMessage) (BuilderSession, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s := BuilderSession{
		ID:            newULID(),
		OrgID:         scope.OrgID,
		EnvID:         scope.EnvID,
		Status:        BuilderSessionDraft,
		DraftWorkflow: draft,
		Prompt:        prompt,
		Mode:          mode,
		TargetSlug:    targetSlug,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO builder_sessions
		   (id, org_id, environment_id, status, draft_workflow, prompt, mode, target_slug, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OrgID, s.EnvID, s.Status, string(s.DraftWorkflow), s.Prompt, s.Mode, s.TargetSlug, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return BuilderSession{}, err
	}
	return s, nil
}

func (repo *builderSessionRepo) Get(ctx context.Context, scope domain.Scope, id string) (BuilderSession, error) {
	var (
		s     BuilderSession
		draft string
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, status, draft_workflow, prompt, mode, target_slug, created_at, updated_at
		 FROM builder_sessions
		 WHERE id = ? AND org_id = ? AND environment_id = ?`,
		id, scope.OrgID, scope.EnvID,
	).Scan(&s.ID, &s.OrgID, &s.EnvID, &s.Status, &draft, &s.Prompt, &s.Mode, &s.TargetSlug, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BuilderSession{}, ErrNotFound
	}
	if err != nil {
		return BuilderSession{}, err
	}
	s.DraftWorkflow = json.RawMessage(draft)
	return s, nil
}

func (repo *builderSessionRepo) UpdateDraft(ctx context.Context, scope domain.Scope, id string, draft json.RawMessage, status string) error {
	if len(draft) == 0 {
		draft = json.RawMessage(`{}`)
	}
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE builder_sessions
		    SET draft_workflow = ?, status = ?, updated_at = ?
		  WHERE id = ? AND org_id = ? AND environment_id = ?`,
		string(draft), status, time.Now().UTC().Format(time.RFC3339Nano),
		id, scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (repo *builderSessionRepo) SetStatus(ctx context.Context, scope domain.Scope, id, status string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE builder_sessions
		    SET status = ?, updated_at = ?
		  WHERE id = ? AND org_id = ? AND environment_id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano),
		id, scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (repo *builderSessionRepo) AppendMessage(ctx context.Context, scope domain.Scope, sessionID, role, content, toolCalls string) error {
	// Scope-check the parent session so a cross-tenant session id cannot have
	// messages grafted onto it.
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return err
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO builder_messages
		   (id, session_id, role, content, tool_calls, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		newULID(), sessionID, role, content, toolCalls,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (repo *builderSessionRepo) Messages(ctx context.Context, scope domain.Scope, sessionID string) ([]BuilderMessage, error) {
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return nil, err
	}
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, session_id, role, content, tool_calls, created_at
		 FROM builder_messages
		 WHERE session_id = ?
		 ORDER BY rowid ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []BuilderMessage
	for rows.Next() {
		var m BuilderMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.ToolCalls, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
