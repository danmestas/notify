package aibuilder

import (
	"context"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// IntegrationLister is the slice of the store the builder needs to gate channel
// steps. store.Store.Integrations() satisfies it, so callers inject the repo
// directly without this package importing the store (avoiding a cycle).
type IntegrationLister interface {
	List(ctx context.Context, scope domain.Scope) ([]domain.Integration, error)
}

// channelSteps maps the registry's channel step types to the integration
// channel that must be active for them to be offered. Action steps (delay,
// digest, throttle, http_request) are absent here: they require no provider and
// are always available. The channel name equals the step name (contracts §4).
var channelSteps = map[string]string{
	"in_app": "in_app",
	"email":  "email",
	"push":   "push",
	"chat":   "chat",
	"sms":    "sms",
	"stream": "stream",
}

// builtinChannels are channels notify delivers WITHOUT any external provider.
// The in-app inbox persists the message and publishes an inbox event directly
// (delivery.HandleInApp never looks up an integration), so — like the reference platform's built-in
// Inbox — in_app is always available regardless of configured integrations. The
// stream channel is likewise provider-less: it publishes to the tenant's durable
// feed directly (delivery.HandleStream never looks up an integration), so it is
// always available too. The external-provider channels (email/sms/chat/push)
// stay gated on an Active integration.
var builtinChannels = map[string]bool{
	"in_app": true,
	"stream": true,
}

// AvailableStepTypes computes the step types the model may add: every action
// step (no channel) plus every channel step whose channel has an Active
// integration. Order follows the registry's registration order so the addStep
// enum is stable. This is the z.enum guard the reference platform uses — an unconfigured channel
// can never appear in the tool schema, so the model cannot emit it.
func AvailableStepTypes(reg *steptype.Registry, integrations []domain.Integration) []string {
	active := make(map[string]bool, len(integrations))
	for _, in := range integrations {
		if in.Active {
			active[in.Channel] = true
		}
	}
	var out []string
	for _, name := range reg.Names() {
		if channel, isChannel := channelSteps[name]; isChannel {
			if builtinChannels[channel] || active[channel] {
				out = append(out, name)
			}
			continue
		}
		out = append(out, name) // action step: always available
	}
	return out
}
