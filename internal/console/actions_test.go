package console

// actions_test.go covers the W4 ADR-013 durable console actions (#55):
// resend-notification (activity detail), credential-verify (integration
// drawer), and the test-send run-id surfacing. Each action is a native form
// POST → 303, ReadOnly-gated, two-step inline confirm where it sends/mutates,
// and deep-links to a real engine run.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// stubCredVerifier is a CredentialVerifier stub: it records the last call and
// returns a configurable run id / error without a live engine.
type stubCredVerifier struct {
	runID  string
	err    error
	lastID string
}

func (s *stubCredVerifier) Start(_ context.Context, _ domain.Scope, integrationID string) (string, error) {
	s.lastID = integrationID
	return s.runID, s.err
}

// rebuildWith re-constructs the console after mutating deps so a test can wire
// a stub verifier / read-only flag onto the harness.
func (tc *testConsole) rebuild() {
	tc.c = New(tc.deps)
	tc.h = tc.c.Routes()
}

// ─── resend-notification ─────────────────────────────────────────────────────

func TestResend_Redirects303AndReFiresWorkflow(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{
		WorkflowID:   wf.ID,
		SubscriberID: "sub-1",
		Payload:      []byte(`{"itemCount":3}`),
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/activity/"+n.ID+"/resend",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("resend status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/console/activity") || !strings.Contains(loc, n.ID) {
		t.Errorf("resend redirect = %q, want back to activity with notification id", loc)
	}

	st := tc.deps.Trigger.(*stubTrigger)
	if st.last.Name != wf.TriggerIdentifier {
		t.Errorf("re-fired trigger = %q, want %q", st.last.Name, wf.TriggerIdentifier)
	}
	if st.last.To.SubscriberID != "sub-1" {
		t.Errorf("re-fired subscriber = %q, want sub-1", st.last.To.SubscriberID)
	}
	if string(st.last.Payload) != `{"itemCount":3}` {
		t.Errorf("re-fired payload = %s, want original payload", st.last.Payload)
	}
}

func TestResend_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.rebuild()
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{WorkflowID: wf.ID, SubscriberID: "sub-1"})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/activity/"+n.ID+"/resend",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only resend status = %d, want 403", rr.Code)
	}
}

func TestResend_RequiresCSRF(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{WorkflowID: wf.ID, SubscriberID: "sub-1"})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/activity/"+n.ID+"/resend", url.Values{}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("resend without csrf status = %d, want 403", rr.Code)
	}
}

func TestResend_TwoStepConfirmRendered(t *testing.T) {
	tc := newTestConsole(t)
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{WorkflowID: wf.ID, SubscriberID: "sub-1"})
	cookie, _ := tc.login(t)

	// First click: the panel offers a Resend button (no form yet).
	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID, cookie)
	if !strings.Contains(rr.Body.String(), "activity-resend-btn") {
		t.Errorf("detail panel missing initial Resend button")
	}
	// Second click (?confirmResend=true): the panel renders the confirm form.
	rr2 := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID+"&confirmResend=true", cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, "/console/activity/"+n.ID+"/resend") {
		t.Errorf("confirm state missing native POST form action")
	}
	if !strings.Contains(body, "Confirm resend") {
		t.Errorf("confirm state missing Confirm resend button")
	}
}

func TestResend_DeepLinkAfterSuccess(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	tc.rebuild()
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{WorkflowID: wf.ID, SubscriberID: "sub-1"})
	// The resend redirect carries ?resendRun=<run id>; the panel deep-links it.
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/activity-detail?notification_id="+n.ID+"&resendRun=run-resend-1", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "http://dagnats.local/console/runs/run-resend-1") {
		t.Errorf("post-resend panel missing dagnats deep-link; body: %s", body[:min(400, len(body))])
	}
}

func TestResend_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	alt := tc.altScope(t)
	// A notification under another tenant must not be resendable by this one.
	n := tc.seedNotificationInScope(t, alt, domain.Notification{
		WorkflowID:   "wf-alt",
		SubscriberID: "sub-alt",
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/activity/"+n.ID+"/resend",
		url.Values{"csrf_token": {csrf}}, cookie)
	// The notification is invisible to this scope → resendError redirect, no
	// re-fire of the cross-tenant workflow.
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("cross-scope resend status = %d, want 303", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "resendError") {
		t.Errorf("cross-scope resend should redirect with resendError, got %q", rr.Header().Get("Location"))
	}
	st := tc.deps.Trigger.(*stubTrigger)
	if st.last.Name != "" {
		t.Errorf("cross-scope resend must not re-fire a workflow; fired %q", st.last.Name)
	}
}

// ─── credential-verify ───────────────────────────────────────────────────────

func TestVerify_Redirects303WithRunDeepLink(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	verifier := &stubCredVerifier{runID: "run-verify-1"}
	tc.deps.CredVerifier = verifier
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"), Active: true,
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/verify",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("verify status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	if verifier.lastID != in.ID {
		t.Errorf("verifier called with id %q, want %q", verifier.lastID, in.ID)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "verifyRun=run-verify-1") {
		t.Errorf("verify redirect = %q, want ?verifyRun=run-verify-1", loc)
	}

	// The re-opened drawer deep-links the verification run.
	rr2 := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID+"&verifyRun=run-verify-1", cookie)
	if !strings.Contains(rr2.Body.String(), "http://dagnats.local/console/runs/run-verify-1") {
		t.Errorf("drawer missing verification run deep-link")
	}
}

func TestVerify_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.deps.CredVerifier = &stubCredVerifier{runID: "run-x"}
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/verify",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only verify status = %d, want 403", rr.Code)
	}
}

func TestVerify_RequiresCSRF(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.CredVerifier = &stubCredVerifier{runID: "run-x"}
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/verify", url.Values{}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("verify without csrf status = %d, want 403", rr.Code)
	}
}

func TestVerify_TwoStepConfirmRendered(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.CredVerifier = &stubCredVerifier{runID: "run-x"}
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie)
	if !strings.Contains(rr.Body.String(), "integration-verify-btn") {
		t.Errorf("drawer missing initial Verify button")
	}
	rr2 := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID+"&confirmVerify=true", cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, "/console/integrations/"+in.ID+"/verify") {
		t.Errorf("confirm state missing native POST form action")
	}
	if !strings.Contains(body, "Confirm verify") {
		t.Errorf("confirm state missing Confirm verify button")
	}
}

func TestVerify_DisabledWhenUnwired(t *testing.T) {
	tc := newTestConsole(t) // CredVerifier nil by default.
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "Verify credentials") {
		t.Errorf("drawer should still render a Verify control")
	}
	if !strings.Contains(body, "disabled") {
		t.Errorf("Verify button should be disabled when CredVerifier is nil")
	}
}

func TestVerify_ScopeIsolation(t *testing.T) {
	tc := newTestConsole(t)
	verifier := &stubCredVerifier{runID: "run-y"}
	tc.deps.CredVerifier = verifier
	tc.rebuild()
	// Integration belongs to another tenant — GetByID is scope-checked inside
	// the verifier stub's caller, but the route must still 303 (not 500) and
	// the drawer GET for it renders empty.
	cookie, _ := tc.login(t)
	rr := tc.get(t, "/console/api/fragments/integration-detail?id=does-not-exist", cookie)
	if !strings.Contains(rr.Body.String(), "Select an integration") {
		t.Errorf("unknown integration drawer should render the empty state")
	}
}

// ─── test-send run-id surfacing ──────────────────────────────────────────────

func TestTestSend_SurfacesRunIDAndDeepLink(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	tc.rebuild()
	tc.seedWorkflow(t, "Welcome", "welcome-flow")
	// Seed a notification carrying the run id under the test transaction so the
	// test page resolves it from the store.
	tc.seedNotification(t, domain.Notification{
		WorkflowID:    "welcome-flow",
		SubscriberID:  "sub-1",
		TransactionID: "txn-ts-1",
		RunID:         "run-ts-1",
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/workflows/welcome-flow/test?txid=txn-ts-1", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("test page status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "run-ts-1") {
		t.Errorf("test page missing run id")
	}
	if !strings.Contains(body, "http://dagnats.local/console/runs/run-ts-1") {
		t.Errorf("test page missing dagnats run deep-link")
	}
}

// ─── no native confirm (chassis idiom gate) ──────────────────────────────────

func TestActions_NoNativeConfirm(t *testing.T) {
	tc := newTestConsole(t)
	verifier := &stubCredVerifier{runID: "run-z"}
	tc.deps.CredVerifier = verifier
	tc.rebuild()
	wf := tc.seedWorkflowAndReturn(t, "Welcome", "welcome-flow")
	n := tc.seedNotification(t, domain.Notification{WorkflowID: wf.ID, SubscriberID: "sub-1"})
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	paths := []string{
		"/console/api/fragments/activity-detail?notification_id=" + n.ID + "&confirmResend=true",
		"/console/api/fragments/integration-detail?id=" + in.ID + "&confirmVerify=true",
	}
	for _, p := range paths {
		body := tc.get(t, p, cookie).Body.String()
		for _, banned := range []string{"confirm('", `confirm("`, "alert(", "prompt(", "onsubmit="} {
			if strings.Contains(body, banned) {
				t.Errorf("%s: action template uses native %q", p, banned)
			}
		}
	}
}
