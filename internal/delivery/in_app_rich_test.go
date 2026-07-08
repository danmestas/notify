package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
)

// richInAppEnvelope is inAppEnvelope with the full rich field set.
func richInAppEnvelope() runenv.InAppEnvelope {
	env := inAppEnvelope()
	env.Step.Avatar = "https://example.com/a.png"
	env.Step.PrimaryAction = &runenv.InAppAction{
		Label:    "Open",
		Redirect: &runenv.InAppRedirect{URL: "/foo", Target: "_self"},
	}
	env.Step.SecondaryAction = &runenv.InAppAction{Label: "Dismiss"}
	env.Step.Redirect = &runenv.InAppRedirect{URL: "/bar"}
	env.Step.InAppData = json.RawMessage(`{"orderId":"123"}`)
	return env
}

// extrasShape is the persisted extras blob the inbox API returns.
type extrasShape struct {
	Avatar        string `json:"avatar"`
	PrimaryAction *struct {
		Label    string `json:"label"`
		Redirect *struct {
			URL    string `json:"url"`
			Target string `json:"target"`
		} `json:"redirect"`
	} `json:"primaryAction"`
	SecondaryAction *struct {
		Label string `json:"label"`
	} `json:"secondaryAction"`
	Redirect *struct {
		URL string `json:"url"`
	} `json:"redirect"`
	Data struct {
		OrderID string `json:"orderId"`
	} `json:"data"`
}

// TestHandleInApp_PersistsRichFields proves the in_app handler snapshots the
// authored rich fields onto the persisted message's Extras blob (the reference platform field names).
func TestHandleInApp_PersistsRichFields(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	input, err := json.Marshal(richInAppEnvelope())
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}

	// The handler output JSON (used for SSE PublishInbox) must carry extras too.
	var outMsg struct {
		Extras extrasShape `json:"extras"`
	}
	if err := json.Unmarshal(out, &outMsg); err != nil {
		t.Fatalf("unmarshal handler output: %v", err)
	}
	if outMsg.Extras.PrimaryAction == nil || outMsg.Extras.PrimaryAction.Label != "Open" {
		t.Errorf("SSE output extras missing primaryAction.label: %s", out)
	}

	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	var got extrasShape
	if err := json.Unmarshal(msgs[0].Extras, &got); err != nil {
		t.Fatalf("unmarshal extras: %v\nextras=%s", err, msgs[0].Extras)
	}
	if got.Avatar != "https://example.com/a.png" {
		t.Errorf("extras.avatar = %q", got.Avatar)
	}
	if got.PrimaryAction == nil || got.PrimaryAction.Label != "Open" {
		t.Fatalf("extras.primaryAction = %+v", got.PrimaryAction)
	}
	if got.PrimaryAction.Redirect == nil || got.PrimaryAction.Redirect.URL != "/foo" {
		t.Errorf("extras.primaryAction.redirect = %+v", got.PrimaryAction.Redirect)
	}
	if got.SecondaryAction == nil || got.SecondaryAction.Label != "Dismiss" {
		t.Errorf("extras.secondaryAction = %+v", got.SecondaryAction)
	}
	if got.Redirect == nil || got.Redirect.URL != "/bar" {
		t.Errorf("extras.redirect = %+v", got.Redirect)
	}
	if got.Data.OrderID != "123" {
		t.Errorf("extras.data.orderId = %q, want 123", got.Data.OrderID)
	}
}

// TestHandleInApp_ExtrasRoundTripViaStore proves the Extras field survives a full
// DB round-trip (INSERT -> SELECT) and is non-empty for a rich in_app message.
func TestHandleInApp_ExtrasRoundTripViaStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	input, _ := json.Marshal(richInAppEnvelope())
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if len(msgs[0].Extras) == 0 || string(msgs[0].Extras) == "{}" {
		t.Errorf("Extras did not survive round-trip: %q", msgs[0].Extras)
	}
}

// TestHandleInApp_SentMessageNoRichFields_ExtrasEmpty proves an in_app step with
// no rich fields persists extras as the empty object (not null), so the inbox API
// marshals a stable {} rather than null.
func TestHandleInApp_SentMessageNoRichFields_ExtrasEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s)

	input, _ := json.Marshal(inAppEnvelope())
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}
	msgs, _ := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if len(msgs) != 1 {
		t.Fatalf("expected one message, got %d", len(msgs))
	}
	if string(msgs[0].Extras) != "{}" {
		t.Errorf("Extras = %q, want {}", msgs[0].Extras)
	}
}

// TestHandleInApp_ThrottledPersistsEmptyExtras is the regression for the critical
// nil-Extras data-loss path: a throttled in_app message constructs a domain.Message
// WITHOUT Extras. After extras becomes NOT NULL, the repo must coerce nil -> {} so
// the INSERT succeeds (no constraint violation / silent loss).
func TestHandleInApp_ThrottledPersistsEmptyExtras(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithThrottle(stubThrottler{allowed: false})

	env := richInAppEnvelope()
	env.Step.ThrottleLimit = 1
	env.Step.ThrottleWindowSec = 60
	input, _ := json.Marshal(env)
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one throttled message persisted, got %d (constraint violation = silent loss)", len(msgs))
	}
	if msgs[0].Status != "throttled" {
		t.Errorf("Status = %q, want throttled", msgs[0].Status)
	}
	if string(msgs[0].Extras) != "{}" {
		t.Errorf("Extras = %q, want {}", msgs[0].Extras)
	}
}

// stubChannelChecker disables the channel so the send path skips.
type stubChannelChecker struct{ enabled bool }

func (s stubChannelChecker) ChannelEnabled(_ context.Context, _ domain.Scope, _, _, _ string, _ bool) (bool, error) {
	return s.enabled, nil
}

// TestHandleInApp_SkippedPersistsEmptyExtras is the regression for the skipped
// persist path (channel disabled): the message persists with Extras {} rather
// than failing the NOT NULL constraint.
func TestHandleInApp_SkippedPersistsEmptyExtras(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	h := New(s).WithPreferences(stubChannelChecker{enabled: false})

	input, _ := json.Marshal(richInAppEnvelope())
	if _, err := h.HandleInApp(ctx, engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input}); err != nil {
		t.Fatalf("HandleInApp: %v", err)
	}
	msgs, err := s.Messages().List(ctx, domain.Scope{OrgID: "org_1", EnvID: "env_1"}, "user_123", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected one skipped message persisted, got %d", len(msgs))
	}
	if msgs[0].Status != "skipped" {
		t.Errorf("Status = %q, want skipped", msgs[0].Status)
	}
	if string(msgs[0].Extras) != "{}" {
		t.Errorf("Extras = %q, want {}", msgs[0].Extras)
	}
}

// TestMessageRepo_NilExtras_CoercedToEmptyObject is the unit-level regression for
// the repo nil-safety contract: a domain.Message constructed without Extras must
// INSERT with extras = '{}' and read back as such, not violate the NOT NULL column.
func TestMessageRepo_NilExtras_CoercedToEmptyObject(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	created, err := s.Messages().Create(ctx, scope, domain.Message{
		SubscriberID: "u1",
		Channel:      "in_app",
		Status:       "error",
		ErrorText:    "boom",
		// Extras left nil on purpose (mirrors persistError).
	})
	if err != nil {
		t.Fatalf("Create with nil Extras failed (NOT NULL violation = silent loss): %v", err)
	}
	if string(created.Extras) != "{}" {
		t.Errorf("returned Extras = %q, want {}", created.Extras)
	}
	msgs, err := s.Messages().List(ctx, scope, "u1", "in_app")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(msgs) != 1 || string(msgs[0].Extras) != "{}" {
		t.Fatalf("round-trip Extras = %q, want {}", msgs[0].Extras)
	}
}

// TestRebuildEnvelope_InAppRichFields proves the self-source (delay-downstream)
// path also reconstructs the rich fields from controlValues, so a throttled or
// delayed in_app step does not silently drop them.
func TestRebuildEnvelope_InAppRichFields(t *testing.T) {
	cv := json.RawMessage(`{"subject":"R","body":"hi","avatar":"https://x/a.png",` +
		`"primaryAction":{"label":"Open","redirect":{"url":"/foo","target":"_self"}},` +
		`"data":{"orderId":"9"}}`)
	h, ctx, _ := seedHTTPThenInApp(t, cv, nil)

	env, _, _, err := RebuildEnvelope(ctx, h.store, engine.TaskPayload{RunID: "r1", StepID: "S1"})
	if err != nil {
		t.Fatalf("RebuildEnvelope: %v", err)
	}
	if env.Step.Avatar != "https://x/a.png" {
		t.Errorf("rebuilt Step.Avatar = %q", env.Step.Avatar)
	}
	if env.Step.PrimaryAction == nil || env.Step.PrimaryAction.Label != "Open" {
		t.Fatalf("rebuilt Step.PrimaryAction = %+v", env.Step.PrimaryAction)
	}
	if env.Step.PrimaryAction.Redirect == nil || env.Step.PrimaryAction.Redirect.URL != "/foo" {
		t.Errorf("rebuilt Step.PrimaryAction.Redirect = %+v", env.Step.PrimaryAction.Redirect)
	}
	if string(env.Step.InAppData) == "" {
		t.Errorf("rebuilt Step.InAppData empty, want the data object")
	}
}
