package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// PhoneVerification is one verification code sent to a phone for a
// subscriber. CodeHash is opaque here: internal/phoneverify owns how it is
// made and checked.
type PhoneVerification struct {
	ID           string
	SubscriberID string
	Phone        string
	CodeHash     string
	Attempts     int
	Consumed     bool
	ExpiresAt    string
	CreatedAt    string
}

// PhoneVerificationRepo persists verification codes. Every method is
// Scope-first.
type PhoneVerificationRepo interface {
	Create(ctx context.Context, scope domain.Scope, v PhoneVerification) error
	// Latest returns the subscriber's newest code, consumed or not, or
	// ErrNotFound when none was ever sent.
	Latest(ctx context.Context, scope domain.Scope, subscriberID string) (PhoneVerification, error)
	// CountSince counts codes created at or after since for the subscriber
	// and, across every subscriber in the environment, for the phone.
	CountSince(ctx context.Context, scope domain.Scope, subscriberID, phone, since string) (bySubscriber, byPhone int, err error)
	// ClaimAttempt spends one attempt on an unconsumed code, atomically, and
	// reports false when maxAttempts attempts were already spent or it is consumed.
	ClaimAttempt(ctx context.Context, scope domain.Scope, id string, maxAttempts int) (bool, error)
	// Consume marks the code used, reporting false when it already was.
	Consume(ctx context.Context, scope domain.Scope, id string) (bool, error)
	// PruneBefore deletes codes created before cutoff.
	PruneBefore(ctx context.Context, scope domain.Scope, cutoff string) error
}

// PhoneVerifications vends the verification-code repository.
func (s *Store) PhoneVerifications() PhoneVerificationRepo {
	return &phoneVerificationRepo{w: s.writer, r: s.reader}
}

type phoneVerificationRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *phoneVerificationRepo) Create(ctx context.Context, scope domain.Scope, v PhoneVerification) error {
	if v.ID == "" {
		v.ID = newULID()
	}
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO phone_verifications
		   (id, org_id, environment_id, subscriber_id, phone, code_hash, attempts, consumed, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		v.ID, scope.OrgID, scope.EnvID, v.SubscriberID, v.Phone, v.CodeHash, v.ExpiresAt, v.CreatedAt)
	return err
}

func (repo *phoneVerificationRepo) Latest(ctx context.Context, scope domain.Scope, subscriberID string) (PhoneVerification, error) {
	var v PhoneVerification
	err := repo.w.QueryRowContext(
		ctx,
		`SELECT id, subscriber_id, phone, code_hash, attempts, consumed, expires_at, created_at
		 FROM phone_verifications
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		scope.OrgID, scope.EnvID, subscriberID,
	).Scan(&v.ID, &v.SubscriberID, &v.Phone, &v.CodeHash, &v.Attempts, &v.Consumed, &v.ExpiresAt, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PhoneVerification{}, ErrNotFound
	}
	return v, err
}

func (repo *phoneVerificationRepo) CountSince(ctx context.Context, scope domain.Scope, subscriberID, phone, since string) (int, int, error) {
	var bySub, byPhone int
	err := repo.w.QueryRowContext(
		ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN subscriber_id = ? THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN phone = ? THEN 1 ELSE 0 END), 0)
		 FROM phone_verifications
		 WHERE org_id = ? AND environment_id = ? AND created_at >= ? AND (subscriber_id = ? OR phone = ?)`,
		subscriberID, phone, scope.OrgID, scope.EnvID, since, subscriberID, phone,
	).Scan(&bySub, &byPhone)
	return bySub, byPhone, err
}

func (repo *phoneVerificationRepo) ClaimAttempt(ctx context.Context, scope domain.Scope, id string, maxAttempts int) (bool, error) {
	return repo.affected(repo.w.ExecContext(ctx,
		`UPDATE phone_verifications SET attempts = attempts + 1
		 WHERE org_id = ? AND environment_id = ? AND id = ? AND consumed = 0 AND attempts < ?`,
		scope.OrgID, scope.EnvID, id, maxAttempts))
}

func (repo *phoneVerificationRepo) Consume(ctx context.Context, scope domain.Scope, id string) (bool, error) {
	return repo.affected(repo.w.ExecContext(ctx,
		`UPDATE phone_verifications SET consumed = 1
		 WHERE org_id = ? AND environment_id = ? AND id = ? AND consumed = 0`,
		scope.OrgID, scope.EnvID, id))
}

func (repo *phoneVerificationRepo) PruneBefore(ctx context.Context, scope domain.Scope, cutoff string) error {
	_, err := repo.w.ExecContext(ctx,
		`DELETE FROM phone_verifications WHERE org_id = ? AND environment_id = ? AND created_at < ?`,
		scope.OrgID, scope.EnvID, cutoff)
	return err
}

func (repo *phoneVerificationRepo) affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
