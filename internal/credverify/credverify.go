// Package credverify implements the ADR-013 credential-verify durable action
// (Wave 4, #55). Verifying an integration's credentials is run through the same
// dagnats engine that delivers notifications: the console hands off to
// Service.Start, which kicks an engine run of the one-step "verify" workflow and
// returns its real run id. The console deep-links that run id into the dagnats
// console exactly like every other engine run.
//
// The single task "notify.verify.credentials" runs inside notify's worker pool (the
// same StartWorkers loop as send.email / send.push). Its handler performs a
// real provider handshake via connectivity.VerifyByID. A rejected credential is
// a completed run with ok=false in the output; an unreachable provider returns a
// retryable error so the engine retries per the step's policy — that is the
// "run-grade durability and visible trace" ADR-013 asks for.
package credverify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/danmestas/dagnats/dag"
	"golang.org/x/sync/singleflight"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
)

// TaskVerify is the engine task type the verify workflow's single step runs.
const TaskVerify = names.VerifyCredentials

// workflowName is the dagnats workflow registered for credential verification.
// It is a fixed, environment-independent name: verification is not per-tenant
// content, so one registration serves every scope (the scope travels in the run
// input, not the workflow name).
const workflowName = names.CredentialVerifyWorkflow

// Verifier is the subset of connectivity.Service credverify needs: probe one
// integration's credentials by id within a scope. *connectivity.Service
// satisfies it; tests pass a stub.
type Verifier interface {
	VerifyByID(ctx context.Context, scope domain.Scope, id string) (connectivity.VerifyResult, error)
}

// Engine is the subset of *engine.Engine credverify needs.
type Engine interface {
	RegisterWorkflow(ctx context.Context, def engine.WorkflowDef) error
	StartRun(ctx context.Context, workflow string, input json.RawMessage) (string, error)
}

// runInput is the StartRun payload: the scope plus the integration id to probe.
// It round-trips into the task handler verbatim.
type runInput struct {
	OrgID         string `json:"orgId"`
	EnvID         string `json:"envId"`
	IntegrationID string `json:"integrationId"`
}

// Service kicks credential-verify runs and handles the verify task. Construct
// with New, register the workflow once with Register (at startup, after the
// engine has a sidecar), and register the task handler via Handler on the
// engine before StartWorkers.
type Service struct {
	eng    Engine
	ver    Verifier
	obs    Observer
	lister Lister
	log    *slog.Logger

	// Every probe goes through check, which shares one in-flight probe per
	// integration and answers from the last completed one while it is under
	// minInterval old, so no caller can make notify hammer a provider.
	now         func() time.Time
	minInterval time.Duration
	flight      singleflight.Group
	mu          sync.Mutex
	last        map[string]checked
}

// checked is the last completed probe of one integration.
type checked struct {
	at  time.Time
	res connectivity.VerifyResult
}

// WithMinCheckInterval sets how soon after a completed check the same
// integration is probed again (default MinCheckInterval; 0 never throttles)
// and returns s.
func (s *Service) WithMinCheckInterval(d time.Duration) *Service {
	s.minInterval = d
	return s
}

// MinCheckInterval is how soon after a completed check the same integration
// is probed again: a check asked for inside it gets the previous verdict.
const MinCheckInterval = 10 * time.Second

// Observer hears the verdict of every completed credential check: ok when
// the provider accepted the credentials, otherwise detail says why not. A
// check whose provider has no probe is not reported. *deliveryhealth.Tracker
// satisfies it.
type Observer interface {
	CredentialChecked(ctx context.Context, scope domain.Scope, integrationID string, ok bool, detail string)
}

// WithObserver attaches o to hear every check's verdict and returns s.
func (s *Service) WithObserver(o Observer) *Service {
	s.obs = o
	return s
}

// startTimeout bounds CheckSoon's start request.
const startTimeout = 30 * time.Second

// CheckSoon starts a check of integration id in the background and returns
// at once. It is what runs right after an integration's credentials change
// (connectivity.Service.OnCredentialsChanged): the caller is an HTTP request
// that must not wait on the engine, nor cancel the start when it returns. A
// start that fails is logged.
func (s *Service) CheckSoon(ctx context.Context, scope domain.Scope, integrationID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	go func() {
		defer cancel()
		// The credentials just changed, so the last verdict no longer applies.
		s.forget(scope, integrationID)
		if _, err := s.Start(ctx, scope, integrationID); err != nil {
			s.log.Warn("credverify: check after credential change did not start",
				"integration", integrationID, "err", err)
		}
	}()
}

// New builds a credverify Service over the engine and the credential verifier.
func New(eng Engine, ver Verifier) *Service {
	if eng == nil {
		panic("credverify.New: engine is nil")
	}
	if ver == nil {
		panic("credverify.New: verifier is nil")
	}
	return &Service{
		eng: eng, ver: ver, log: slog.Default(),
		now: time.Now, minInterval: MinCheckInterval, last: map[string]checked{},
	}
}

func checkKey(scope domain.Scope, id string) string {
	return scope.OrgID + "/" + scope.EnvID + "/" + id
}

// forget drops integration id's remembered verdict so the next check probes.
func (s *Service) forget(scope domain.Scope, id string) {
	s.mu.Lock()
	delete(s.last, checkKey(scope, id))
	s.mu.Unlock()
}

func (s *Service) recent(key string) (connectivity.VerifyResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.last[key]
	if !ok || s.now().Sub(c.at) >= s.minInterval {
		return connectivity.VerifyResult{}, false
	}
	return c.res, true
}

// CheckNow probes integration id's credentials and reports the verdict to the
// observer, so the integration's health reflects it when CheckNow returns. It
// is what an operator's "verify now" runs, and what the durable check and the
// daily check run per integration.
//
// A check asked for within MinCheckInterval of the last completed one returns
// that verdict without probing, and concurrent checks of one integration
// share a single probe. An error means the probe could not complete (the
// integration is missing, its credentials will not decrypt, or its config is
// malformed): nothing is reported or remembered. A provider with no probe
// returns Unsupported and reports nothing.
func (s *Service) CheckNow(ctx context.Context, scope domain.Scope, id string) (connectivity.VerifyResult, error) {
	key := checkKey(scope, id)
	if res, ok := s.recent(key); ok {
		return res, nil
	}
	v, err, _ := s.flight.Do(key, func() (any, error) {
		if res, ok := s.recent(key); ok {
			return res, nil
		}
		// Detached from the first caller's cancellation: the other callers
		// sharing this probe still want its answer.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyTimeout)
		defer cancel()
		res, err := s.ver.VerifyByID(pctx, scope, id)
		if err != nil {
			return connectivity.VerifyResult{}, err
		}
		if !res.Unsupported && s.obs != nil {
			s.obs.CredentialChecked(pctx, scope, id, res.OK, res.Detail)
		}
		s.mu.Lock()
		s.last[key] = checked{at: s.now(), res: res}
		s.mu.Unlock()
		return res, nil
	})
	if err != nil {
		return connectivity.VerifyResult{}, err
	}
	return v.(connectivity.VerifyResult), nil
}

// Register declares the one-step verify workflow with the engine. It is
// idempotent on the engine side (re-registering the same name is accepted), so
// it is safe to call once at startup.
func (s *Service) Register(ctx context.Context) error {
	def := engine.WorkflowDef{
		Name: workflowName,
		Steps: []engine.StepDef{{
			ID:   "verify",
			Task: TaskVerify,
			Type: dag.StepTypeNormal,
		}},
	}
	if err := s.eng.RegisterWorkflow(ctx, def); err != nil {
		return fmt.Errorf("credverify: register workflow: %w", err)
	}
	return s.registerVerifyAll(ctx)
}

// Handler is the engine task handler for TaskVerify. Register it with
// engine.Handle(credverify.TaskVerify, svc.Handler) before StartWorkers.
func (s *Service) Handler(ctx context.Context, t engine.TaskPayload) (json.RawMessage, error) {
	var in runInput
	if err := json.Unmarshal(t.Input, &in); err != nil {
		return nil, fmt.Errorf("credverify: decode task input: %w", err)
	}
	scope := domain.Scope{OrgID: in.OrgID, EnvID: in.EnvID}
	res, err := s.CheckNow(ctx, scope, in.IntegrationID)
	if err != nil {
		// The probe could not be completed (provider unreachable, decrypt
		// failure). Treat as retryable so the engine retries per policy and the
		// run stays visible while it does.
		return nil, engine.NewRetryableError(fmt.Errorf("credverify: probe: %w", err))
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("credverify: encode result: %w", err)
	}
	// A rejected credential is a completed run with ok=false — a definite,
	// non-retryable answer the operator can act on. Returning the result as
	// output (not an error) keeps the run terminal-completed.
	return out, nil
}

// Start kicks an engine run that verifies integration id under scope and returns
// the new run id. The run id is real: it identifies a dagnats run the console
// can deep-link to immediately, even before the run completes.
func (s *Service) Start(ctx context.Context, scope domain.Scope, integrationID string) (string, error) {
	if integrationID == "" {
		return "", fmt.Errorf("credverify: integration id is required")
	}
	input, err := json.Marshal(runInput{
		OrgID:         scope.OrgID,
		EnvID:         scope.EnvID,
		IntegrationID: integrationID,
	})
	if err != nil {
		return "", fmt.Errorf("credverify: encode run input: %w", err)
	}
	runID, err := s.eng.StartRun(ctx, workflowName, input)
	if err != nil {
		return "", fmt.Errorf("credverify: start run: %w", err)
	}
	return runID, nil
}
