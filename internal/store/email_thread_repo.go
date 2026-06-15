package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/danmestas/notify/internal/domain"
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
	CreatedAt        string `db:"created_at"`
	LastActivityAt   string `db:"last_activity_at"`
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := newULID()
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO email_threads (id, org_id, environment_id, agent_identifier,
		   platform_thread_id, subscriber_id, subscriber_email, inbound_domain,
		   created_at, last_activity_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (org_id, environment_id, platform_thread_id)
		 DO UPDATE SET agent_identifier=excluded.agent_identifier,
		   subscriber_id=excluded.subscriber_id,
		   subscriber_email=excluded.subscriber_email,
		   inbound_domain=excluded.inbound_domain,
		   last_activity_at=excluded.last_activity_at
		 WHERE org_id = excluded.org_id AND environment_id = excluded.environment_id`,
		id, scope.OrgID, scope.EnvID, t.AgentIdentifier, t.PlatformThreadID,
		t.SubscriberID, t.SubscriberEmail, t.InboundDomain, now, now,
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
		   subscriber_id, subscriber_email, inbound_domain, created_at, last_activity_at
		 FROM email_threads
		 WHERE org_id = ? AND environment_id = ? AND platform_thread_id = ?`,
		scope.OrgID, scope.EnvID, platformThreadID,
	).Scan(&out.ID, &out.OrgID, &out.EnvID, &out.AgentIdentifier, &out.PlatformThreadID,
		&out.SubscriberID, &out.SubscriberEmail, &out.InboundDomain, &out.CreatedAt, &out.LastActivityAt)
	if err == sql.ErrNoRows {
		return EmailThread{}, ErrNotFound
	}
	if err != nil {
		return EmailThread{}, err
	}
	return out, nil
}
