package console

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// seedEmailThread upserts one email-thread binding under the test scope.
func (tc *testConsole) seedEmailThread(t *testing.T, th store.EmailThread) {
	t.Helper()
	tc.seedEmailThreadInScope(t, tc.scope, th)
}

// seedEmailThreadInScope upserts one email-thread binding under an explicit
// scope (scope-isolation tests).
func (tc *testConsole) seedEmailThreadInScope(t *testing.T, scope domain.Scope, th store.EmailThread) {
	t.Helper()
	if _, err := tc.st.EmailThreads().Upsert(context.Background(), scope, th); err != nil {
		t.Fatalf("seed email thread: %v", err)
	}
}

func TestInboundEmail_PageRenders(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/inbound-email", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Inbound Email") {
		t.Error("page missing 'Inbound Email' title")
	}
	if !strings.Contains(body, "Subscriber Email") {
		t.Error("page missing threads table 'Subscriber Email' header")
	}
}

func TestInboundEmail_ListsSeededThreads(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedEmailThread(t, store.EmailThread{
		AgentIdentifier:  "support-bot",
		PlatformThreadID: "thr-1",
		SubscriberID:     "sub-1",
		SubscriberEmail:  "alice@example.com",
		InboundDomain:    "inbound.acme.com",
	})
	tc.seedEmailThread(t, store.EmailThread{
		AgentIdentifier:  "sales-bot",
		PlatformThreadID: "thr-2",
		SubscriberID:     "sub-2",
		SubscriberEmail:  "bob@example.com",
		InboundDomain:    "inbound.acme.com",
	})
	rr := tc.get(t, "/console/inbound-email", cookie)
	body := rr.Body.String()
	for _, want := range []string{"alice@example.com", "bob@example.com", "support-bot", "sales-bot"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestInboundEmail_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	alt := tc.altScope(t)
	tc.seedEmailThreadInScope(t, alt, store.EmailThread{
		AgentIdentifier:  "other-bot",
		PlatformThreadID: "thr-alt",
		SubscriberID:     "sub-alt",
		SubscriberEmail:  "leak@other.example.com",
		InboundDomain:    "inbound.other.com",
	})
	rr := tc.get(t, "/console/inbound-email", cookie)
	if strings.Contains(rr.Body.String(), "leak@other.example.com") {
		t.Error("other-scope thread leaked into this scope's page")
	}
}

func TestInboundEmail_SecretConfiguredChip(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.EmailThreads = tc.st.EmailThreads()
	tc.deps.InboundSecretConfigured = true
	tc.rebuild()
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/inbound-email", cookie)
	if !strings.Contains(rr.Body.String(), "Endpoint live") {
		t.Error("configured secret should render 'Endpoint live'")
	}

	tc.deps.InboundSecretConfigured = false
	tc.rebuild()
	cookie2, _ := tc.login(t)
	rr2 := tc.get(t, "/console/inbound-email", cookie2)
	body := rr2.Body.String()
	if !strings.Contains(body, "Endpoint disabled") {
		t.Error("unset secret should render 'Endpoint disabled'")
	}
	if !strings.Contains(body, "501") {
		t.Error("unset secret should mention the 501 response")
	}
}

func TestInboundEmail_WebhookURL(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.EmailThreads = tc.st.EmailThreads()
	tc.deps.PublicBaseURL = "https://notify.example.com"
	tc.rebuild()
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/inbound-email", cookie)
	if !strings.Contains(rr.Body.String(), "https://notify.example.com/v1/inbound/email") {
		t.Error("webhook URL should be the configured base + /v1/inbound/email")
	}

	tc.deps.PublicBaseURL = ""
	tc.rebuild()
	cookie2, _ := tc.login(t)
	rr2 := tc.get(t, "/console/inbound-email", cookie2)
	if !strings.Contains(rr2.Body.String(), "{base_url}/v1/inbound/email") {
		t.Error("empty base should render the {base_url} placeholder")
	}
}

func TestInboundEmail_VerifiedDomainCount(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	ctx := context.Background()
	if _, err := tc.deps.InboundDomains.Create(ctx, tc.scope, "inbound.acme.com"); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if err := tc.st.InboundDomains().SetStatus(ctx, tc.scope, "inbound.acme.com", "verified", true); err != nil {
		t.Fatalf("set verified: %v", err)
	}
	rr := tc.get(t, "/console/inbound-email", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "1 of 1") {
		t.Error("page should show '1 of 1' domains verified")
	}
	if !strings.Contains(body, `href="/console/domains"`) {
		t.Error("page should link to /console/domains (no CRUD duplication)")
	}
}

func TestInboundEmail_FragmentReturnsTbodyOnly(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	tc.seedEmailThread(t, store.EmailThread{
		AgentIdentifier:  "support-bot",
		PlatformThreadID: "thr-1",
		SubscriberID:     "sub-1",
		SubscriberEmail:  "alice@example.com",
		InboundDomain:    "inbound.acme.com",
	})
	rr := tc.get(t, "/console/api/fragments/inbound-email-list", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "alice@example.com") {
		t.Error("fragment missing seeded thread row")
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment leaked full-page layout chrome")
	}
}

func TestInboundEmail_NavEntryIsLive(t *testing.T) {
	tc := newTestConsole(t)
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/inbound-email", cookie)
	if !strings.Contains(rr.Body.String(), `href="/console/inbound-email"`) {
		t.Error("inbound-email nav link missing")
	}
}

func TestInboundEmail_RouteAuth(t *testing.T) {
	tc := newTestConsole(t)
	rr := tc.get(t, "/console/inbound-email", nil)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated GET: status = %d, want 303", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/console/login" {
		t.Errorf("Location = %q, want /console/login", got)
	}
}

func TestInboundEmail_RouteUnmountedWhenDepsNil(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.EmailThreads = nil
	tc.rebuild()
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/inbound-email", cookie)
	if rr.Code != http.StatusNotFound {
		t.Errorf("nil EmailThreads: status = %d, want 404 (route unmounted)", rr.Code)
	}
}
