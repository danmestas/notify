package deliveryhealth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
)

var scope = domain.Scope{OrgID: "o1", EnvID: "e1"}

// rig is a store with one email integration and a Tracker observing its
// messages, recording every transition.
type rig struct {
	t     *testing.T
	st    *store.Store
	tr    *Tracker
	integ string

	mu          sync.Mutex
	transitions []Transition
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	in, err := st.Integrations().Create(ctx, scope, domain.Integration{
		ProviderID: "cloudflare-email", Channel: "email", Active: true, Identifier: "cf-mail",
	})
	if err != nil {
		t.Fatalf("create integration: %v", err)
	}
	r := &rig{t: t, st: st, integ: in.ID}
	r.tr = New(st, DefaultSettings(), nil)
	r.tr.OnTransition(func(tr Transition) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.transitions = append(r.transitions, tr)
	})
	st.SetMessageObserver(r.tr)
	return r
}

func (r *rig) deliver(status string, class providers.ErrorClass, text string) {
	r.t.Helper()
	_, err := r.st.Messages().Create(context.Background(), scope, domain.Message{
		Channel: "email", ProviderID: "cloudflare-email", IntegrationID: r.integ,
		Status: status, ErrorClass: string(class), ErrorText: text,
	})
	if err != nil {
		r.t.Fatalf("create message: %v", err)
	}
}

func (r *rig) sent()                                   { r.deliver("sent", "", "") }
func (r *rig) failed(c providers.ErrorClass, t string) { r.deliver("error", c, t) }

func (r *rig) health() domain.IntegrationHealth {
	r.t.Helper()
	h, err := r.st.IntegrationHealth().Get(context.Background(), scope, r.integ)
	if err != nil {
		r.t.Fatalf("get health: %v", err)
	}
	return h
}

func (r *rig) wantState(want State) {
	r.t.Helper()
	if got := State(r.health().State); got != want {
		r.t.Fatalf("state = %q, want %q", got, want)
	}
}

func (r *rig) lastTransition() Transition {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transitions) == 0 {
		r.t.Fatal("no transition emitted")
	}
	return r.transitions[len(r.transitions)-1]
}

const unauthorized = `cloudflare email send: status 401: {"errors":[{"code":10000,"message":"Authentication error"}]}`

func TestThreeAuthFailuresWithNoSuccessAreFailing(t *testing.T) {
	r := newRig(t)
	for range 3 {
		r.failed(providers.ClassAuth, unauthorized)
	}
	h := r.health()
	if State(h.State) != Failing {
		t.Fatalf("state = %q, want failing", h.State)
	}
	if h.LastErrorClass != "auth" || h.LastErrorMessage != unauthorized {
		t.Fatalf("last error = %q %q", h.LastErrorClass, h.LastErrorMessage)
	}
	tr := r.lastTransition()
	if tr.To != Failing || tr.From != Degraded || tr.Class != providers.ClassAuth || tr.Message != unauthorized {
		t.Fatalf("transition = %+v", tr)
	}
	if tr.Identifier != "cf-mail" || tr.Channel != "email" || tr.IntegrationID != r.integ {
		t.Fatalf("transition does not describe the integration: %+v", tr)
	}
}

func TestTwoFailuresAreNotYetFailing(t *testing.T) {
	r := newRig(t)
	r.failed(providers.ClassAuth, unauthorized)
	r.failed(providers.ClassAuth, unauthorized)
	r.wantState(Degraded)
}

func TestSuccessEndsFailingWithAFreshWindow(t *testing.T) {
	r := newRig(t)
	for range 3 {
		r.failed(providers.ClassAuth, unauthorized)
	}
	r.wantState(Failing)
	r.sent()
	r.wantState(Healthy)
	if tr := r.lastTransition(); tr.From != Failing || tr.To != Healthy || tr.Class != "" {
		t.Fatalf("transition = %+v", tr)
	}
	// The three failures before the recovery no longer count: another
	// success keeps it healthy rather than degraded (3/5 failed).
	r.sent()
	r.wantState(Healthy)
}

func TestFailingHoldsWithoutASuccess(t *testing.T) {
	r := newRig(t)
	for range 3 {
		r.failed(providers.ClassAuth, unauthorized)
	}
	r.failed(providers.ClassProviderDown, "cloudflare email send: status 503: unavailable")
	r.wantState(Failing)
	if h := r.health(); h.LastErrorClass != "provider_down" {
		t.Fatalf("last error class = %q, want the latest", h.LastErrorClass)
	}
}

func TestPassingCheckEndsFailing(t *testing.T) {
	r := newRig(t)
	for range 3 {
		r.failed(providers.ClassAuth, unauthorized)
	}
	r.tr.CredentialChecked(context.Background(), scope, r.integ, true, "cloudflare-email: token accepted for account")
	r.wantState(Healthy)
	h := r.health()
	if !h.LastCheckOK || h.LastCheckAt == "" {
		t.Fatalf("check not recorded: %+v", h)
	}
	// The window restarted at the check: one success is all it holds.
	r.sent()
	r.wantState(Healthy)
}

func TestPassingCheckVerifiesAnUnverifiedIntegration(t *testing.T) {
	r := newRig(t)
	r.tr.CredentialChecked(context.Background(), scope, r.integ, true, "smtp: authenticated with mx")
	r.wantState(Healthy)
}

func TestRejectedCheckIsFailing(t *testing.T) {
	r := newRig(t)
	r.sent()
	r.wantState(Healthy)
	detail := "cloudflare-email: credentials rejected (status 401)"
	r.tr.CredentialChecked(context.Background(), scope, r.integ, false, detail)
	h := r.health()
	if State(h.State) != Failing || h.LastErrorClass != "auth" || h.LastErrorMessage != detail || h.LastCheckOK {
		t.Fatalf("health = %+v", h)
	}
	if tr := r.lastTransition(); tr.From != Healthy || tr.To != Failing || tr.Class != providers.ClassAuth {
		t.Fatalf("transition = %+v", tr)
	}
	// Sends that keep failing leave it failing; a success ends it.
	r.failed(providers.ClassAuth, unauthorized)
	r.wantState(Failing)
	r.sent()
	r.wantState(Healthy)
}

func TestUnreachableCheckIsNoVerdict(t *testing.T) {
	r := newRig(t)
	r.sent()
	r.tr.CredentialChecked(context.Background(), scope, r.integ, false,
		"smtp: dial mx:587 failed: dial tcp: connection refused")
	h := r.health()
	if State(h.State) != Healthy || h.LastErrorClass != "" {
		t.Fatalf("health = %+v", h)
	}
	if h.LastCheckAt == "" || h.LastCheckOK {
		t.Fatalf("check not recorded: %+v", h)
	}
}

func TestRecipientErrorsChangeNothing(t *testing.T) {
	r := newRig(t)
	r.sent()
	before := r.health()
	for range 5 {
		r.failed(providers.ClassRecipient, "telegram: status 400: Bad Request: chat not found")
	}
	after := r.health()
	if after != before {
		t.Fatalf("recipient errors changed health:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestRecipientErrorsAloneLeaveUnverified(t *testing.T) {
	r := newRig(t)
	for range 5 {
		r.failed(providers.ClassRecipient, "resend: empty recipient")
	}
	if _, err := r.st.IntegrationHealth().Get(context.Background(), scope, r.integ); err != store.ErrNotFound {
		t.Fatalf("health row written for recipient errors only: %v", err)
	}
}

func TestDegradedAboveTheRate(t *testing.T) {
	r := newRig(t)
	for range 4 {
		r.sent()
	}
	r.failed(providers.ClassRateLimited, "resend: status 429: slow down")
	// 1 of 5 is 20%: not above the default rate.
	r.wantState(Healthy)
	r.failed(providers.ClassRateLimited, "resend: status 429: slow down")
	// 2 of 6 is 33%.
	r.wantState(Degraded)
	if tr := r.lastTransition(); tr.From != Healthy || tr.To != Degraded || tr.Class != providers.ClassRateLimited {
		t.Fatalf("transition = %+v", tr)
	}
	for range 6 {
		r.sent()
	}
	// 2 of 12 is 17%.
	r.wantState(Healthy)
}

func TestCustomSettings(t *testing.T) {
	r := newRig(t)
	r.tr.set = Settings{DegradedRate: 0.5, FailingErrors: 1, Window: DefaultSettings().Window}
	r.failed(providers.ClassProviderDown, "ses: status 500: boom")
	r.wantState(Failing)
}

func TestMessagesWithoutAnIntegrationAreIgnored(t *testing.T) {
	r := newRig(t)
	_, err := r.st.Messages().Create(context.Background(), scope, domain.Message{
		Channel: "in_app", Status: "error", ErrorClass: "rejected_message", ErrorText: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.st.IntegrationHealth().List(context.Background(), scope)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %v, err = %v", rows, err)
	}
}

func TestSettingsValidate(t *testing.T) {
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	for _, s := range []Settings{
		{DegradedRate: 0, FailingErrors: 3, Window: 1},
		{DegradedRate: 1, FailingErrors: 3, Window: 1},
		{DegradedRate: 0.2, FailingErrors: 0, Window: 1},
		{DegradedRate: 0.2, FailingErrors: 3, Window: 0},
	} {
		if s.Validate() == nil {
			t.Errorf("%+v validated", s)
		}
	}
}

func TestTruncateKeepsRunesWhole(t *testing.T) {
	if got := truncate("héllo", 2); got != "h" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("truncate = %q", got)
	}
}
