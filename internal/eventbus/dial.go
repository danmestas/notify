package eventbus

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/metrics"
)

// DialNATS opens a resilient NATS connection to the dagnats sidecar and is the
// single place notify's reconnect policy lives. All three consumers (eventbus,
// engine, mesh) dial through it so the options below are never triplicated.
//
// The invariant it enforces: a connection survives an arbitrarily long dagnats
// outage. nats.go's defaults cap reconnects at 60 attempts × ~2s (~2 minutes),
// after which the client gives up and closes the connection permanently — the
// zombie-process failure mode where notify's HTTP surface stays up but its
// poll loop is wired to a dead connection that never comes back. MaxReconnects(-1)
// removes that cap; the jittered wait spreads reconnect storms when many
// clients rediscover the sidecar at once.
//
// name is set as the connection name so dagnats-side and notify-side logs can
// attribute a connection to its owner ("notify-eventbus", "notify-engine",
// "notify-mesh").
func DialNATS(natsURL, name string) (*nats.Conn, error) {
	nc, err := nats.Connect(
		natsURL,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.ReconnectJitter(500*time.Millisecond, 2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			metrics.NATSDisconnect()
			slog.Default().Warn("eventbus: NATS disconnected",
				"conn", name, "err", err)
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			metrics.NATSReconnect()
			slog.Default().Info("eventbus: NATS reconnected",
				"conn", name, "url", c.ConnectedUrl())
		}),
		nats.ClosedHandler(func(c *nats.Conn) {
			// A close with no LastError is a graceful nc.Close() (shutdown);
			// with MaxReconnects(-1) that is the only ordinary way to reach
			// here, so log it at Info. A non-nil LastError means the client
			// gave up on a real fault — keep that at Error.
			if err := c.LastError(); err != nil {
				slog.Default().Error("eventbus: NATS connection closed",
					"conn", name, "err", err)
				return
			}
			slog.Default().Info("eventbus: NATS connection closed", "conn", name)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("eventbus: dial %q (%s): %w", natsURL, name, err)
	}
	// Publish a live status gauge for this connection so /metrics reflects the
	// current link state (the zombie-mode symptom this dialer guards against)
	// rather than a value cached at dial time. Keyed by name, so a re-dial under
	// the same owner replaces the entry instead of leaking one.
	metrics.SetNATSStatus(name, func() string { return nc.Status().String() })
	return nc, nil
}
