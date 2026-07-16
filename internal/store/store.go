package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Store holds the writer/reader pools and vends Scope-typed repositories.
// SQLite is single-writer: the writer pool is capped at one connection so
// writes serialise instead of racing into SQLITE_BUSY; the reader pool fans
// out under WAL for concurrent reads.
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open opens both pools against path with WAL + busy-timeout + foreign-keys
// pragmas, runs all embedded migrations to head on the writer, and returns the
// ready Store. Idempotent w.r.t. already-applied migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	// modernc.org/sqlite reads pragmas via _pragma=NAME(VALUE) params; the
	// mattn-style _journal_mode/_busy_timeout/_foreign_keys form is silently
	// ignored by this driver. Same semantics as the plan's locked DSN: WAL
	// journal, 5s busy-timeout, foreign keys on.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		path,
	)

	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)

	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	reader.SetMaxOpenConns(8)

	if err := Migrate(ctx, writer); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, err
	}

	return &Store{writer: writer, reader: reader}, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	werr := s.writer.Close()
	rerr := s.reader.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

func (s *Store) Organizations() OrganizationRepo { return &orgRepo{w: s.writer, r: s.reader} }
func (s *Store) Environments() EnvironmentRepo   { return &envRepo{w: s.writer, r: s.reader} }
func (s *Store) ApiKeys() ApiKeyRepo             { return &apiKeyRepo{w: s.writer, r: s.reader} }
func (s *Store) Subscribers() SubscriberRepo     { return &subscriberRepo{w: s.writer, r: s.reader} }

func (s *Store) SubscriberChannels() SubscriberChannelRepo {
	return &subscriberChannelRepo{w: s.writer, r: s.reader}
}

func (s *Store) Workflows() WorkflowRepo { return &workflowRepo{w: s.writer, r: s.reader} }

// WorkflowVersions vends the per-publish compiled-DAG snapshot repository:
// authoring.Service.Publish appends a row here every time it registers a DAG
// with the engine, and .Rollback reads an earlier row back to re-register it
// byte-for-byte.
func (s *Store) WorkflowVersions() WorkflowVersionRepo {
	return &workflowVersionRepo{w: s.writer, r: s.reader}
}

func (s *Store) Notifications() NotificationRepo { return &notificationRepo{w: s.writer, r: s.reader} }
func (s *Store) Messages() MessageRepo           { return &messageRepo{w: s.writer, r: s.reader} }
func (s *Store) Integrations() IntegrationRepo   { return &integrationRepo{w: s.writer, r: s.reader} }

// StreamSources vends the inbound webhook-push source repository: per-source
// push secrets, topic scoping, and ingest-limit columns. Every method is
// Scope-first except GetBySecretHash, whose argument (the secret hash) IS the
// credential and therefore resolves the owning scope rather than being checked
// against one — mirroring ApiKeys().GetByHash.
func (s *Store) StreamSources() StreamSourceRepo {
	return &streamSourceRepo{w: s.writer, r: s.reader}
}

func (s *Store) Preferences() PreferenceRepo { return &preferenceRepo{w: s.writer, r: s.reader} }

func (s *Store) Analytics() AnalyticsRepo { return &analyticsRepo{w: s.writer, r: s.reader} }

func (s *Store) WebhookEndpoints() WebhookEndpointRepo {
	return &webhookEndpointRepo{w: s.writer, r: s.reader}
}

func (s *Store) Topics() TopicRepo { return &topicRepo{w: s.writer, r: s.reader} }

func (s *Store) Translations() TranslationRepo {
	return &translationRepo{w: s.writer, r: s.reader}
}

func (s *Store) Layouts() LayoutRepo {
	return &layoutRepo{w: s.writer, r: s.reader}
}

func (s *Store) Contexts() ContextRepo { return &contextRepo{w: s.writer, r: s.reader} }

func (s *Store) EnvVars() EnvVarRepo { return &envVarRepo{w: s.writer, r: s.reader} }
func (s *Store) InboundDomains() InboundDomainRepo {
	return &inboundDomainRepo{w: s.writer, r: s.reader}
}

func (s *Store) TopicTriggers() TopicTriggerRepo {
	return &topicTriggerRepo{w: s.writer, r: s.reader}
}

// EmailThreads vends the email two-way loop's correlation repository, mapping
// email-origin conversation threads to their recipient address and inbound
// domain.
func (s *Store) EmailThreads() EmailThreadRepo {
	return &emailThreadRepo{w: s.writer, r: s.reader}
}

// ChatThreads vends the chat two-way loop's correlation repository, mapping
// chat-origin conversation threads to the provider and chat_id (Telegram
// chat_id / Discord channel_id) the user messaged from.
func (s *Store) ChatThreads() ChatThreadRepo {
	return &chatThreadRepo{w: s.writer, r: s.reader}
}

// StepOutputs vends the per-step action-output repository: an http_request step
// writes its response here keyed by (run_id, step_id), and downstream channel
// steps read every output for a run via ListByRun.
func (s *Store) StepOutputs() StepOutputRepo {
	return &stepOutputRepo{w: s.writer, r: s.reader}
}

// BuilderSessions vends the AI workflow-builder session repository: the Guided
// create flow persists each generate run (prompt + proposed draft) here, scoped
// by org + environment, and the confirm step reads the draft back before
// compiling it through authoring.
func (s *Store) BuilderSessions() BuilderSessionRepo {
	return &builderSessionRepo{w: s.writer, r: s.reader}
}

// Pools exposes the writer/reader pairs so sibling bounded-context stores
// (internal/agents/store) run their Scope-enforced queries over the same
// SQLite database and migration set instead of opening parallel pools.
func (s *Store) Pools() (writer, reader *sql.DB) {
	return s.writer, s.reader
}
