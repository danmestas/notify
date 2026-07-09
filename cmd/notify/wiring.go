package main

import (
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/preferences"
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
func registerDeliveryHandlers(eng *engine.Engine, st *store.Store, conn *connectivity.Service, prefs *preferences.Service, bus *eventbus.Bus) {
	d := deliveryDeps{prefs: prefs, bus: bus}
	eng.Handle("send.in_app", wireCommon(delivery.New(st), d).HandleInApp)
	eng.Handle("send.email", wireCommon(delivery.NewEmail(st, conn), d).HandleEmail)
	eng.Handle("send.push", wireCommon(delivery.NewPush(st, conn), d).HandlePush)
	eng.Handle("send.chat", wireCommon(delivery.NewChat(st, conn), d).HandleChat)
	eng.Handle("send.sms", wireCommon(delivery.NewSMS(st, conn), d).HandleSMS)
	// send.stream carries a 5th dependency (the durable-feed publisher) none
	// of the other channels need, so it goes through wireCommon for the 4
	// shared ones and then .WithFeed(bus) on top — *eventbus.Bus satisfies
	// delivery.FeedPublisher alongside EventPublisher/Throttler/Digester.
	eng.Handle("send.stream", wireCommon(delivery.NewStream(st), d).WithFeed(bus).HandleStream)
}
