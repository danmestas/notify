package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// DraftRetention is how long a draft is kept for its mailbox owner to fetch.
// Long enough to cover a laptop closed over a holiday.
const DraftRetention = 30 * 24 * time.Hour

// DraftRepo persists agent drafts awaiting review.
type DraftRepo interface {
	// Create stores d and returns it with its id, sequence and creation time.
	// A draft for an activity that already has one returns the existing draft,
	// so producing the same draft twice stores it once.
	Create(ctx context.Context, scope domain.Scope, d agentdomain.Draft) (agentdomain.Draft, error)
	// List returns a subscriber's drafts in the given mailboxes with a sequence
	// after afterSeq, oldest first, at most limit (1-200).
	List(ctx context.Context, scope domain.Scope, subscriberID string, mailboxes []string, afterSeq int64, limit int) ([]agentdomain.Draft, error)
	// Request records that the agent's reply to the message composed under
	// clientMessageID must become a draft carrying d's mail context. It is
	// saved before the message is composed, so the turn always finds it.
	Request(ctx context.Context, scope domain.Scope, clientMessageID string, d agentdomain.Draft) error
	// Produce turns the reply to the message composed under clientMessageID
	// into a draft, if a request for it exists and has no draft yet. It
	// reports false when there is nothing to do. A retried turn produces no
	// second draft.
	Produce(ctx context.Context, scope domain.Scope, clientMessageID string, reply agentdomain.Draft) (agentdomain.Draft, bool, error)
	// PurgeOlderThan deletes drafts and draft requests created before cutoff
	// and reports how many drafts it removed.
	PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

type draftRepo struct {
	w *sql.DB
	r *sql.DB
}

func (s *Store) Drafts() DraftRepo { return &draftRepo{w: s.w, r: s.r} }

const draftColumns = `seq, id, org_id, environment_id, subscriber_id, mailbox, agent_identifier, agent_name,
	conversation_id, activity_id, source_message_id, in_reply_to, refs, subject, recipients, text, created_at`

func (repo *draftRepo) Create(ctx context.Context, scope domain.Scope, d agentdomain.Draft) (agentdomain.Draft, error) {
	if d.Mailbox == "" || d.SubscriberID == "" || d.ActivityID == "" {
		return agentdomain.Draft{}, fmt.Errorf("agents: draft needs a mailbox, subscriber and activity")
	}
	refs, err := json.Marshal(nonNil(d.References))
	if err != nil {
		return agentdomain.Draft{}, err
	}
	to, err := json.Marshal(nonNil(d.To))
	if err != nil {
		return agentdomain.Draft{}, err
	}
	_, err = repo.w.ExecContext(
		ctx,
		`INSERT INTO agent_drafts (id, org_id, environment_id, subscriber_id, mailbox, agent_identifier, agent_name,
			conversation_id, activity_id, source_message_id, in_reply_to, refs, subject, recipients, text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, activity_id) DO NOTHING`,
		corestore.NewULID(), scope.OrgID, scope.EnvID, d.SubscriberID, d.Mailbox, d.AgentIdentifier, d.AgentName,
		d.ConversationID, d.ActivityID, d.SourceMessageID, d.InReplyTo, string(refs), d.Subject, string(to), d.Text,
		timestamp.Now(),
	)
	if err != nil {
		return agentdomain.Draft{}, fmt.Errorf("agents: create draft: %w", err)
	}
	rows, err := repo.w.QueryContext(ctx,
		`SELECT `+draftColumns+` FROM agent_drafts WHERE org_id = ? AND environment_id = ? AND activity_id = ?`,
		scope.OrgID, scope.EnvID, d.ActivityID)
	if err != nil {
		return agentdomain.Draft{}, err
	}
	drafts, err := scanDrafts(rows)
	if err != nil {
		return agentdomain.Draft{}, err
	}
	if len(drafts) != 1 {
		return agentdomain.Draft{}, fmt.Errorf("agents: draft for activity %s not found after insert", d.ActivityID)
	}
	return drafts[0], nil
}

func (repo *draftRepo) List(ctx context.Context, scope domain.Scope, subscriberID string, mailboxes []string, afterSeq int64, limit int) ([]agentdomain.Draft, error) {
	if len(mailboxes) == 0 {
		return []agentdomain.Draft{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	args := []any{scope.OrgID, scope.EnvID, subscriberID}
	for _, m := range mailboxes {
		args = append(args, m)
	}
	args = append(args, afterSeq, limit)
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+draftColumns+` FROM agent_drafts
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?
		   AND mailbox IN (?`+strings.Repeat(", ?", len(mailboxes)-1)+`)
		   AND seq > ?
		 ORDER BY seq LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("agents: list drafts: %w", err)
	}
	return scanDrafts(rows)
}

func (repo *draftRepo) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	if _, err := repo.w.ExecContext(ctx, `DELETE FROM agent_draft_requests WHERE created_at < ?`, timestamp.Format(cutoff)); err != nil {
		return 0, fmt.Errorf("agents: purge draft requests: %w", err)
	}
	res, err := repo.w.ExecContext(ctx, `DELETE FROM agent_drafts WHERE created_at < ?`, timestamp.Format(cutoff))
	if err != nil {
		return 0, fmt.Errorf("agents: purge drafts: %w", err)
	}
	return res.RowsAffected()
}

func (repo *draftRepo) Request(ctx context.Context, scope domain.Scope, clientMessageID string, d agentdomain.Draft) error {
	if clientMessageID == "" || d.Mailbox == "" || d.SubscriberID == "" || d.SourceMessageID == "" {
		return fmt.Errorf("agents: draft request needs a message key, mailbox, subscriber and source message")
	}
	refs, err := json.Marshal(nonNil(d.References))
	if err != nil {
		return err
	}
	to, err := json.Marshal(nonNil(d.To))
	if err != nil {
		return err
	}
	// A retried step re-records the same request; the first one stands.
	_, err = repo.w.ExecContext(ctx,
		`INSERT INTO agent_draft_requests (org_id, environment_id, client_message_id, subscriber_id, mailbox,
			source_message_id, in_reply_to, refs, subject, recipients, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, client_message_id) DO NOTHING`,
		scope.OrgID, scope.EnvID, clientMessageID, d.SubscriberID, d.Mailbox,
		d.SourceMessageID, d.InReplyTo, string(refs), d.Subject, string(to), timestamp.Now())
	if err != nil {
		return fmt.Errorf("agents: record draft request: %w", err)
	}
	return nil
}

func (repo *draftRepo) Produce(ctx context.Context, scope domain.Scope, clientMessageID string, reply agentdomain.Draft) (agentdomain.Draft, bool, error) {
	if clientMessageID == "" {
		return agentdomain.Draft{}, false, nil
	}
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return agentdomain.Draft{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var d agentdomain.Draft
	var refs, to string
	var draftID sql.NullString
	err = tx.QueryRowContext(
		ctx,
		`SELECT subscriber_id, mailbox, source_message_id, in_reply_to, refs, subject, recipients, draft_id
		 FROM agent_draft_requests WHERE org_id = ? AND environment_id = ? AND client_message_id = ?`,
		scope.OrgID, scope.EnvID, clientMessageID,
	).Scan(&d.SubscriberID, &d.Mailbox, &d.SourceMessageID, &d.InReplyTo, &refs, &d.Subject, &to, &draftID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && draftID.Valid) {
		return agentdomain.Draft{}, false, nil // no request, or already produced
	}
	if err != nil {
		return agentdomain.Draft{}, false, fmt.Errorf("agents: load draft request: %w", err)
	}
	d.ID = corestore.NewULID()
	d.AgentIdentifier, d.AgentName = reply.AgentIdentifier, reply.AgentName
	d.ConversationID, d.ActivityID, d.Text = reply.ConversationID, reply.ActivityID, reply.Text
	d.CreatedAt = timestamp.Now()
	_ = json.Unmarshal([]byte(refs), &d.References)
	_ = json.Unmarshal([]byte(to), &d.To)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO agent_drafts (id, org_id, environment_id, subscriber_id, mailbox, agent_identifier, agent_name,
			conversation_id, activity_id, source_message_id, in_reply_to, refs, subject, recipients, text, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, scope.OrgID, scope.EnvID, d.SubscriberID, d.Mailbox, d.AgentIdentifier, d.AgentName,
		d.ConversationID, d.ActivityID, d.SourceMessageID, d.InReplyTo, refs, d.Subject, to, d.Text, d.CreatedAt)
	if err != nil {
		return agentdomain.Draft{}, false, fmt.Errorf("agents: insert draft: %w", err)
	}
	if d.Seq, err = res.LastInsertId(); err != nil {
		return agentdomain.Draft{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_draft_requests SET draft_id = ? WHERE org_id = ? AND environment_id = ? AND client_message_id = ?`,
		d.ID, scope.OrgID, scope.EnvID, clientMessageID); err != nil {
		return agentdomain.Draft{}, false, fmt.Errorf("agents: mark draft request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return agentdomain.Draft{}, false, err
	}
	d.OrgID, d.EnvID = scope.OrgID, scope.EnvID
	return d, true, nil
}

func scanDrafts(rows *sql.Rows) ([]agentdomain.Draft, error) {
	defer func() { _ = rows.Close() }()
	out := []agentdomain.Draft{}
	for rows.Next() {
		var d agentdomain.Draft
		var refs, to string
		if err := rows.Scan(&d.Seq, &d.ID, &d.OrgID, &d.EnvID, &d.SubscriberID, &d.Mailbox, &d.AgentIdentifier, &d.AgentName,
			&d.ConversationID, &d.ActivityID, &d.SourceMessageID, &d.InReplyTo, &refs, &d.Subject, &to, &d.Text, &d.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(refs), &d.References)
		_ = json.Unmarshal([]byte(to), &d.To)
		out = append(out, d)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
