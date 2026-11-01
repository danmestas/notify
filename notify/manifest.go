package notify

import (
	"github.com/danmestas/dagnats/program"

	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/version"
)

// dagnatsRange is the dagnats release range notify is built and tested against.
const dagnatsRange = ">=0.0.44 <0.1.0"

// Manifest is what dagnats needs to admit notify as a program. Its task types
// and workflows come from internal/names, the same constants the wiring
// registers handlers under, so the two cannot drift.
//
// Authored (per-tenant) and per-agent workflows are named
// "notify.<env>-<trigger>" and "notify.agent-turn-<id>"; they are created at
// runtime and so are not listed.
//
// Streams and Buckets are the ones the event bus creates itself in standalone
// mode; dagnats provisions them at registration, since a program's NATS user
// may not create either. Protocols are the mesh subjects, which sit under
// agents. and a2a., not notify.: mesh traffic to a dagnats rides the program
// connection. Since dagnats v0.0.43 notify logs in as its own NKey user with
// only the permissions this manifest derives (program.PermissionsFor), so
// anything notify touches must be declared here.
func Manifest() program.Manifest {
	return program.Manifest{
		Name:      "notify",
		Version:   version.Version,
		Dagnats:   dagnatsRange,
		TaskTypes: names.TaskTypes(),
		Workflows: names.Workflows(),
		Streams:   eventbus.Streams(),
		Buckets:   eventbus.Buckets(),
		Protocols: mesh.Protocols(),
	}
}
