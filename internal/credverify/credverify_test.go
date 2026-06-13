package credverify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// stubEngine records workflow registrations and run starts without a live
// dagnats sidecar.
type stubEngine struct {
	registered []engine.WorkflowDef
	startedWf  string
	startedIn  json.RawMessage
	runID      string
	startErr   error
	regErr     error
}

func (s *stubEngine) RegisterWorkflow(_ context.Context, def engine.WorkflowDef) error {
	s.registered = append(s.registered, def)
	return s.regErr
}

func (s *stubEngine) StartRun(_ context.Context, workflow string, input json.RawMessage) (string, error) {
	s.startedWf = workflow
	s.startedIn = input
	return s.runID, s.startErr
}

// stubVerifier returns a canned result / error and records the call.
type stubVerifier struct {
	res    connectivity.VerifyResult
	err    error
	gotID  string
	gotOrg string
}

func (s *stubVerifier) VerifyByID(_ context.Context, scope domain.Scope, id string) (connectivity.VerifyResult, error) {
	s.gotID = id
	s.gotOrg = scope.OrgID
	return s.res, s.err
}

func TestStart_KicksRunAndReturnsRunID(t *testing.T) {
	eng := &stubEngine{runID: "run-42"}
	svc := New(eng, &stubVerifier{})
	scope := domain.Scope{OrgID: "org-1", EnvID: "env-1"}

	runID, err := svc.Start(context.Background(), scope, "int-9")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if runID != "run-42" {
		t.Errorf("runID = %q, want run-42", runID)
	}
	if eng.startedWf != workflowName {
		t.Errorf("started workflow = %q, want %q", eng.startedWf, workflowName)
	}
	var in runInput
	if err := json.Unmarshal(eng.startedIn, &in); err != nil {
		t.Fatalf("unmarshal run input: %v", err)
	}
	if in.OrgID != "org-1" || in.EnvID != "env-1" || in.IntegrationID != "int-9" {
		t.Errorf("run input = %+v, want scope + int-9", in)
	}
}

func TestStart_EmptyIntegrationID(t *testing.T) {
	svc := New(&stubEngine{}, &stubVerifier{})
	if _, err := svc.Start(context.Background(), domain.Scope{}, ""); err == nil {
		t.Fatalf("Start with empty id should error")
	}
}

func TestRegister_DeclaresOneStepVerifyWorkflow(t *testing.T) {
	eng := &stubEngine{}
	svc := New(eng, &stubVerifier{})
	if err := svc.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(eng.registered) != 1 {
		t.Fatalf("registered %d workflows, want 1", len(eng.registered))
	}
	def := eng.registered[0]
	if def.Name != workflowName {
		t.Errorf("workflow name = %q, want %q", def.Name, workflowName)
	}
	if len(def.Steps) != 1 || def.Steps[0].Task != TaskVerify {
		t.Errorf("workflow steps = %+v, want one %q step", def.Steps, TaskVerify)
	}
}

func TestHandler_CompletesWithResultOnVerify(t *testing.T) {
	ver := &stubVerifier{res: connectivity.VerifyResult{OK: true, ProviderID: "smtp", Detail: "authenticated"}}
	svc := New(&stubEngine{}, ver)
	input, _ := json.Marshal(runInput{OrgID: "org-1", EnvID: "env-1", IntegrationID: "int-7"})

	out, err := svc.Handler(context.Background(), engine.TaskPayload{Input: input})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if ver.gotID != "int-7" || ver.gotOrg != "org-1" {
		t.Errorf("verifier called with id=%q org=%q, want int-7/org-1", ver.gotID, ver.gotOrg)
	}
	var res connectivity.VerifyResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !res.OK || res.ProviderID != "smtp" {
		t.Errorf("output = %+v, want ok smtp result", res)
	}
}

func TestHandler_RejectedCredentialIsCompletedNotError(t *testing.T) {
	// A rejected credential (ok=false, nil error) must complete the run, not
	// fail it — the operator gets a definite, non-retried answer.
	ver := &stubVerifier{res: connectivity.VerifyResult{OK: false, Detail: "auth rejected"}}
	svc := New(&stubEngine{}, ver)
	input, _ := json.Marshal(runInput{IntegrationID: "int-1"})

	out, err := svc.Handler(context.Background(), engine.TaskPayload{Input: input})
	if err != nil {
		t.Fatalf("rejected credential should not error the run: %v", err)
	}
	var res connectivity.VerifyResult
	_ = json.Unmarshal(out, &res)
	if res.OK {
		t.Errorf("output OK = true, want false for rejected credential")
	}
}

func TestHandler_ProbeFailureIsRetryable(t *testing.T) {
	// A probe that could not be completed (provider unreachable) is retryable
	// so the engine retries per policy and the run stays visible.
	ver := &stubVerifier{err: errors.New("dial timeout")}
	svc := New(&stubEngine{}, ver)
	input, _ := json.Marshal(runInput{IntegrationID: "int-1"})

	_, err := svc.Handler(context.Background(), engine.TaskPayload{Input: input})
	if err == nil {
		t.Fatalf("probe failure should error")
	}
	var re *engine.RetryableError
	if !errors.As(err, &re) {
		t.Errorf("probe failure error = %T, want *engine.RetryableError", err)
	}
}
