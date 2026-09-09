package store

import (
	"context"
	"database/sql"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// EmailThread is one email-origin conversation binding: it maps a conversation
// thread (platform_thread_id) to the recipient email address, the responder
// agent, and the verified inbound domain whose tokenized reply-to mailbox the
// recipient replies into. The email two-way loop reads it to thread inbound
// replies and to mail agent answers back out.
type EmailThread struct {
	ID               string `db:"id"`
	OrgID            string `db:"org_id"`
	EnvID            string `db:"environment_id"`
	AgentIdentifier  string `db:"agent_identifier"`
	PlatformThreadID string `db:"platform_thread_id"`
	SubscriberID     string `db:"subscriber_id"`
	SubscriberEmail  string `db:"subscriber_email"`
	InboundDomain    string `db:"inbound_domain"`
	// Kind selects the responder for the thread: "agent" (every row that
	// predates the column) or "webhook" (#345). Constants live in emailloop.
	Kind string `db:"kind"`
	// TransactionID and WorkflowIdentifier are carried on webhook-kind rows so
	// a reply event can name what was replied to. The thread itself is keyed
	// on the per-recipient notification id -- a multicast trigger shares one
	// transaction id across every recipient, which would collide here.
	TransactionID      string `db:"transaction_id"`
	WorkflowIdentifier string `db:"workflow_identifier"`
	CreatedAt          string `db:"created_at"`
	LastActivityAt     string `db:"last_activity_at"`
}

// EmailThreadRepo persists email-origin conversation bindings, scoped to
// (org_id, environment_id). Keyed by platform_thread_id within a tenant.
type EmailThreadRepo interface {
	// Upsert inserts the binding for its (org, env, platform_thread_id) key, or
	// refreshes last_activity_at (and the mutable email/domain/agent fields) on
	// a re-bind of an existing thread. The original id is kept.
	Upsert(ctx context.Context, scope domain.Scope, t EmailThread) (EmailThread, error)
	// Get reads the binding for a thread without creating one. ErrNotFound when
	// absent.
	Get(ctx context.Context, scope domain.Scope, platformThreadID string) (EmailThread, error)
	// List returns the scope's email-thread bindings, keyset-paginated on
	// platform_thread_id (the stable, scope-unique key) ascending. The over-fetch
	// + cursor mirrors InboundDomainRepo.List. SQL ordering is by
	// platform_thread_id ASC for cursor correctness; callers that want a
	// presentation order (e.g. newest-first) re-sort the returned page.
	List(ctx context.Context, scope domain.Scope, after string, limit int) (items []EmailThread, nextCursor string, total int, err error)
}

type emailThreadRepo struct {
	w *sql.DB
	r *sql.DB
}

// Upsert inserts the binding by its (org_id, environment_id, platform_thread_id)
// key. A fresh row mints a ULID id and timestamps; a re-upsert refreshes the
// mutable fields and last_activity_at while keeping the original id. The DO
// UPDATE arm is scope-guarded so a key collision against another tenant never
// mutates their row.
func (repo *emailThreadRepo) Upsert(ctx context.Context, scope domain.Scope, t EmailThread) (EmailThread, error) {
	now := timestamp.Now()
	id := newULID()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO email_threads (id, org_id, environment_id, agent_identifier,
		   platform_thread_id, subscriber_id, subscriber_email, inbound_domain,
		   kind, transaction_id, workflow_identifier,
		   created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, environment_id, platform_thread_id)
		 DO UPDATE SET agent_identifier=excluded.agent_identifier,
		   subscriber_id=excluded.subscriber_id,
		   subscriber_email=excluded.subscriber_email,
		   inbound_domain=excluded.inbound_domain,
		   kind=excluded.kind,
		   transaction_id=excluded.transaction_id,
		   workflow_identifier=excluded.workflow_identifier,
		   last_activity_at=excluded.last_activity_at
		 WHERE org_id = excluded.org_id AND environment_id = excluded.environment_id`,
		id, scope.OrgID, scope.EnvID, t.AgentIdentifier, t.PlatformThreadID,
		t.SubscriberID, t.SubscriberEmail, t.InboundDomain,
		kindOrAgent(t.Kind), t.TransactionID, t.WorkflowIdentifier, now, now,
	)
	if err != nil {
		return EmailThread{}, err
	}
	return repo.Get(ctx, scope, t.PlatformThreadID)
}

// Get reads the binding for platformThreadID within scope.
func (repo *emailThreadRepo) Get(ctx context.Context, scope domain.Scope, platformThreadID string) (EmailThread, error) {
	var out EmailThread
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, agent_identifier, platform_thread_id,
		   subscriber_id, subscriber_email, inbound_domain, kind, transaction_id, workflow_identifier, created_at, last_activity_at
		 FROM email_threads
		 WHERE org_id = ? AND environment_id = ? AND platform_thread_id = ?`,
		scope.OrgID, scope.EnvID, platformThreadID,
	).Scan(&out.ID, &out.OrgID, &out.EnvID, &out.AgentIdentifier, &out.PlatformThreadID,
		&out.SubscriberID, &out.SubscriberEmail, &out.InboundDomain, &out.Kind, &out.TransactionID, &out.WorkflowIdentifier, &out.CreatedAt, &out.LastActivityAt)
	if err == sql.ErrNoRows {
		return EmailThread{}, ErrNotFound
	}
	if err != nil {
		return EmailThread{}, err
	}
	return out, nil
}

// List returns the scope's email-thread bindings, keyset-paginated on
// platform_thread_id ASC. The query is scope-guarded (org_id, environment_id),
// over-fetches limit+1 to derive nextCursor, and clamps the limit. No dedicated
// index is required at current scale (mirrors InboundDomainRepo.List).
func (repo *emailThreadRepo) List(ctx context.Context, scope domain.Scope, after string, limit int) ([]EmailThread, string, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	var total int
	if err := repo.r.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM email_threads WHERE org_id = ? AND environment_id = ?`,
		scope.OrgID, scope.EnvID,
	).Scan(&total); err != nil {
		return nil, "", 0, err
	}

	var rows *sql.Rows
	var err error
	if after == "" {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, agent_identifier, platform_thread_id,
			   subscriber_id, subscriber_email, inbound_domain, kind, transaction_id, workflow_identifier, created_at, last_activity_at
			 FROM email_threads
			 WHERE org_id = ? AND environment_id = ?
			 ORDER BY platform_thread_id ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, limit+1,
		)
	} else {
		rows, err = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, agent_identifier, platform_thread_id,
			   subscriber_id, subscriber_email, inbound_domain, kind, transaction_id, workflow_identifier, created_at, last_activity_at
			 FROM email_threads
			 WHERE org_id = ? AND environment_id = ? AND platform_thread_id > ?
			 ORDER BY platform_thread_id ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, after, limit+1,
		)
	}
	if err != nil {
		return nil, "", 0, err
	}
	defer func() { _ = rows.Close() }()

	var items []EmailThread
	for rows.Next() {
		var out EmailThread
		if err := rows.Scan(&out.ID, &out.OrgID, &out.EnvID, &out.AgentIdentifier, &out.PlatformThreadID,
			&out.SubscriberID, &out.SubscriberEmail, &out.InboundDomain, &out.Kind, &out.TransactionID, &out.WorkflowIdentifier, &out.CreatedAt, &out.LastActivityAt); err != nil {
			return nil, "", 0, err
		}
		items = append(items, out)
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}

	var nextCursor string
	if len(items) > limit {
		items = items[:limit]
		// Cursor is the last returned row's platform_thread_id; the next page's
		// WHERE filters on platform_thread_id > cursor, so no row is skipped.
		nextCursor = items[limit-1].PlatformThreadID
	}
	return items, nextCursor, total, nil
}

// kindOrAgent defaults an unset Kind to "agent", matching the column default,
// so callers that predate the column keep writing the rows they always did.
func kindOrAgent(k string) string {
	if k == "" {
		return "agent"
	}
	return k
}
