package ingress_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

// TestDrafts_ProduceMatchesTheTurnNotTheKey: an activity carrying a pending
// request's client_message_id from another conversation, or another agent, is
// not that request's turn (danmestas/notify#87). It produces nothing, leaves
// the request pending, and the intended turn still produces the draft.
func TestDrafts_ProduceMatchesTheTurnNotTheKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	drafts := h.agents.Drafts()
	req := agentdomain.Draft{
		SubscriberID: subscriber, Mailbox: "acct-1", SourceMessageID: "m-1",
		AgentIdentifier: "stub", ConversationID: "conv-a",
	}
	if err := drafts.Request(ctx, testScope, "k1", req); err != nil {
		t.Fatal(err)
	}
	reply := func(agent, conv, act string) agentdomain.Draft {
		return agentdomain.Draft{AgentIdentifier: agent, ConversationID: conv, ActivityID: act, Text: act}
	}
	for name, r := range map[string]agentdomain.Draft{
		"other conversation": reply("stub", "conv-b", "act-b"),
		"other agent":        reply("other", "conv-a", "act-c"),
	} {
		if _, ok, err := drafts.Produce(ctx, testScope, "k1", r); err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v, want no draft and no error", name, ok, err)
		}
	}
	if ds, _ := drafts.List(ctx, testScope, subscriber, []string{"acct-1"}, 0, 50); len(ds) != 0 {
		t.Fatalf("mismatched turns made drafts: %+v", ds)
	}
	other := domain.Scope{OrgID: testScope.OrgID, EnvID: "env_other"}
	if _, ok, err := drafts.Produce(ctx, other, "k1", reply("stub", "conv-a", "act-t")); err != nil || ok {
		t.Fatalf("other environment: ok=%v err=%v", ok, err)
	}
	d, ok, err := drafts.Produce(ctx, testScope, "k1", reply("stub", "conv-a", "act-a"))
	if err != nil || !ok || d.Text != "act-a" {
		t.Fatalf("intended turn: %+v ok=%v err=%v, want the draft (request still pending)", d, ok, err)
	}
}

// TestDrafts_LegacyRequestMatchesOnKey: a request recorded without an agent or
// conversation (a row from before migration 0049) is produced by any reply
// carrying its key, as it was, until retention purges it.
func TestDrafts_LegacyRequestMatchesOnKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	drafts := h.agents.Drafts()
	if err := drafts.Request(ctx, testScope, "legacy", agentdomain.Draft{SubscriberID: subscriber, Mailbox: "acct-1", SourceMessageID: "m-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := drafts.Produce(ctx, testScope, "legacy", agentdomain.Draft{AgentIdentifier: "any", ConversationID: "any", ActivityID: "a", Text: "x"}); err != nil || !ok {
		t.Fatalf("legacy produce: ok=%v err=%v", ok, err)
	}
}

// TestAgentMessageStep_ForeignActivityDoesNotStealTheDraft: through the real
// turn path, an inbox message in another conversation that reuses a pending
// review request's key makes no draft; the request's own conversation does.
func TestAgentMessageStep_ForeignActivityDoesNotStealTheDraft(t *testing.T) {
	h := newHarness(t)
	h.startStub(t)
	ctx := context.Background()
	key := "w" + strings.Repeat("a", 63)
	if err := h.agents.Drafts().Request(ctx, testScope, key, agentdomain.Draft{
		SubscriberID: subscriber, Mailbox: "acct-1", SourceMessageID: "m-1",
		AgentIdentifier: h.agent.Identifier, ConversationID: "victim-thread",
	}); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.composeKeyed(t, key, "attacker-thread", "steer"); status != http.StatusAccepted {
		t.Fatalf("foreign compose status %d", status)
	}
	h.waitMessages(t, 1, 20*time.Second) // the foreign turn finished, and its reply ran Produce
	if ds, _ := h.agents.Drafts().List(ctx, testScope, subscriber, []string{"acct-1"}, 0, 50); len(ds) != 0 {
		t.Fatalf("foreign activity produced a draft: %+v", ds)
	}
	if status, _ := h.composeKeyed(t, key, "victim-thread", "the real turn"); status != http.StatusAccepted {
		t.Fatalf("intended compose status %d", status)
	}
	if ds := h.waitDrafts(t, "acct-1", 1); len(ds) != 1 || ds[0].ConversationID != "victim-thread" {
		t.Fatalf("drafts = %+v, want the intended turn's", ds)
	}
}
