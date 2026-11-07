package dagtest

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// StartBareNATS runs a plain NATS server with no dagnats on it and returns
// its client URL: requests to dagnats' routes get no responders, which is
// what a dagnats release that lacks a route looks like. The server is
// stopped when the test ends.
func StartBareNATS(t *testing.T) string {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("dagtest: bare nats server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		s.Shutdown()
		t.Fatal("dagtest: bare nats server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s.ClientURL()
}
