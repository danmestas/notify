package authoring_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

func boolPtr(b bool) *bool { return &b }

// ensureCriticalFixture creates a critical workflow and returns the service,
// scope and a function that ensures it with a given body and critical value.
func ensureCriticalFixture(t *testing.T) (*authoring.Service, domain.Scope, func(body string, critical *bool) (domain.Workflow, authoring.EnsureOutcome)) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "authoring.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := authoring.New(st, nil)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	ensure := func(body string, critical *bool) (domain.Workflow, authoring.EnsureOutcome) {
		t.Helper()
		wf, outcome, err := svc.Ensure(ctx, scope, "alerts", authoring.EnsureInput{
			Name:              "Alerts",
			TriggerIdentifier: "alerts",
			Steps:             []domain.Step{{StepID: "S1", Type: "in_app", ControlValues: json.RawMessage(`{"body":"` + body + `"}`)}},
			Critical:          critical,
		}, authoring.OnConflictUpdate)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		return wf, outcome
	}
	if wf, outcome := ensure("v1", boolPtr(true)); outcome != authoring.EnsureCreated || !wf.Critical {
		t.Fatalf("create: outcome %v critical %v, want created and critical", outcome, wf.Critical)
	}
	return svc, scope, ensure
}

func storedCritical(t *testing.T, svc *authoring.Service, scope domain.Scope) bool {
	t.Helper()
	wf, err := svc.GetBySlug(context.Background(), scope, "alerts")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	return wf.Critical
}

// TestEnsure_TemplateChangeKeepsCritical is the first half of issue 2: a
// changed definition used to be written with Critical false.
func TestEnsure_TemplateChangeKeepsCritical(t *testing.T) {
	for _, tc := range []struct {
		name     string
		critical *bool
	}{
		{"critical sent as true", boolPtr(true)},
		{"critical not sent", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, scope, ensure := ensureCriticalFixture(t)
			if _, outcome := ensure("v2", tc.critical); outcome != authoring.EnsureUpdated {
				t.Fatalf("outcome = %v, want updated", outcome)
			}
			if !storedCritical(t, svc, scope) {
				t.Fatal("a template change cleared the critical flag")
			}
		})
	}
}

// TestEnsure_CriticalOnlyChangeIsApplied is the second half: a definition
// that differs only in Critical used to compare equal and be skipped.
func TestEnsure_CriticalOnlyChangeIsApplied(t *testing.T) {
	svc, scope, ensure := ensureCriticalFixture(t)
	if _, outcome := ensure("v1", boolPtr(false)); outcome != authoring.EnsureUpdated {
		t.Fatalf("outcome = %v, want updated for a critical-only change", outcome)
	}
	if storedCritical(t, svc, scope) {
		t.Fatal("critical-only change to false was not applied")
	}
	if _, outcome := ensure("v1", boolPtr(true)); outcome != authoring.EnsureUpdated || !storedCritical(t, svc, scope) {
		t.Fatalf("critical-only change back to true: outcome %v", outcome)
	}
	if _, outcome := ensure("v1", boolPtr(true)); outcome != authoring.EnsureUnchanged {
		t.Fatalf("same definition and critical: outcome %v, want unchanged", outcome)
	}
	if _, outcome := ensure("v1", nil); outcome != authoring.EnsureUnchanged || !storedCritical(t, svc, scope) {
		t.Fatalf("critical not sent: outcome %v, want unchanged and still critical", outcome)
	}
}
