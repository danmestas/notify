package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/timestamp"
)

// MaxTaskNotes caps the notes kept per task; later ones are dropped.
const MaxTaskNotes = 200

// TaskStatusRepo persists the status record of each turn, keyed by the user
// activity id. A state only moves while the task is open (submitted or
// working): once it is completed, failed, canceled or input-required it stays.
type TaskStatusRepo interface {
	// Submit creates the record in state submitted. Calling it again, or after
	// the record exists, changes nothing.
	Submit(ctx context.Context, scope domain.Scope, taskID string) error
	// Note appends a progress note and moves an open task to working.
	Note(ctx context.Context, scope domain.Scope, taskID, text string) error
	// Close moves an open task to state. A non-empty usage is stored whatever
	// the state; an empty state only stores usage.
	Close(ctx context.Context, scope domain.Scope, taskID, state string, usage json.RawMessage) error
	// Reopen moves a failed task back to submitted and appends note. It reports
	// whether it did: a missing record, or a task that is open or finished, is
	// left alone, so of two concurrent retries exactly one wins.
	Reopen(ctx context.Context, scope domain.Scope, taskID, note string) (bool, error)
	// Get returns the record, or ErrNotFound.
	Get(ctx context.Context, scope domain.Scope, taskID string) (agentdomain.TaskStatus, error)
}

type taskStatusRepo struct {
	w *sql.DB
	r *sql.DB
}

func (s *Store) TaskStatuses() TaskStatusRepo { return &taskStatusRepo{w: s.w, r: s.r} }

const taskOpen = `state IN ('submitted','working')`

func (repo *taskStatusRepo) Submit(ctx context.Context, scope domain.Scope, taskID string) error {
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO task_status (org_id, environment_id, task_id, state, updated_at)
		 VALUES (?, ?, ?, 'submitted', ?)
		 ON CONFLICT (org_id, environment_id, task_id) DO NOTHING`,
		scope.OrgID, scope.EnvID, taskID, timestamp.Now())
	return err
}

func (repo *taskStatusRepo) Note(ctx context.Context, scope domain.Scope, taskID, text string) error {
	now := timestamp.Now()
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO task_status (org_id, environment_id, task_id, state, notes, updated_at)
		 VALUES (?1, ?2, ?3, 'working', json_array(json_object('text', ?4, 'at', ?5)), ?5)
		 ON CONFLICT (org_id, environment_id, task_id) DO UPDATE SET
		   notes = CASE WHEN json_array_length(notes) < ?6
		                THEN json_insert(notes, '$[#]', json_object('text', ?4, 'at', ?5)) ELSE notes END,
		   state = CASE WHEN `+taskOpen+` THEN 'working' ELSE state END,
		   updated_at = ?5`,
		scope.OrgID, scope.EnvID, taskID, text, now, MaxTaskNotes)
	return err
}

func (repo *taskStatusRepo) Close(ctx context.Context, scope domain.Scope, taskID, state string, usage json.RawMessage) error {
	now := timestamp.Now()
	_, err := repo.w.ExecContext(ctx,
		`INSERT INTO task_status (org_id, environment_id, task_id, state, usage, updated_at)
		 VALUES (?1, ?2, ?3, CASE WHEN ?4 = '' THEN 'working' ELSE ?4 END, ?5, ?6)
		 ON CONFLICT (org_id, environment_id, task_id) DO UPDATE SET
		   state = CASE WHEN ?4 <> '' AND `+taskOpen+` THEN ?4 ELSE state END,
		   usage = CASE WHEN ?5 <> '' THEN ?5 ELSE usage END,
		   updated_at = ?6`,
		scope.OrgID, scope.EnvID, taskID, state, string(usage), now)
	return err
}

func (repo *taskStatusRepo) Reopen(ctx context.Context, scope domain.Scope, taskID, note string) (bool, error) {
	now := timestamp.Now()
	res, err := repo.w.ExecContext(ctx,
		`UPDATE task_status SET
		   state = 'submitted',
		   notes = CASE WHEN json_array_length(notes) < ?4
		                THEN json_insert(notes, '$[#]', json_object('text', ?5, 'at', ?6)) ELSE notes END,
		   updated_at = ?6
		 WHERE org_id = ?1 AND environment_id = ?2 AND task_id = ?3 AND state = 'failed'`,
		scope.OrgID, scope.EnvID, taskID, MaxTaskNotes, note, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (repo *taskStatusRepo) Get(ctx context.Context, scope domain.Scope, taskID string) (agentdomain.TaskStatus, error) {
	var (
		st    agentdomain.TaskStatus
		notes string
		usage string
	)
	err := repo.r.QueryRowContext(
		ctx,
		`SELECT task_id, state, notes, usage, updated_at FROM task_status
		 WHERE org_id = ? AND environment_id = ? AND task_id = ?`,
		scope.OrgID, scope.EnvID, taskID,
	).Scan(&st.TaskID, &st.State, &notes, &usage, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return agentdomain.TaskStatus{}, ErrNotFound
	}
	if err != nil {
		return agentdomain.TaskStatus{}, err
	}
	if err := json.Unmarshal([]byte(notes), &st.Notes); err != nil {
		return agentdomain.TaskStatus{}, err
	}
	if usage != "" {
		st.Usage = json.RawMessage(usage)
	}
	return st, nil
}
