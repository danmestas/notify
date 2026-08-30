package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// MessageRepo persists per-channel messages and serves the inbox list keyed by
// (environment_id, subscriber_id, channel) — backed by the msg_inbox index.
// Scope-first throughout.
type MessageRepo interface {
	Create(ctx context.Context, scope domain.Scope, m domain.Message) (domain.Message, error)
	// Get returns a single message by id within scope. Returns ErrNotFound
	// when the id doesn't exist, or belongs to a different org/env.
	Get(ctx context.Context, scope domain.Scope, id string) (domain.Message, error)
	List(ctx context.Context, scope domain.Scope, subscriberID, channel string) ([]domain.Message, error)
	// ListPage returns up to limit messages for subscriberID (optionally
	// narrowed by channel), ordered newest-first by id — ULIDs embed a
	// millisecond timestamp and are lexicographically sortable, so id DESC
	// matches created_at DESC without a second sort key or an opaque cursor
	// token. When before != "", only rows with id < before are returned,
	// giving simple keyset pagination anchored on the message id itself: pass
	// the last id of a page back in as the next page's before.
	//
	// archived is a tri-state filter: "" or "false" excludes archived rows
	// (the default inbox feed), "true" returns only archived rows, and
	// "all" returns both. See the Count doc comment for the same contract.
	//
	// transactionID, when non-empty, further narrows the page to messages
	// from that transaction — the same filter the legacy array branch
	// (List + a Go-side loop) applies, so the envelope path can honor it
	// too.
	ListPage(ctx context.Context, scope domain.Scope, subscriberID, channel, before string, limit int, archived, transactionID string) ([]domain.Message, error)
	// ListByIntegrationID returns messages for the given integration, ordered
	// newest-first. Capped at limit rows (1–100; 0 defaults to 100). Rows
	// written before migration 0017 (integration_id='') are excluded.
	// Scope-first: org_id + environment_id must match the integration's scope.
	ListByIntegrationID(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]domain.Message, error)
	// ListByNotificationID returns all messages for a notification, ordered
	// newest-first. Scope-first.
	ListByNotificationID(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.Message, error)
	// Count returns the number of messages for subscriberID (optionally
	// narrowed by channel), further narrowed to unread (read=false) and/or
	// unseen (seen=false) rows when onlyUnread / onlyUnseen is set. With both
	// false it counts every message matching subscriberID/channel that also
	// passes the archived filter.
	//
	// archived is a tri-state filter, matching the reference platform's inbox semantics of
	// excluding archived messages from the default feed and its counts:
	// "" or "false" excludes archived rows, "true" counts only archived
	// rows, and "all" counts both. Passing "" here (not "all") is what
	// keeps unreadCount/unseenCount from drifting after a message is
	// archived.
	Count(ctx context.Context, scope domain.Scope, subscriberID, channel string, onlyUnread, onlyUnseen bool, archived string) (int, error)
	// MarkOne sets one boolean flag (field must be "seen", "read", or
	// "archived") on a single message, scoped by org/env AND subscriberID —
	// so a caller can never flip a flag on a message belonging to a
	// different subscriber. Returns ErrNotFound when no row matches (the id
	// doesn't exist in scope, or belongs to a different subscriber; the two
	// cases are indistinguishable on purpose, so a caller can't use this to
	// probe for another subscriber's message ids).
	MarkOne(ctx context.Context, scope domain.Scope, id, subscriberID, field string, value bool) error
	// MarkBulk sets one boolean flag on a set of messages for subscriberID:
	// either the explicit ids (subscriberID-scoped — an id belonging to a
	// different subscriber is silently skipped, not an error), or every
	// message for subscriberID when all is true. Returns the number of rows
	// actually updated.
	MarkBulk(ctx context.Context, scope domain.Scope, subscriberID string, ids []string, all bool, field string, value bool) (int64, error)
}

// markableFields whitelists the boolean flag columns MarkOne/MarkBulk may
// set. field is interpolated into the UPDATE statement (SQL has no
// column-name placeholder), so every caller path must resolve through this
// set first — never build the SQL from an unchecked field string.
var markableFields = map[string]bool{"seen": true, "read": true, "archived": true}

type messageRepo struct {
	w *sql.DB
	r *sql.DB
}

// messageColumns is the canonical ordered column list for SELECT and INSERT.
// Keeping it in one place means adding a column only requires updating this
// constant and the corresponding Scan call.
const messageColumns = `id, org_id, environment_id, workflow_id, notification_id, run_id, step_id,
	subscriber_id, channel, provider_id, integration_id, content, subject, status, seen, read,
	archived, transaction_id, error_text, skip_reason, created_at, extras`

func scanMessage(rows *sql.Rows) (domain.Message, error) {
	var m domain.Message
	var extras []byte
	err := rows.Scan(
		&m.ID, &m.OrgID, &m.EnvID, &m.WorkflowID, &m.NotificationID, &m.RunID, &m.StepID,
		&m.SubscriberID, &m.Channel, &m.ProviderID, &m.IntegrationID, &m.Content, &m.Subject,
		&m.Status, &m.Seen, &m.Read, &m.Archived, &m.TransactionID, &m.ErrorText, &m.SkipReason, &m.CreatedAt,
		&extras,
	)
	m.Extras = json.RawMessage(extras)
	return m, err
}

func (repo *messageRepo) Create(ctx context.Context, scope domain.Scope, m domain.Message) (domain.Message, error) {
	m.OrgID = scope.OrgID
	m.EnvID = scope.EnvID
	if m.ID == "" {
		m.ID = newULID()
	}
	if m.CreatedAt == "" {
		m.CreatedAt = timestamp.Now()
	}
	// Nil-safety contract: extras is a NOT NULL column (migration 0022). Callers
	// that build a domain.Message without rich fields — including the
	// persistSkipped/persistThrottled/persistError delivery paths — leave Extras
	// nil; coerce it here so the INSERT never violates the constraint and silently
	// drops the row.
	if len(m.Extras) == 0 {
		m.Extras = json.RawMessage("{}")
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO messages
		   (id, org_id, environment_id, workflow_id, notification_id, run_id, step_id, subscriber_id,
		    channel, provider_id, integration_id, content, subject, status, seen, read, archived,
		    transaction_id, error_text, skip_reason, created_at, extras)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.OrgID, m.EnvID, m.WorkflowID, m.NotificationID, m.RunID, m.StepID, m.SubscriberID,
		m.Channel, m.ProviderID, m.IntegrationID, m.Content, m.Subject, m.Status, m.Seen, m.Read,
		m.Archived, m.TransactionID, m.ErrorText, m.SkipReason, m.CreatedAt, []byte(m.Extras),
	)
	if err != nil {
		return domain.Message{}, err
	}
	return m, nil
}

// scanMessageRow scans a single-row *sql.Row into a domain.Message, mapping
// sql.ErrNoRows to ErrNotFound.
func scanMessageRow(row *sql.Row) (domain.Message, error) {
	var m domain.Message
	var extras []byte
	err := row.Scan(
		&m.ID, &m.OrgID, &m.EnvID, &m.WorkflowID, &m.NotificationID, &m.RunID, &m.StepID,
		&m.SubscriberID, &m.Channel, &m.ProviderID, &m.IntegrationID, &m.Content, &m.Subject,
		&m.Status, &m.Seen, &m.Read, &m.Archived, &m.TransactionID, &m.ErrorText, &m.SkipReason, &m.CreatedAt,
		&extras,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Message{}, ErrNotFound
	}
	if err != nil {
		return domain.Message{}, err
	}
	m.Extras = json.RawMessage(extras)
	return m, nil
}

func (repo *messageRepo) Get(ctx context.Context, scope domain.Scope, id string) (domain.Message, error) {
	row := repo.r.QueryRowContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	)
	return scanMessageRow(row)
}

func (repo *messageRepo) List(ctx context.Context, scope domain.Scope, subscriberID, channel string) ([]domain.Message, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND channel = ?
		 ORDER BY created_at DESC, id DESC`,
		scope.OrgID, scope.EnvID, subscriberID, channel,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ListByIntegrationID returns up to limit messages for the given integration,
// ordered newest-first (created_at DESC, id DESC). Rows from before migration
// 0017 (integration_id=”) are excluded by the non-empty constraint on the
// queried integration_id. Limit is clamped to [1, 100]; 0 → 100.
func (repo *messageRepo) ListByIntegrationID(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		scope.OrgID, scope.EnvID, integrationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// archivedCond appends the SQL condition (if any) for the tri-state
// archived filter shared by ListPage and Count: "" or "false" excludes
// archived rows, "true" returns only archived rows, "all" adds no
// condition. Any other value is treated as "" (excluded).
func archivedCond(conds []string, archived string) []string {
	switch archived {
	case "true":
		return append(conds, "archived = 1")
	case "all":
		return conds
	default:
		return append(conds, "archived = 0")
	}
}

// ListPage returns up to limit messages for subscriberID (optionally
// narrowed by channel), ordered newest-first by id. See the MessageRepo
// doc comment for why id alone is a safe, sufficient sort/cursor key, and
// for the archived tri-state contract.
func (repo *messageRepo) ListPage(ctx context.Context, scope domain.Scope, subscriberID, channel, before string, limit int, archived, transactionID string) ([]domain.Message, error) {
	conds := []string{"org_id = ?", "environment_id = ?", "subscriber_id = ?"}
	args := []any{scope.OrgID, scope.EnvID, subscriberID}
	if channel != "" {
		conds = append(conds, "channel = ?")
		args = append(args, channel)
	}
	if before != "" {
		conds = append(conds, "id < ?")
		args = append(args, before)
	}
	if transactionID != "" {
		conds = append(conds, "transaction_id = ?")
		args = append(args, transactionID)
	}
	conds = archivedCond(conds, archived)
	args = append(args, limit)

	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE `+strings.Join(conds, " AND ")+`
		 ORDER BY id DESC
		 LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// Count returns the number of messages matching subscriberID/channel/flag
// filters. See the MessageRepo doc comment for the onlyUnread/onlyUnseen
// and archived contracts.
func (repo *messageRepo) Count(ctx context.Context, scope domain.Scope, subscriberID, channel string, onlyUnread, onlyUnseen bool, archived string) (int, error) {
	conds := []string{"org_id = ?", "environment_id = ?", "subscriber_id = ?"}
	args := []any{scope.OrgID, scope.EnvID, subscriberID}
	if channel != "" {
		conds = append(conds, "channel = ?")
		args = append(args, channel)
	}
	if onlyUnread {
		conds = append(conds, "read = 0")
	}
	if onlyUnseen {
		conds = append(conds, "seen = 0")
	}
	conds = archivedCond(conds, archived)
	var n int
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM messages WHERE `+strings.Join(conds, " AND "),
		args...,
	).Scan(&n)
	return n, err
}

// MarkOne sets one boolean flag on a single message. See the MessageRepo doc
// comment for the scoping and ErrNotFound contract.
func (repo *messageRepo) MarkOne(ctx context.Context, scope domain.Scope, id, subscriberID, field string, value bool) error {
	if !markableFields[field] {
		return fmt.Errorf("store: unknown message flag %q", field)
	}
	return affectedOne(repo.w.ExecContext(
		ctx,
		`UPDATE messages SET `+field+` = ?
		 WHERE org_id = ? AND environment_id = ? AND id = ? AND subscriber_id = ?`,
		value, scope.OrgID, scope.EnvID, id, subscriberID,
	))
}

// MarkBulk sets one boolean flag on a set of messages for subscriberID. See
// the MessageRepo doc comment for the ids-vs-all contract.
func (repo *messageRepo) MarkBulk(ctx context.Context, scope domain.Scope, subscriberID string, ids []string, all bool, field string, value bool) (int64, error) {
	if !markableFields[field] {
		return 0, fmt.Errorf("store: unknown message flag %q", field)
	}
	conds := []string{"org_id = ?", "environment_id = ?", "subscriber_id = ?"}
	args := []any{scope.OrgID, scope.EnvID, subscriberID}
	if !all {
		if len(ids) == 0 {
			return 0, nil
		}
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		conds = append(conds, "id IN ("+strings.Join(placeholders, ",")+")")
	}
	execArgs := append([]any{value}, args...)
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE messages SET `+field+` = ? WHERE `+strings.Join(conds, " AND "),
		execArgs...,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListByNotificationID returns all messages for a notification, ordered
// newest-first. Used by GET /v1/notifications/{id}/messages (#43).
func (repo *messageRepo) ListByNotificationID(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.Message, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE org_id = ? AND environment_id = ? AND notification_id = ?
		 ORDER BY created_at DESC, id DESC`,
		scope.OrgID, scope.EnvID, notificationID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
