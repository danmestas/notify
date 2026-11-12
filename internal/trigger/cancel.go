package trigger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// ErrTransactionNotFound is Cancel's error for a transaction with no
// notification in the caller's environment.
var ErrTransactionNotFound = errors.New("trigger: transaction not found")

// ErrRunNotVisible is Cancel's error for a run dagnats started but does not
// show yet, so it can be neither read nor cancelled. A retry shortly after
// succeeds.
var ErrRunNotVisible = errors.New("trigger: run not visible in the engine yet")

// prunedAfter is how old a notification must be before a run dagnats does not
// know is taken as pruned, and so finished, rather than not yet recorded.
const prunedAfter = time.Hour

// createdBefore reports whether n was created before t. An unparsable
// timestamp counts as old.
func createdBefore(n domain.Notification, t time.Time) bool {
	created, err := timestamp.Parse(n.CreatedAt)
	return err != nil || created.Before(t)
}

// errNoCanceller is Cancel's error when the Service's engine cannot cancel runs.
var errNoCanceller = errors.New("trigger: cancel needs an engine that can cancel runs")

// CancelResult is the POST /v1/events/trigger/{transactionId}/cancel response:
// the ids of the transaction's notifications, split by outcome. Both lists are
// always present, empty rather than null.
type CancelResult struct {
	// Cancelled holds every notification that is now cancelled, including any
	// an earlier cancel already stopped.
	Cancelled []string `json:"cancelled"`
	// AlreadyFinished holds every notification whose run had already ended
	// (completed or failed) before the cancel; those keep their status.
	AlreadyFinished []string `json:"alreadyFinished"`
}

// Cancel stops every run the trigger with transactionID started in scope and
// marks their notifications cancelled.
//
// A notification with no run (its start is in flight, timed out or was
// refused) is marked cancelled without asking the engine; a retried trigger
// then starts nothing, and a start still in flight cancels its own run when it
// finds the row cancelled. A running run is cancelled in the engine before its
// row is marked, so a failed engine call leaves the row as it was for a retry.
//
// dagnats' cancel ends the run: its pending sleeps (a delay, a digest window)
// no longer advance it and no further step is dispatched. A step a worker has
// already claimed sees its context end but may finish what it was doing; a
// send already under way can still go out.
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
	for _, n := range rows {
		finished, err := svc.cancelOne(ctx, scope, n)
		if err != nil {
			return CancelResult{}, err
		}
		if finished {
			res.AlreadyFinished = append(res.AlreadyFinished, n.ID)
		} else {
			res.Cancelled = append(res.Cancelled, n.ID)
		}
	}
	return res, nil
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
		// dagnats answers not found both for a run it has pruned and for one
		// it started moments ago but has not yet recorded (dagnats#1006).
		// Only the first has certainly finished.
		if !createdBefore(n, time.Now().Add(-prunedAfter)) {
			return false, fmt.Errorf("%w: run %s of notification %s", ErrRunNotVisible, n.RunID, n.ID)
		}
		return n.Status != domain.NotificationCancelled, nil
	case err != nil:
		return false, fmt.Errorf("trigger: read run %s: %w", n.RunID, err)
	}
	if snap.IsTerminal() && snap.Status != "cancelled" && n.Status != domain.NotificationCancelled {
		return true, nil
	}
	if !snap.IsTerminal() {
		if err := svc.runs.CancelRun(ctx, n.RunID); err != nil {
			return false, fmt.Errorf("trigger: cancel run %s: %w", n.RunID, err)
		}
	}
	if _, err := svc.s.Notifications().MarkCancelled(ctx, scope, n.ID, n.RunID); err != nil {
		return false, fmt.Errorf("trigger: cancel notification %s: %w", n.ID, err)
	}
	return false, nil
}
