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
func (s *Store) Workflows() WorkflowRepo         { return &workflowRepo{w: s.writer, r: s.reader} }
func (s *Store) Notifications() NotificationRepo { return &notificationRepo{w: s.writer, r: s.reader} }
func (s *Store) Messages() MessageRepo           { return &messageRepo{w: s.writer, r: s.reader} }
func (s *Store) Integrations() IntegrationRepo   { return &integrationRepo{w: s.writer, r: s.reader} }

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

// Pools exposes the writer/reader pairs so sibling bounded-context stores
// (internal/agents/store) run their Scope-enforced queries over the same
// SQLite database and migration set instead of opening parallel pools.
func (s *Store) Pools() (writer, reader *sql.DB) {
	return s.writer, s.reader
}
