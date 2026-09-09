package trigger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// stubWindows is a DigestWindowResolver returning a fixed override.
type stubWindows struct {
	window domain.DigestWindow
	ok     bool
	err    error
}

func (s stubWindows) DigestWindowFor(context.Context, domain.Scope, string, string) (domain.DigestWindow, bool, error) {
	return s.window, s.ok, s.err
}

var digestNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// TestResolveDigestDeadline is the core of #336: the deadline written into a
// subscriber's run input is their own window, so one workflow serves people on
// different cadences.
func TestResolveDigestDeadline(t *testing.T) {
	tests := []struct {
		name     string
		controls string
		resolver DigestWindowResolver
		want     string
	}{
		{
			name:     "no resolver uses the step window",
			controls: `{"type":"regular","amount":30,"unit":"minutes"}`,
			want:     digestNow.Add(30 * time.Minute).Format(time.RFC3339),
		},
		{
			name:     "no override uses the step window",
			controls: `{"type":"regular","amount":30,"unit":"minutes"}`,
			resolver: stubWindows{},
			want:     digestNow.Add(30 * time.Minute).Format(time.RFC3339),
		},
		{
			name:     "override wins over the step window",
			controls: `{"type":"regular","amount":30,"unit":"minutes"}`,
			resolver: stubWindows{window: domain.DigestWindow{Amount: 2, Unit: "hours"}, ok: true},
			want:     digestNow.Add(2 * time.Hour).Format(time.RFC3339),
		},
		{
			name:     "legacy controls with no type are a regular window",
			controls: `{"amount":1,"unit":"hours"}`,
			resolver: stubWindows{},
			want:     digestNow.Add(time.Hour).Format(time.RFC3339),
		},
		{
			// A cron cadence is a schedule, not a duration: digest.wait resolves
			// it at dispatch and never reads the deadline.
			name:     "scheduled window carries no deadline",
			controls: `{"type":"scheduled","cron":"0 9 * * *"}`,
			resolver: stubWindows{window: domain.DigestWindow{Amount: 2, Unit: "hours"}, ok: true},
			want:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{windows: tc.resolver}
			got, err := svc.resolveDigestDeadline(context.Background(), domain.Scope{OrgID: "o", EnvID: "e"},
				"alice", "wf_1", json.RawMessage(tc.controls), digestNow)
			if err != nil {
				t.Fatalf("resolveDigestDeadline: %v", err)
			}
			if got != tc.want {
				t.Errorf("deadline = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveDigestDeadline_ResolverErrorPropagates: a failed preference read
// must not fall back to the step window. Doing so would silently ignore a
// cadence the subscriber actually set — the silent-wrong-answer shape this
// package has been bitten by before.
func TestResolveDigestDeadline_ResolverErrorPropagates(t *testing.T) {
	svc := &Service{windows: stubWindows{err: context.DeadlineExceeded}}
	_, err := svc.resolveDigestDeadline(context.Background(), domain.Scope{OrgID: "o", EnvID: "e"},
		"alice", "wf_1", json.RawMessage(`{"type":"regular","amount":30,"unit":"minutes"}`), digestNow)
	if err == nil {
		t.Fatal("a failed override lookup was swallowed; the subscriber's cadence would be silently ignored")
	}
}

// TestResolveDigestDeadline_TwoSubscribersDiverge states the feature in one
// assertion: the same step, two subscribers, two different deadlines.
func TestResolveDigestDeadline_TwoSubscribersDiverge(t *testing.T) {
	controls := json.RawMessage(`{"type":"regular","amount":30,"unit":"minutes"}`)
	scope := domain.Scope{OrgID: "o", EnvID: "e"}

	fast := &Service{windows: stubWindows{}}
	slow := &Service{windows: stubWindows{window: domain.DigestWindow{Amount: 12, Unit: "hours"}, ok: true}}

	a, err := fast.resolveDigestDeadline(context.Background(), scope, "alice", "wf_1", controls, digestNow)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := slow.resolveDigestDeadline(context.Background(), scope, "bob", "wf_1", controls, digestNow)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if a == b {
		t.Fatalf("both subscribers got %q; one workflow must be able to serve different cadences", a)
	}
}
