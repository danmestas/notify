package console

// live_tail.go owns the Wave-4 (W4-W2) SSE live-tail for the activity views.
// ONE slice, ONE endpoint, serving BOTH consumers (gate 4.2):
//
//	GET /console/api/stream/activity              global Activity feed
//	GET /console/api/stream/activity?target=wf&workflow=<id>   per-workflow tab
//
// The handler subscribes to the eventbus delivery-status subject for the
// viewer's environment (notify.delivery.<envID>) and, for each delivered
// message, prepends a freshly-rendered row to the activity table via Datastar
// PatchElements — the remove-then-prepend double-patch idiom from the dagnats
// streams.go:emitRunPatch pattern. New notifications appear in real time with
// no manual refresh; if the stream drops the page still works (the page-load
// fragment + "Load more" pagination are unaffected).
//
// Scope isolation is the load-bearing invariant: a viewer in org A / env X
// must never see org B or env Y events. Two guards enforce it:
//
//  1. The subject is per-environment, so env Y's events are never delivered
//     to env X's subscription at the NATS layer.
//  2. Every event is re-checked against the request Scope (OrgID AND EnvID)
//     before it is rendered — defence in depth, so a future subject change or
//     a cross-env publish bug can never leak a row across the tenancy boundary.

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/danmestas/notify/internal/domain"
)

// DeliverySubscriber is the eventbus port the live-tail subscribes through.
// *eventbus.Bus satisfies it structurally; tests supply a lightweight stub so
// the SSE plumbing can be exercised without a live NATS sidecar. Nil disables
// the live-tail entirely — the stream endpoint answers 503 and the activity
// pages degrade gracefully to manual-refresh pagination.
type DeliverySubscriber interface {
	// SubscribeDelivery delivers each delivery-status event for the
	// environment to handler until the returned unsubscribe func is called.
	SubscribeDelivery(environmentID string, handler func([]byte)) (unsubscribe func() error, err error)
}

// liveTailBuffer bounds the per-connection event channel. A slow browser
// drops events rather than stalling the NATS delivery goroutine — the next
// full fragment fetch reconciles any gap, so dropping under back-pressure is
// safe. Sized generously enough that a normal burst never drops.
const liveTailBuffer = 64

// liveStreamTarget captures which activity view a live-tail connection feeds.
// Both consumers share the one handler; the target only changes the tbody
// selector, the row id prefix, and the single-row template name.
type liveStreamTarget struct {
	tbodySelector string // CSS selector of the tbody PatchElements prepends into
	rowIDPrefix   string // <tr id="<prefix><notificationID>"> for the remove patch
	rowTemplate   string // named single-row template renderFragment executes
}

var (
	// activityStreamTarget feeds the global /console/activity feed.
	activityStreamTarget = liveStreamTarget{
		tbodySelector: "#activity-tbody",
		rowIDPrefix:   "activity-row-",
		rowTemplate:   "activity-row",
	}
	// wfActivityStreamTarget feeds the per-workflow Activity tab.
	wfActivityStreamTarget = liveStreamTarget{
		tbodySelector: "#wf-activity-tbody",
		rowIDPrefix:   "wf-activity-row-",
		rowTemplate:   "workflow-activity-row",
	}
)

// serveActivityStream answers GET /console/api/stream/activity as a long-lived
// SSE connection. It runs behind sessionMiddleware so the request context
// carries the viewer's Scope. Query params:
//
//	target=wf       feed the per-workflow tbody (#wf-activity-tbody) instead of
//	                the global feed (#activity-tbody).
//	workflow=<id>   only emit rows for this workflow id (used by the per-workflow
//	                tab so its stream stays scoped to the one workflow).
//
// The handler blocks until the client disconnects (r.Context().Done()), then
// unsubscribes — the same prompt-cleanup proof shape as realtime.Gateway.
func (c *Console) serveActivityStream(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveActivityStream: w is nil")
	}
	if r == nil {
		panic("serveActivityStream: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveActivityStream: no scope in context (route not behind sessionMiddleware)")
	}
	if c.deps.LiveTail == nil {
		// No event bus wired: the page already rendered its rows and "Load
		// more" still works. Tell the client the live channel is unavailable
		// rather than holding a dead connection open.
		http.Error(w, "live-tail unavailable", http.StatusServiceUnavailable)
		return
	}

	target := activityStreamTarget
	if r.URL.Query().Get("target") == "wf" {
		target = wfActivityStreamTarget
	}
	workflowFilter := r.URL.Query().Get("workflow")

	// Buffered so the bus callback never blocks; a slow client just drops
	// events rather than stalling the NATS delivery goroutine.
	events := make(chan []byte, liveTailBuffer)
	unsub, err := c.deps.LiveTail.SubscribeDelivery(scope.EnvID, func(d []byte) {
		// Copy: the bus may reuse the backing array after the callback
		// returns, and the payload now crosses a channel into another
		// goroutine.
		cp := make([]byte, len(d))
		copy(cp, d)
		select {
		case events <- cp:
		default: // buffer full — drop; the next fragment fetch reconciles.
		}
	})
	if err != nil {
		c.deps.Logger.Error("console: activity stream subscribe", "err", err)
		http.Error(w, "subscribe failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = unsub() }()

	// Build the workflow id→name map once per connection. The delivered
	// message carries the internal workflow id (trigger.go sets
	// envelope.WorkflowID = wf.ID), so names must be resolved by id, not by
	// trigger identifier. A miss falls back to the id. A new workflow created
	// mid-stream shows its id until the operator reconnects — acceptable for a
	// live tail.
	nameFn := c.buildWorkflowNamer(r.Context(), scope)

	sse := datastar.NewSSE(w, r)
	c.pumpActivityStream(r.Context(), sse, events, scope, workflowFilter, target, nameFn)
}

// buildWorkflowNamer returns a workflow id→display-name resolver for the scope,
// built from one Authoring.List call. On a list error or a miss it returns the
// id unchanged, so buildActivityRow still renders something meaningful.
func (c *Console) buildWorkflowNamer(ctx context.Context, scope domain.Scope) func(workflowID string) string {
	names := map[string]string{}
	if c.deps.Authoring != nil {
		wfs, err := c.deps.Authoring.List(ctx, scope)
		if err != nil {
			c.deps.Logger.Debug("console: activity stream workflow names", "err", err)
		} else {
			for _, wf := range wfs {
				names[wf.ID] = wf.Name
			}
		}
	}
	return func(workflowID string) string {
		if n := names[workflowID]; n != "" {
			return n
		}
		return workflowID
	}
}

// pumpActivityStream is the inner loop: it drains delivery events, applies the
// scope + workflow guards, and emits one row patch per surviving event. Exits
// when the request context is done (client disconnect). Split out so the
// handler stays small and the loop is unit-testable without an HTTP server.
func (c *Console) pumpActivityStream(
	ctx context.Context,
	sse *datastar.ServerSentEventGenerator,
	events <-chan []byte,
	scope domain.Scope,
	workflowFilter string,
	target liveStreamTarget,
	nameFn func(workflowID string) string,
) {
	if c == nil {
		panic("pumpActivityStream: c is nil")
	}
	if sse == nil {
		panic("pumpActivityStream: sse is nil")
	}
	if nameFn == nil {
		nameFn = func(id string) string { return id }
	}
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-events:
			if !ok {
				return
			}
			row, keep := c.liveRowFromEvent(data, scope, workflowFilter, nameFn)
			if !keep {
				continue
			}
			if err := emitActivityRowPatch(sse, c.ts.base, target, row); err != nil {
				// A write error means the client is gone; stop pumping.
				c.deps.Logger.Debug("console: activity stream patch", "err", err)
				return
			}
		}
	}
}

// liveRowFromEvent decodes one delivery-status event into an activityRow,
// returning keep=false when the event must be dropped. Drops happen for:
//
//   - undecodable payloads (defensive; never fail the stream),
//   - SCOPE MISMATCH — the event's org or env differs from the viewer's
//     (the load-bearing isolation guard),
//   - a workflow filter that doesn't match (per-workflow tab consumer).
//
// The workflow display name is resolved via nameFn (an id→name map built once
// per connection); a miss falls back to the raw workflow id.
func (c *Console) liveRowFromEvent(data []byte, scope domain.Scope, workflowFilter string, nameFn func(string) string) (activityRow, bool) {
	if c == nil {
		panic("liveRowFromEvent: c is nil")
	}
	if nameFn == nil {
		nameFn = func(id string) string { return id }
	}
	var msg domain.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		c.deps.Logger.Debug("console: activity stream decode", "err", err)
		return activityRow{}, false
	}
	// Scope isolation — defence in depth on top of the per-env subject.
	if msg.OrgID != scope.OrgID || msg.EnvID != scope.EnvID {
		return activityRow{}, false
	}
	if workflowFilter != "" && msg.WorkflowID != workflowFilter {
		return activityRow{}, false
	}

	name := nameFn(msg.WorkflowID)
	row := c.buildActivityRow(notificationFromMessage(msg), []domain.Message{msg}, name, c.runURL(msg.RunID))
	return row, true
}

// notificationFromMessage projects the fields buildActivityRow reads off a
// Notification out of the delivered Message. The live event is per-message, so
// the notification identity is the message's NotificationID — that becomes the
// row's data-id and the remove-patch target, matching the page-load rows.
func notificationFromMessage(msg domain.Message) domain.Notification {
	return domain.Notification{
		ID:            msg.NotificationID,
		OrgID:         msg.OrgID,
		EnvID:         msg.EnvID,
		WorkflowID:    msg.WorkflowID,
		SubscriberID:  msg.SubscriberID,
		TransactionID: msg.TransactionID,
		RunID:         msg.RunID,
		CreatedAt:     msg.CreatedAt,
	}
}

// emitActivityRowPatch renders the single-row template and patches it onto the
// table head. Two patches, exactly like dagnats streams.go:emitRunPatch:
//
//  1. remove the row by id (no-op if the page never rendered it — the common
//     case for a brand-new notification arriving while the operator watches),
//  2. prepend the freshly-rendered row into the tbody.
//
// The result is "this row, on top, fresh content" whether or not the page had
// already loaded it, with no duplicate when a re-delivery re-emits the id.
func emitActivityRowPatch(
	sse *datastar.ServerSentEventGenerator,
	tmpl *template.Template,
	target liveStreamTarget,
	row activityRow,
) error {
	if sse == nil {
		panic("emitActivityRowPatch: sse is nil")
	}
	if tmpl == nil {
		panic("emitActivityRowPatch: tmpl is nil")
	}
	html, err := renderFragment(tmpl, target.rowTemplate, row)
	if err != nil {
		return fmt.Errorf("render %s: %w", target.rowTemplate, err)
	}
	// First: remove the row if it exists. A missing selector logs a Datastar
	// warning client-side but does not error; the prepend is what matters.
	rmOpts := []datastar.PatchElementOption{
		datastar.WithSelector("#" + target.rowIDPrefix + row.ID),
		datastar.WithMode(datastar.ElementPatchModeRemove),
	}
	_ = sse.PatchElements("", rmOpts...)
	// Then: prepend the fresh row to the tbody head.
	prependOpts := []datastar.PatchElementOption{
		datastar.WithSelector(target.tbodySelector),
		datastar.WithMode(datastar.ElementPatchModePrepend),
	}
	if err := sse.PatchElements(html, prependOpts...); err != nil {
		return fmt.Errorf("patch activity row: %w", err)
	}
	return nil
}
