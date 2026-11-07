package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
)

type stubRetrier struct{ calls [][3]string }

func (s *stubRetrier) RetryTurn(_ context.Context, _ domain.Scope, agent, conv, activity string) error {
	s.calls = append(s.calls, [3]string{agent, conv, activity})
	return nil
}

// failedUserTurn seeds a conversation whose only user turn's task failed.
func failedUserTurn(t *testing.T, tc *testConsole) (agentdomain.Conversation, agentdomain.Activity) {
	t.Helper()
	a := seedBasicAgent(t, tc, "support-bot", "Support Bot")
	conv := tc.seedConversation(t, agentdomain.Conversation{AgentID: a.ID, PlatformThreadID: "thread-1", SubscriberID: "sub-1", Status: "open"})
	content, _ := json.Marshal(map[string][]map[string]string{"parts": {{"text": "Hello there"}}})
	act := tc.seedActivity(t, agentdomain.Activity{ConversationID: conv.ID, Role: "user", Content: content})
	ctx := context.Background()
	if err := tc.agentStore.TaskStatuses().Submit(ctx, tc.scope, act.ID); err != nil {
		t.Fatal(err)
	}
	if err := tc.agentStore.TaskStatuses().Close(ctx, tc.scope, act.ID, agentdomain.TaskFailed, nil); err != nil {
		t.Fatal(err)
	}
	return conv, act
}

func TestConversation_RetryButtonOnFailedTurn(t *testing.T) {
	tc := newTestConsole(t)
	retrier := &stubRetrier{}
	tc.deps.Retrier = retrier
	tc.rebuild()
	cookie, csrf := tc.login(t)
	conv, act := failedUserTurn(t, tc)
	base := "/console/agents/support-bot/conversations/" + conv.ID

	body := tc.get(t, base, cookie).Body.String()
	retryPath := base + "/activities/" + act.ID + "/retry"
	if !strings.Contains(body, `action="`+retryPath+`"`) {
		t.Fatalf("failed turn has no Retry form to %s; body:\n%s", retryPath, body)
	}

	rr := tc.postForm(t, retryPath, url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != base {
		t.Fatalf("retry = %d -> %q, want 303 -> %s", rr.Code, rr.Header().Get("Location"), base)
	}
	if len(retrier.calls) != 1 || retrier.calls[0] != [3]string{"support-bot", conv.ID, act.ID} {
		t.Errorf("retrier calls = %v", retrier.calls)
	}

	// Without the CSRF token the post is refused and nothing retries.
	if rr := tc.postForm(t, retryPath, url.Values{}, cookie); rr.Code != http.StatusForbidden || len(retrier.calls) != 1 {
		t.Errorf("post without csrf = %d with %d calls, want 403 and 1", rr.Code, len(retrier.calls))
	}
}

func TestConversation_RetryButtonHidden(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		tc := newTestConsole(t)
		tc.deps.Retrier = &stubRetrier{}
		tc.rebuild()
		cookie, _ := tc.login(t)
		conv, _ := failedUserTurn(t, tc)
		content, _ := json.Marshal(map[string][]map[string]string{"parts": {{"text": "hi"}}})
		tc.seedActivity(t, agentdomain.Activity{ConversationID: conv.ID, Role: "agent", Content: content})
		body := tc.get(t, "/console/agents/support-bot/conversations/"+conv.ID, cookie).Body.String()
		if strings.Contains(body, "/retry") {
			t.Error("Retry shown for an answered turn")
		}
	})

	t.Run("not failed", func(t *testing.T) {
		tc := newTestConsole(t)
		tc.deps.Retrier = &stubRetrier{}
		tc.rebuild()
		cookie, _ := tc.login(t)
		_, conv := seedConvWithTurn(t, tc)
		body := tc.get(t, "/console/agents/support-bot/conversations/"+conv.ID, cookie).Body.String()
		if strings.Contains(body, "/retry") {
			t.Error("Retry shown for a turn that has not failed")
		}
	})
}
