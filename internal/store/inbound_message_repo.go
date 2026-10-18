package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// InboundMessage is an inbound email notify accepted. Its original bytes are
// in the blob store under SHA256.
type InboundMessage struct {
	Seq        int64
	ID         string
	SHA256     string
	Size       int64
	Recipient  string
	Sender     string
	Subject    string
	MessageID  string
	ReceivedAt string
}

// InboundMessageRepo records accepted inbound mail.
type InboundMessageRepo interface {
	// Record stores m and returns it with its id, sequence and time. The same
	// message for the same recipient returns the row already recorded, so a
	// sender's retry adds nothing.
	Record(ctx context.Context, scope domain.Scope, m InboundMessage) (InboundMessage, error)
	// List returns messages after afterSeq, oldest first, at most limit (1-200).
	List(ctx context.Context, scope domain.Scope, afterSeq int64, limit int) ([]InboundMessage, error)
	// Get returns one message, or ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, id string) (InboundMessage, error)
}

type inboundMessageRepo struct {
	w *sql.DB
	r *sql.DB
}

// InboundMessages vends the accepted-inbound-mail repository.
func (s *Store) InboundMessages() InboundMessageRepo {
	return &inboundMessageRepo{w: s.writer, r: s.reader}
}

const inboundMessageColumns = `seq, id, sha256, size, recipient, sender, subject, message_id, received_at`

func (repo *inboundMessageRepo) Record(ctx context.Context, scope domain.Scope, m InboundMessage) (InboundMessage, error) {
	if m.SHA256 == "" || m.Recipient == "" {
		return InboundMessage{}, errors.New("store: inbound message needs a sha256 and a recipient")
	}
	if _, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO inbound_messages (id, org_id, environment_id, sha256, size, recipient, sender, subject, message_id, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, sha256, recipient) DO NOTHING`,
		NewULID(), scope.OrgID, scope.EnvID, m.SHA256, m.Size, m.Recipient, m.Sender, m.Subject, m.MessageID, timestamp.Now(),
	); err != nil {
		return InboundMessage{}, fmt.Errorf("store: record inbound message: %w", err)
	}
	row := repo.w.QueryRowContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages
		 WHERE org_id = ? AND environment_id = ? AND sha256 = ? AND recipient = ?`,
		scope.OrgID, scope.EnvID, m.SHA256, m.Recipient)
	return scanInboundMessage(row)
}

func (repo *inboundMessageRepo) List(ctx context.Context, scope domain.Scope, afterSeq int64, limit int) ([]InboundMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages
		 WHERE org_id = ? AND environment_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
		scope.OrgID, scope.EnvID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list inbound messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []InboundMessage{}
	for rows.Next() {
		m, err := scanInboundMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (repo *inboundMessageRepo) Get(ctx context.Context, scope domain.Scope, id string) (InboundMessage, error) {
	row := repo.r.QueryRowContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id)
	m, err := scanInboundMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return InboundMessage{}, ErrNotFound
	}
	return m, err
}

func scanInboundMessage(row interface{ Scan(...any) error }) (InboundMessage, error) {
	var m InboundMessage
	err := row.Scan(&m.Seq, &m.ID, &m.SHA256, &m.Size, &m.Recipient, &m.Sender, &m.Subject, &m.MessageID, &m.ReceivedAt)
	return m, err
}
