package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// TopicRepo persists named subscriber sets keyed by (EnvID, Key). Every method
// is Scope-first; both topics and topic_subscribers carry environment_id so a
// topic and its membership are invisible outside their owning org+env.
type TopicRepo interface {
	Create(ctx context.Context, scope domain.Scope, key, name string) (domain.Topic, error)
	GetByKey(ctx context.Context, scope domain.Scope, key string) (domain.Topic, error)
	AddSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error
	ListSubscriberIDs(ctx context.Context, scope domain.Scope, topicKey string) ([]string, error)
	// List returns all topics in scope ordered by key ASC, each annotated with
	// its current member count (count of topic_subscribers rows).
	List(ctx context.Context, scope domain.Scope) ([]TopicWithCount, error)
	// Delete removes a topic and all its memberships permanently.
	// Returns ErrNotFound when no matching topic exists.
	Delete(ctx context.Context, scope domain.Scope, key string) error
	// RemoveSubscriber removes a single subscriber's membership from a topic.
	// Returns ErrNotFound when the topic does not exist.
	RemoveSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error
}

// TopicWithCount is a domain.Topic annotated with its current member count.
type TopicWithCount struct {
	domain.Topic
	MemberCount int
}

type topicRepo struct {
	w *sql.DB
	r *sql.DB
}

// Create upserts a topic by its (environment_id, key) dedup key. A fresh row
// mints a ULID id; a re-create of an existing key updates name in place and
// keeps the original id.
func (repo *topicRepo) Create(ctx context.Context, scope domain.Scope, key, name string) (domain.Topic, error) {
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO topics (id, org_id, environment_id, key, name)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (environment_id, key) DO UPDATE SET name=excluded.name`,
		newULID(), scope.OrgID, scope.EnvID, key, name,
	)
	if err != nil {
		return domain.Topic{}, err
	}
	return repo.GetByKey(ctx, scope, key)
}

func (repo *topicRepo) GetByKey(ctx context.Context, scope domain.Scope, key string) (domain.Topic, error) {
	var t domain.Topic
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT id, org_id, environment_id, key, name
		 FROM topics WHERE org_id = ? AND environment_id = ? AND key = ?`,
		scope.OrgID, scope.EnvID, key,
	).Scan(&t.ID, &t.OrgID, &t.EnvID, &t.Key, &t.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Topic{}, ErrNotFound
	}
	if err != nil {
		return domain.Topic{}, err
	}
	return t, nil
}

// AddSubscriber resolves the topic by key (ErrNotFound if missing) then records
// the membership scoped to the same environment. Duplicate inserts are ignored.
func (repo *topicRepo) AddSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error {
	t, err := repo.GetByKey(ctx, scope, topicKey)
	if err != nil {
		return err
	}
	_, err = repo.w.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO topic_subscribers (topic_id, subscriber_id, environment_id)
		 VALUES (?, ?, ?)`,
		t.ID, subscriberID, scope.EnvID,
	)
	return err
}

func (repo *topicRepo) ListSubscriberIDs(ctx context.Context, scope domain.Scope, topicKey string) ([]string, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT ts.subscriber_id
		 FROM topic_subscribers ts
		 JOIN topics t ON t.id = ts.topic_id
		 WHERE t.org_id = ? AND t.environment_id = ? AND t.key = ?
		   AND ts.environment_id = ?
		 ORDER BY ts.subscriber_id`,
		scope.OrgID, scope.EnvID, topicKey, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// List returns all topics in scope ordered by key ASC, each annotated with
// its current member count from topic_subscribers.
func (repo *topicRepo) List(ctx context.Context, scope domain.Scope) ([]TopicWithCount, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT t.id, t.org_id, t.environment_id, t.key, t.name,
		        COUNT(ts.subscriber_id) AS member_count
		 FROM topics t
		 LEFT JOIN topic_subscribers ts ON ts.topic_id = t.id AND ts.environment_id = t.environment_id
		 WHERE t.org_id = ? AND t.environment_id = ?
		 GROUP BY t.id
		 ORDER BY t.key ASC`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []TopicWithCount
	for rows.Next() {
		var tc TopicWithCount
		if err := rows.Scan(&tc.ID, &tc.OrgID, &tc.EnvID, &tc.Key, &tc.Name, &tc.MemberCount); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// Delete removes a topic and all its memberships permanently. Returns
// ErrNotFound when no matching topic exists in scope.
func (repo *topicRepo) Delete(ctx context.Context, scope domain.Scope, key string) error {
	t, err := repo.GetByKey(ctx, scope, key)
	if err != nil {
		return err // propagates ErrNotFound
	}
	// Remove memberships first (SQLite doesn't enforce FK constraints by default).
	if _, err := repo.w.ExecContext(
		ctx,
		`DELETE FROM topic_subscribers WHERE topic_id = ? AND environment_id = ?`,
		t.ID, scope.EnvID,
	); err != nil {
		return err
	}
	_, err = repo.w.ExecContext(
		ctx,
		`DELETE FROM topics WHERE org_id = ? AND environment_id = ? AND key = ?`,
		scope.OrgID, scope.EnvID, key,
	)
	return err
}

// RemoveSubscriber removes a single subscriber's membership from a topic.
// Returns ErrNotFound when the topic does not exist in scope.
func (repo *topicRepo) RemoveSubscriber(ctx context.Context, scope domain.Scope, topicKey, subscriberID string) error {
	t, err := repo.GetByKey(ctx, scope, topicKey)
	if err != nil {
		return err // propagates ErrNotFound
	}
	_, err = repo.w.ExecContext(
		ctx,
		`DELETE FROM topic_subscribers
		 WHERE topic_id = ? AND subscriber_id = ? AND environment_id = ?`,
		t.ID, subscriberID, scope.EnvID,
	)
	return err
}
