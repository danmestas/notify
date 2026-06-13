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
	// ListConversations returns one ConversationSummary per distinct
	// conversation_id, ordered by the most recent activity in that conversation
	// (last_message_at DESC, last_activity_id DESC for tie-breaking).
	// limit ≤ 0 → default 20, hard cap 200.
	// agentIdentifier, when non-empty, narrows the result to a single agent.
	// cursor is the LastActivityID of the last row on the previous page;
	// empty cursor returns the first page.
	// Returns the summaries and the next-page cursor ("" when no more pages).
	ListConversations(ctx context.Context, scope domain.Scope, agentIdentifier, cursor string, limit int) ([]ConversationSummary, string, error)
}

// ConversationSummary is one row of the cross-agent conversation list returned
// by ActivityRepo.ListConversations. It carries the most recent activity
// timestamp for a conversation thread together with the conversation's metadata.
type ConversationSummary struct {
	ConversationID   string // conversations.id
	AgentIdentifier  string // agents.identifier
	AgentName        string // agents.name (may be empty)
	SubscriberID     string // conversations.subscriber_id
	PlatformThreadID string // conversations.platform_thread_id
	Status           string // conversations.status ("open"/"closed")
	LastMessageAt    string // max(conversation_activities.created_at) for this conversation
	LastActivityID   string // id of that most-recent activity row (keyset cursor anchor)
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

// listConversationsDefaultLimit and listConversationsMaxLimit cap
// ListConversations pages.
const (
	listConversationsDefaultLimit = 20
	listConversationsMaxLimit     = 200
)

// ListConversations returns one ConversationSummary per distinct conversation,
// ordered by max(activity.created_at) DESC with a ULID id tiebreaker.
// It joins the conversations and agents tables to resolve display metadata.
func (repo *activityRepo) ListConversations(ctx context.Context, scope domain.Scope, agentIdentifier, cursor string, limit int) ([]ConversationSummary, string, error) {
	if limit <= 0 {
		limit = listConversationsDefaultLimit
	}
	if limit > listConversationsMaxLimit {
		limit = listConversationsMaxLimit
	}

	// Base query: one row per conversation with max activity timestamp + the
	// activity ULID of that max row (for deterministic keyset ordering).
	// The join walks: conversation_activities → conversations → agents.
	// All three tables are scoped by org_id + environment_id.
	//
	// Keyset: (last_message_at DESC, last_activity_id DESC). Since activity IDs
	// are ULIDs, lexicographic DESC on id is equivalent to time DESC with
	// sub-millisecond tiebreaking, consistent with the rest of the codebase.
	const baseCols = `
		ca.conversation_id,
		ag.identifier   AS agent_identifier,
		ag.name         AS agent_name,
		c.subscriber_id,
		c.platform_thread_id,
		c.status,
		MAX(ca.created_at) AS last_message_at,
		MAX(ca.id)          AS last_activity_id`

	const baseFrom = `
		FROM conversation_activities ca
		JOIN conversations c
		  ON c.id = ca.conversation_id
		 AND c.org_id = ca.org_id
		 AND c.environment_id = ca.environment_id
		JOIN agents ag
		  ON ag.id = c.agent_id
		 AND ag.org_id = ca.org_id
		 AND ag.environment_id = ca.environment_id`

	var (
		dbRows *sql.Rows
		err    error
	)

	if agentIdentifier == "" && cursor == "" {
		dbRows, err = repo.r.QueryContext(
			ctx,
			`SELECT`+baseCols+baseFrom+`
			 WHERE ca.org_id = ? AND ca.environment_id = ?
			 GROUP BY ca.conversation_id
			 ORDER BY last_message_at DESC, last_activity_id DESC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, limit,
		)
	} else if agentIdentifier != "" && cursor == "" {
		dbRows, err = repo.r.QueryContext(
			ctx,
			`SELECT`+baseCols+baseFrom+`
			 WHERE ca.org_id = ? AND ca.environment_id = ?
			   AND ag.identifier = ?
			 GROUP BY ca.conversation_id
			 ORDER BY last_message_at DESC, last_activity_id DESC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, agentIdentifier, limit,
		)
	} else {
		// Resolve the anchor row's (created_at, id) from the activity table.
		// The cursor is the last_activity_id of the last row on the previous page.
		var anchorAt string
		if err := repo.r.QueryRowContext(
			ctx,
			`SELECT created_at FROM conversation_activities
			 WHERE org_id = ? AND environment_id = ? AND id = ?`,
			scope.OrgID, scope.EnvID, cursor,
		).Scan(&anchorAt); err != nil {
			// Anchor is absent or wrong scope: fall through to first page.
			anchorAt = ""
			cursor = ""
		}

		if cursor == "" {
			// Anchor gone; return first page (may miss a race-deleted activity).
			if agentIdentifier == "" {
				dbRows, err = repo.r.QueryContext(
					ctx,
					`SELECT`+baseCols+baseFrom+`
					 WHERE ca.org_id = ? AND ca.environment_id = ?
					 GROUP BY ca.conversation_id
					 ORDER BY last_message_at DESC, last_activity_id DESC
					 LIMIT ?`,
					scope.OrgID, scope.EnvID, limit,
				)
			} else {
				dbRows, err = repo.r.QueryContext(
					ctx,
					`SELECT`+baseCols+baseFrom+`
					 WHERE ca.org_id = ? AND ca.environment_id = ?
					   AND ag.identifier = ?
					 GROUP BY ca.conversation_id
					 ORDER BY last_message_at DESC, last_activity_id DESC
					 LIMIT ?`,
					scope.OrgID, scope.EnvID, agentIdentifier, limit,
				)
			}
		} else if agentIdentifier == "" {
			dbRows, err = repo.r.QueryContext(
				ctx,
				`SELECT`+baseCols+baseFrom+`
				 WHERE ca.org_id = ? AND ca.environment_id = ?
				 GROUP BY ca.conversation_id
				 HAVING (last_message_at < ? OR (last_message_at = ? AND last_activity_id < ?))
				 ORDER BY last_message_at DESC, last_activity_id DESC
				 LIMIT ?`,
				scope.OrgID, scope.EnvID, anchorAt, anchorAt, cursor, limit,
			)
		} else {
			dbRows, err = repo.r.QueryContext(
				ctx,
				`SELECT`+baseCols+baseFrom+`
				 WHERE ca.org_id = ? AND ca.environment_id = ?
				   AND ag.identifier = ?
				 GROUP BY ca.conversation_id
				 HAVING (last_message_at < ? OR (last_message_at = ? AND last_activity_id < ?))
				 ORDER BY last_message_at DESC, last_activity_id DESC
				 LIMIT ?`,
				scope.OrgID, scope.EnvID, agentIdentifier, anchorAt, anchorAt, cursor, limit,
			)
		}
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = dbRows.Close() }()

	var out []ConversationSummary
	for dbRows.Next() {
		var s ConversationSummary
		if err := dbRows.Scan(
			&s.ConversationID, &s.AgentIdentifier, &s.AgentName,
			&s.SubscriberID, &s.PlatformThreadID, &s.Status,
			&s.LastMessageAt, &s.LastActivityID,
		); err != nil {
			return nil, "", err
		}
		out = append(out, s)
	}
	if err := dbRows.Err(); err != nil {
		return nil, "", err
	}
	if out == nil {
		return []ConversationSummary{}, "", nil
	}

	// Build next-page cursor: the LastActivityID of the last row.
	var nextCursor string
	if len(out) == limit {
		nextCursor = out[len(out)-1].LastActivityID
	}
	return out, nextCursor, nil
}
