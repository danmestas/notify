package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// NotificationFilter narrows a NotificationRepo.List query. Zero values and
// zero-valued time.Time are ignored (no filtering on that field).
type NotificationFilter struct {
	WorkflowID    string
	SubscriberID  string
	TransactionID string
	// Since filters to notifications created at or after this time (inclusive).
	// A zero Time disables the filter.
	Since time.Time
}

// NotificationPage is the result of a keyset-paginated List call.
//
// Cursor contract:
//   - Cursor is an opaque base64url-encoded JSON token containing the
//     (created_at, id) pair of the last row on the returned page.
//   - Pass the returned Cursor as the next call's cursor argument to advance
//     the page forward (older rows, since ordering is created_at DESC, id DESC).
//   - An empty Cursor means no further pages exist.
//   - Inserting new rows does not invalidate an in-flight cursor; the keyset
//     (created_at, id) anchor is stable even under concurrent writes.
type NotificationPage struct {
	Items      []domain.Notification
	Cursor     string // empty → last page
	TotalCount int    // total rows matching the filter in this scope (no cursor applied)
}

// notificationCursor is the decoded form of the opaque page token.
type notificationCursor struct {
	CreatedAt string `json:"ca"`
	ID        string `json:"id"`
}

// encodeCursor base64url-encodes a (created_at, id) pair.
func encodeCursor(createdAt, id string) string {
	raw, _ := json.Marshal(notificationCursor{CreatedAt: createdAt, ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor unpacks an opaque cursor; returns empty strings for a blank cursor.
func decodeCursor(token string) (createdAt, id string, err error) {
	if token == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", errors.New("store: invalid cursor")
	}
	var c notificationCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", "", errors.New("store: invalid cursor")
	}
	return c.CreatedAt, c.ID, nil
}

// NotificationRepo persists notification records (one per trigger fan-out).
// Scope-first throughout.
type NotificationRepo interface {
	Create(ctx context.Context, scope domain.Scope, n domain.Notification) (domain.Notification, error)
	Get(ctx context.Context, scope domain.Scope, id string) (domain.Notification, error)
	GetByRunID(ctx context.Context, runID string) (domain.Notification, error)
	// List returns a keyset-paginated page of notifications within scope,
	// filtered by f and ordered by (created_at DESC, id DESC). Pass limit ≤ 0
	// to use the default of 50; the hard cap is 200. Pass cursor = "" for the
	// first page; subsequent pages use the Cursor returned by the prior call.
	List(ctx context.Context, scope domain.Scope, f NotificationFilter, cursor string, limit int) (NotificationPage, error)
}

type notificationRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *notificationRepo) Create(ctx context.Context, scope domain.Scope, n domain.Notification) (domain.Notification, error) {
	n.OrgID = scope.OrgID
	n.EnvID = scope.EnvID
	if n.ID == "" {
		n.ID = newULID()
	}
	if n.CreatedAt == "" {
		n.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO notifications
		   (id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.OrgID, n.EnvID, n.WorkflowID, n.SubscriberID, n.TransactionID, n.RunID,
		[]byte(n.Payload), n.CreatedAt, n.DigestKey,
	)
	if err != nil {
		return domain.Notification{}, err
	}
	return n, nil
}

func (repo *notificationRepo) Get(ctx context.Context, scope domain.Scope, id string) (domain.Notification, error) {
	var (
		n       domain.Notification
		payload []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key
		 FROM notifications WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&n.ID, &n.OrgID, &n.EnvID, &n.WorkflowID, &n.SubscriberID, &n.TransactionID, &n.RunID,
		&payload, &n.CreatedAt, &n.DigestKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, err
	}
	n.Payload = payload
	return n, nil
}

// GetByRunID resolves a notification by its globally-unique dagnats run id.
// Unscoped by design: a worker handling a task has only the opaque RunID (no
// tenancy ctx); the returned row carries OrgID/EnvID for subsequent scoped use.
func (repo *notificationRepo) GetByRunID(ctx context.Context, runID string) (domain.Notification, error) {
	var (
		n       domain.Notification
		payload []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key
		 FROM notifications WHERE run_id = ? ORDER BY created_at DESC LIMIT 1`,
		runID,
	).Scan(&n.ID, &n.OrgID, &n.EnvID, &n.WorkflowID, &n.SubscriberID, &n.TransactionID, &n.RunID,
		&payload, &n.CreatedAt, &n.DigestKey)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Notification{}, ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, err
	}
	n.Payload = payload
	return n, nil
}

const (
	notificationListDefaultLimit = 50
	notificationListMaxLimit     = 200
)

// List implements keyset-paginated listing ordered by (created_at DESC, id DESC).
// The cursor anchors at the last row of the previous page so concurrent inserts
// do not shift rows into or out of an in-flight cursor window.
func (repo *notificationRepo) List(ctx context.Context, scope domain.Scope, f NotificationFilter, cursor string, limit int) (NotificationPage, error) {
	if limit <= 0 {
		limit = notificationListDefaultLimit
	}
	if limit > notificationListMaxLimit {
		limit = notificationListMaxLimit
	}

	curCreatedAt, curID, err := decodeCursor(cursor)
	if err != nil {
		return NotificationPage{}, err
	}

	// Build the WHERE clause dynamically. Base conditions are always scope-first.
	conds := []string{"org_id = ?", "environment_id = ?"}
	args := []any{scope.OrgID, scope.EnvID}

	if f.WorkflowID != "" {
		conds = append(conds, "workflow_id = ?")
		args = append(args, f.WorkflowID)
	}
	if f.SubscriberID != "" {
		conds = append(conds, "subscriber_id = ?")
		args = append(args, f.SubscriberID)
	}
	if f.TransactionID != "" {
		conds = append(conds, "transaction_id = ?")
		args = append(args, f.TransactionID)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}

	baseWhere := strings.Join(conds, " AND ")

	// Count total matching rows (no cursor applied) so the caller can show totals.
	var totalCount int
	if err := repo.r.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM notifications WHERE "+baseWhere,
		args...,
	).Scan(&totalCount); err != nil {
		return NotificationPage{}, err
	}

	// Apply keyset: rows strictly before the cursor anchor (older in DESC order).
	pageArgs := append([]any(nil), args...)
	pageWhere := baseWhere
	if curCreatedAt != "" {
		pageWhere += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		pageArgs = append(pageArgs, curCreatedAt, curCreatedAt, curID)
	}

	// Fetch limit+1 to detect whether a next page exists.
	pageArgs = append(pageArgs, limit+1)
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, workflow_id, subscriber_id, transaction_id, run_id, payload, created_at, digest_key
		 FROM notifications
		 WHERE `+pageWhere+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		pageArgs...,
	)
	if err != nil {
		return NotificationPage{}, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]domain.Notification, 0, limit)
	for rows.Next() {
		var (
			n       domain.Notification
			payload []byte
		)
		if err := rows.Scan(&n.ID, &n.OrgID, &n.EnvID, &n.WorkflowID, &n.SubscriberID,
			&n.TransactionID, &n.RunID, &payload, &n.CreatedAt, &n.DigestKey); err != nil {
			return NotificationPage{}, err
		}
		n.Payload = payload
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return NotificationPage{}, err
	}

	var nextCursor string
	if len(items) > limit {
		// There is a next page; use the last item we will return as the anchor.
		items = items[:limit]
		last := items[len(items)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}

	return NotificationPage{
		Items:      items,
		Cursor:     nextCursor,
		TotalCount: totalCount,
	}, nil
}
