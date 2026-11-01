package notify

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	_ "modernc.org/sqlite"

	"github.com/danmestas/dagnats/program"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/names"
)

func TestManifest_Validates(t *testing.T) {
	if err := program.Validate(Manifest()); err != nil {
		t.Fatalf("program.Validate(Manifest()): %v", err)
	}
}

type noAI struct{}

func (noAI) Message(context.Context, aibuilder.MessageRequest) (aibuilder.MessageResponse, error) {
	return aibuilder.MessageResponse{}, nil
}

// TestManifest_MatchesRegisteredHandlers boots the real server wiring and
// compares what the engine handles with the manifest: every manifest task type
// has a handler, every handler is a manifest task type or its pre-prefix
// spelling (kept for one release), and nothing else.
func TestManifest_MatchesRegisteredHandlers(t *testing.T) {
	nc, err := nats.Connect(dagtest.StartSidecar(t))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	n, err := New(context.Background(), Host{NC: nc}, Config{
		DBPath:    filepath.Join(t.TempDir(), "notify.db"),
		MasterKey: make([]byte, 32),
		AI:        noAI{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Stop(context.Background()) })

	var current, legacy []string
	for _, tt := range n.eng.TaskTypes() {
		if strings.HasPrefix(tt, names.Prefix) {
			current = append(current, tt)
		} else {
			legacy = append(legacy, tt)
		}
	}
	want := slices.Clone(Manifest().TaskTypes)
	slices.Sort(want)
	if !slices.Equal(current, want) {
		t.Errorf("registered notify.* task types differ from Manifest().TaskTypes\n got: %v\nwant: %v", current, want)
	}
	// Remove in the release after N4, with the legacy registrations.
	wantLegacy := make([]string, 0, len(want))
	for _, tt := range want {
		wantLegacy = append(wantLegacy, names.Legacy(tt))
	}
	slices.Sort(wantLegacy)
	if !slices.Equal(legacy, wantLegacy) {
		t.Errorf("pre-prefix handlers differ from the old spelling of the manifest\n got: %v\nwant: %v", legacy, wantLegacy)
	}
}
