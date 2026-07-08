package preferences_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/store"
)

func newService(t *testing.T) (*preferences.Service, domain.Scope) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return preferences.New(s), domain.Scope{OrgID: "o1", EnvID: "e1"}
}

func TestResolveChannels_DefaultEnabled(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", false)
	if err != nil {
		t.Fatalf("ChannelEnabled: %v", err)
	}
	if !ok {
		t.Fatalf("expected in_app enabled by default, got disabled")
	}
}

func TestResolveChannels_GlobalDisable(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", false)
	if err != nil {
		t.Fatalf("ChannelEnabled in_app: %v", err)
	}
	if ok {
		t.Fatalf("expected in_app disabled by global, got enabled")
	}

	ok, err = svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "email", false)
	if err != nil {
		t.Fatalf("ChannelEnabled email: %v", err)
	}
	if !ok {
		t.Fatalf("expected email enabled (absent => enabled), got disabled")
	}
}

func TestResolveChannels_WorkflowOverridesGlobal(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"email": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}
	if err := svc.SetForWorkflow(ctx, scope, "sub1", "wf1", map[string]bool{"email": true}); err != nil {
		t.Fatalf("SetForWorkflow: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "email", false)
	if err != nil {
		t.Fatalf("ChannelEnabled wf1: %v", err)
	}
	if !ok {
		t.Fatalf("expected email enabled for wf1 (per-workflow override), got disabled")
	}

	// A different workflow has no override, so global (false) applies.
	ok, err = svc.ChannelEnabled(ctx, scope, "sub1", "wf2", "email", false)
	if err != nil {
		t.Fatalf("ChannelEnabled wf2: %v", err)
	}
	if ok {
		t.Fatalf("expected email disabled for wf2 (global applies), got enabled")
	}
}

// TestChannelEnabled_CriticalBypassesGlobalMute is the critical-workflow
// no-opt-out contract: a subscriber who has muted a channel globally still
// gets it when the triggering workflow is critical (e.g. password-reset).
func TestChannelEnabled_CriticalBypassesGlobalMute(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", true)
	if err != nil {
		t.Fatalf("ChannelEnabled: %v", err)
	}
	if !ok {
		t.Fatalf("expected critical workflow to bypass the global mute, got disabled")
	}
}

// TestChannelEnabled_CriticalBypassesWorkflowMute mirrors the above for a
// per-workflow (rather than global) mute.
func TestChannelEnabled_CriticalBypassesWorkflowMute(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetForWorkflow(ctx, scope, "sub1", "wf1", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("SetForWorkflow: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", true)
	if err != nil {
		t.Fatalf("ChannelEnabled: %v", err)
	}
	if !ok {
		t.Fatalf("expected critical workflow to bypass the per-workflow mute, got disabled")
	}
}

// TestChannelEnabled_NonCriticalStillRespectsGlobalMute is the regression: a
// non-critical workflow (the default) must keep honoring a subscriber's mute.
func TestChannelEnabled_NonCriticalStillRespectsGlobalMute(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("SetGlobal: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", false)
	if err != nil {
		t.Fatalf("ChannelEnabled: %v", err)
	}
	if ok {
		t.Fatalf("expected non-critical workflow to still be muted, got enabled")
	}
}

func TestSet_Idempotent(t *testing.T) {
	ctx := context.Background()
	svc, scope := newService(t)

	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"in_app": false}); err != nil {
		t.Fatalf("first SetGlobal: %v", err)
	}
	if err := svc.SetGlobal(ctx, scope, "sub1", map[string]bool{"in_app": true}); err != nil {
		t.Fatalf("second SetGlobal: %v", err)
	}

	ok, err := svc.ChannelEnabled(ctx, scope, "sub1", "wf1", "in_app", false)
	if err != nil {
		t.Fatalf("ChannelEnabled: %v", err)
	}
	if !ok {
		t.Fatalf("expected latest SetGlobal to win (in_app enabled), got disabled")
	}
}
