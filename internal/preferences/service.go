// Package preferences resolves subscriber channel preferences over the store
// preferences layer. It merges the two subscriber levels — global and
// per-workflow — into an effective enabled-map. A channel is enabled unless a
// level explicitly sets it false; per-workflow overrides global per channel.
//
// TODO(parity): the reference platform resolves four levels — workflow-critical override >
// subscriber per-workflow > subscriber global > workflow default. This service
// implements the two subscriber levels; workflow-default/critical layers are a
// documented follow-up.
package preferences

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

const (
	typeGlobal   = "subscriberGlobal"
	typeWorkflow = "workflow"
)

// Service merges stored channel preferences into effective enabled-maps.
type Service struct {
	store *store.Store
}

// New returns a Service backed by s.
func New(s *store.Store) *Service { return &Service{store: s} }

// SetGlobal upserts the subscriber's global channel prefs.
func (svc *Service) SetGlobal(ctx context.Context, scope domain.Scope, subscriberID string, channels map[string]bool) error {
	raw, err := json.Marshal(channels)
	if err != nil {
		return err
	}
	_, err = svc.store.Preferences().Upsert(ctx, scope, domain.Preference{
		Type:         typeGlobal,
		SubscriberID: subscriberID,
		Channels:     raw,
	})
	return err
}

// SetForWorkflow upserts the subscriber's per-workflow channel prefs.
func (svc *Service) SetForWorkflow(ctx context.Context, scope domain.Scope, subscriberID, workflowID string, channels map[string]bool) error {
	raw, err := json.Marshal(channels)
	if err != nil {
		return err
	}
	_, err = svc.store.Preferences().Upsert(ctx, scope, domain.Preference{
		Type:         typeWorkflow,
		SubscriberID: subscriberID,
		WorkflowID:   workflowID,
		Channels:     raw,
	})
	return err
}

// ResolveChannels merges global + per-workflow into the effective enabled-map.
// Missing entry => enabled (true). Per-workflow overrides global per channel.
func (svc *Service) ResolveChannels(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) (map[string]bool, error) {
	global, err := svc.load(svc.store.Preferences().GetGlobal(ctx, scope, subscriberID))
	if err != nil {
		return nil, err
	}
	perWorkflow, err := svc.load(svc.store.Preferences().GetForWorkflow(ctx, scope, subscriberID, workflowID))
	if err != nil {
		return nil, err
	}

	merged := make(map[string]bool, len(global)+len(perWorkflow))
	for k, v := range global {
		merged[k] = v
	}
	for k, v := range perWorkflow {
		merged[k] = v
	}
	return merged, nil
}

// ChannelEnabled is the convenience check used by delivery: a channel is
// enabled unless explicitly set false (absent => enabled).
func (svc *Service) ChannelEnabled(ctx context.Context, scope domain.Scope, subscriberID, workflowID, channel string) (bool, error) {
	m, err := svc.ResolveChannels(ctx, scope, subscriberID, workflowID)
	if err != nil {
		return false, err
	}
	v, ok := m[channel]
	return !ok || v, nil
}

// GetForSubscriber returns all preference rows (global + per-workflow) for the
// subscriber in scope. The caller can inspect Type / WorkflowID to distinguish
// the two layers. Returns an empty slice (not an error) when no preferences
// have been saved.
func (svc *Service) GetForSubscriber(ctx context.Context, scope domain.Scope, subscriberID string) ([]domain.Preference, error) {
	return svc.store.Preferences().ListAllForSubscriber(ctx, scope, subscriberID)
}

// load unmarshals a fetched preference's channels, treating ErrNotFound as an
// empty map.
func (svc *Service) load(p domain.Preference, err error) (map[string]bool, error) {
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(p.Channels) == 0 {
		return nil, nil
	}
	var m map[string]bool
	if err := json.Unmarshal(p.Channels, &m); err != nil {
		return nil, err
	}
	return m, nil
}
