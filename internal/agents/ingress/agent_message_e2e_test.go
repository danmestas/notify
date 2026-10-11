package ingress_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/agents/agentstep"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// publishAgentWorkflow creates and publishes a one-step agent_message
// workflow and returns a trigger service for it.
func (h *harness) publishAgentWorkflow(t *testing.T, identifier, controls string) *trigger.Service {
	t.Helper()
	ctx := context.Background()
	auth := authoring.New(h.store, h.eng)
	if _, err := auth.Create(ctx, testScope, authoring.WorkflowInput{
		Name:              identifier,
		TriggerIdentifier: identifier,
		Steps: []domain.Step{
			{StepID: "a1", Type: "agent_message", ControlValues: json.RawMessage(controls)},
		},
	}); err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}
	if err := auth.Publish(ctx, testScope, identifier); err != nil {
		t.Fatalf("authoring.Publish: %v", err)
	}
	return trigger.New(audience.New(h.store), h.store, h.eng)
}

// fire triggers the workflow for the harness subscriber and waits for its run
// to finish, returning the run's final snapshot and its step_outputs.
func (h *harness) fire(t *testing.T, trg *trigger.Service, identifier, transactionID, payload string) (engine.RunSnapshot, map[string]map[string]any) {
	t.Helper()
	ctx := context.Background()
	if _, err := trg.Ingest(ctx, testScope, trigger.TriggerRequest{
		Name:          identifier,
		To:            trigger.ToField{SubscriberID: subscriber},
		Payload:       json.RawMessage(payload),
		TransactionID: transactionID,
	}); err != nil {
		t.Fatalf("trigger %s: %v", transactionID, err)
	}
	page, err := h.store.Notifications().List(ctx, testScope, store.NotificationFilter{TransactionID: transactionID}, "", 10)
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("notification for %s: %v (%d)", transactionID, err, len(page.Items))
	}
	runID := page.Items[0].RunID
	deadline := time.Now().Add(20 * time.Second)
	for {
		snap, err := h.eng.GetRun(ctx, runID)
		if err == nil && snap.IsTerminal() {
			outs, err := h.store.StepOutputs().ListByRun(ctx, runID)
			if err != nil {
				t.Fatalf("step outputs: %v", err)
			}
			return snap, outs
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s not finished in 20s (last %+v, %v)", runID, snap, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// userTexts returns the text of each user message in the agent's
// conversation on thread, oldest first.
func (h *harness) userTexts(t *testing.T, agentID, thread string) []string {
	t.Helper()
	ctx := context.Background()
	conv, err := h.agents.Conversations().GetByThread(ctx, testScope, agentID, "", thread)
	if err != nil {
		t.Fatalf("conversation %q: %v", thread, err)
	}
	acts, err := h.agents.Activities().History(ctx, testScope, conv.ID, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var out []string
	for _, a := range acts {
		if a.Role != "user" {
			continue
		}
		var c struct {
			Parts []agentdomain.Part `json:"parts"`
		}
		if err := json.Unmarshal(a.Content, &c); err != nil || len(c.Parts) != 1 {
			t.Fatalf("activity content %s: %v", a.Content, err)
		}
		out = append(out, c.Parts[0].Text)
	}
	return out
}

// TestAgentMessageStep_TriggerPostsToAgent is issue 4: a triggered
// agent_message step posts the rendered text into the agent's conversation
// and starts its turn; a re-trigger of the same transaction posts nothing
// more, and a later event on the same thread continues the conversation.
func TestAgentMessageStep_TriggerPostsToAgent(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)
	trg := h.publishAgentWorkflow(t, "mail-in", `{"agent":"stub","text":"Mail from {{payload.from}}: {{payload.subject}}","conversation":"{{payload.thread}}"}`)

	payload := `{"from":"ann@example.com","subject":"Re: \"plans\" & <you>\nsecond line","thread":"th-1"}`
	snap, outs := h.fire(t, trg, "mail-in", "tx-1", payload)
	if !snap.Succeeded() {
		t.Fatalf("run status %q, want completed", snap.Status)
	}
	want := "Mail from ann@example.com: Re: \"plans\" & <you>\nsecond line"
	if got := h.userTexts(t, h.agent.ID, "th-1"); len(got) != 1 || got[0] != want {
		t.Fatalf("user messages = %q, want [%q]", got, want)
	}
	out := outs["a1"]
	if out["conversationId"] != "th-1" || out["activityId"] == "" || out["runId"] == "" {
		t.Fatalf("step output = %v", out)
	}

	// The same transaction again: the trigger and the compose both dedupe.
	h.fire(t, trg, "mail-in", "tx-1", payload)
	if got := h.userTexts(t, h.agent.ID, "th-1"); len(got) != 1 {
		t.Fatalf("after re-trigger: %d user messages, want 1", len(got))
	}

	// The engine running the step again (a retry after a partial failure)
	// returns the message already posted.
	ing := ingress.New(h.agents, h.store, h.eng, h.bus).WithRuntime(agentdomain.RuntimeMesh, h.mesh)
	again, err := agentstep.New(h.store, ing).Handle(context.Background(), engine.TaskPayload{RunID: snap.RunID, StepID: "a1"})
	if err != nil {
		t.Fatalf("step re-run: %v", err)
	}
	var rerun map[string]any
	if err := json.Unmarshal(again, &rerun); err != nil || rerun["activityId"] != out["activityId"] {
		t.Fatalf("step re-run output %s, want activity %v", again, out["activityId"])
	}
	if got := h.userTexts(t, h.agent.ID, "th-1"); len(got) != 1 {
		t.Fatalf("after step re-run: %d user messages, want 1", len(got))
	}

	// A new event on the same thread continues the conversation.
	h.fire(t, trg, "mail-in", "tx-2", `{"from":"bob@example.com","subject":"follow-up","thread":"th-1"}`)
	if got := h.userTexts(t, h.agent.ID, "th-1"); len(got) != 2 || got[1] != "Mail from bob@example.com: follow-up" {
		t.Fatalf("after second event: %q", got)
	}
}

// TestAgentMessageStep_NoConversationStartsOnePerRun: with no conversation
// template, each run opens its own conversation.
func TestAgentMessageStep_NoConversationStartsOnePerRun(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)
	trg := h.publishAgentWorkflow(t, "ping", `{"agent":"stub","text":"ping {{payload.n}}"}`)

	_, first := h.fire(t, trg, "ping", "tx-a", `{"n":1}`)
	_, second := h.fire(t, trg, "ping", "tx-b", `{"n":2}`)
	a, b := first["a1"]["conversationId"], second["a1"]["conversationId"]
	if a == "" || b == "" || a == b {
		t.Fatalf("conversations %v and %v, want two distinct", a, b)
	}
	convs, err := h.agents.Conversations().ListByAgent(context.Background(), testScope, h.agent.ID, 0, "")
	if err != nil || len(convs) != 2 {
		t.Fatalf("conversations: %d %v, want 2", len(convs), err)
	}
}

// TestAgentMessageStep_FailurePolicy: an unknown or inactive agent fails the
// run under fail_closed and records an empty output under continue.
func TestAgentMessageStep_FailurePolicy(t *testing.T) {
	h := newHarness(t)
	if _, err := h.agents.Agents().Create(context.Background(), testScope, agentdomain.Agent{
		Identifier: "sleeping", Name: "Sleeping", Runtime: agentdomain.RuntimeMesh, Active: false, MeshSubject: stubSubject,
	}); err != nil {
		t.Fatalf("create inactive agent: %v", err)
	}

	cases := []struct {
		name, controls string
		wantOK         bool
	}{
		{"unknown-closed", `{"agent":"nobody","text":"hi"}`, false},
		{"inactive-closed", `{"agent":"sleeping","text":"hi"}`, false},
		{"empty-text-closed", `{"agent":"stub","text":"{{payload.missing}}"}`, false},
		{"unknown-continue", `{"agent":"nobody","text":"hi","failurePolicy":"continue"}`, true},
	}
	for _, c := range cases {
		trg := h.publishAgentWorkflow(t, c.name, c.controls)
		snap, outs := h.fire(t, trg, c.name, "tx-"+c.name, `{}`)
		if snap.Succeeded() != c.wantOK {
			t.Errorf("%s: run status %q, want succeeded=%v", c.name, snap.Status, c.wantOK)
		}
		out, saved := outs["a1"]
		switch {
		case c.wantOK && (!saved || out["conversationId"] != "" || out["runId"] != ""):
			t.Errorf("%s: output %v (saved %v), want an empty output", c.name, out, saved)
		case !c.wantOK && saved:
			t.Errorf("%s: output %v saved for a failed step", c.name, out)
		}
	}
}
