package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	corestore "github.com/danmestas/notify/internal/store"
)

// retryNote is the task-status note left when an operator retries a turn.
const retryNote = "Retried by operator"

// Refusals of RetryTurn. Callers map them to 409.
var (
	// ErrAlreadyAnswered: an agent reply already follows the activity.
	ErrAlreadyAnswered = errors.New("activity already answered")
	// ErrNotUserTurn: only a user activity starts a turn.
	ErrNotUserTurn = errors.New("activity is not a user message")
	// ErrTurnRunning: the activity's turn has not failed, so it is still
	// running (or already finished) and a second run must not start.
	ErrTurnRunning = errors.New("a turn for this activity is already running")
	// ErrNoTurnStatus: the activity has no turn status record (it predates
	// status tracking, or its write failed), so whether it was answered or is
	// running cannot be known and a second run could double-reply.
	ErrNoTurnStatus = errors.New("no turn status recorded for this activity")
	// ErrConversationBusy: another run of the conversation holds the turn
	// lock, so the retry was not admitted. The turn is still failed.
	ErrConversationBusy = errors.New("conversation busy, retry shortly")
	// ErrRetryUnavailable: no task status is kept, so a running turn could
	// not be told from a failed one. Callers map it to 503.
	ErrRetryUnavailable = errors.New("turn status is not recorded on this server")
)

// RetryTurn runs a failed turn again. conversationRef is the conversation's
// platform thread handle or its row id. The activity must be a user message
// in that conversation of that agent with no agent reply after it, and its
// turn must have failed: the failed -> submitted status change is the claim,
// so of two concurrent retries only one starts a run. The new run's
// idempotency key is "<activityId>#r<ulid>", distinct from the first run's, so
// the engine does not hand back the old failed run.
func (s *Service) RetryTurn(ctx context.Context, scope domain.Scope, agentIdentifier, conversationRef, activityID string) (ComposeResult, error) {
	agent, err := s.agents.Agents().GetByIdentifier(ctx, scope, agentIdentifier)
	if err != nil {
		return ComposeResult{}, err
	}
	conv, err := s.agents.Conversations().Get(ctx, scope, conversationRef)
	if errors.Is(err, agentstore.ErrNotFound) {
		conv, err = s.agents.Conversations().GetByThread(ctx, scope, agent.ID, "", conversationRef)
	}
	if err != nil {
		return ComposeResult{}, err
	}
	if conv.AgentID != agent.ID {
		return ComposeResult{}, agentstore.ErrNotFound
	}
	act, err := s.agents.Activities().Get(ctx, scope, activityID)
	if err != nil {
		return ComposeResult{}, err
	}
	if act.ConversationID != conv.ID {
		return ComposeResult{}, agentstore.ErrNotFound
	}
	if act.Role != "user" {
		return ComposeResult{}, ErrNotUserTurn
	}
	if err := s.requireUnanswered(ctx, scope, conv.ID, act.ID); err != nil {
		return ComposeResult{}, err
	}
	if _, ok := s.runtimes[agent.Runtime]; !ok {
		return ComposeResult{}, fmt.Errorf("%w: %q", ErrUnknownRuntime, agent.Runtime)
	}
	if s.tasks == nil {
		return ComposeResult{}, ErrRetryUnavailable
	}

	if _, ok := s.tasks.Get(ctx, scope, act.ID); !ok {
		return ComposeResult{}, ErrNoTurnStatus
	}
	reopened, err := s.tasks.Reopen(ctx, scope, act.ID, retryNote)
	if err != nil {
		return ComposeResult{}, err
	}
	if !reopened {
		return ComposeResult{}, ErrTurnRunning
	}
	runID, err := s.startTurn(ctx, scope, agent, conv.ID, act.ID, act.ID+"#r"+corestore.NewULID())
	if err != nil {
		// No run exists to close the task: put it back so it can be retried.
		s.closeTask(ctx, scope, act.ID, agentdomain.TaskFailed)
		return ComposeResult{}, err
	}
	// The workflow serialises turns per conversation (singleton skip): a run
	// started while another holds the lock is recorded as cancelled and never
	// executes, yet StartRun still answers with its id. Admission is
	// asynchronous, so wait for the run's first snapshot and put the task back
	// to failed when the run was skipped.
	if s.startSkipped(ctx, runID) {
		s.closeTask(ctx, scope, act.ID, agentdomain.TaskFailed)
		return ComposeResult{}, ErrConversationBusy
	}
	return ComposeResult{ConversationID: conv.PlatformThreadID, ActivityID: act.ID, RunID: runID}, nil
}

// admissionWait bounds how long RetryTurn waits to learn whether the engine
// admitted the new run.
const admissionWait = 5 * time.Second

// startSkipped reports whether the engine refused to run runID because the
// conversation's singleton lock was held. A run cancelled before it did
// anything is a skip. If no snapshot appears within admissionWait the run is
// taken as admitted: it is the only outcome that leaves a turn to execute.
func (s *Service) startSkipped(ctx context.Context, runID string) bool {
	deadline := time.Now().Add(admissionWait)
	for time.Now().Before(deadline) {
		snap, err := s.eng.GetRun(ctx, runID)
		if err == nil && snap.Status != "" {
			return snap.Status == "cancelled"
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return false
}

// requireUnanswered returns ErrAlreadyAnswered when an agent activity follows
// the user activity in the conversation.
func (s *Service) requireUnanswered(ctx context.Context, scope domain.Scope, conversationID, activityID string) error {
	acts, err := s.agents.Activities().History(ctx, scope, conversationID, 0)
	if err != nil {
		return err
	}
	seen := false
	for _, a := range acts {
		switch {
		case a.ID == activityID:
			seen = true
		case seen && a.Role == "agent":
			return ErrAlreadyAnswered
		}
	}
	return nil
}

// retryTurn serves POST /v1/agents/{identifier}/conversations/{conversationId}/activities/{activityId}/retry.
func (s *Service) retryTurn(w http.ResponseWriter, r *http.Request) {
	res, err := s.RetryTurn(r.Context(), identity.ScopeFrom(r.Context()),
		chi.URLParam(r, "identifier"), chi.URLParam(r, "conversationId"), chi.URLParam(r, "activityId"))
	if err != nil {
		switch {
		case errors.Is(err, ErrAlreadyAnswered), errors.Is(err, ErrNotUserTurn), errors.Is(err, ErrTurnRunning),
			errors.Is(err, ErrNoTurnStatus), errors.Is(err, ErrConversationBusy):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, ErrRetryUnavailable):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			writeComposeError(w, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(composeResponse(res))
}
