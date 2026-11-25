package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// EmailAttachmentRepo keeps the references from a notification to the files
// its email steps send (#177). The bytes live in the blob store. Scope-first.
type EmailAttachmentRepo interface {
	// Add records the attachments of one notification. The rows' own id,
	// scope, notification, subscriber and time are set here; a repeat of the
	// same (notification, position) adds nothing, so a retried trigger is safe.
	Add(ctx context.Context, scope domain.Scope, notificationID, subscriberID string, atts []domain.EmailAttachment) error
	// ListByNotification returns the notification's attachments in position order.
	ListByNotification(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.EmailAttachment, error)
	// ListByNotifications returns the attachments of each listed notification,
	// keyed by notification id; notifications with none are absent.
	ListByNotifications(ctx context.Context, scope domain.Scope, notificationIDs []string) (map[string][]domain.EmailAttachment, error)
	// SHA256sBySubscriber returns the distinct blob addresses the subscriber's
	// attachments point at.
	SHA256sBySubscriber(ctx context.Context, scope domain.Scope, subscriberID string) ([]string, error)
	// BlobReferenced reports whether anything in any scope still points at the
	// blob sha256: an attachment row or an inbound mail's raw bytes. The blob
	// store is shared by every scope, so a blob is deleted only when this is false.
	BlobReferenced(ctx context.Context, sha256 string) (bool, error)
}

type emailAttachmentRepo struct {
	w *sql.DB
	r *sql.DB
}

// EmailAttachments vends the email attachment repository.
func (s *Store) EmailAttachments() EmailAttachmentRepo {
	return &emailAttachmentRepo{w: s.writer, r: s.reader}
}

func (repo *emailAttachmentRepo) Add(ctx context.Context, scope domain.Scope, notificationID, subscriberID string, atts []domain.EmailAttachment) error {
	return inTx(ctx, repo.w, func(tx *sql.Tx) error {
		now := timestamp.Now()
		for i, a := range atts {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO email_attachments
				   (id, org_id, environment_id, notification_id, subscriber_id, position, filename, content_type, sha256, size, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				newULID(), scope.OrgID, scope.EnvID, notificationID, subscriberID, i,
				a.Filename, a.ContentType, a.SHA256, a.Size, now); err != nil {
				return fmt.Errorf("store: add email attachment: %w", err)
			}
		}
		return nil
	})
}

const emailAttachmentColumns = `id, notification_id, subscriber_id, position, filename, content_type, sha256, size, created_at`

func scanEmailAttachments(rows *sql.Rows) ([]domain.EmailAttachment, error) {
	defer func() { _ = rows.Close() }()
	var out []domain.EmailAttachment
	for rows.Next() {
		var a domain.EmailAttachment
		if err := rows.Scan(&a.ID, &a.NotificationID, &a.SubscriberID, &a.Position,
			&a.Filename, &a.ContentType, &a.SHA256, &a.Size, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (repo *emailAttachmentRepo) ListByNotification(ctx context.Context, scope domain.Scope, notificationID string) ([]domain.EmailAttachment, error) {
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+emailAttachmentColumns+` FROM email_attachments
		 WHERE org_id = ? AND environment_id = ? AND notification_id = ? ORDER BY position`,
		scope.OrgID, scope.EnvID, notificationID)
	if err != nil {
		return nil, fmt.Errorf("store: list email attachments: %w", err)
	}
	return scanEmailAttachments(rows)
}

func (repo *emailAttachmentRepo) ListByNotifications(ctx context.Context, scope domain.Scope, ids []string) (map[string][]domain.EmailAttachment, error) {
	out := map[string][]domain.EmailAttachment{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{scope.OrgID, scope.EnvID}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := repo.r.QueryContext(ctx,
		`SELECT `+emailAttachmentColumns+` FROM email_attachments
		 WHERE org_id = ? AND environment_id = ? AND notification_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
		 ORDER BY notification_id, position`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list email attachments: %w", err)
	}
	atts, err := scanEmailAttachments(rows)
	if err != nil {
		return nil, fmt.Errorf("store: list email attachments: %w", err)
	}
	for _, a := range atts {
		out[a.NotificationID] = append(out[a.NotificationID], a)
	}
	return out, nil
}

func (repo *emailAttachmentRepo) SHA256sBySubscriber(ctx context.Context, scope domain.Scope, subscriberID string) ([]string, error) {
	rows, err := repo.r.QueryContext(ctx,
		`SELECT DISTINCT sha256 FROM email_attachments WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?`,
		scope.OrgID, scope.EnvID, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("store: list attachment blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (repo *emailAttachmentRepo) BlobReferenced(ctx context.Context, sha256 string) (bool, error) {
	var found int
	err := repo.w.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM email_attachments WHERE sha256 = ?)
		     OR EXISTS (SELECT 1 FROM inbound_messages WHERE sha256 = ?)`,
		sha256, sha256).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: blob references: %w", err)
	}
	return found == 1, nil
}

// deleteEmailAttachments drops the subscriber's attachment rows, in the
// transaction that purges the subscriber. The blobs are the caller's to remove
// afterwards (audience.Service.PurgeDeleted), since a transaction cannot span
// the blob store.
func deleteEmailAttachments(ctx context.Context, ex execer, scope domain.Scope, subscriberID string) error {
	_, err := ex.ExecContext(ctx,
		`DELETE FROM email_attachments WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?`,
		scope.OrgID, scope.EnvID, subscriberID)
	return err
}
