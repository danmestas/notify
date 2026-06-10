// Command notify is the entrypoint for the notify platform binary. It connects
// to an already-running dagnats sidecar (start it first with `.bin/dagnats
// serve`); notify never spawns the sidecar itself.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
	"github.com/danmestas/notify/internal/webhooksout"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("notify %s\n", version.Version)
		fmt.Fprintln(os.Stderr, "usage: notify <serve|bootstrap> [flags]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify serve: %v\n", err)
			os.Exit(1)
		}
	case "bootstrap":
		if err := runBootstrap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify bootstrap: %v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Printf("notify %s\n", version.Version)
	default:
		fmt.Fprintf(os.Stderr, "notify: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

// envOr returns the environment variable value if set, otherwise def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// masterKey resolves the 32-byte AES-256-GCM master key. A non-empty hex flag
// is decoded and length-checked; an empty flag derives a fixed dev key so a
// local operator can boot without ceremony.
func masterKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		k := sha256.Sum256([]byte("notify-dev-master-key"))
		return k[:], nil
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decode master-key-hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("master-key-hex must decode to 32 bytes, got %d", len(raw))
	}
	return raw, nil
}

// runServe wires the full stack and serves the HTTP API until interrupted.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	natsURL := fs.String("nats-url", envOr("NOTIFY_NATS_URL", "nats://127.0.0.1:4222"), "dagnats NATS client URL")
	bridgeURL := fs.String("bridge-url", envOr("NOTIFY_BRIDGE_URL", "http://127.0.0.1:8080"), "dagnats worker bridge URL")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	addr := fs.String("addr", envOr("NOTIFY_ADDR", "127.0.0.1:3000"), "HTTP listen address")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (empty derives a dev key)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mk, err := masterKey(*masterKeyHex)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	eng, err := engine.New(*natsURL, *bridgeURL)
	if err != nil {
		return fmt.Errorf("engine.New: %w", err)
	}
	defer func() { _ = eng.Stop() }()

	bus, err := eventbus.Connect(ctx, *natsURL)
	if err != nil {
		return fmt.Errorf("eventbus.Connect: %w", err)
	}
	defer func() { _ = bus.Close() }()

	prefs := preferences.New(st)
	eng.Handle("send.in_app", delivery.New(st).WithPreferences(prefs).WithEventBus(bus).WithThrottle(bus).WithDigester(bus).HandleInApp)
	conn := connectivity.New(st, mk)
	eng.Handle("send.email", delivery.NewEmail(st, conn).WithPreferences(prefs).WithThrottle(bus).WithDigester(bus).HandleEmail)
	eng.StartWorkers(ctx)

	whStop, err := webhooksout.New(bus, st).Start(ctx)
	if err != nil {
		return fmt.Errorf("webhooksout.Start: %w", err)
	}
	defer func() { _ = whStop() }()

	id := identity.New(st, mk)
	aud := audience.New(st)
	_ = authoring.New(st, eng)
	trg := trigger.New(aud, st, eng).WithDigester(bus)
	inbox := inboxapi.New(st)
	rt := realtime.New(bus)
	inb := inbound.New(trg)

	an, anClose, err := analyticsBackend(ctx, st, bus)
	if err != nil {
		return fmt.Errorf("analyticsBackend: %w", err)
	}
	defer func() { _ = anClose() }()
	anStop, err := an.Start(ctx)
	if err != nil {
		return fmt.Errorf("analytics.Start: %w", err)
	}
	defer func() { _ = anStop() }()

	handler := httpapi.New(id, aud, trg, inbox, rt, inb, an)
	srv := &http.Server{Addr: *addr, Handler: handler}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("notify %s serving on %s (nats=%s bridge=%s db=%s)\n",
		version.Version, *addr, *natsURL, *bridgeURL, *dbPath)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("ListenAndServe: %w", err)
	}
	return nil
}

// runBootstrap provisions a tenant (org + env + API key) and prints the key and
// scope so an operator can begin triggering.
func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	org := fs.String("org", "", "organization name")
	env := fs.String("env", "", "environment identifier")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (empty derives a dev key)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}

	mk, err := masterKey(*masterKeyHex)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	key, scope, err := identity.New(st, mk).Bootstrap(ctx, *org, *env)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	out, _ := json.MarshalIndent(struct {
		APIKey string `json:"apiKey"`
		OrgID  string `json:"orgId"`
		EnvID  string `json:"envId"`
	}{key, scope.OrgID, scope.EnvID}, "", "  ")
	fmt.Println(string(out))
	return nil
}
