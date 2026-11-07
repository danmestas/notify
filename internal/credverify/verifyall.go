package credverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/store"
)

// DefaultVerifyCron is when the daily credential check runs: 06:17 UTC, an
// off-hour minute so it does not land with the many jobs that fire on the
// hour.
const DefaultVerifyCron = "17 6 * * *"

// VerifyCronOff, as the schedule, turns the daily check off.
const VerifyCronOff = "off"

// verifyTimeout bounds one integration's probe in the daily check, so one
// hung provider cannot stall the rest.
const verifyTimeout = 30 * time.Second

// Lister lists the integrations the daily check visits. *store.Store
// satisfies it.
type Lister interface {
	ActiveIntegrationScopes(ctx context.Context) ([]store.ScopedIntegration, error)
}

// WithLister sets where the daily check finds integrations and returns s.
func (s *Service) WithLister(l Lister) *Service {
	s.lister = l
	return s
}

// verifyAllSummary is the output of one daily check.
type verifyAllSummary struct {
	Checked     int `json:"checked"`
	Passed      int `json:"passed"`
	Rejected    int `json:"rejected"`
	Unsupported int `json:"unsupported"`
	Errored     int `json:"errored"`
}

// VerifyAllHandler is the engine task handler for names.VerifyAll: it probes
// every active integration in every environment under that integration's own
// scope and reports each verdict to the observer, like a single check does.
// A probe that cannot complete (provider unreachable) is counted and skipped;
// the next day's check tries again.
func (s *Service) VerifyAllHandler(ctx context.Context, _ engine.TaskPayload) (json.RawMessage, error) {
	if s.lister == nil {
		return nil, errors.New("credverify: no integration lister")
	}
	all, err := s.lister.ActiveIntegrationScopes(ctx)
	if err != nil {
		return nil, engine.NewRetryableError(fmt.Errorf("credverify: list integrations: %w", err))
	}
	var sum verifyAllSummary
	for _, si := range all {
		if ctx.Err() != nil {
			return nil, engine.NewRetryableError(ctx.Err())
		}
		sum.Checked++
		switch s.checkOne(ctx, si.Scope, si.IntegrationID) {
		case outcomePassed:
			sum.Passed++
		case outcomeRejected:
			sum.Rejected++
		case outcomeUnsupported:
			sum.Unsupported++
		default:
			sum.Errored++
		}
	}
	s.log.Info("credverify: daily check done",
		"checked", sum.Checked, "passed", sum.Passed, "rejected", sum.Rejected,
		"unsupported", sum.Unsupported, "errored", sum.Errored)
	out, err := json.Marshal(sum)
	if err != nil {
		return nil, fmt.Errorf("credverify: encode summary: %w", err)
	}
	return out, nil
}

type outcome int

const (
	outcomeErrored outcome = iota
	outcomePassed
	outcomeRejected
	outcomeUnsupported
)

func (s *Service) checkOne(ctx context.Context, scope domain.Scope, id string) outcome {
	res, err := s.CheckNow(ctx, scope, id)
	switch {
	case err != nil:
		s.log.Warn("credverify: daily check could not probe", "integration", id, "err", err)
		return outcomeErrored
	case res.Unsupported:
		return outcomeUnsupported
	case res.OK:
		return outcomePassed
	default:
		return outcomeRejected
	}
}

// CheckNow probes integration id's credentials in the caller's goroutine and
// reports the verdict to the observer, so the integration's health reflects it
// when CheckNow returns. It is what an operator's "verify now" runs, and what
// the daily check runs per integration. An error means the probe could not
// complete (the integration is missing, its credentials will not decrypt, or
// its config is malformed): nothing is reported. A provider with no probe
// returns Unsupported and reports nothing.
func (s *Service) CheckNow(ctx context.Context, scope domain.Scope, id string) (connectivity.VerifyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	res, err := s.ver.VerifyByID(ctx, scope, id)
	if err != nil {
		return connectivity.VerifyResult{}, err
	}
	if !res.Unsupported && s.obs != nil {
		s.obs.CredentialChecked(ctx, scope, id, res.OK, res.Detail)
	}
	return res, nil
}

// registerVerifyAll declares the one-step workflow the daily trigger starts.
func (s *Service) registerVerifyAll(ctx context.Context) error {
	def := engine.WorkflowDef{
		Name: names.HealthVerifyAllWorkflow,
		Steps: []engine.StepDef{{
			ID:   "verify-all",
			Task: names.VerifyAll,
			Type: dag.StepTypeNormal,
		}},
	}
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return fmt.Errorf("credverify: register health-verify-all workflow: %w", err)
	}
	return nil
}

// Scheduler manages the cron triggers notify owns. *engine.Engine satisfies
// it.
type Scheduler interface {
	UpsertTrigger(ctx context.Context, t engine.CronTrigger) error
	DeleteTrigger(ctx context.Context, id string) error
}

// ValidateVerifyCron reports a schedule that is neither "off" nor a standard
// 5-field cron expression. Empty means the default and is valid. dagnats
// parses the expression itself; this only catches a typo at start.
func ValidateVerifyCron(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" || strings.EqualFold(expr, VerifyCronOff) {
		return nil
	}
	if n := len(strings.Fields(expr)); n != 5 {
		return fmt.Errorf("credverify: cron %q must have 5 fields (minute hour day month weekday) or be %q, got %d",
			expr, VerifyCronOff, n)
	}
	return nil
}

// scheduleTimeout bounds the trigger request at start.
const scheduleTimeout = 10 * time.Second

// ScheduleDaily asserts the daily credential check's cron trigger with
// dagnats: it creates or replaces the trigger on every start, deletes it when
// expr is "off", and uses DefaultVerifyCron when expr is empty. It never
// fails start: a dagnats without program triggers, or a refusal, is logged
// once and the check is simply not scheduled.
func (s *Service) ScheduleDaily(ctx context.Context, sched Scheduler, expr string) {
	expr = strings.TrimSpace(expr)
	ctx, cancel := context.WithTimeout(ctx, scheduleTimeout)
	defer cancel()
	if strings.EqualFold(expr, VerifyCronOff) {
		if err := sched.DeleteTrigger(ctx, names.HealthVerifyTrigger); err != nil {
			if !errors.Is(err, engine.ErrTriggersUnsupported) {
				s.log.Warn("credverify: could not remove the daily credential check trigger", "err", err)
			}
			return
		}
		s.log.Info("credverify: daily credential check is off", "trigger", names.HealthVerifyTrigger)
		return
	}
	if expr == "" {
		expr = DefaultVerifyCron
	}
	err := sched.UpsertTrigger(ctx, engine.CronTrigger{
		ID:         names.HealthVerifyTrigger,
		WorkflowID: names.HealthVerifyAllWorkflow,
		Expression: expr,
		Timezone:   "UTC",
	})
	if err != nil {
		s.logNotScheduled(err)
		return
	}
	s.log.Info("credverify: daily credential check scheduled",
		"trigger", names.HealthVerifyTrigger, "cron", expr, "timezone", "UTC")
}

func (s *Service) logNotScheduled(err error) {
	if errors.Is(err, engine.ErrTriggersUnsupported) {
		s.log.Info("daily credential check not scheduled: dagnats does not support program triggers yet")
		return
	}
	s.log.Warn("daily credential check not scheduled: dagnats refused the trigger request", "err", err)
}
