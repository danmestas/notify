package notify

import (
	"github.com/danmestas/dagnats/program"

	"github.com/danmestas/notify/internal/names"
	"github.com/danmestas/notify/internal/version"
)

// dagnatsRange is the dagnats release range notify is built and tested against.
const dagnatsRange = ">=0.0.42 <0.1.0"

// Manifest is what dagnats needs to admit notify as a program. Its task types
// and workflows come from internal/names, the same constants the wiring
// registers handlers under, so the two cannot drift.
//
// Authored (per-tenant) and per-agent workflows are named
// "notify.<env>-<trigger>" and "notify.agent-turn-<id>"; they are created at
// runtime and so are not listed.
func Manifest() program.Manifest {
	return program.Manifest{
		Name:      "notify",
		Version:   version.Version,
		Dagnats:   dagnatsRange,
		TaskTypes: names.TaskTypes(),
		Workflows: names.Workflows(),
	}
}
