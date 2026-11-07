// Package tasks keeps the status record of each agent turn (an A2A task,
// keyed by the user activity id) and wakes live readers when it changes.
//
// Runtimes report into a StatusSink during Dispatch; the sink writes the
// record (so tasks/get works across restarts and for late joiners) and then
// signals the in-process watchers of that task. A watcher is only woken: it
// re-reads the record, so a missed or coalesced signal costs nothing and a
// reader in another process still sees every note by polling.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/domain"
)

const writeTimeout = 5 * time.Second

// Service records task status and fans change signals out to watchers.
type Service struct {
	repo agentstore.TaskStatusRepo
	log  *slog.Logger

	mu      sync.Mutex
	watches map[string]map[chan struct{}]struct{}
}

// New builds the service over the status repository.
func New(repo agentstore.TaskStatusRepo, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, log: log, watches: map[string]map[chan struct{}]struct{}{}}
}

func key(scope domain.Scope, taskID string) string {
	return scope.OrgID + "\x00" + scope.EnvID + "\x00" + taskID
}

// Submitted records that the turn was accepted.
func (s *Service) Submitted(ctx context.Context, scope domain.Scope, taskID string) {
	s.write(ctx, scope, taskID, "submit", func(c context.Context) error {
		return s.repo.Submit(c, scope, taskID)
	})
}

// Close moves an open task to a final state (completed, failed, ...).
func (s *Service) Close(ctx context.Context, scope domain.Scope, taskID, state string) {
	s.write(ctx, scope, taskID, "close", func(c context.Context) error {
		return s.repo.Close(c, scope, taskID, state, nil)
	})
}

// Reopen puts a failed task back to submitted with note and wakes watchers. It
// reports whether the task was reopened; false means it is open, finished, or
// otherwise not failed, and the caller must not start another turn for it.
func (s *Service) Reopen(ctx context.Context, scope domain.Scope, taskID, note string) (bool, error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	ok, err := s.repo.Reopen(wctx, scope, taskID, note)
	if ok {
		s.notify(scope, taskID)
	}
	return ok, err
}

// Get returns the record, or the zero record and false when none exists.
func (s *Service) Get(ctx context.Context, scope domain.Scope, taskID string) (agentdomain.TaskStatus, bool) {
	st, err := s.repo.Get(ctx, scope, taskID)
	if err != nil {
		if !errors.Is(err, agentstore.ErrNotFound) {
			s.log.ErrorContext(ctx, "tasks: read status", "task", taskID, "err", err)
		}
		return agentdomain.TaskStatus{}, false
	}
	return st, true
}

// Sink returns the sink a turn's runtime reports into.
func (s *Service) Sink(scope domain.Scope, taskID string) agentdomain.StatusSink {
	return sink{s: s, scope: scope, id: taskID}
}

// Watch returns a channel signalled (coalesced) whenever the task's record
// changes in this process, and a function that stops the watch.
func (s *Service) Watch(scope domain.Scope, taskID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	k := key(scope, taskID)
	s.mu.Lock()
	if s.watches[k] == nil {
		s.watches[k] = map[chan struct{}]struct{}{}
	}
	s.watches[k][ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.watches[k], ch)
		if len(s.watches[k]) == 0 {
			delete(s.watches, k)
		}
		s.mu.Unlock()
	}
}

func (s *Service) notify(scope domain.Scope, taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.watches[key(scope, taskID)] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// write runs a repository write on a context that outlives ctx's cancellation
// (a turn cut short still records why), logs a failure and signals watchers.
func (s *Service) write(ctx context.Context, scope domain.Scope, taskID, what string, fn func(context.Context) error) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := fn(wctx); err != nil {
		s.log.Error("tasks: write status", "op", what, "task", taskID, "err", err)
		return
	}
	s.notify(scope, taskID)
}

type sink struct {
	s     *Service
	scope domain.Scope
	id    string
}

func (k sink) Note(text string) {
	if text == "" {
		return
	}
	k.s.write(context.Background(), k.scope, k.id, "note", func(c context.Context) error {
		return k.s.repo.Note(c, k.scope, k.id, text)
	})
}

func (k sink) Final(state string, usage json.RawMessage) {
	if state == agentdomain.TaskCompleted || state == agentdomain.TaskWorking || state == agentdomain.TaskSubmitted {
		state = ""
	}
	if state == "" && len(usage) == 0 {
		return
	}
	k.s.write(context.Background(), k.scope, k.id, "final", func(c context.Context) error {
		return k.s.repo.Close(c, k.scope, k.id, state, usage)
	})
}
