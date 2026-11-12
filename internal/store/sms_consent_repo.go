package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// Why an attestation stopped counting, recorded on its audit row.
const (
	// RevokedByAPI: DELETE /v1/subscribers/{subscriberId}/phone/consent.
	RevokedByAPI = "revoked"
	// RevokedSuperseded: a newer attestation for the subscriber replaced it.
	RevokedSuperseded = "superseded"
	// RevokedPhoneChanged: the subscriber's phone changed to another number.
	RevokedPhoneChanged = "phone_changed"
	// RevokedSubscriberDeleted: the subscriber was deleted.
	RevokedSubscriberDeleted = "subscriber_deleted"
)

// SMSConsentSettings is an environment's operator-attestation setting.
type SMSConsentSettings struct {
	// Enabled lets API keys allowed to attest do so, and lets sms steps send
	// to attested phones.
	Enabled bool
	// PolicyURL is the privacy policy the operator's consent rests on.
	PolicyURL string
}

// SMSAttestation is one audit row: an operator's statement that a subscriber
// consented to texts at Phone.
type SMSAttestation struct {
	ID           string
	SubscriberID string
	Phone        string
	// APIKeyID identifies the key that attested: its SHA-256 hash, the
	// api_keys primary key. It is kept after the key is rotated away.
	APIKeyID string
	// Source is the operator's free-text note on where consent came from.
	Source string
	// ConsentedAt is when the operator says consent was given; "" when it did
	// not say.
	ConsentedAt string
	// PolicyURL is the environment's privacy policy URL when attested.
	PolicyURL string
	// CreatedAt is when notify recorded the attestation.
	CreatedAt string
	// RevokedAt is when the attestation stopped counting; "" while it counts.
	RevokedAt     string
	RevokedReason string
}

// SMSConsentRepo stores operator-attested SMS consent: the environment
// setting and the attestations. Scope-first.
type SMSConsentRepo interface {
	// Settings reads the environment's setting. ErrNotFound when the scope
	// names no environment.
	Settings(ctx context.Context, scope domain.Scope) (SMSConsentSettings, error)
	// SetSettings writes it. ErrNotFound when the scope names no environment.
	SetSettings(ctx context.Context, scope domain.Scope, s SMSConsentSettings) error
	// Attest records a for the live subscriber a.SubscriberID: their phone
	// becomes a.Phone (clearing a self-verification of another number) and is
	// marked attested, and any earlier open attestation is superseded. It
	// fills in ID and CreatedAt. ErrNotFound when no live subscriber matches.
	Attest(ctx context.Context, scope domain.Scope, a SMSAttestation) (SMSAttestation, error)
	// Revoke clears the live subscriber's attestation and closes its audit
	// rows, returning how many it closed. A self-verification is untouched.
	// ErrNotFound when no live subscriber matches.
	Revoke(ctx context.Context, scope domain.Scope, subscriberID string) (int, error)
	// List returns the subscriber's attestations, revoked ones included,
	// oldest first.
	List(ctx context.Context, scope domain.Scope, subscriberID string) ([]SMSAttestation, error)
}

type smsConsentRepo struct {
	w *sql.DB
	r *sql.DB
}

// SMSConsents vends the operator-attested SMS consent repository.
func (s *Store) SMSConsents() SMSConsentRepo {
	return &smsConsentRepo{w: s.writer, r: s.reader}
}

func (repo *smsConsentRepo) Settings(ctx context.Context, scope domain.Scope) (SMSConsentSettings, error) {
	var s SMSConsentSettings
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT sms_consent_attestation, sms_consent_policy_url FROM environments WHERE org_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID,
	).Scan(&s.Enabled, &s.PolicyURL)
	if errors.Is(err, sql.ErrNoRows) {
		return SMSConsentSettings{}, ErrNotFound
	}
	return s, err
}

func (repo *smsConsentRepo) SetSettings(ctx context.Context, scope domain.Scope, s SMSConsentSettings) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE environments SET sms_consent_attestation = ?, sms_consent_policy_url = ? WHERE org_id = ? AND id = ?`,
		s.Enabled, s.PolicyURL, scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (repo *smsConsentRepo) Attest(ctx context.Context, scope domain.Scope, a SMSAttestation) (SMSAttestation, error) {
	a.ID, a.CreatedAt, a.RevokedAt, a.RevokedReason = newULID(), timestamp.Now(), "", ""
	err := inTx(ctx, repo.w, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(
			ctx,
			`UPDATE subscribers SET
			   phone_verified_at = CASE WHEN phone = ? THEN phone_verified_at ELSE '' END,
			   phone = ?, phone_attested_at = ?
			 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND deleted = 0`,
			a.Phone, a.Phone, a.CreatedAt, scope.OrgID, scope.EnvID, a.SubscriberID,
		)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		if _, err := closeAttestations(ctx, tx, scope, a.SubscriberID, a.CreatedAt, RevokedSuperseded); err != nil {
			return err
		}
		_, err = tx.ExecContext(
			ctx,
			`INSERT INTO sms_consent_attestations
			   (id, org_id, environment_id, subscriber_id, phone, api_key_id, source, consented_at, policy_url, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, scope.OrgID, scope.EnvID, a.SubscriberID, a.Phone, a.APIKeyID, a.Source, a.ConsentedAt, a.PolicyURL, a.CreatedAt,
		)
		return err
	})
	if err != nil {
		return SMSAttestation{}, err
	}
	return a, nil
}

func (repo *smsConsentRepo) Revoke(ctx context.Context, scope domain.Scope, subscriberID string) (int, error) {
	var closed int
	err := inTx(ctx, repo.w, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(
			ctx,
			`UPDATE subscribers SET phone_attested_at = ''
			 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND deleted = 0`,
			scope.OrgID, scope.EnvID, subscriberID,
		)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		closed, err = closeAttestations(ctx, tx, scope, subscriberID, timestamp.Now(), RevokedByAPI)
		return err
	})
	return closed, err
}

func (repo *smsConsentRepo) List(ctx context.Context, scope domain.Scope, subscriberID string) ([]SMSAttestation, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT id, subscriber_id, phone, api_key_id, source, consented_at, policy_url, created_at, revoked_at, revoked_reason
		 FROM sms_consent_attestations
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?
		 ORDER BY created_at ASC, id ASC`,
		scope.OrgID, scope.EnvID, subscriberID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SMSAttestation
	for rows.Next() {
		var a SMSAttestation
		if err := rows.Scan(&a.ID, &a.SubscriberID, &a.Phone, &a.APIKeyID, &a.Source, &a.ConsentedAt,
			&a.PolicyURL, &a.CreatedAt, &a.RevokedAt, &a.RevokedReason); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// closeAttestations marks every open attestation of the subscriber revoked
// at at for reason, returning how many it closed.
func closeAttestations(ctx context.Context, ex execer, scope domain.Scope, subscriberID, at, reason string) (int, error) {
	res, err := ex.ExecContext(
		ctx,
		`UPDATE sms_consent_attestations SET revoked_at = ?, revoked_reason = ?
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND revoked_at = ''`,
		at, reason, scope.OrgID, scope.EnvID, subscriberID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// closeStaleAttestations closes the subscriber's open attestations once the
// subscriber no longer carries one: after a write that changed their phone or
// deleted them. While the attestation still stands it closes nothing.
func closeStaleAttestations(ctx context.Context, ex execer, scope domain.Scope, subscriberID, reason string) error {
	_, err := ex.ExecContext(
		ctx,
		`UPDATE sms_consent_attestations SET revoked_at = ?, revoked_reason = ?
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND revoked_at = ''
		   AND NOT EXISTS (SELECT 1 FROM subscribers s
		     WHERE s.org_id = ? AND s.environment_id = ? AND s.subscriber_id = ?
		       AND s.deleted = 0 AND s.phone_attested_at <> '')`,
		timestamp.Now(), reason, scope.OrgID, scope.EnvID, subscriberID,
		scope.OrgID, scope.EnvID, subscriberID,
	)
	return err
}

// inTx runs fn in a transaction on db, committing when fn succeeds.
func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
