package steptype

import (
	"sort"

	"github.com/danmestas/notify/internal/names"
)

// TaskTyper is optionally implemented by step types that compile to a NORMAL
// engine task, i.e. one requiring a registered handler in the worker.
//
// It exists so the set of task types the application must wire is derivable
// from the registry rather than restated in a comment. Several call sites used
// to carry notes of the form "must be registered whenever steptype.Default()
// includes X" -- true, unenforced, and exactly the kind of invariant that
// silently rots when a step type is added.
//
// Step types that compile only to the engine's native sleep (a regular delay, a
// regular digest) implement nothing: they need no handler, so they contribute
// no task types.
type TaskTyper interface {
	// TaskTypes returns every engine task name this step type can compile to,
	// across all of its sub-types.
	TaskTypes() []string
}

// TaskTypes returns the sorted, de-duplicated set of engine task names that any
// registered step type can compile to. The application must have a handler
// registered for each; see the wiring coverage test in cmd/notify.
func (r *Registry) TaskTypes() []string {
	seen := map[string]bool{}
	for _, st := range r.All() {
		tt, ok := st.(TaskTyper)
		if !ok {
			continue
		}
		for _, name := range tt.TaskTypes() {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Channel steps compile to "send.<type>"; the name is derived the same way the
// compiler derives it so the two cannot disagree.
func (s inApp) TaskTypes() []string  { return []string{names.Send(s.Name())} }
func (s email) TaskTypes() []string  { return []string{names.Send(s.Name())} }
func (s push) TaskTypes() []string   { return []string{names.Send(s.Name())} }
func (s chat) TaskTypes() []string   { return []string{names.Send(s.Name())} }
func (s sms) TaskTypes() []string    { return []string{names.Send(s.Name())} }
func (s stream) TaskTypes() []string { return []string{names.Send(s.Name())} }

// A delay compiles to a native sleep when it is regular; the scheduled and
// dynamic sub-types need handlers because their wait depends on wall-clock time
// or the trigger payload.
func (delay) TaskTypes() []string { return []string{names.DelayScheduled, names.DelayDynamic} }

// A regular digest compiles to a native sleep; a scheduled (cron) window needs
// a handler to sleep until the next tick.
func (digest) TaskTypes() []string { return []string{names.DigestWait} }

func (throttle) TaskTypes() []string { return []string{names.CheckThrottle} }

func (httpRequest) TaskTypes() []string { return []string{names.HTTPRequest} }
