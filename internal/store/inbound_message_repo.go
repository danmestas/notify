package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// InboundMessage is an inbound email or text notify accepted. A mail's
// original bytes are in the blob store under SHA256. A text (Channel "sms")
// has no blob: Body holds it, and SHA256/Size describe that body.
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
	// Channel is "email" or "sms".
	Channel string
	// Body is a text's body. Empty for mail, whose bytes are in the blob store.
	Body string
	// IntegrationID is the SMS integration a text arrived through.
	IntegrationID string
	// SubscriberID is the subscriber whose phone sent a text, when one matched.
	SubscriberID string
	// PhoneVerified says the matched subscriber had verified that phone.
	PhoneVerified bool
}

// InboundMessageRepo records accepted inbound mail and texts.
type InboundMessageRepo interface {
	// Record stores m and returns it with its id, sequence and time. The same
	// message for the same recipient returns the row already recorded, so a
	// sender's retry adds nothing.
	Record(ctx context.Context, scope domain.Scope, m InboundMessage) (InboundMessage, error)
	// RecordSMS stores an inbound text (its channel is "sms"). It is kept once
	// per (IntegrationID, MessageID): a provider's retry returns the row
	// already recorded with created=false.
	RecordSMS(ctx context.Context, scope domain.Scope, m InboundMessage) (rec InboundMessage, created bool, err error)
	// List returns messages after afterSeq, oldest first, at most limit (1-200).
	List(ctx context.Context, scope domain.Scope, afterSeq int64, limit int) ([]InboundMessage, error)
	// ListSMSByIntegration returns the texts that arrived through
	// integrationID, newest first, at most limit (1-200).
	ListSMSByIntegration(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]InboundMessage, error)
	// Get returns one message, or ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, id string) (InboundMessage, error)
}

type inboundMessageRepo struct {
	w *sql.DB
	r *sql.DB
}

// InboundMessages vends the accepted-inbound-message repository.
func (s *Store) InboundMessages() InboundMessageRepo {
	return &inboundMessageRepo{w: s.writer, r: s.reader}
}

const inboundMessageColumns = `seq, id, sha256, size, recipient, sender, subject, message_id, received_at,
	channel, body, integration_id, subscriber_id, phone_verified`

func (repo *inboundMessageRepo) Record(ctx context.Context, scope domain.Scope, m InboundMessage) (InboundMessage, error) {
	if m.SHA256 == "" || m.Recipient == "" {
		return InboundMessage{}, errors.New("store: inbound message needs a sha256 and a recipient")
	}
	if _, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO inbound_messages (id, org_id, environment_id, sha256, size, recipient, sender, subject, message_id, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, sha256, recipient) WHERE channel = 'email' DO NOTHING`,
		NewULID(), scope.OrgID, scope.EnvID, m.SHA256, m.Size, m.Recipient, m.Sender, m.Subject, m.MessageID, timestamp.Now(),
	); err != nil {
		return InboundMessage{}, fmt.Errorf("store: record inbound message: %w", err)
	}
	row := repo.w.QueryRowContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages
		 WHERE org_id = ? AND environment_id = ? AND sha256 = ? AND recipient = ? AND channel = 'email'`,
		scope.OrgID, scope.EnvID, m.SHA256, m.Recipient)
	return scanInboundMessage(row)
}

func (repo *inboundMessageRepo) RecordSMS(ctx context.Context, scope domain.Scope, m InboundMessage) (InboundMessage, bool, error) {
	if m.IntegrationID == "" || m.MessageID == "" {
		return InboundMessage{}, false, errors.New("store: inbound text needs an integration and a provider message id")
	}
	sum := sha256.Sum256([]byte(m.Body))
	res, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO inbound_messages (id, org_id, environment_id, sha256, size, recipient, sender, subject, message_id, received_at,
		   channel, body, integration_id, subscriber_id, phone_verified)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, 'sms', ?, ?, ?, ?)
		 ON CONFLICT(org_id, environment_id, integration_id, message_id) WHERE channel = 'sms' DO NOTHING`,
		NewULID(), scope.OrgID, scope.EnvID, hex.EncodeToString(sum[:]), len(m.Body), m.Recipient, m.Sender, m.MessageID, timestamp.Now(),
		m.Body, m.IntegrationID, m.SubscriberID, m.PhoneVerified,
	)
	if err != nil {
		return InboundMessage{}, false, fmt.Errorf("store: record inbound text: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return InboundMessage{}, false, fmt.Errorf("store: record inbound text: %w", err)
	}
	row := repo.w.QueryRowContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages
		 WHERE org_id = ? AND environment_id = ? AND integration_id = ? AND message_id = ? AND channel = 'sms'`,
		scope.OrgID, scope.EnvID, m.IntegrationID, m.MessageID)
	rec, err := scanInboundMessage(row)
	return rec, n == 1, err
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
	return collectInbound(rows)
}

func (repo *inboundMessageRepo) ListSMSByIntegration(ctx context.Context, scope domain.Scope, integrationID string, limit int) ([]InboundMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+inboundMessageColumns+` FROM inbound_messages
		 WHERE org_id = ? AND environment_id = ? AND channel = 'sms' AND integration_id = ?
		 ORDER BY seq DESC LIMIT ?`,
		scope.OrgID, scope.EnvID, integrationID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list inbound texts: %w", err)
	}
	return collectInbound(rows)
}

func collectInbound(rows *sql.Rows) ([]InboundMessage, error) {
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
	err := row.Scan(&m.Seq, &m.ID, &m.SHA256, &m.Size, &m.Recipient, &m.Sender, &m.Subject, &m.MessageID, &m.ReceivedAt,
		&m.Channel, &m.Body, &m.IntegrationID, &m.SubscriberID, &m.PhoneVerified)
	return m, err
}
