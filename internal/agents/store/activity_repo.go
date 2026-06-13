package store

import (
	"context"
	"database/sql"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// activityPageDefaultLimit and activityPageMaxLimit cap Page results.
const (
	activityPageDefaultLimit = 50
	activityPageMaxLimit     = 200
)

// ActivityRepo persists the append-only conversation turn log. Every method is
// Scope-first.
type ActivityRepo interface {
	Append(ctx context.Context, scope domain.Scope, a agentdomain.Activity) (agentdomain.Activity, error)
	// History returns the last n activities of the conversation oldest-first
	// (the Turn.History window). n <= 0 returns the full transcript.
	History(ctx context.Context, scope domain.Scope, conversationID string, n int) ([]agentdomain.Activity, error)
	// Page returns a cursor-bounded slice of activities for the conversation,
	// ordered oldest-first. limit ≤ 0 → default 50, hard cap 200.
	// before={ulid} fetches up to limit rows older than that id (id < before).
	// after={ulid}  fetches up to limit rows newer than that id (id > after).
	// Both cursors are applied when non-empty; if both are supplied, before
	// takes precedence (only before is applied). Empty cursors return the most
	// recent limit rows.
	Page(ctx context.Context, scope domain.Scope, conversationID string, limit int, before, after string) ([]agentdomain.Activity, error)
}

type activityRepo struct {
	w *sql.DB
	r *sql.DB
}

// Append mints a ULID id and created_at, inserts the activity, and returns
// the persisted row.
func (repo *activityRepo) Append(ctx context.Context, scope domain.Scope, a agentdomain.Activity) (agentdomain.Activity, error) {
	a.ID = corestore.NewULID()
	a.OrgID = scope.OrgID
	a.EnvID = scope.EnvID
	a.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO conversation_activities (id, org_id, environment_id, conversation_id, role, content, run_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OrgID, a.EnvID, a.ConversationID, a.Role, string(a.Content), a.RunID, a.CreatedAt,
	)
	if err != nil {
		return agentdomain.Activity{}, err
	}
	return a, nil
}

// Page returns a cursor-bounded page of activities oldest-first. See the
// ActivityRepo interface for full cursor semantics. The sort key is
// (created_at, id) — same as History — so Page and History return rows in a
// consistent order even when multiple activities share the same millisecond.
func (repo *activityRepo) Page(ctx context.Context, scope domain.Scope, conversationID string, limit int, before, after string) ([]agentdomain.Activity, error) {
	if limit <= 0 {
		limit = activityPageDefaultLimit
	}
	if limit > activityPageMaxLimit {
		limit = activityPageMaxLimit
	}

	const cols = `id, org_id, environment_id, conversation_id, role, content, run_id, created_at`

	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case before != "":
		// Resolve the anchor row's created_at so the keyset predicate is
		// (created_at, id) consistent with the History ordering.
		var anchorAt string
		if scanErr := repo.r.QueryRowContext(
			ctx,
			`SELECT created_at FROM conversation_activities WHERE org_id = ? AND environment_id = ? AND id = ?`,
			scope.OrgID, scope.EnvID, before,
		).Scan(&anchorAt); scanErr != nil {
			// Anchor absent or wrong scope: return empty rather than error.
			return []agentdomain.Activity{}, nil
		}
		// Take the newest `limit` rows older than (anchorAt, before), then
		// flip to oldest-first for the caller.
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT `+cols+` FROM (
			   SELECT `+cols+`
			   FROM conversation_activities
			   WHERE org_id = ? AND environment_id = ? AND conversation_id = ?
			     AND (created_at < ? OR (created_at = ? AND id < ?))
			   ORDER BY created_at DESC, id DESC
			   LIMIT ?
			 ) ORDER BY created_at ASC, id ASC`,
			scope.OrgID, scope.EnvID, conversationID,
			anchorAt, anchorAt, before,
			limit,
		)
	case after != "":
		// Resolve anchor's created_at.
		var anchorAt string
		if scanErr := repo.r.QueryRowContext(
			ctx,
			`SELECT created_at FROM conversation_activities WHERE org_id = ? AND environment_id = ? AND id = ?`,
			scope.OrgID, scope.EnvID, after,
		).Scan(&anchorAt); scanErr != nil {
			return []agentdomain.Activity{}, nil
		}
		// Rows newer than (anchorAt, after), oldest-first.
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT `+cols+`
			 FROM conversation_activities
			 WHERE org_id = ? AND environment_id = ? AND conversation_id = ?
			   AND (created_at > ? OR (created_at = ? AND id > ?))
			 ORDER BY created_at ASC, id ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, conversationID,
			anchorAt, anchorAt, after,
			limit,
		)
	default:
		// No cursor: most recent limit rows, oldest-first.
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT `+cols+` FROM (
			   SELECT `+cols+`
			   FROM conversation_activities
			   WHERE org_id = ? AND environment_id = ? AND conversation_id = ?
			   ORDER BY created_at DESC, id DESC
			   LIMIT ?
			 ) ORDER BY created_at ASC, id ASC`,
			scope.OrgID, scope.EnvID, conversationID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []agentdomain.Activity
	for rows.Next() {
		var (
			a       agentdomain.Activity
			content string
		)
		if err := rows.Scan(&a.ID, &a.OrgID, &a.EnvID, &a.ConversationID, &a.Role, &content, &a.RunID, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Content = []byte(content)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return []agentdomain.Activity{}, nil
	}
	return out, nil
}

func (repo *activityRepo) History(ctx context.Context, scope domain.Scope, conversationID string, n int) ([]agentdomain.Activity, error) {
	// The inner query takes the newest n rows (conv_hist index order, id as the
	// same-timestamp tiebreaker since ULIDs sort by mint time); the outer query
	// flips them oldest-first for prompt assembly.
	limit := n
	if limit <= 0 {
		limit = -1 // SQLite: LIMIT -1 means no limit.
	}
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, org_id, environment_id, conversation_id, role, content, run_id, created_at FROM (
		   SELECT id, org_id, environment_id, conversation_id, role, content, run_id, created_at
		   FROM conversation_activities
		   WHERE org_id = ? AND environment_id = ? AND conversation_id = ?
		   ORDER BY created_at DESC, id DESC LIMIT ?
		 ) ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID, conversationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []agentdomain.Activity
	for rows.Next() {
		var (
			a       agentdomain.Activity
			content string
		)
		if err := rows.Scan(&a.ID, &a.OrgID, &a.EnvID, &a.ConversationID, &a.Role, &content, &a.RunID, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Content = []byte(content)
		out = append(out, a)
	}
	return out, rows.Err()
}
