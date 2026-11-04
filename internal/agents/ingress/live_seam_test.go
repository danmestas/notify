package ingress

import (
	"context"
	"testing"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
)

type runtimeFunc func(context.Context, agentdomain.Turn) (agentdomain.Reply, error)

func (f runtimeFunc) Dispatch(ctx context.Context, t agentdomain.Turn) (agentdomain.Reply, error) {
	return f(ctx, t)
}

type liveFunc func(context.Context, agentdomain.Turn, agentdomain.Runtime) (agentdomain.Reply, error)

func (f liveFunc) Dispatch(ctx context.Context, t agentdomain.Turn, rt agentdomain.Runtime) (agentdomain.Reply, error) {
	return f(ctx, t, rt)
}

// The turn dispatch seam hands the configured runtime to the live receivers as
// the fallback, and uses the runtime directly when none are wired.
func TestDispatchSeamConsultsLiveReceiversFirst(t *testing.T) {
	var ran string
	rt := runtimeFunc(func(context.Context, agentdomain.Turn) (agentdomain.Reply, error) {
		ran = "runtime"
		return agentdomain.Reply{}, nil
	})
	s := &Service{}
	if _, err := s.dispatch(context.Background(), rt, agentdomain.Turn{}); err != nil || ran != "runtime" {
		t.Fatalf("no live receivers: ran=%q err=%v", ran, err)
	}

	ran = ""
	s.WithLiveReceivers(liveFunc(func(ctx context.Context, tn agentdomain.Turn, fb agentdomain.Runtime) (agentdomain.Reply, error) {
		ran = "live"
		if fb == nil {
			t.Error("live receivers got no fallback")
		}
		return agentdomain.Reply{}, nil
	}))
	if _, err := s.dispatch(context.Background(), rt, agentdomain.Turn{}); err != nil || ran != "live" {
		t.Fatalf("with live receivers: ran=%q err=%v", ran, err)
	}
}
