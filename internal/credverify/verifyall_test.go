package credverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/store"
)

type stubLister struct {
	all []store.ScopedIntegration
	err error
}

func (l stubLister) ActiveIntegrationScopes(context.Context) ([]store.ScopedIntegration, error) {
	return l.all, l.err
}

// perIDVerifier answers per integration id and records every scope probed.
type perIDVerifier struct {
	res   map[string]connectivity.VerifyResult
	errs  map[string]error
	probe []string
}

func (v *perIDVerifier) VerifyByID(_ context.Context, scope domain.Scope, id string) (connectivity.VerifyResult, error) {
	v.probe = append(v.probe, scope.OrgID+"/"+scope.EnvID+"/"+id)
	return v.res[id], v.errs[id]
}

func TestVerifyAllHandler_ChecksEveryActiveIntegrationAcrossTenants(t *testing.T) {
	lister := stubLister{all: []store.ScopedIntegration{
		{Scope: domain.Scope{OrgID: "o1", EnvID: "e1"}, IntegrationID: "ok"},
		{Scope: domain.Scope{OrgID: "o1", EnvID: "e2"}, IntegrationID: "bad"},
		{Scope: domain.Scope{OrgID: "o2", EnvID: "e3"}, IntegrationID: "down"},
		{Scope: domain.Scope{OrgID: "o2", EnvID: "e3"}, IntegrationID: "nop"},
		{Scope: domain.Scope{OrgID: "o3", EnvID: "e4"}, IntegrationID: "ok2"},
	}}
	ver := &perIDVerifier{
		res: map[string]connectivity.VerifyResult{
			"ok":  {OK: true, Detail: "accepted"},
			"bad": {Detail: "credentials rejected (status 401)"},
			"nop": {Unsupported: true},
			"ok2": {OK: true},
		},
		errs: map[string]error{"down": errors.New("dial tcp: timeout")},
	}
	obs := &recordingObserver{}
	svc := New(&stubEngine{}, ver).WithObserver(obs).WithLister(lister)

	out, err := svc.VerifyAllHandler(context.Background(), engine.TaskPayload{})
	if err != nil {
		t.Fatalf("VerifyAllHandler: %v", err)
	}

	wantProbes := "[o1/e1/ok o1/e2/bad o2/e3/down o2/e3/nop o3/e4/ok2]"
	if fmt.Sprint(ver.probe) != wantProbes {
		t.Errorf("probed %v, want %s (each under its own scope)", ver.probe, wantProbes)
	}
	// Health hears every completed verdict; not the unreachable provider, nor
	// the one with no probe.
	want := []string{
		"o1/e1/ok ok=true accepted",
		"o1/e2/bad ok=false credentials rejected (status 401)",
		"o3/e4/ok2 ok=true ",
	}
	if fmt.Sprint(obs.calls) != fmt.Sprint(want) {
		t.Errorf("observer heard %q, want %q", obs.calls, want)
	}
	var sum verifyAllSummary
	if err := json.Unmarshal(out, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != (verifyAllSummary{Checked: 5, Passed: 2, Rejected: 1, Unsupported: 1, Errored: 1}) {
		t.Errorf("summary = %+v", sum)
	}
}

func TestVerifyAllHandler_ListFailureIsRetryable(t *testing.T) {
	svc := New(&stubEngine{}, &perIDVerifier{}).WithLister(stubLister{err: errors.New("db closed")})
	_, err := svc.VerifyAllHandler(context.Background(), engine.TaskPayload{})
	if err == nil {
		t.Fatal("want an error")
	}
}

func TestRegister_DeclaresVerifyAllWorkflow(t *testing.T) {
	eng := &stubEngine{}
	if err := New(eng, &perIDVerifier{}).Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found *engine.WorkflowDef
	for i := range eng.registered {
		if eng.registered[i].Name == names.HealthVerifyAllWorkflow {
			found = &eng.registered[i]
		}
	}
	if found == nil || len(found.Steps) != 1 || found.Steps[0].Task != names.VerifyAll {
		t.Fatalf("health-verify-all workflow not registered with one verify.all step: %+v", eng.registered)
	}
}

type stubScheduler struct {
	upserted []engine.CronTrigger
	deleted  []string
	err      error
}

func (s *stubScheduler) UpsertTrigger(_ context.Context, t engine.CronTrigger) error {
	s.upserted = append(s.upserted, t)
	return s.err
}

func (s *stubScheduler) DeleteTrigger(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return s.err
}

func loggedService(buf *bytes.Buffer) *Service {
	svc := New(&stubEngine{}, &perIDVerifier{})
	svc.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return svc
}

func TestScheduleDaily_UpsertsTheTrigger(t *testing.T) {
	for _, tc := range []struct{ name, expr, want string }{
		{"default", "", DefaultVerifyCron},
		{"custom", " 5 */6 * * 1-5 ", "5 */6 * * 1-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sched := &stubScheduler{}
			loggedService(&buf).ScheduleDaily(context.Background(), sched, tc.expr)
			want := engine.CronTrigger{
				ID: "notify.health-verify-daily", WorkflowID: "notify.health-verify-all",
				Expression: tc.want, Timezone: "UTC",
			}
			if len(sched.upserted) != 1 || sched.upserted[0] != want || len(sched.deleted) != 0 {
				t.Fatalf("upserted %+v deleted %v, want %+v", sched.upserted, sched.deleted, want)
			}
			if !strings.HasPrefix(want.ID, names.Prefix) || !strings.HasPrefix(want.WorkflowID, names.Prefix) {
				t.Fatal("ids must be under notify.")
			}
		})
	}
}

func TestScheduleDaily_OffDeletesTheTrigger(t *testing.T) {
	for _, expr := range []string{"off", "OFF", " off "} {
		var buf bytes.Buffer
		sched := &stubScheduler{}
		loggedService(&buf).ScheduleDaily(context.Background(), sched, expr)
		if len(sched.upserted) != 0 || fmt.Sprint(sched.deleted) != "[notify.health-verify-daily]" {
			t.Fatalf("%q: upserted %+v deleted %v", expr, sched.upserted, sched.deleted)
		}
	}
}

func TestScheduleDaily_ToleratesUnsupportedAndRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		err        error
		wantLog    string
		wantLevel  string
	}{
		{
			"no responders", "", fmt.Errorf("x: %w", engine.ErrTriggersUnsupported),
			"daily credential check not scheduled: dagnats does not support program triggers yet", "INFO",
		},
		{
			"refused", "", errors.New("engine: trigger id is not under notify."),
			"daily credential check not scheduled", "WARN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sched := &stubScheduler{err: tc.err}
			loggedService(&buf).ScheduleDaily(context.Background(), sched, tc.expr)
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], tc.wantLog) || !strings.Contains(lines[0], "level="+tc.wantLevel) {
				t.Fatalf("log = %q, want one %s line containing %q", buf.String(), tc.wantLevel, tc.wantLog)
			}
		})
	}
}

func TestScheduleDaily_OffWithoutSupportIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	sched := &stubScheduler{err: engine.ErrTriggersUnsupported}
	loggedService(&buf).ScheduleDaily(context.Background(), sched, "off")
	if strings.TrimSpace(buf.String()) != "" {
		t.Fatalf("logged %q; there is nothing to remove on a dagnats without triggers", buf.String())
	}
}

func TestValidateVerifyCron(t *testing.T) {
	for expr, ok := range map[string]bool{
		"": true, "off": true, "OFF": true, "17 6 * * *": true, "*/5 * * * *": true,
		"17 6 * *": false, "0 0 0 * * *": false, "daily": false,
	} {
		if err := ValidateVerifyCron(expr); (err == nil) != ok {
			t.Errorf("ValidateVerifyCron(%q) = %v, ok=%v", expr, err, ok)
		}
	}
}

// TestScheduleDaily_RealDagnatsWithoutProgramTriggers runs the real engine
// against the pinned dagnats, which has no program trigger endpoint: the
// request gets no responders and start-up carries on after one info line.
func TestScheduleDaily_RealDagnatsWithoutProgramTriggers(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t), engine.InboxOption())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var buf bytes.Buffer
	loggedService(&buf).ScheduleDaily(context.Background(), engine.FromConn(nc), "")
	out := strings.TrimSpace(buf.String())
	if strings.Count(out, "\n") != 0 || !strings.Contains(out, "level=INFO") ||
		!strings.Contains(out, "dagnats does not support program triggers yet") {
		t.Fatalf("log = %q", out)
	}
}
