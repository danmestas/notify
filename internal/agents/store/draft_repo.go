package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// PurgeOlderThan deletes drafts created before cutoff and reports how many.
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
	res, err := repo.w.ExecContext(ctx, `DELETE FROM agent_drafts WHERE created_at < ?`, timestamp.Format(cutoff))
	if err != nil {
		return 0, fmt.Errorf("agents: purge drafts: %w", err)
	}
	return res.RowsAffected()
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
