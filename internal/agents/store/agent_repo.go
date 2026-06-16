package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// AgentRepo persists agents keyed by (EnvID, Identifier). Every method is
// Scope-first; an agent is invisible outside its owning org+env.
type AgentRepo interface {
	Create(ctx context.Context, scope domain.Scope, a agentdomain.Agent) (agentdomain.Agent, error)
	GetByIdentifier(ctx context.Context, scope domain.Scope, identifier string) (agentdomain.Agent, error)
	List(ctx context.Context, scope domain.Scope) ([]agentdomain.Agent, error)
	Update(ctx context.Context, scope domain.Scope, a agentdomain.Agent) (agentdomain.Agent, error)
	Delete(ctx context.Context, scope domain.Scope, identifier string) error
}

type agentRepo struct {
	w *sql.DB
	r *sql.DB
}

const agentColumns = `id, org_id, environment_id, identifier, name, description, runtime,
 active, mesh_subject, bridge_url, COALESCE(managed, ''), welcome_message, created_at, updated_at`

// Create mints a ULID id and timestamps, inserts the row, and returns the
// persisted agent. A duplicate (environment_id, identifier) surfaces the
// SQLite UNIQUE violation to the caller.
func (repo *agentRepo) Create(ctx context.Context, scope domain.Scope, a agentdomain.Agent) (agentdomain.Agent, error) {
	a.ID = corestore.NewULID()
	a.OrgID = scope.OrgID
	a.EnvID = scope.EnvID
	a.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	a.UpdatedAt = a.CreatedAt
	_, err := repo.w.ExecContext(
		ctx,
		`INSERT INTO agents (id, org_id, environment_id, identifier, name, description, runtime,
		   active, mesh_subject, bridge_url, managed, welcome_message, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OrgID, a.EnvID, a.Identifier, a.Name, a.Description, a.Runtime,
		a.Active, a.MeshSubject, a.BridgeURL, string(a.Managed), a.WelcomeMessage, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return agentdomain.Agent{}, err
	}
	return a, nil
}

func (repo *agentRepo) GetByIdentifier(ctx context.Context, scope domain.Scope, identifier string) (agentdomain.Agent, error) {
	row := repo.r.QueryRowContext(
		ctx,
		`SELECT `+agentColumns+`
		 FROM agents WHERE org_id = ? AND environment_id = ? AND identifier = ?`,
		scope.OrgID, scope.EnvID, identifier,
	)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return agentdomain.Agent{}, ErrNotFound
	}
	return a, err
}

func (repo *agentRepo) List(ctx context.Context, scope domain.Scope) ([]agentdomain.Agent, error) {
	rows, err := repo.r.QueryContext(
		ctx,
		`SELECT `+agentColumns+`
		 FROM agents WHERE org_id = ? AND environment_id = ?
		 ORDER BY identifier`,
		scope.OrgID, scope.EnvID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []agentdomain.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Update rewrites the mutable columns of the agent addressed by
// (scope, a.Identifier), refreshes updated_at, and returns the stored row.
// A missing identifier surfaces ErrNotFound.
func (repo *agentRepo) Update(ctx context.Context, scope domain.Scope, a agentdomain.Agent) (agentdomain.Agent, error) {
	res, err := repo.w.ExecContext(
		ctx,
		`UPDATE agents SET name = ?, description = ?, runtime = ?, active = ?,
		   mesh_subject = ?, bridge_url = ?, managed = ?, welcome_message = ?, updated_at = ?
		 WHERE org_id = ? AND environment_id = ? AND identifier = ?`,
		a.Name, a.Description, a.Runtime, a.Active,
		a.MeshSubject, a.BridgeURL, string(a.Managed), a.WelcomeMessage,
		time.Now().UTC().Format(time.RFC3339Nano),
		scope.OrgID, scope.EnvID, a.Identifier,
	)
	if err != nil {
		return agentdomain.Agent{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return agentdomain.Agent{}, err
	}
	if n == 0 {
		return agentdomain.Agent{}, ErrNotFound
	}
	return repo.GetByIdentifier(ctx, scope, a.Identifier)
}

// Delete removes the agent addressed by (scope, identifier) along with every
// row that hangs off it — agent_links, conversations, conversation_activities,
// chat_threads, and email_threads — as one transactional cascade. SQLite does
// not enforce foreign keys here, so the children would otherwise be orphaned and
// a same-identifier recreate could collide with stale data. A missing identifier
// surfaces ErrNotFound. Every statement is scope-guarded by (org_id,
// environment_id) so the cascade never reaches across tenants. agent_links and
// conversations key on the agent's internal ULID id; chat_threads and
// email_threads key on the identifier string.
func (repo *agentRepo) Delete(ctx context.Context, scope domain.Scope, identifier string) error {
	tx, err := repo.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve the agent's internal id within the transaction; a missing row is
	// ErrNotFound, matching the prior single-statement semantics.
	var agentID string
	err = tx.QueryRowContext(
		ctx,
		`SELECT id FROM agents WHERE org_id = ? AND environment_id = ? AND identifier = ?`,
		scope.OrgID, scope.EnvID, identifier,
	).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	// Children keyed on the internal id. conversation_activities go first
	// (they reference conversations.id), then conversations, then agent_links.
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM conversation_activities
		 WHERE conversation_id IN (
		   SELECT id FROM conversations
		   WHERE org_id = ? AND environment_id = ? AND agent_id = ?)`,
		scope.OrgID, scope.EnvID, agentID,
	); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM conversations WHERE org_id = ? AND environment_id = ? AND agent_id = ?`,
		scope.OrgID, scope.EnvID, agentID,
	); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM agent_links WHERE org_id = ? AND environment_id = ? AND agent_id = ?`,
		scope.OrgID, scope.EnvID, agentID,
	); err != nil {
		return err
	}

	// Children keyed on the identifier string.
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM chat_threads WHERE org_id = ? AND environment_id = ? AND agent_identifier = ?`,
		scope.OrgID, scope.EnvID, identifier,
	); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM email_threads WHERE org_id = ? AND environment_id = ? AND agent_identifier = ?`,
		scope.OrgID, scope.EnvID, identifier,
	); err != nil {
		return err
	}

	// Finally the agent row itself.
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM agents WHERE org_id = ? AND environment_id = ? AND identifier = ?`,
		scope.OrgID, scope.EnvID, identifier,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// rowScanner is the shared surface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAgent reads one agents row in agentColumns order. The managed column is
// scanned through a string so a SQL NULL (COALESCEd to the empty string)
// round-trips as an empty json.RawMessage.
func scanAgent(row rowScanner) (agentdomain.Agent, error) {
	var (
		a       agentdomain.Agent
		managed string
	)
	err := row.Scan(
		&a.ID, &a.OrgID, &a.EnvID, &a.Identifier, &a.Name, &a.Description, &a.Runtime,
		&a.Active, &a.MeshSubject, &a.BridgeURL, &managed, &a.WelcomeMessage, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return agentdomain.Agent{}, err
	}
	if managed != "" {
		a.Managed = []byte(managed)
	}
	return a, nil
}
