package notify

import (
	"time"

	"github.com/danmestas/notify/internal/agents/agentstep"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/steptype"
	"github.com/danmestas/notify/internal/store"
)

// deliveryDeps bundles the dependencies wireCommon attaches to every channel
// delivery handler. bus satisfies delivery.EventPublisher, delivery.Throttler,
// and delivery.Digester simultaneously (see internal/eventbus.Bus), so one
// field covers all three of the four shared .With* calls.
type deliveryDeps struct {
	prefs delivery.PreferenceChecker
	bus   *eventbus.Bus
}

// common is satisfied by every channel delivery handler (*delivery.Handler,
// *delivery.EmailHandler, *delivery.PushHandler, *delivery.ChatHandler,
// *delivery.SMSHandler): each With* method returns the concrete handler type
// for chaining, which is exactly what the generic constraint below requires.
type common[T any] interface {
	WithPreferences(delivery.PreferenceChecker) T
	WithEventBus(delivery.EventPublisher) T
	WithThrottle(delivery.Throttler) T
	WithDigester(delivery.Digester) T
}

// wireCommon attaches every shared dependency in d to h and returns it. This
// replaces the hand-repeated
// .WithPreferences(...).WithEventBus(...).WithThrottle(...).WithDigester(...)
// chain that used to appear once per channel in registerDeliveryHandlers —
// and could silently drop one of the four calls, as send.email's registration
// once did with .WithEventBus (notify hardening #8): with wireCommon, a
// channel handler that satisfies common[T] cannot be wired without all four.
func wireCommon[T common[T]](h T, d deliveryDeps) T {
	return h.WithPreferences(d.prefs).WithEventBus(d.bus).WithThrottle(d.bus).WithDigester(d.bus)
}

// registerDeliveryHandlers registers the 6 channel send handlers
// (in_app/email/push/chat/sms/stream) with eng, each wired through
// wireCommon so every channel gets the identical
// preferences/event-bus/throttle/digest dependencies — including send.email,
// which previously never received .WithEventBus and so never published
// delivery-status events (the analytics service's live counts are driven
// entirely off those events via bus.SubscribeAllDelivery, so email sends
// were silently invisible to /v1/analytics/deliveries and
// /v1/analytics/timeseries).
func registerDeliveryHandlers(eng taskRegistrar, st *store.Store, conn *connectivity.Service, prefs *preferences.Service, bus *eventbus.Bus, armer delivery.ReplyArmer) {
	d := deliveryDeps{prefs: prefs, bus: bus}
	eng.Handle("send.in_app", wireCommon(delivery.New(st), d).HandleInApp)
	// send.email carries one dependency the other channels do not: the reply
	// loop, so an email step can opt into reply-to-notification.
	eng.Handle("send.email", wireCommon(delivery.NewEmail(st, conn), d).WithReplyArmer(armer).HandleEmail)
	eng.Handle("send.push", wireCommon(delivery.NewPush(st, conn), d).HandlePush)
	eng.Handle("send.chat", wireCommon(delivery.NewChat(st, conn), d).HandleChat)
	eng.Handle("send.sms", wireCommon(delivery.NewSMS(st, conn), d).HandleSMS)
	// send.stream carries a 5th dependency (the durable-feed publisher) none
	// of the other channels need, so it goes through wireCommon for the 4
	// shared ones and then .WithFeed(bus) on top — *eventbus.Bus satisfies
	// delivery.FeedPublisher alongside EventPublisher/Throttler/Digester.
	eng.Handle("send.stream", wireCommon(delivery.NewStream(st), d).WithFeed(bus).HandleStream)
}

// runTimeMargin is added to a task type's own worst case to make its max run
// time: room for the store reads and writes around the bounded work.
const runTimeMargin = 5 * time.Minute

// taskRegistrar is the slice of *engine.Engine the task wiring needs. Narrowing
// it to Handle lets the coverage test observe the REAL registration code path
// with a recorder, instead of asserting against a second hand-maintained list
// that could drift from the first -- which is the very failure this guards.
type taskRegistrar interface {
	Handle(taskType string, fn engine.HandlerFunc, opts ...engine.HandleOption)
}

// stepTaskDeps carries what the step-task handlers need to be constructed.
type stepTaskDeps struct {
	store   *store.Store
	conn    *connectivity.Service
	prefs   *preferences.Service
	bus     *eventbus.Bus
	secrets *secretbox.Box
	// replyArmer is the email loop, for steps that opt into
	// reply-to-notification (#345). nil is allowed (tests, or a deployment
	// with no email loop): the step's opt-in is then logged and ignored.
	replyArmer delivery.ReplyArmer
	// composer posts agent_message steps into agent conversations: the
	// ingress service, the same path the inbox and reply loops use.
	composer agentstep.Composer
	// drafts records review-mode draft requests for agent_message steps.
	drafts agentstep.DraftRequester
}

// registerStepTaskHandlers registers a handler for every engine task type a
// step type can compile to.
//
// It is the single place that wiring happens, so steptype.Registry.TaskTypes()
// and this function can be compared mechanically (see the coverage test).
// Previously the send.* handlers lived here and the rest were registered inline
// during boot, each with a comment asking the reader to remember the invariant.
func registerStepTaskHandlers(eng taskRegistrar, d stepTaskDeps) {
	registerDeliveryHandlers(eng, d.store, d.conn, d.prefs, d.bus, d.replyArmer)

	// Scheduled (cron) digest window: sleeps until the next tick, then the
	// downstream channel step self-sources and drains the accumulated events.
	// Regular (amount+unit) digests use the engine's native sleep -- no handler.
	eng.Handle("digest.wait", delivery.NewDigestWait(d.store).HandleDigestWait)

	// Scheduled (calendar) and dynamic (payload-sourced) delays: the wait
	// depends on wall-clock time or the trigger payload, so it cannot be baked
	// into a static sleep StepDef. The handler computes it at dispatch and
	// blocks context-aware; on shutdown it returns a RetryableError so dagnats
	// requeues and the wait resumes after restart. Regular delays use the
	// engine's native sleep.
	dw := delivery.NewDelayWait(d.store)
	eng.Handle("delay.scheduled", dw.HandleScheduled)
	eng.Handle("delay.dynamic", dw.HandleDynamic)

	// check.throttle: gate a run by a fixed-window counter.
	eng.Handle(delivery.TaskThrottle, delivery.NewThrottleHandler(d.store, d.bus).Handle)

	// http_request: outbound HTTP action step. Makes the interpolated call, then
	// captures statusCode/body/headers into step_outputs for downstream steps.
	eng.Handle("http_request", httpaction.New(d.store, httpaction.WithSecretBox(d.secrets)).Handle,
		engine.WithMaxRunTime(steptype.HTTPMaxTimeoutSec*time.Second+runTimeMargin))

	// agent_message: post the rendered text to an agent conversation and
	// start the agent's turn, in process (#4).
	eng.Handle("agent_message", agentstep.New(d.store, d.composer, d.drafts).Handle)
}
