package store

import (
	"context"
	"database/sql"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// ActivityRepo persists the append-only conversation turn log. Every method is
// Scope-first.
type ActivityRepo interface {
	Append(ctx context.Context, scope domain.Scope, a agentdomain.Activity) (agentdomain.Activity, error)
	// History returns the last n activities of the conversation oldest-first
	// (the Turn.History window). n <= 0 returns the full transcript.
	History(ctx context.Context, scope domain.Scope, conversationID string, n int) ([]agentdomain.Activity, error)
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
