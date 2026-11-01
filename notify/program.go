package notify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/danmestas/dagnats/program"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/notify/internal/eventbus"
)

// Program runs notify as a dagnats program: program.Run (cmd/notify) or any
// host that Attaches it. Start builds notify on the program connection and
// registers every task handler through Host.Handle, so the worker Attach runs
// claims notify's tasks and the engine runs none of its own. dagnats has
// already provisioned notify's streams at the Grant's sizes.
//
// Under Program notify serves only the notify.* task types its Manifest
// declares. The pre-prefix names the standalone engine still serves for N4's
// drain are not declarable, so a run started under them before N4 is not
// claimed here; deploy a program-mode notify only once none is in flight.
//
// Start's ctx bounds notify's background work and its handlers' contexts, as
// in standalone mode. Cancel it before stopping the program (cmd/notify does,
// on SIGTERM) so a waiting handler returns before Attach's worker drains,
// instead of holding the drain for the worker's timeout.
//
// Use a Program once: Stop closes the store, and a later Start is a no-op.
type Program struct {
	// Config is notify's configuration, as for New.
	Config Config
	// Mesh, when set, is a connection to a NATS that is not a dagnats, which
	// mesh agents are dispatched over. Nil means the program connection.
	Mesh *nats.Conn
	// BasePath is Host.BasePath: the prefix Handler serves notify under.
	BasePath string

	mu      sync.Mutex
	started bool
	running atomic.Pointer[Notify]
	grant   program.Grant
}

var (
	_ program.Program     = (*Program)(nil)
	_ program.HTTPHandler = (*Program)(nil)
)

// Manifest is notify's Manifest.
func (p *Program) Manifest() program.Manifest { return Manifest() }

// Start builds and starts notify on h.Conn. It is idempotent: a second call
// returns nil without doing anything.
func (p *Program) Start(ctx context.Context, h program.Host) error {
	if h.Conn == nil || h.Handle == nil {
		panic("notify.Program.Start: Host.Conn and Host.Handle must be set")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	if err := checkStreamGrant(h.Grant); err != nil {
		return err
	}
	eventbus.TrackStatus("notify", h.Conn)
	n, err := New(ctx, Host{
		NC: h.Conn, Mesh: p.Mesh, Logger: h.Logger, BasePath: p.BasePath, handle: h.Handle,
	}, p.Config)
	if err != nil {
		return err
	}
	if err := n.Start(ctx); err != nil {
		return errors.Join(err, n.Stop(context.WithoutCancel(ctx)))
	}
	p.started, p.grant = true, h.Grant
	p.running.Store(n)
	return nil
}

// checkStreamGrant refuses a Grant that sizes none of notify's streams, before
// anything touches NATS.
func checkStreamGrant(g program.Grant) error {
	for _, s := range Manifest().Streams {
		if g.StreamMaxBytes[s.Name] <= 0 {
			return fmt.Errorf("notify: dagnats granted no storage for stream %s", s.Name)
		}
	}
	return nil
}

// Stop drains notify and releases what Start opened. It makes no dagnats
// control-plane request. Safe to call without Start, and more than once.
func (p *Program) Stop(ctx context.Context) error {
	n := p.running.Swap(nil)
	if n == nil {
		return nil
	}
	return n.Stop(ctx)
}

// Handler serves notify's routes once Start has run, and 503 before it or
// after Stop. Serve it on a listener of the host's own, never on dagnats'.
func (p *Program) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := p.notify()
		if n == nil {
			http.Error(w, "notify is not running", http.StatusServiceUnavailable)
			return
		}
		n.Handler().ServeHTTP(w, r)
	})
}

// notify is the running server, or nil.
func (p *Program) notify() *Notify { return p.running.Load() }
