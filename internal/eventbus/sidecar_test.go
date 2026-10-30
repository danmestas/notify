package eventbus

import (
	"testing"

	"github.com/danmestas/notify/internal/dagtest"
)

// startSidecar runs an in-process dagnats server and returns its NATS URL.
func startSidecar(t *testing.T) (natsURL string) {
	t.Helper()
	natsURL, _ = dagtest.StartSidecar(t)
	return natsURL
}
