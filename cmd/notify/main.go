// Command notify is the entrypoint for the notify platform binary. `notify
// serve` runs notify as a dagnats program: it registers with an
// already-running dagnats (NOTIFY_DAGNATS_URL), attaches over its own program
// connection and serves notify's HTTP on a listener of its own. notify never
// spawns dagnats itself.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danmestas/dagnats/program"

	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/observe"
	"github.com/danmestas/notify/internal/version"
	"github.com/danmestas/notify/notify"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("notify %s\n", version.Version)
		fmt.Fprintln(os.Stderr, "usage: notify <serve|bootstrap|rotate-key> [flags]")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		exitOn("serve", runServe(os.Args[2:]), 1)
	case "bootstrap":
		exitOn("bootstrap", runBootstrap(os.Args[2:]), 1)
	case "rotate-key":
		exitOn("rotate-key", runRotateKey(os.Args[2:]), 1)
	case "validate-steps":
		exitOn("validate-steps", runValidateSteps(os.Args[2:]), 1)
	case "inbound":
		err := runInbound(os.Args[2:], os.Stdin, os.Stdout)
		exitOn("inbound", err, exitCodeFor(err))
	case "version", "-v", "--version":
		fmt.Printf("notify %s\n", version.Version)
	default:
		fmt.Fprintf(os.Stderr, "notify: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

// exitOn reports err from a subcommand and exits with code; nil returns.
func exitOn(command string, err error, code int) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "notify %s: %v\n", command, err)
	os.Exit(code)
}

// runServe runs notify under program.Run until SIGTERM, or until its dagnats
// connection closes for good, which is an error so launchd restarts notify.
// Process-wide setup (the SSRF allowlist, tracing, the log handler) and the
// listener stay here; everything else is the notify package's.
func runServe(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	// Process-wide, like observe.Setup below, and before anything dials: an
	// unset value leaves the guard blocking every internal range.
	if err := httpaction.ConfigureAllowlist(cfg.ssrfAllowedHosts); err != nil {
		return err
	}

	// ctx is Start's ctx too: SIGTERM ends notify's handler contexts before
	// Running.Stop drains the worker.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A no-op when OTEL_EXPORTER_OTLP_ENDPOINT is unset.
	shutdownTracing, err := observe.Setup(ctx, cfg.otelEndpoint, cfg.otelServiceName, version.Version)
	if err != nil {
		return fmt.Errorf("observe.Setup: %w", err)
	}
	defer func() {
		// Detached from ctx: on SIGTERM it is already cancelled, and the batch
		// processor still needs a window to flush.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(fctx)
	}()
	installTraceLogging(cfg.otelEndpoint)

	prog := &notify.Program{Config: cfg.app}
	if cfg.meshNATSURL != "" {
		meshConn, err := eventbus.DialNATS(cfg.meshNATSURL, "notify-mesh", "mesh hub (-mesh-nats-url)")
		if err != nil {
			return err
		}
		defer meshConn.Close()
		prog.Mesh = meshConn
	}

	opts := programOptions(cfg)
	running, err := program.Run(ctx, prog, opts)
	if refused := (*program.ErrRefused)(nil); errors.As(err, &refused) {
		return fmt.Errorf("dagnats at %s refused to admit notify (%s): %s; nothing was started",
			opts.URL, refused.Code, refused.Detail)
	}
	if err != nil {
		return err
	}
	defer func() { _ = running.Stop(context.WithoutCancel(ctx)) }()

	srv := &http.Server{Addr: cfg.addr, Handler: prog.Handler()}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	defer func() { _ = srv.Close() }()
	fmt.Printf("notify %s serving on %s (dagnats=%s db=%s)\n",
		version.Version, cfg.addr, opts.URL, cfg.app.DBPath)

	select {
	case <-ctx.Done():
		return nil
	case err := <-running.Done():
		if err != nil {
			return fmt.Errorf("dagnats connection lost, exiting so the supervisor restarts notify: %w", err)
		}
		return nil
	case err := <-served:
		return fmt.Errorf("ListenAndServe: %w", err)
	}
}
