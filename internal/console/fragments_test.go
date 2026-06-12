// fragments_test.go pins the Datastar fragment conventions every later
// console slice builds on: the signals/query merge (URL params win), the
// single PatchElements SSE event framing, and the toast-lift JSON shape.
//
// Methodology (dagnats fragments_test.go port):
//   - Signals ride the canonical `datastar` query parameter the SDK
//     reads on GET.
//   - The dashboard-kpis endpoint is exercised over the real seeded
//     store through the full auth stack — same path production takes.
package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

func TestMergeFragmentParams_SignalApplies(t *testing.T) {
	v := url.Values{}
	v.Set("datastar", `{"kpiChannel":"push"}`)
	req := httptest.NewRequest(http.MethodGet, "/console/api/fragments/dashboard-kpis?"+v.Encode(), nil)
	q := mergeFragmentParams(req, "kpiChannel")
	if got := q["channel"]; len(got) != 1 || got[0] != "push" {
		t.Errorf("channel = %v, want [push]", got)
	}
	if _, present := q["kpiChannel"]; present {
		t.Errorf("raw signal key leaked into the query map")
	}
}

func TestMergeFragmentParams_QueryWinsOverSignal(t *testing.T) {
	v := url.Values{}
	v.Set("datastar", `{"kpiChannel":"push"}`)
	v.Set("channel", "email")
	req := httptest.NewRequest(http.MethodGet, "/x?"+v.Encode(), nil)
	q := mergeFragmentParams(req, "kpiChannel")
	if got := q["channel"]; len(got) != 1 || got[0] != "email" {
		t.Errorf("channel = %v, want [email] (URL param must win)", got)
	}
	if len(q) != 2 { // channel + the datastar param itself
		t.Errorf("unexpected merged keys: %v", q)
	}
}

func TestMergeFragmentParams_MalformedSignalsFallBackToQuery(t *testing.T) {
	v := url.Values{}
	v.Set("datastar", `{not-json`)
	req := httptest.NewRequest(http.MethodGet, "/x?"+v.Encode(), nil)
	q := mergeFragmentParams(req, "kpiChannel")
	if _, present := q["channel"]; present {
		t.Errorf("malformed signals produced a channel filter: %v", q)
	}
	if len(q["datastar"]) != 1 {
		t.Errorf("URL params lost on signal parse failure: %v", q)
	}
}

func TestFragment_DashboardKPIs_SSEFraming(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "failed"},
	})
	cookie, _ := tc.login(t)

	rr := tc.get(t, "/console/api/fragments/dashboard-kpis", cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("fragment: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "datastar-patch-elements") {
		t.Errorf("fragment response missing SSE event marker")
	}
	if !strings.Contains(body, `id="dashboard-kpis"`) {
		t.Errorf("fragment missing the stable patch target id")
	}
	if !strings.Contains(body, "failed") {
		t.Errorf("unfiltered fragment missing the failed tile")
	}
}

func TestFragment_DashboardKPIs_ChannelSignalFilters(t *testing.T) {
	tc := newTestConsole(t)
	tc.seedDeliveries(t, []domain.AnalyticsDelivery{
		{Channel: "in_app", Status: "sent"},
		{Channel: "in_app", Status: "sent"},
		{Channel: "email", Status: "failed"},
	})
	cookie, _ := tc.login(t)

	v := url.Values{}
	v.Set("datastar", `{"kpiChannel":"in_app"}`)
	rr := tc.get(t, "/console/api/fragments/dashboard-kpis?"+v.Encode(), cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("filtered fragment: status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-channel="in_app"`) {
		t.Errorf("fragment does not echo the channel filter")
	}
	if !strings.Contains(body, "sent") {
		t.Errorf("filtered fragment missing the in_app sent tile")
	}
	if strings.Contains(body, "failed") {
		t.Errorf("filtered fragment leaked the email-only failed tile")
	}
}

func TestWriteToast_LiftShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeToast(rr, "Workflow saved.")
	if rr.Code != http.StatusOK {
		t.Fatalf("toast: status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("toast: Content-Type = %q, want application/json", got)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"toast":"Workflow saved."}` {
		t.Errorf("toast body = %s, want {\"toast\":\"Workflow saved.\"}", got)
	}
}
