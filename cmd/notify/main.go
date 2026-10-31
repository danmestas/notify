// Command notify is the entrypoint for the notify platform binary. It connects
// to an already-running dagnats sidecar (start it first with `.bin/dagnats
// serve`); notify never spawns the sidecar itself.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify serve: %v\n", err)
			os.Exit(1)
		}
	case "bootstrap":
		if err := runBootstrap(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify bootstrap: %v\n", err)
			os.Exit(1)
		}
	case "rotate-key":
		if err := runRotateKey(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify rotate-key: %v\n", err)
			os.Exit(1)
		}
	case "validate-steps":
		if err := runValidateSteps(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "notify validate-steps: %v\n", err)
			os.Exit(1)
		}
	case "inbound":
		if err := runInbound(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "notify inbound: %v\n", err)
			os.Exit(exitCodeFor(err))
		}
	case "version", "-v", "--version":
		fmt.Printf("notify %s\n", version.Version)
	default:
		fmt.Fprintf(os.Stderr, "notify: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

// installTraceLogging stamps trace_id/span_id onto every record logged inside a
// span — but ONLY when tracing is enabled. It reports whether it installed, so
// the gate itself is testable (see trace_logging_test.go); silently losing this
// gate would regress every disabled operator's log format.
//
// Doing this unconditionally would switch notify's log output from the log
// package's default format to slog text format — and reroute stdlib log through
// slog — for every operator, including those who never opted into OTel. Gating
// on the endpoint keeps the disabled path byte-identical to pre-OTel notify.
//
// When enabled, the format change is an accepted deviation: correlating logs
// with traces requires the structured handler, and configuring an OTLP endpoint
// is the opt-in.
//
// The wrapped handler is an explicit TextHandler, NOT slog.Default()'s. Until
// the first SetDefault, slog's default handler emits through the log package,
// and SetDefault points log's output back at the slog default — so wrapping it
// would route log -> wrapper -> inner default -> log and recurse forever,
// hanging the process before it ever listens.
func installTraceLogging(otelEndpoint string) bool {
	if otelEndpoint == "" {
		return false
	}
	slog.SetDefault(slog.New(observe.NewLogHandler(slog.NewTextHandler(os.Stderr, nil))))
	return true
}

// runServe dials dagnats, builds the notify server on that connection and
// serves its HTTP API until interrupted. Process-wide setup (the SSRF
// allowlist, tracing, the log handler) and the listener stay here; everything
// else is the notify package's.
func runServe(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}

	// Installs the outbound SSRF allowlist process-wide, like observe.Setup
	// below. It must happen before anything dials: an unset value leaves the
	// guard blocking every internal range, which is the default posture.
	if err := httpaction.ConfigureAllowlist(cfg.ssrfAllowedHosts); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tracing is a complete no-op when OTEL_EXPORTER_OTLP_ENDPOINT is unset:
	// nothing is constructed and the otel globals stay untouched. runServe is
	// the only Setup call site — it mutates process-wide state.
	shutdownTracing, err := observe.Setup(ctx, cfg.otelEndpoint, cfg.otelServiceName, version.Version)
	if err != nil {
		return fmt.Errorf("observe.Setup: %w", err)
	}
	defer func() {
		// Bounded, and detached from ctx: on SIGTERM ctx is already cancelled,
		// and the batch processor still needs a window to flush.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(fctx)
	}()

	installTraceLogging(cfg.otelEndpoint)

	// The connections are this process's: notify uses them and Stop leaves
	// them open, so they close here, after notify has drained.
	nc, err := eventbus.DialNATS(cfg.natsURL, "notify", "dagnats sidecar (-nats-url)")
	if err != nil {
		return err
	}
	defer nc.Close()
	host := notify.Host{NC: nc, Logger: slog.Default()}
	if cfg.meshNATSURL != "" {
		meshConn, err := eventbus.DialNATS(cfg.meshNATSURL, "notify-mesh", "mesh hub (-mesh-nats-url)")
		if err != nil {
			return err
		}
		defer meshConn.Close()
		host.Mesh = meshConn
	}

	n, err := notify.New(ctx, host, cfg.app)
	if err != nil {
		return err
	}
	// Detached from ctx so the drain still runs after SIGTERM cancelled it.
	defer func() { _ = n.Stop(context.WithoutCancel(ctx)) }()
	if err := n.Start(ctx); err != nil {
		return err
	}

	srv := &http.Server{Addr: cfg.addr, Handler: n.Handler()}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("notify %s serving on %s (nats=%s db=%s)\n",
		version.Version, cfg.addr, cfg.natsURL, cfg.app.DBPath)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("ListenAndServe: %w", err)
	}
	return nil
}
