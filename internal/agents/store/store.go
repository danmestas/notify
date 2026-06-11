// Package store persists the agents bounded context: agents, their channel
// links, and the durable conversation transcript. It reuses the core store's
// SQLite pools and migration set (tables land via migration 0010_agents) and
// follows the same Scope-first repository idiom: every query carries
// org_id + environment_id so nothing leaks across tenants.
package store

import (
	"database/sql"

	corestore "github.com/danmestas/notify/internal/store"
)

// ErrNotFound aliases the core store sentinel so callers compare one error
// regardless of which repository answered.
var ErrNotFound = corestore.ErrNotFound

// Store vends the Scope-typed agents repositories over the core store's
// writer/reader pools.
type Store struct {
	w *sql.DB
	r *sql.DB
}

// New builds the agents Store over the core store's pools.
func New(st *corestore.Store) *Store {
	w, r := st.Pools()
	return &Store{w: w, r: r}
}

func (s *Store) Agents() AgentRepo { return &agentRepo{w: s.w, r: s.r} }

func (s *Store) Links() AgentLinkRepo { return &agentLinkRepo{w: s.w, r: s.r} }

func (s *Store) Conversations() ConversationRepo { return &conversationRepo{w: s.w, r: s.r} }

func (s *Store) Activities() ActivityRepo { return &activityRepo{w: s.w, r: s.r} }
