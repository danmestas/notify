// Package deliveryhealth tracks the delivery health of every integration
// (#100): whether the provider behind it is taking messages. It is computed
// from the delivery results notify already records (every sent or failed
// message carries the integration that sent it and, when it failed, an
// error class from providers.ClassifyError) and from credential checks.
//
// States:
//
//   - unverified: nothing has proven the integration works yet.
//   - healthy: deliveries are succeeding.
//   - degraded: more than Settings.DegradedRate of the deliveries in the
//     window failed.
//   - failing: Settings.FailingErrors or more failures in the window with no
//     success, or a credential check the provider rejected. Failing holds
//     until a delivery succeeds or a credential check passes.
//
// Errors of class "recipient" (a bad address) say nothing about the
// integration and change nothing. Recovering from failing starts a fresh
// window, so the failures that caused it do not leave the integration
// degraded once it works again.
//
// A change of state is an internal event: Tracker.OnTransition registers a
// consumer. Nothing outside notify sees it yet (#101 decides how it is
// emitted).
package deliveryhealth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// State is an integration's delivery health.
type State string

// The health states.
const (
	Unverified State = "unverified"
	Healthy    State = "healthy"
	Degraded   State = "degraded"
	Failing    State = "failing"
)

// Settings are the thresholds health is evaluated against.
type Settings struct {
	// DegradedRate is the failure rate over the window above which an
	// integration is degraded.
	DegradedRate float64
	// FailingErrors is how many failures in the window, with no success,
	// make an integration failing.
	FailingErrors int
	// Window is how far back delivery results count.
	Window time.Duration
}

// DefaultSettings are the thresholds when the operator sets none: degraded
// above a 20% failure rate, failing at 3 failures with no success, over the
// last hour.
func DefaultSettings() Settings {
	return Settings{DegradedRate: 0.20, FailingErrors: 3, Window: time.Hour}
}

// Validate reports a setting that cannot be evaluated.
func (s Settings) Validate() error {
	switch {
	case s.DegradedRate <= 0 || s.DegradedRate >= 1:
		return fmt.Errorf("deliveryhealth: degraded rate %v must be between 0 and 1", s.DegradedRate)
	case s.FailingErrors < 1:
		return fmt.Errorf("deliveryhealth: failing errors %d must be at least 1", s.FailingErrors)
	case s.Window <= 0:
		return fmt.Errorf("deliveryhealth: window %v must be positive", s.Window)
	}
	return nil
}

// Transition is the internal event a change of state emits.
type Transition struct {
	OrgID         string
	EnvID         string
	IntegrationID string
	// Identifier, Channel and ProviderID describe the integration; empty
	// when it could not be loaded.
	Identifier string
	Channel    string
	ProviderID string
	From       State
	To         State
	// Class and Message are the error behind a move to degraded or failing
	// (the provider's message, never a secret); empty on a move to healthy.
	Class   providers.ErrorClass
	Message string
	At      time.Time
}

// maxMessageLen bounds the provider message kept with the last error.
const maxMessageLen = 500

// Tracker evaluates and persists integration health. It is a
// store.DeliveryObserver: install it with Store.SetDeliveryObserver and every
// delivery outcome reaches it. Credential checks report through
// CredentialChecked.
type Tracker struct {
	st  *store.Store
	set Settings
	log *slog.Logger
	now func() time.Time

	// locks serialises the read-evaluate-write of one integration, so two
	// results landing together cannot both report the same transition.
	locks sync.Map // integration id -> *sync.Mutex

	mu        sync.RWMutex
	listeners []func(Transition)
}

// New builds a Tracker over st. It panics on invalid settings, which are a
// wiring error: cmd/notify validates the operator's values first.
func New(st *store.Store, set Settings, log *slog.Logger) *Tracker {
	if err := set.Validate(); err != nil {
		panic(err.Error())
	}
	if log == nil {
		log = slog.Default()
	}
	return &Tracker{st: st, set: set, log: log, now: time.Now}
}

// OnTransition registers fn to receive every change of state, after it is
// stored. fn runs on the goroutine that recorded the result, so it must be
// quick.
func (t *Tracker) OnTransition(fn func(Transition)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.listeners = append(t.listeners, fn)
}

// MessageCreated records a delivery result. Messages that are not a result
// of an integration (no integration id, or a status other than sent or
// error) and recipient errors are ignored.
func (t *Tracker) MessageCreated(ctx context.Context, scope domain.Scope, m domain.Message) {
	switch m.Status {
	case "sent":
		t.record(ctx, scope, m.IntegrationID, true, "", "")
	case "error":
		t.record(ctx, scope, m.IntegrationID, false, providers.ErrorClass(m.ErrorClass), m.ErrorText)
	}
}

// AttemptFailed records a failed attempt that wrote no message: a transient
// provider failure the engine will retry. It counts like a failed message, so
// an outage on a channel that retries still reaches health; the retry that
// finally succeeds writes a sent message and counts as the success.
func (t *Tracker) AttemptFailed(ctx context.Context, scope domain.Scope, integrationID, errorClass, errorText string) {
	t.record(ctx, scope, integrationID, false, providers.ErrorClass(errorClass), errorText)
}

// record folds one delivery outcome into the integration's health. Outcomes
// with no integration, and recipient errors, are ignored.
func (t *Tracker) record(ctx context.Context, scope domain.Scope, integrationID string, ok bool, class providers.ErrorClass, text string) {
	if integrationID == "" || (!ok && class == providers.ClassRecipient) {
		return
	}
	t.update(ctx, scope, integrationID, func(h *domain.IntegrationHealth, now time.Time) error {
		return t.applyResult(ctx, scope, h, ok, class, text, now)
	})
}

// CredentialChecked records the outcome of a credential check: ok when the
// provider accepted the credentials, otherwise detail says why not. A pass
// clears failing (and unverified). A failure the provider gave as a
// credential refusal sets failing; a check that could not reach the provider
// is no verdict on the credentials and changes no state.
func (t *Tracker) CredentialChecked(ctx context.Context, scope domain.Scope, integrationID string, ok bool, detail string) {
	t.update(ctx, scope, integrationID, func(h *domain.IntegrationHealth, now time.Time) error {
		h.LastCheckAt = timestamp.Format(now)
		h.LastCheckOK = ok
		h.LastCheckDetail = ""
		if !ok {
			h.LastCheckDetail = truncate(detail, maxMessageLen)
		}
		if ok {
			if State(h.State) == Failing || State(h.State) == Unverified {
				floor, err := t.st.IntegrationHealth().LatestResultSeq(ctx)
				if err != nil {
					return fmt.Errorf("window floor: %w", err)
				}
				restore(h, floor)
			}
			return nil
		}
		class := providers.ClassifyText(detail)
		if class != providers.ClassAuth {
			return nil
		}
		setLastError(h, class, detail, now)
		h.State = string(Failing)
		return nil
	})
}

// applyResult appends one delivery outcome to the integration's results and
// folds it into h.
func (t *Tracker) applyResult(ctx context.Context, scope domain.Scope, h *domain.IntegrationHealth, ok bool, class providers.ErrorClass, text string, now time.Time) error {
	repo := t.st.IntegrationHealth()
	since := timestamp.Format(now.Add(-t.set.Window))
	seq, err := repo.RecordResult(ctx, scope, h.IntegrationID, ok, string(class), timestamp.Format(now), since)
	if err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	if !ok {
		setLastError(h, class, text, now)
	}
	state := State(h.State)
	if ok && (state == Failing || state == Unverified) {
		// The window restarts with this success, so it counts.
		restore(h, seq-1)
		return nil
	}
	if state == Failing {
		// Only a success or a passing check ends failing.
		return nil
	}
	sent, failed, err := repo.WindowCounts(ctx, scope, h.IntegrationID, since, h.WindowFloor)
	if err != nil {
		return fmt.Errorf("window counts: %w", err)
	}
	h.State = string(t.evaluate(state, sent, failed))
	return nil
}

// evaluate is the state the window's counts call for.
func (t *Tracker) evaluate(current State, sent, failed int) State {
	switch {
	case failed >= t.set.FailingErrors && sent == 0:
		return Failing
	case failed > 0 && float64(failed)/float64(sent+failed) > t.set.DegradedRate:
		return Degraded
	case sent > 0:
		return Healthy
	default:
		return current
	}
}

// restore sets h healthy and restarts its window after the result seq floor.
func restore(h *domain.IntegrationHealth, floor int64) {
	h.WindowFloor = floor
	h.State = string(Healthy)
}

func setLastError(h *domain.IntegrationHealth, class providers.ErrorClass, message string, now time.Time) {
	h.LastErrorClass = string(class)
	h.LastErrorMessage = truncate(message, maxMessageLen)
	h.LastErrorAt = timestamp.Format(now)
}

// update loads the integration's health, lets fn change it, stores it and
// emits a transition when the state changed. Failures are logged: health
// must never fail the delivery that reported it.
func (t *Tracker) update(ctx context.Context, scope domain.Scope, integrationID string, fn func(*domain.IntegrationHealth, time.Time) error) {
	lock, _ := t.locks.LoadOrStore(integrationID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	tr, changed, err := t.updateLocked(ctx, scope, integrationID, fn)
	mu.Unlock()
	if err != nil {
		t.log.Warn("deliveryhealth: update failed", "integration", integrationID, "err", err)
		return
	}
	if changed {
		t.emit(ctx, scope, tr)
	}
}

func (t *Tracker) updateLocked(ctx context.Context, scope domain.Scope, integrationID string, fn func(*domain.IntegrationHealth, time.Time) error) (Transition, bool, error) {
	now := t.now()
	repo := t.st.IntegrationHealth()
	h, err := repo.Get(ctx, scope, integrationID)
	if errors.Is(err, store.ErrNotFound) {
		h = domain.IntegrationHealth{
			IntegrationID: integrationID,
			State:         string(Unverified),
			Since:         timestamp.Format(now),
		}
	} else if err != nil {
		return Transition{}, false, fmt.Errorf("load: %w", err)
	}
	from := State(h.State)
	if err := fn(&h, now); err != nil {
		return Transition{}, false, err
	}
	to := State(h.State)
	if to != from {
		h.Since = timestamp.Format(now)
	}
	h.UpdatedAt = timestamp.Format(now)
	if err := repo.Put(ctx, scope, h); err != nil {
		return Transition{}, false, fmt.Errorf("store: %w", err)
	}
	if to == from {
		return Transition{}, false, nil
	}
	tr := Transition{
		OrgID: scope.OrgID, EnvID: scope.EnvID, IntegrationID: integrationID,
		From: from, To: to, At: now,
	}
	if to != Healthy {
		tr.Class = providers.ErrorClass(h.LastErrorClass)
		tr.Message = h.LastErrorMessage
	}
	return tr, true, nil
}

// emit describes the integration on tr and hands it to every listener.
func (t *Tracker) emit(ctx context.Context, scope domain.Scope, tr Transition) {
	if in, err := t.st.Integrations().GetByID(ctx, scope, tr.IntegrationID); err == nil {
		tr.Identifier, tr.Channel, tr.ProviderID = in.Identifier, in.Channel, in.ProviderID
	}
	t.log.Info("deliveryhealth: integration health changed",
		"integration", tr.IntegrationID, "identifier", tr.Identifier, "channel", tr.Channel,
		"from", tr.From, "to", tr.To, "class", tr.Class)
	t.mu.RLock()
	listeners := append([]func(Transition){}, t.listeners...)
	t.mu.RUnlock()
	for _, fn := range listeners {
		fn(tr)
	}
}

// truncate shortens s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
