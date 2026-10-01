// Package preferences resolves subscriber channel preferences over the store
// preferences layer. It merges the two subscriber levels — global and
// per-workflow — into an effective enabled-map. A channel is enabled unless a
// level explicitly sets it false; per-workflow overrides global per channel.
//
// TODO(parity): resolve four levels — workflow-critical override >
// subscriber per-workflow > subscriber global > workflow default. This service
// implements the two subscriber levels plus the workflow-critical override
// (ChannelEnabled's critical parameter); workflow-default is a documented
// follow-up.
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

// typeWorkflowDigest is the preference row holding a subscriber's per-workflow
// digest window override. It reuses the preferences table unchanged: the unique
// key is (env, subscriber, TYPE, workflow), so a digest row coexists with the
// channel-toggle row for the same workflow with no migration.
const typeWorkflowDigest = "workflowDigest"

// SetDigestWindow records a subscriber's digest cadence override for one
// workflow. It is stored in the same `channels` blob column as a channel map --
// the column holds whatever that row type means -- so no schema change is
// needed.
func (svc *Service) SetDigestWindow(ctx context.Context, scope domain.Scope, subscriberID, workflowID string, w domain.DigestWindow) error {
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = svc.store.Preferences().Upsert(ctx, scope, domain.Preference{
		Type:         typeWorkflowDigest,
		SubscriberID: subscriberID,
		WorkflowID:   workflowID,
		Channels:     raw,
	})
	return err
}

// ClearDigestWindow removes a subscriber's override so the workflow step's own
// window applies again. Storing a zero window rather than deleting the row
// keeps Upsert as the single write path; resolution treats a zero window as
// "no override", identical to an absent row.
func (svc *Service) ClearDigestWindow(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) error {
	return svc.SetDigestWindow(ctx, scope, subscriberID, workflowID, domain.DigestWindow{})
}

// DigestWindowFor returns the subscriber's override for one workflow. A zero
// window (ok=false) means "no override" and the caller must fall back to the
// step's authored window.
func (svc *Service) DigestWindowFor(ctx context.Context, scope domain.Scope, subscriberID, workflowID string) (domain.DigestWindow, bool, error) {
	rows, err := svc.GetForSubscriber(ctx, scope, subscriberID)
	if err != nil {
		return domain.DigestWindow{}, false, err
	}
	for _, row := range rows {
		if row.Type != typeWorkflowDigest || row.WorkflowID != workflowID {
			continue
		}
		var w domain.DigestWindow
		if len(row.Channels) > 0 {
			if err := json.Unmarshal(row.Channels, &w); err != nil {
				return domain.DigestWindow{}, false, err
			}
		}
		return w, !w.IsZero(), nil
	}
	return domain.DigestWindow{}, false, nil
}

// DigestWindowRows filters GetForSubscriber output to the digest overrides,
// keyed by workflow id. The DTO builder uses it to report every override in one
// pass rather than a lookup per workflow.
func (svc *Service) DigestWindowRows(rows []domain.Preference) (map[string]domain.DigestWindow, error) {
	out := make(map[string]domain.DigestWindow)
	for _, row := range rows {
		if row.Type != typeWorkflowDigest {
			continue
		}
		var w domain.DigestWindow
		if len(row.Channels) > 0 {
			if err := json.Unmarshal(row.Channels, &w); err != nil {
				return nil, err
			}
		}
		if !w.IsZero() {
			out[row.WorkflowID] = w
		}
	}
	return out, nil
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
// enabled unless explicitly set false (absent => enabled). critical is the
// triggering workflow's domain.Workflow.Critical flag: a critical (no-opt-out)
// workflow always reports enabled, bypassing subscriber preferences entirely —
// this is the single enforcement point for the critical override so delivery
// call sites never duplicate the short-circuit.
func (svc *Service) ChannelEnabled(ctx context.Context, scope domain.Scope, subscriberID, workflowID, channel string, critical bool) (bool, error) {
	if critical {
		return true, nil
	}
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
