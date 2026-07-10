package console

// actions_test.go covers the W4 ADR-013 durable console actions (#55):
// resend-notification (activity detail), credential-verify (integration
// drawer), and the test-send run-id surfacing. Each action is a native form
// POST → 303, ReadOnly-gated, two-step inline confirm where it sends/mutates,
// and deep-links to a real engine run.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
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

// ─── credential-verify: synchronous pass/fail surfaced inline (M5) ───────────
//
// The durable engine run started above gives dagnats-level durability/trace,
// but the console previously left the operator with nothing but "a run
// started" — no pass/fail, ever, without leaving the console. These prove the
// verify handler also probes synchronously (the same check the durable run's
// task handler performs) and surfaces a definite verdict inline.

func TestVerify_SyncProbeSuccess_ShowsVerifiedBanner(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.DagnatsConsoleURL = "http://dagnats.local"
	tc.deps.CredVerifier = &stubCredVerifier{runID: "run-verify-ok"}
	tc.rebuild()
	addr := startFakeSMTP(t)
	in, err := tc.deps.Connectivity.CreateSMTP(context.Background(), tc.scope, "smtp-1",
		connectivity.SMTPConfig{Addr: addr, Host: "localhost", From: "n@example.com"}, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/verify",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("verify status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "verified=1") {
		t.Errorf("verify redirect = %q, want ?verified=1 on a passing probe", loc)
	}
	if !strings.Contains(loc, "verifyRun=run-verify-ok") {
		t.Errorf("verify redirect = %q, want the durable run id still carried for the audit deep-link", loc)
	}

	rr2 := tc.get(t, loc, cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, "Credentials verified") {
		t.Errorf("page missing 'Credentials verified' banner; body:\n%s", body[:min(len(body), 2000)])
	}
	if !strings.Contains(body, "http://dagnats.local/console/runs/run-verify-ok") {
		t.Errorf("page missing dagnats deep-link alongside the verdict; body:\n%s", body[:min(len(body), 2000)])
	}
}

func TestVerify_SyncProbeRejected_ShowsErrorBanner(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.CredVerifier = &stubCredVerifier{runID: "run-verify-bad"}
	tc.rebuild()
	// Port 1 is reserved and refuses connections — a definite dial failure.
	in, err := tc.deps.Connectivity.CreateSMTP(context.Background(), tc.scope, "smtp-bad",
		connectivity.SMTPConfig{Addr: "127.0.0.1:1", Host: "localhost"}, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/verify",
		url.Values{"csrf_token": {csrf}}, cookie)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("verify status = %d, want 303 (body: %s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "verifyError=") {
		t.Errorf("verify redirect = %q, want ?verifyError= on a failing probe", loc)
	}
	if strings.Contains(loc, "verified=1") {
		t.Errorf("verify redirect = %q, must not claim verified=1 on a rejected probe", loc)
	}

	rr2 := tc.get(t, loc, cookie)
	body := rr2.Body.String()
	if !strings.Contains(body, "dial") {
		t.Errorf("page missing dial-failure detail in the error banner; body:\n%s", body[:min(len(body), 2000)])
	}
	if strings.Contains(body, "Credentials verified") {
		t.Errorf("page must not show the success banner on a rejected probe")
	}
}

// startFakeSMTP starts a minimal SMTP greeter that speaks just enough of the
// protocol for net/smtp's NewClient + EHLO + QUIT (no STARTTLS/AUTH) so
// VerifyByID's no-auth path resolves ok=true. Mirrors
// internal/connectivity/verify_test.go's helper of the same shape (unexported
// there, so the console package needs its own copy).
func startFakeSMTP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSMTP(conn)
		}
	}()
	return ln.Addr().String()
}

func serveFakeSMTP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.ToUpper(strings.TrimSpace(sc.Text()))
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			w("250-fake greets you")
			w("250 SIZE 10240000")
		case strings.HasPrefix(line, "QUIT"):
			w("221 bye")
			return
		default:
			w("250 ok")
		}
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
