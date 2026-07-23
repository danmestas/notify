package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// Builder session lifecycle statuses. A session starts in Draft; a turn's
// dispatch marks it Generating (the in-flight marker Slice 3's guard rejects a
// concurrent turn on), and the durable run's terminal finish step flips it to
// Ready once the final validated draft is persisted. Only a Ready session may be
// confirmed (create) or applied (refine) — neither Draft nor Generating is a
// terminal state, so guarding on Ready alone keeps a caller from confirming an
// original or partial draft mid-run. Confirmed/Discarded are the post-action
// terminal states.
const (
	BuilderSessionDraft = "draft"
	// BuilderSessionGenerating is the in-flight marker: set at a turn's dispatch
	// and cleared when the run finishes (→ Ready) or fails (reset off Generating).
	// The copilot's /message and /retry endpoints reject a new turn while a
	// session is Generating so two runs never race on one session's draft.
	BuilderSessionGenerating = "generating"
	BuilderSessionReady      = "ready"
	BuilderSessionConfirmed  = "confirmed"
	BuilderSessionDiscarded  = "discarded"
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

// BuilderCheckpoint is one per-turn snapshot of a session's draft workflow: the
// draft_workflow as it stood BEFORE a follow-up chat turn ran. Checkpoints form
// a per-session stack ordered by Seq; reverting restores the session's draft to a
// snapshot so a turn can be undone. MessageID links the snapshot to the
// builder_messages row (the user turn) that triggered it.
type BuilderCheckpoint struct {
	ID        string
	SessionID string
	Seq       int
	MessageID string
	DraftJSON json.RawMessage
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
	// AppendMessage records one message-history entry for the session and returns
	// the new message's id. The session is scope-checked before the insert.
	AppendMessage(ctx context.Context, scope domain.Scope, sessionID, role, content, toolCalls string) (string, error)
	// Messages returns the session's message history in insertion order.
	Messages(ctx context.Context, scope domain.Scope, sessionID string) ([]BuilderMessage, error)
	// DeleteMessagesFrom removes messages of the scope-checked session from the
	// row identified by messageID onward (in insertion order). When inclusive is
	// true the anchor message itself is removed too (Discard: drop the turn's user
	// turn AND its assistant reply); when false only the messages after it are
	// removed (Try-again: keep the user instruction, drop the reply about to be
	// regenerated). A messageID not belonging to the session deletes nothing.
	DeleteMessagesFrom(ctx context.Context, scope domain.Scope, sessionID, messageID string, inclusive bool) error
	// PushCheckpoint snapshots draft as a new per-turn checkpoint on the session's
	// stack (Seq = prior max + 1), linked to messageID, and returns it. The session
	// is scope-checked before the insert.
	PushCheckpoint(ctx context.Context, scope domain.Scope, sessionID, messageID string, draft json.RawMessage) (BuilderCheckpoint, error)
	// LatestCheckpoint returns the top of the session's checkpoint stack (highest
	// Seq), or ErrNotFound when the session has no checkpoints.
	LatestCheckpoint(ctx context.Context, scope domain.Scope, sessionID string) (BuilderCheckpoint, error)
	// RevertToCheckpoint restores the session's draft_workflow to the named
	// checkpoint's snapshot (non-destructive: the checkpoint stack is left intact).
	// The checkpoint must belong to the scope-checked session.
	RevertToCheckpoint(ctx context.Context, scope domain.Scope, sessionID, checkpointID string) error
	// PopCheckpoint removes the top of the session's checkpoint stack, restores the
	// session's draft_workflow to that snapshot, and returns the popped checkpoint.
	PopCheckpoint(ctx context.Context, scope domain.Scope, sessionID string) (BuilderCheckpoint, error)
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
	now := timestamp.Now()
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
		string(draft), status, timestamp.Now(),
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
		status, timestamp.Now(),
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

func (repo *builderSessionRepo) AppendMessage(ctx context.Context, scope domain.Scope, sessionID, role, content, toolCalls string) (string, error) {
	// Scope-check the parent session so a cross-tenant session id cannot have
	// messages grafted onto it.
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return "", err
	}
	id := newULID()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO builder_messages
		   (id, session_id, role, content, tool_calls, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, sessionID, role, content, toolCalls,
		timestamp.Now(),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (repo *builderSessionRepo) PushCheckpoint(ctx context.Context, scope domain.Scope, sessionID, messageID string, draft json.RawMessage) (BuilderCheckpoint, error) {
	// Scope-check the parent session so a cross-tenant session id cannot have
	// checkpoints grafted onto it.
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return BuilderCheckpoint{}, err
	}
	if len(draft) == 0 {
		draft = json.RawMessage(`{}`)
	}
	cp := BuilderCheckpoint{
		ID:        newULID(),
		SessionID: sessionID,
		MessageID: messageID,
		DraftJSON: draft,
		CreatedAt: timestamp.Now(),
	}
	// Allocate seq and insert in ONE statement: the MAX(seq)+1 is computed inside
	// the INSERT...SELECT, so two concurrent pushes on the same session cannot both
	// read the same stale MAX between a separate SELECT and INSERT. The UNIQUE
	// (session_id, seq) index (migration 0025) is the hard backstop — a genuine race
	// makes the second insert fail loudly rather than duplicate a seq.
	if _, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO builder_checkpoints
		   (id, session_id, seq, message_id, draft_json, created_at)
		 SELECT ?, ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?
		   FROM builder_checkpoints WHERE session_id = ?`,
		cp.ID, cp.SessionID, cp.MessageID, string(cp.DraftJSON), cp.CreatedAt, cp.SessionID,
	); err != nil {
		return BuilderCheckpoint{}, err
	}
	// Read back the allocated seq for the returned value.
	if err := repo.w.QueryRowContext(
		ctx,
		`SELECT seq FROM builder_checkpoints WHERE id = ?`,
		cp.ID,
	).Scan(&cp.Seq); err != nil {
		return BuilderCheckpoint{}, err
	}
	return cp, nil
}

func (repo *builderSessionRepo) LatestCheckpoint(ctx context.Context, scope domain.Scope, sessionID string) (BuilderCheckpoint, error) {
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return BuilderCheckpoint{}, err
	}
	var (
		cp    BuilderCheckpoint
		draft string
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, session_id, seq, message_id, draft_json, created_at
		 FROM builder_checkpoints
		 WHERE session_id = ?
		 ORDER BY seq DESC
		 LIMIT 1`,
		sessionID,
	).Scan(&cp.ID, &cp.SessionID, &cp.Seq, &cp.MessageID, &draft, &cp.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BuilderCheckpoint{}, ErrNotFound
	}
	if err != nil {
		return BuilderCheckpoint{}, err
	}
	cp.DraftJSON = json.RawMessage(draft)
	return cp, nil
}

func (repo *builderSessionRepo) RevertToCheckpoint(ctx context.Context, scope domain.Scope, sessionID, checkpointID string) error {
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return err
	}
	var draft string
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT draft_json FROM builder_checkpoints WHERE id = ? AND session_id = ?`,
		checkpointID, sessionID,
	).Scan(&draft)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Restoring a snapshot returns the session to an editable state; the reverted
	// draft is a prior turn's completed workflow, so mark it Ready (confirm/apply
	// eligible) rather than Draft.
	return repo.UpdateDraft(ctx, scope, sessionID, json.RawMessage(draft), BuilderSessionReady)
}

func (repo *builderSessionRepo) PopCheckpoint(ctx context.Context, scope domain.Scope, sessionID string) (BuilderCheckpoint, error) {
	cp, err := repo.LatestCheckpoint(ctx, scope, sessionID)
	if err != nil {
		return BuilderCheckpoint{}, err
	}
	if _, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM builder_checkpoints WHERE id = ? AND session_id = ?`,
		cp.ID, sessionID,
	); err != nil {
		return BuilderCheckpoint{}, err
	}
	if err := repo.UpdateDraft(ctx, scope, sessionID, cp.DraftJSON, BuilderSessionReady); err != nil {
		return BuilderCheckpoint{}, err
	}
	return cp, nil
}

func (repo *builderSessionRepo) DeleteMessagesFrom(ctx context.Context, scope domain.Scope, sessionID, messageID string, inclusive bool) error {
	// Scope-check the parent session so a cross-tenant id cannot truncate another
	// tenant's history.
	if _, err := repo.Get(ctx, scope, sessionID); err != nil {
		return err
	}
	// Resolve the anchor's rowid (messages are ordered by rowid — see Messages).
	// A messageID from another session, or a missing one, resolves to no row and
	// deletes nothing.
	var anchor int64
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT rowid FROM builder_messages WHERE id = ? AND session_id = ?`,
		messageID, sessionID,
	).Scan(&anchor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cmp := ">"
	if inclusive {
		cmp = ">="
	}
	_, err = repo.w.ExecContext(
		ctx,
		`DELETE FROM builder_messages WHERE session_id = ? AND rowid `+cmp+` ?`,
		sessionID, anchor,
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
