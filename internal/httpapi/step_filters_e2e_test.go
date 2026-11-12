package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

const hasEmailCondition = `[{"combinator":"AND","conditions":[{"on":"subscriber","field":"email","operator":"NOT_EQUAL","value":""}]}]`

func stepFiltersWorkflowBody(slug string) string {
	return `{"name":"Filtered","triggerIdentifier":"` + slug + `","steps":[` +
		`{"stepId":"inbox","type":"in_app","controlValues":{"subject":"Hi","body":"b"}},` +
		`{"stepId":"mail","type":"email","controlValues":{"subject":"Hi","body":"b"},"filters":` + hasEmailCondition + `}]}`
}

func newStepFiltersHarness(t *testing.T) (srv *httptest.Server, st *store.Store, scope domain.Scope, apiKey string) {
	t.Helper()
	eng, err := engine.New(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop() })
	ctx := context.Background()
	st, err = store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng.Handle("notify.send.in_app", delivery.New(st).HandleInApp)
	eng.Handle("notify.send.email", delivery.NewEmail(st, capResolver{c: providers.NewCapture(), from: "noreply@acme.io"}).HandleEmail)
	if err := eng.StartWorkers(ctx); err != nil {
		t.Fatalf("StartWorkers: %v", err)
	}
	id := identity.New(st, bytesRepeat(0x2a, 32))
	aud := audience.New(st)
	srv = httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Authoring: authoring.New(st, eng),
		Trigger: trigger.New(aud, st, eng), Inbox: inboxapi.New(st), Store: st,
	}))
	t.Cleanup(srv.Close)
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return srv, st, scope, "ApiKey " + key
}

// assertFilteredRun triggers slug for an email-less and an emailed subscriber
// and checks the condition skips only the email step, for the former.
func assertFilteredRun(t *testing.T, srv *httptest.Server, st *store.Store, scope domain.Scope, apiKey, slug string) {
	t.Helper()
	ctx := context.Background()
	subs := map[string]string{
		"noemail":  `{"subscriberId":"noemail"}`,
		"hasemail": `{"subscriberId":"hasemail","email":"h@acme.io"}`,
	}
	for sid, body := range subs {
		if status, resp := doPost(t, srv.URL+"/v1/subscribers", apiKey, body); status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", sid, status, resp)
		}
		trig := `{"name":"` + slug + `","to":{"subscriberId":"` + sid + `"},"payload":{},"transactionId":"tx-` + sid + `"}`
		if status, resp := doPost(t, srv.URL+"/v1/events/trigger", apiKey, trig); status != http.StatusCreated {
			t.Fatalf("trigger %s: %d %s", sid, status, resp)
		}
	}
	get := func(sid, ch string) (domain.Message, bool) {
		msgs, err := st.Messages().List(ctx, scope, sid, ch)
		if err != nil || len(msgs) == 0 {
			return domain.Message{}, false
		}
		return msgs[0], true
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, a := get("noemail", "email")
		_, b := get("hasemail", "email")
		if a && b {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runs did not reach the email step within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if m, _ := get("noemail", "email"); m.Status != "skipped" || m.SkipReason != delivery.SkipReasonCondition {
		t.Errorf("noemail email: status %q skip %q, want skipped/condition", m.Status, m.SkipReason)
	}
	if m, _ := get("noemail", "in_app"); m.Status != "sent" {
		t.Errorf("noemail in_app: status %q, want sent", m.Status)
	}
	if m, _ := get("hasemail", "email"); m.Status != "sent" {
		t.Errorf("hasemail email: status %q skip %q, want sent", m.Status, m.SkipReason)
	}
}

func assertFiltersOnGet(t *testing.T, srv *httptest.Server, apiKey, slug string) {
	t.Helper()
	status, body := req(t, http.MethodGet, srv.URL+"/v1/workflows/"+slug, apiKey, "")
	if status != http.StatusOK {
		t.Fatalf("GET workflow: %d %s", status, body)
	}
	var got struct {
		Steps []struct {
			StepID  string          `json:"stepId"`
			Filters json.RawMessage `json:"filters"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Steps) != 2 || got.Steps[0].Filters != nil {
		t.Fatalf("steps = %+v, want 2 with no filters on the first", got.Steps)
	}
	var want, have any
	_ = json.Unmarshal([]byte(hasEmailCondition), &want)
	if err := json.Unmarshal(got.Steps[1].Filters, &have); err != nil {
		t.Fatalf("filters on GET = %s: %v", got.Steps[1].Filters, err)
	}
	w, _ := json.Marshal(want)
	h, _ := json.Marshal(have)
	if string(w) != string(h) {
		t.Errorf("filters on GET = %s, want %s", h, w)
	}
}

func TestPostWorkflow_StepFiltersStoredReturnedAndEvaluated(t *testing.T) {
	srv, st, scope, apiKey := newStepFiltersHarness(t)
	if status, resp := doPost(t, srv.URL+"/v1/workflows", apiKey, stepFiltersWorkflowBody("filt-post")); status != http.StatusCreated {
		t.Fatalf("POST workflow: %d %s", status, resp)
	}
	assertFiltersOnGet(t, srv, apiKey, "filt-post")
	assertFilteredRun(t, srv, st, scope, apiKey, "filt-post")
}

func TestPutWorkflow_StepFiltersStoredReturnedAndEvaluated(t *testing.T) {
	srv, st, scope, apiKey := newStepFiltersHarness(t)
	if status, resp := doPut(t, srv.URL+"/v1/workflows/filt-put", apiKey, stepFiltersWorkflowBody("filt-put")); status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("PUT workflow: %d %s", status, resp)
	}
	assertFiltersOnGet(t, srv, apiKey, "filt-put")
	assertFilteredRun(t, srv, st, scope, apiKey, "filt-put")
}

func TestWorkflow_MalformedStepFiltersReturn400(t *testing.T) {
	srv, _, _, apiKey := newStepFiltersHarness(t)
	for name, filters := range map[string]string{
		"object not array": `{"combinator":"AND"}`,
		"string":           `"nope"`,
		"bad condition":    `[{"combinator":"AND","conditions":"x"}]`,
	} {
		body := `{"name":"Bad","triggerIdentifier":"bad","steps":[{"type":"in_app","controlValues":{"subject":"s","body":"b"},"filters":` + filters + `}]}`
		if status, resp := doPost(t, srv.URL+"/v1/workflows", apiKey, body); status != http.StatusBadRequest {
			t.Errorf("POST %s: %d %s, want 400", name, status, resp)
		}
		if status, resp := doPut(t, srv.URL+"/v1/workflows/bad", apiKey, body); status != http.StatusBadRequest {
			t.Errorf("PUT %s: %d %s, want 400", name, status, resp)
		}
	}
}
