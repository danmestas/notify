package delivery

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// TestRebuildEnvelope_StreamTopic proves the self-source (rebuild) path maps
// controlValues "topic" onto runenv.Step.Topic the same way the trigger's
// envelope build does (internal/trigger/trigger.go), so a stream step that
// self-sources (e.g. it DependsOn a delay step) does not silently drop its
// routing key.
func TestRebuildEnvelope_StreamTopic(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}

	steps, err := json.Marshal([]domain.Step{
		{StepID: "S1", Type: "stream", ControlValues: json.RawMessage(`{"topic":"orders.{{payload.r}}","body":"b"}`)},
	})
	if err != nil {
		t.Fatalf("marshal steps: %v", err)
	}
	wf, err := s.Workflows().Create(ctx, scope, domain.Workflow{
		Name:              "Stream rebuild",
		TriggerIdentifier: "stream-rebuild",
		Active:            true,
		Steps:             steps,
	})
	if err != nil {
		t.Fatalf("Workflows.Create: %v", err)
	}
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_1", FirstName: "Ada"}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID:           "ntf_1",
		WorkflowID:   wf.ID,
		SubscriberID: "user_1",
		RunID:        "r1",
		Payload:      json.RawMessage(`{"r":"42"}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	h := New(s)
	env, _, _, err := RebuildEnvelope(ctx, h.store, engine.TaskPayload{RunID: "r1", StepID: "S1"})
	if err != nil {
		t.Fatalf("RebuildEnvelope: %v", err)
	}
	if env.Step.Channel != "stream" {
		t.Errorf("rebuilt Step.Channel = %q, want %q", env.Step.Channel, "stream")
	}
	if env.Step.Topic != "orders.{{payload.r}}" {
		t.Errorf("rebuilt Step.Topic = %q, want %q", env.Step.Topic, "orders.{{payload.r}}")
	}
}
