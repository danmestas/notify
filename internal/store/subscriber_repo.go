package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// SubscriberRepo persists subscribers keyed by (EnvID, SubscriberID). Every
// method is Scope-first; the unique dedup key is (environment_id, subscriber_id).
type SubscriberRepo interface {
	Upsert(ctx context.Context, scope domain.Scope, sub domain.Subscriber) (domain.Subscriber, error)
	GetByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Subscriber, error)
	// GetByID resolves by the internal ULID id (not the caller-owned subscriber_id).
	GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Subscriber, error)
	// List returns a cursor-paginated page of non-deleted subscribers ordered by
	// id ASC. limit ≤ 0 → default 50, hard cap 100. Pass after="" for the first
	// page; subsequent pages pass the returned NextCursor.
	List(ctx context.Context, scope domain.Scope, after string, limit int) (SubscriberPage, error)
	// SoftDelete marks the subscriber deleted without removing the row. Returns
	// ErrNotFound when no matching active row exists.
	SoftDelete(ctx context.Context, scope domain.Scope, id string) error
	// PurgeDeletedByExternalID hard-deletes ONLY a soft-deleted row matching
	// (org_id, environment_id, subscriber_id, deleted=1). The deleted=1 guard
	// ensures a concurrently-inserted live row is never removed. Returns the
	// number of rows deleted (0 or 1); zero means no soft-deleted row existed.
	PurgeDeletedByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (int64, error)
}

// SubscriberPage is the result of a keyset-paginated List call.
//
// Cursor contract:
//   - NextCursor is an opaque base64url-encoded JSON token anchoring the last
//     row's id on the returned page.
//   - Pass NextCursor as the next call's after argument to advance forward.
//   - An empty NextCursor means there are no further pages.
//   - Concurrent inserts do not invalidate an in-flight cursor; the keyset id
//     anchor is stable under concurrent writes.
type SubscriberPage struct {
	Items      []domain.Subscriber
	NextCursor string // empty → last page
	TotalCount int    // total non-deleted rows in scope (cursor not applied)
}

// subscriberCursor is the decoded form of the opaque after token.
type subscriberCursor struct {
	ID string `json:"id"`
}

func encodeSubscriberCursor(id string) string {
	raw, _ := json.Marshal(subscriberCursor{ID: id})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeSubscriberCursor(token string) (id string, err error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", ErrInvalidCursor
	}
	var c subscriberCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", ErrInvalidCursor
	}
	return c.ID, nil
}

const (
	subscriberListDefaultLimit = 50
	subscriberListMaxLimit     = 100
)

type subscriberRepo struct {
	w *sql.DB
	r *sql.DB
}

func (repo *subscriberRepo) Upsert(ctx context.Context, scope domain.Scope, sub domain.Subscriber) (domain.Subscriber, error) {
	sub.OrgID = scope.OrgID
	sub.EnvID = scope.EnvID
	if sub.ID == "" {
		sub.ID = newULID()
	}
	// On conflict of the (environment_id, subscriber_id) dedup key, MERGE the
	// mutable profile in place: each column takes the incoming value only when
	// it is non-empty, otherwise it keeps the stored value. This is a
	// subscriber patch-upsert: a partial re-upsert (e.g. a trigger carrying
	// only `to.subscriberId`) must not blank out a profile populated by an
	// earlier full upsert. id stays the original row's id.
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO subscribers
		   (id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, subscriber_id) DO UPDATE SET
		   email=COALESCE(NULLIF(excluded.email, ''), subscribers.email),
		   phone=COALESCE(NULLIF(excluded.phone, ''), subscribers.phone),
		   first_name=COALESCE(NULLIF(excluded.first_name, ''), subscribers.first_name),
		   last_name=COALESCE(NULLIF(excluded.last_name, ''), subscribers.last_name),
		   locale=COALESCE(NULLIF(excluded.locale, ''), subscribers.locale),
		   timezone=COALESCE(NULLIF(excluded.timezone, ''), subscribers.timezone),
		   data=COALESCE(NULLIF(excluded.data, X''), subscribers.data),
		   deleted=excluded.deleted`,
		sub.ID, sub.OrgID, sub.EnvID, sub.SubscriberID, sub.Email, sub.Phone,
		sub.FirstName, sub.LastName, sub.Locale, sub.Timezone, []byte(sub.Data), sub.Deleted,
	)
	if err != nil {
		return domain.Subscriber{}, err
	}
	return repo.GetByExternalID(ctx, scope, sub.SubscriberID)
}

func (repo *subscriberRepo) GetByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (domain.Subscriber, error) {
	var (
		sub  domain.Subscriber
		data []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
		 FROM subscribers WHERE org_id = ? AND environment_id = ? AND subscriber_id = ?`,
		scope.OrgID, scope.EnvID, subscriberID,
	).Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Email, &sub.Phone,
		&sub.FirstName, &sub.LastName, &sub.Locale, &sub.Timezone, &data, &sub.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Subscriber{}, ErrNotFound
	}
	if err != nil {
		return domain.Subscriber{}, err
	}
	sub.Data = data
	return sub, nil
}

// GetByID resolves by the internal ULID id (not the caller-owned subscriber_id).
func (repo *subscriberRepo) GetByID(ctx context.Context, scope domain.Scope, id string) (domain.Subscriber, error) {
	var (
		sub  domain.Subscriber
		data []byte
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
		 FROM subscribers WHERE org_id = ? AND environment_id = ? AND id = ?`,
		scope.OrgID, scope.EnvID, id,
	).Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Email, &sub.Phone,
		&sub.FirstName, &sub.LastName, &sub.Locale, &sub.Timezone, &data, &sub.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Subscriber{}, ErrNotFound
	}
	if err != nil {
		return domain.Subscriber{}, err
	}
	sub.Data = data
	return sub, nil
}

// List returns a cursor-paginated page of non-deleted subscribers ordered by
// id ASC. limit ≤ 0 → default 50, hard cap 100.
func (repo *subscriberRepo) List(ctx context.Context, scope domain.Scope, after string, limit int) (SubscriberPage, error) {
	if limit <= 0 {
		limit = subscriberListDefaultLimit
	}
	if limit > subscriberListMaxLimit {
		limit = subscriberListMaxLimit
	}

	curID, err := decodeSubscriberCursor(after)
	if err != nil {
		return SubscriberPage{}, err
	}

	// Count total non-deleted rows in scope (no cursor applied).
	var total int
	if err := repo.r.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM subscribers WHERE org_id = ? AND environment_id = ? AND deleted = 0`,
		scope.OrgID, scope.EnvID,
	).Scan(&total); err != nil {
		return SubscriberPage{}, err
	}

	// Keyset: advance past the anchor id if one was supplied.
	var (
		rows *sql.Rows
		qErr error
	)
	if curID == "" {
		rows, qErr = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
			 FROM subscribers
			 WHERE org_id = ? AND environment_id = ? AND deleted = 0
			 ORDER BY id ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, limit+1,
		)
	} else {
		rows, qErr = repo.r.QueryContext(
			ctx,
			`SELECT id, org_id, environment_id, subscriber_id, email, phone, first_name, last_name, locale, timezone, data, deleted
			 FROM subscribers
			 WHERE org_id = ? AND environment_id = ? AND deleted = 0 AND id > ?
			 ORDER BY id ASC
			 LIMIT ?`,
			scope.OrgID, scope.EnvID, curID, limit+1,
		)
	}
	if qErr != nil {
		return SubscriberPage{}, qErr
	}
	defer func() { _ = rows.Close() }()

	var subs []domain.Subscriber
	for rows.Next() {
		var (
			sub  domain.Subscriber
			data []byte
		)
		if err := rows.Scan(&sub.ID, &sub.OrgID, &sub.EnvID, &sub.SubscriberID, &sub.Email, &sub.Phone,
			&sub.FirstName, &sub.LastName, &sub.Locale, &sub.Timezone, &data, &sub.Deleted); err != nil {
			return SubscriberPage{}, err
		}
		sub.Data = data
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return SubscriberPage{}, err
	}

	var nextCursor string
	if len(subs) > limit {
		subs = subs[:limit]
		nextCursor = encodeSubscriberCursor(subs[len(subs)-1].ID)
	}

	return SubscriberPage{
		Items:      subs,
		NextCursor: nextCursor,
		TotalCount: total,
	}, nil
}

// SoftDelete sets deleted=1 for the subscriber identified by internal id within
// scope. Returns ErrNotFound when no matching active row exists.
func (repo *subscriberRepo) SoftDelete(ctx context.Context, scope domain.Scope, id string) error {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE subscribers SET deleted = 1
		 WHERE org_id = ? AND environment_id = ? AND id = ? AND deleted = 0`,
		scope.OrgID, scope.EnvID, id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeDeletedByExternalID hard-deletes ONLY the soft-deleted row for the
// given external subscriberID within scope. The AND deleted=1 predicate is
// required: a concurrently-inserted live row with the same external id must
// never be removed by this call. Returns the count of rows deleted (0 or 1).
func (repo *subscriberRepo) PurgeDeletedByExternalID(ctx context.Context, scope domain.Scope, subscriberID string) (int64, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM subscribers
		 WHERE org_id = ? AND environment_id = ? AND subscriber_id = ? AND deleted = 1`,
		scope.OrgID, scope.EnvID, subscriberID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}
