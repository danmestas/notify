package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestTestSend_ReadOnlyBlocked: the test-send action is forbidden in read-only
// console, mirroring the credential-verify guard.
func TestTestSend_ReadOnlyBlocked(t *testing.T) {
	tc := newTestConsole(t)
	tc.deps.ReadOnly = true
	tc.rebuild()
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/test-send",
		url.Values{"csrf_token": {csrf}, "destination": {"x@example.com"}}, cookie)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only test-send status = %d, want 403", rr.Code)
	}
}

// TestTestSend_EmptyDestination400: a blank destination is a client error.
func TestTestSend_EmptyDestination400(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, csrf := tc.login(t)

	rr := tc.postForm(t, "/console/integrations/"+in.ID+"/test-send",
		url.Values{"csrf_token": {csrf}, "destination": {"   "}}, cookie)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty destination test-send status = %d, want 400", rr.Code)
	}
}

// TestTestSend_DrawerRendersSection: the detail fragment shows the Send-test
// form (action + destination input) when Connectivity is wired and not
// read-only.
func TestTestSend_DrawerRendersSection(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID, cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "/console/integrations/"+in.ID+"/test-send") {
		t.Errorf("drawer missing test-send form action")
	}
	if !strings.Contains(body, `name="destination"`) {
		t.Errorf("drawer missing destination input")
	}
}

// TestTestSend_OKBannerFromQueryParam: ?testSendOK=1 renders the success banner.
func TestTestSend_OKBannerFromQueryParam(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID+"&testSendOK=1", cookie)
	if !strings.Contains(rr.Body.String(), "integration-testsend-success") {
		t.Errorf("drawer missing test-send success banner for ?testSendOK=1")
	}
}

// TestTestSend_ErrorBannerFromQueryParam: ?testSendError=msg renders the error.
func TestTestSend_ErrorBannerFromQueryParam(t *testing.T) {
	tc := newTestConsole(t)
	in := tc.seedIntegration(t, domain.Integration{
		ProviderID: "smtp", Channel: "email", Identifier: "smtp-1",
		EncCredentials: []byte("fake-ciphertext"),
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/integration-detail?id="+in.ID+"&testSendError=boom", cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "integration-testsend-error") {
		t.Errorf("drawer missing test-send error banner for ?testSendError=")
	}
	if !strings.Contains(body, "boom") {
		t.Errorf("drawer error banner missing the error text")
	}
}
