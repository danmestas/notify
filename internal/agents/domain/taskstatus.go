package domain

import (
	"context"
	"encoding/json"
)

// Task states a status record moves through. The first two are open.
const (
	TaskSubmitted     = "submitted"
	TaskWorking       = "working"
	TaskInputRequired = "input-required"
	TaskFailed        = "failed"
	TaskCanceled      = "canceled"
	TaskCompleted     = "completed"
)

// TaskNote is one progress line a turn reported, with the time it arrived.
type TaskNote struct {
	Text string `json:"text"`
	At   string `json:"at"`
}

// TaskStatus is the status record of one turn, keyed by the user activity id
// (the A2A taskId): its state, the progress notes in arrival order, and the
// usage metadata (tokens, cost) the agent reported, verbatim.
type TaskStatus struct {
	TaskID    string
	State     string
	Notes     []TaskNote
	Usage     json.RawMessage
	UpdatedAt string
}

// StatusSink receives a turn's progress while a runtime works on it. Methods
// never fail and are safe to call from any goroutine; an implementation that
// cannot record drops the call, because progress is never worth failing a
// turn over.
type StatusSink interface {
	// Note adds a progress line ("Running search_vault: ...").
	Note(text string)
	// Final records the state the agent reported for the turn and its usage
	// metadata. "completed" is not kept as a state: a turn completes when its
	// reply is stored.
	Final(state string, usage json.RawMessage)
}

type statusSinkKey struct{}

// WithStatusSink returns ctx carrying the sink Dispatch reports into. A
// context keeps the Runtime interface and every receiver unchanged.
func WithStatusSink(ctx context.Context, s StatusSink) context.Context {
	return context.WithValue(ctx, statusSinkKey{}, s)
}

type discardSink struct{}

func (discardSink) Note(string)                   {}
func (discardSink) Final(string, json.RawMessage) {}

// StatusSinkFrom returns the sink ctx carries, or one that drops everything,
// so a runtime reports without checking.
func StatusSinkFrom(ctx context.Context) StatusSink {
	if s, ok := ctx.Value(statusSinkKey{}).(StatusSink); ok {
		return s
	}
	return discardSink{}
}
