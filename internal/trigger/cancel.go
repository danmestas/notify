package trigger

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
)

// ErrTransactionNotFound is Cancel's error for a transaction with no
// notification in the caller's environment.
var ErrTransactionNotFound = errors.New("trigger: transaction not found")

// ErrEngine marks a failure dagnats answered or never answered while Cancel read
// or cancelled a run. engine.IsTransient tells a timeout from a refusal.
var ErrEngine = errors.New("trigger: engine request failed")

// errNoCanceller is Cancel's error when the Service's engine cannot cancel runs.
var errNoCanceller = errors.New("trigger: cancel needs an engine that can cancel runs")

// CancelResult is the POST /v1/events/trigger/{transactionId}/cancel response:
// the ids of the transaction's notifications, split by outcome. Cancelled and
// AlreadyFinished are always present, empty rather than null.
type CancelResult struct {
	// Cancelled holds every notification that is now cancelled, including any
	// an earlier cancel already stopped.
	Cancelled []string `json:"cancelled"`
	// AlreadyFinished holds every notification whose run had completed or
	// failed in dagnats before a cancel took effect.
	AlreadyFinished []string `json:"alreadyFinished"`
	// Failed holds the notifications this call could not settle; retry the
	// cancel for them. Absent when every notification was settled.
	Failed []CancelFailure `json:"failed,omitempty"`
}

// CancelFailure is one notification Cancel could not settle.
type CancelFailure struct {
	NotificationID string `json:"notificationId"`
	// Reason is engine_unavailable, engine_error or internal.
	Reason string `json:"reason"`
}

// Cancel stops every run the trigger with transactionID started in scope and
// marks their notifications cancelled. It settles every notification it can:
// one that fails is listed in Failed and the rest go on, and the returned error
// joins the failures' errors. The result is meaningful whenever the error is
// not ErrTransactionNotFound.
//
// A notification with no run (its start is in flight, timed out or was
// refused) is marked cancelled without asking the engine, and a retried
// trigger then starts nothing. If dagnats did start a run for it, that run is
// stopped later: by the trigger when its start returns, or by the run's first
// step, which finds the row cancelled (delivery.StopCancelled). A running run
// is cancelled in the engine before its row is marked, so a failed engine call
// leaves the row as it was for a retry.
//
// dagnats' cancel ends the run: a pending sleep (a delay, a digest window)
// that fires afterwards no longer advances it, and no further step is
// dispatched. A step a worker has already claimed sees its context end but may
// finish what it was doing, so a send already under way can still go out; a
// run that finishes before dagnats handles the cancel is reported cancelled by
// this call and alreadyFinished by the next.
func (svc *Service) Cancel(ctx context.Context, scope domain.Scope, transactionID string) (CancelResult, error) {
	if svc.runs == nil {
		return CancelResult{}, errNoCanceller
	}
	rows, err := svc.transactionRows(ctx, scope, transactionID)
	if err != nil {
		return CancelResult{}, err
	}
	if len(rows) == 0 {
		return CancelResult{}, ErrTransactionNotFound
	}
	res := CancelResult{Cancelled: []string{}, AlreadyFinished: []string{}}
	var errs []error
	for _, n := range rows {
		finished, err := svc.cancelOne(ctx, scope, n)
		switch {
		case err != nil:
			res.Failed = append(res.Failed, CancelFailure{NotificationID: n.ID, Reason: failureReason(err)})
			errs = append(errs, err)
		case finished:
			res.AlreadyFinished = append(res.AlreadyFinished, n.ID)
		default:
			res.Cancelled = append(res.Cancelled, n.ID)
		}
	}
	return res, errors.Join(errs...)
}

// failureReason names err's class for CancelFailure.Reason.
func failureReason(err error) string {
	switch {
	case errors.Is(err, ErrEngine) && engine.IsTransient(err):
		return "engine_unavailable"
	case errors.Is(err, ErrEngine):
		return "engine_error"
	default:
		return "internal"
	}
}

// transactionRows lists every notification of transactionID in scope, oldest
// page last, across as many pages as there are.
func (svc *Service) transactionRows(ctx context.Context, scope domain.Scope, transactionID string) ([]domain.Notification, error) {
	var rows []domain.Notification
	cursor := ""
	for {
		page, err := svc.s.Notifications().List(ctx, scope,
			store.NotificationFilter{TransactionID: transactionID}, cursor, 0)
		if err != nil {
			return nil, fmt.Errorf("trigger: list notifications of %s: %w", transactionID, err)
		}
		rows = append(rows, page.Items...)
		if page.Cursor == "" {
			return rows, nil
		}
		cursor = page.Cursor
	}
}

// cancelOne cancels notification n and reports whether its run had already
// finished instead.
func (svc *Service) cancelOne(ctx context.Context, scope domain.Scope, n domain.Notification) (bool, error) {
	if n.RunID == "" {
		ok, err := svc.s.Notifications().MarkCancelled(ctx, scope, n.ID, "")
		if err != nil {
			return false, fmt.Errorf("trigger: cancel notification %s: %w", n.ID, err)
		}
		if ok {
			return false, nil
		}
		// A run was recorded after the row was read: cancel that run.
		if n, err = svc.s.Notifications().Get(ctx, scope, n.ID); err != nil {
			return false, fmt.Errorf("trigger: reread notification %s: %w", n.ID, err)
		}
	}
	snap, err := svc.runs.GetRun(ctx, n.RunID)
	switch {
	case errors.Is(err, engine.ErrRunNotFound):
		// A run dagnats started is never not found (v0.0.65), so a missing one
		// has been pruned and has finished.
		return n.Status != domain.NotificationCancelled, nil
	case err != nil:
		return false, fmt.Errorf("%w: read run %s: %w", ErrEngine, n.RunID, err)
	}
	if snap.IsTerminal() && snap.Status != "cancelled" {
		return true, nil
	}
	if !snap.IsTerminal() {
		// A pending run (accepted, snapshot not written yet) is cancelled like a
		// running one. dagnats#1008: a cancel of a run queued behind a
		// concurrency limit is ignored though it replies cancelled; notify sets
		// no concurrency limits, so no run of ours queues.
		if err := svc.runs.CancelRun(ctx, n.RunID); err != nil {
			return false, fmt.Errorf("%w: cancel run %s: %w", ErrEngine, n.RunID, err)
		}
	}
	if _, err := svc.s.Notifications().MarkCancelled(ctx, scope, n.ID, n.RunID); err != nil {
		return false, fmt.Errorf("trigger: cancel notification %s: %w", n.ID, err)
	}
	return false, nil
}
