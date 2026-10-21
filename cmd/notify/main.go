// Command notify is the entrypoint for the notify platform binary. It connects
// to an already-running dagnats sidecar (start it first with `.bin/dagnats
// serve`); notify never spawns the sidecar itself.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/managed"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/chatloop"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/console"
	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/credverify"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/emailloop"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/feedingest"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/metrics"
	"github.com/danmestas/notify/internal/observe"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/replyhook"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/smtpin"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/version"
	"github.com/danmestas/notify/internal/webhooksout"
)

// consoleComposer adapts *ingress.Service to console.ConversationComposer:
// the console's send-message form needs only the conversation thread handle
// back, so this flattens ingress.ComposeResult to its ConversationID.
type consoleComposer struct{ ing *ingress.Service }

func (c consoleComposer) ComposeMessage(
	ctx context.Context, scope domain.Scope,
	agentIdentifier, conversationID, subscriberID, text string,
) (string, error) {
	res, err := c.ing.ComposeMessage(ctx, scope, agentIdentifier, conversationID, subscriberID, text)
	if err != nil {
		return "", err
	}
	return res.ConversationID, nil
}

// chatStatus adapts *chatloop.Service to console.ChatStatusProvider, pinning
// the chat default scope (NOTIFY_CHAT_DEFAULT_ORG/ENV) the loop resolves its
// default agent in — the console operator's request scope is irrelevant to the
// env-driven chat config (#106).
type chatStatus struct {
	chat  *chatloop.Service
	scope domain.Scope
}

func (c chatStatus) Status(ctx context.Context, _ domain.Scope) chatloop.Snapshot {
	return c.chat.Status(ctx, c.scope)
}

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

// runServe wires the full stack and serves the HTTP API until interrupted.
func runServe(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	mk := cfg.masterKey

	// Installs the outbound SSRF allowlist process-wide, like observe.Setup
	// below. It must happen before anything dials: an unset value leaves the
	// guard blocking every internal range, which is the default posture.
	if err := httpaction.ConfigureAllowlist(cfg.ssrfAllowedHosts); err != nil {
		return err
	}

	// One secret box for at-rest encryption of declared secret control fields
	// (http_request headers). Shared by authoring (seal on write) and the
	// http_request handler (open at run time).
	secrets, err := secretbox.New(mk)
	if err != nil {
		return fmt.Errorf("secretbox.New: %w", err)
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

	st, err := store.Open(ctx, cfg.dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	eng, err := engine.New(cfg.natsURL, cfg.bridgeURL, engine.WithBridgeToken(cfg.bridgeToken))
	if err != nil {
		return fmt.Errorf("engine.New: %w", err)
	}
	defer func() { _ = eng.Stop() }()

	// Boot-time dagnats bridge reachability probe (notify hardening #6):
	// engine.New only validates the NATS control-plane connection; the HTTP
	// worker bridge is otherwise first touched lazily on the first poll (see
	// engine.go's poll), so a misconfigured -bridge-url/NOTIFY_BRIDGE_URL
	// would silently boot and then have every send.* task fail forever. A
	// transport-level failure (nothing listening) is fatal here; see
	// probeBridgeReachable's doc comment for why a reachable-but-not-2xx
	// bridge is logged and swallowed instead.
	bridgeClient := &http.Client{Timeout: bridgeProbeTimeout}
	if err := probeBridgeReachable(ctx, bridgeClient, cfg.bridgeURL); err != nil {
		return fmt.Errorf("probeBridgeReachable: %w", err)
	}

	// Reachability is not permission. dagnats serves /ready outside its auth
	// middleware, so a bridge that refuses every one of our task polls still
	// probes healthy -- which is exactly how #325 stayed invisible. Verify the
	// credentials before serving, and refuse to boot rather than accept
	// triggers we can never execute.
	if cfg.bridgeToken == "" {
		slog.Default().Info("dagnats bridge auth disabled: DAGNATS_BRIDGE_TOKEN unset")
	} else {
		slog.Default().Info("dagnats bridge auth enabled", "url", cfg.bridgeURL)
	}
	if err := probeBridgeAuth(ctx, bridgeClient, cfg.bridgeURL, cfg.bridgeToken); err != nil {
		return fmt.Errorf("probeBridgeAuth: %w", err)
	}

	bus, err := eventbus.Connect(ctx, cfg.natsURL)
	if err != nil {
		return fmt.Errorf("eventbus.Connect: %w", err)
	}
	defer func() { _ = bus.Close() }()

	prefs := preferences.New(st)
	conn := connectivity.New(st, mk,
		connectivity.WithSendmailCommand(cfg.sendmailCommand),
		connectivity.WithPublicBaseURL(cfg.publicBaseURL))
	// step_outputs grows without bound otherwise: every http_request action
	// writes a row holding the captured response body and nothing removed them.
	startStepOutputPurge(ctx, st)
	// delay_deadlines grows without bound otherwise once a deployment stops
	// creating new relative dynamic delays: Save no longer sweeps, so nothing
	// else would ever reclaim an abandoned deadline row.
	startDelayDeadlinePurge(ctx, st)

	// W4 ADR-013 (#55): durable credential-verify action. The verify task runs
	// in this worker pool (registered before StartWorkers); the workflow is
	// declared with the engine just after the workers start (below).
	credVerify := credverify.New(eng, conn)
	eng.Handle(credverify.TaskVerify, credVerify.Handler)

	meshHub := cfg.meshNATSURL
	if meshHub == "" {
		meshHub = cfg.natsURL
	}
	meshClient, err := mesh.Dial(meshHub, mesh.WithAllowedSubjects(cfg.meshAllow))
	if err != nil {
		return fmt.Errorf("mesh.Dial: %w", err)
	}
	defer meshClient.Close()
	ags := agentstore.New(st)
	// agent_drafts would otherwise keep every draft no one fetched.
	startDraftPurge(ctx, ags)
	ing := ingress.New(ags, st, eng, bus).
		WithRuntime(agentdomain.RuntimeMesh, meshClient).
		// Dev mode lets managed agents reach MCP servers on loopback over http.
		WithRuntime(agentdomain.RuntimeManaged, managed.New(secrets, managed.DefaultClient).WithMCP(cfg.dev))

	// Email two-way loop: inbound email replies route back to the agent and the
	// agent's reply mails back out. The responder defaults to the conversation's
	// agent (ing.ComposeMessage); conn resolves the outbound provider. The reply
	// hook closes the loop. NOTIFY_INBOUND_SECRET authenticates the mail
	// forwarder's POST to /v1/inbound/email; an empty secret disables inbound.
	emlCompose := func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error) {
		res, err := ing.ComposeMessage(ctx, scope, agentIdentifier, conversationID, subscriberID, text)
		if err != nil {
			return "", err
		}
		return res.ConversationID, nil
	}
	blobs, err := openBlobStore(cfg)
	if err != nil {
		return err
	}
	eml := emailloop.New(
		st, ags, emailloop.NewMinter(mk), conn,
		emailloop.NewAgentResponder(emlCompose),
		cfg.inboundSecret, slog.Default(),
	).WithEventPublisher(bus). // webhook-kind replies ride the delivery stream (#345)
					WithBlobStore(blobs)

	// All step-task handlers register in one place so the set can be compared
	// mechanically against steptype.Registry.TaskTypes() -- see the coverage
	// test. Registered here, after the email loop exists, because send.email
	// takes it as its reply armer; nothing between the old site and this one
	// dispatches a task, and StartWorkers is later still.
	registerStepTaskHandlers(eng, stepTaskDeps{
		store: st, conn: conn, prefs: prefs, bus: bus, secrets: secrets, replyArmer: eml,
		composer: ing, drafts: ags.Drafts(),
	})

	// Chat two-way loop: inbound Telegram (getUpdates long-poll) and Discord
	// (gateway WS) messages route to the agent and the agent's reply is sent
	// back into the originating chat. Inbound dials OUT (no public tunnel). The
	// compose primitive mirrors email's, threading a returned conversation id.
	// Cold inbound (a chat with no binding) routes to NOTIFY_CHAT_DEFAULT_AGENT
	// in scope (NOTIFY_CHAT_DEFAULT_ORG, NOTIFY_CHAT_DEFAULT_ENV); an empty bot
	// token disables that platform's poller, and an empty default agent drops
	// cold inbound.
	chatCompose := func(ctx context.Context, scope domain.Scope, agentIdentifier, conversationID, subscriberID, text string) (string, error) {
		res, err := ing.ComposeMessage(ctx, scope, agentIdentifier, conversationID, subscriberID, text)
		if err != nil {
			return "", err
		}
		return res.ConversationID, nil
	}
	chat := chatloop.New(
		st,
		chatloop.NewAgentResponder(chatCompose),
		cfg.telegramBotToken,
		cfg.discordBotToken,
		cfg.chatDefaultAgent,
		slog.Default(),
	)
	// NOTIFY_DISCORD_RESPOND_ALL=1/true makes the Discord gateway reply to every
	// message in a guild channel (for a dedicated single-purpose channel).
	// Default (unset) requires the bot to be @-mentioned in guild channels; DMs
	// always route. See chatloop.Service.discordRespondAll.
	if cfg.discordRespondAll {
		chat.SetDiscordRespondAll(true)
	}
	chatScope := domain.Scope{
		OrgID: cfg.chatDefaultOrg,
		EnvID: cfg.chatDefaultEnv,
	}
	// Fediverse inbound (mastodon/bluesky mention polling, #296) reads its
	// credentials from the integrations table, not env vars -- unlike Telegram
	// and Discord above. conn.DecryptedCredentials resolves the active
	// integration for that specific provider id in chatScope; chatloop.Start
	// disables the corresponding poller when it comes back empty.
	chat.SetCredentialSource(func(ctx context.Context, providerID string) (map[string]string, error) {
		creds, err := conn.DecryptedCredentials(ctx, chatScope, providerID)
		if errors.Is(err, store.ErrNotFound) {
			// No active integration for this provider: not a failure, just
			// "unconfigured" -- chatloop.CredentialSource's nil/nil sentinel.
			return nil, nil
		}
		return creds, err
	})

	// ingress has a single reply-hook slot but both two-way loops need to act on
	// an agent reply, so compose them: each loop's hook no-ops when the
	// conversation is not its binding, and a panic in one is recovered so it
	// never fails the turn or blocks the other.
	ing = ing.WithReplyHook(replyhook.Combine(slog.Default(), eml.ReplyHook, chat.ReplyHook))
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)
	eng.Handle(ingress.TaskManaged, ing.HandleTurn)

	// B3: durable AI workflow-builder loop, hosted as a dagnats agent-loop
	// handler alongside the synchronous console path (console.Builder.Run).
	// Provider selection (in priority order):
	//   NOTIFY_AI_API_KEY → OpenRouter (default) or any OpenAI-compatible endpoint
	//     NOTIFY_AI_BASE_URL overrides the base URL (default: https://openrouter.ai/api/v1)
	//     NOTIFY_AI_MODEL   overrides the model     (default: openai/gpt-4.1)
	//   ANTHROPIC_API_KEY  → Anthropic directly (fallback)
	//   Neither            → builder gracefully disabled (Guided tab shows a note)
	// The handler must be registered before StartWorkers; the workflow is declared
	// after (below), like credverify.
	var aiLoop *aibuilder.AgentLoopBuilder
	if aiClient := aibuilder.NewClientFromEnv(os.Getenv); aiClient != nil {
		aiLoop = aibuilder.NewAgentLoopBuilder(
			eng,
			aiClient,
			st.Integrations(),
			st.BuilderSessions(),
		)
		aiLoop.Register()
	}

	eng.StartWorkers(ctx)

	// Launch the chat inbound pollers now that the engine worker pool is live.
	// Each poller no-ops when its bot token is unset.
	chat.Start(ctx, chatScope)

	// Register the credential-verify workflow now that the engine is live.
	// A failure here is non-fatal: the console renders the Verify button
	// disabled if the run later cannot start, rather than refusing to boot.
	if err := credVerify.Register(ctx); err != nil {
		slog.Default().Error("credverify: register workflow", "err", err)
	}

	// Declare the AI-builder agent-loop workflow now that the engine is live. A
	// failure here is non-fatal: the durable builder simply cannot be triggered
	// until a later boot succeeds, rather than refusing to start the binary.
	if aiLoop != nil {
		if err := aiLoop.RegisterWorkflow(ctx); err != nil {
			slog.Default().Error("aibuilder: register agent-loop workflow", "err", err)
		}
	}

	whStop, err := webhooksout.New(bus, st).WithSecrets(secrets).Start(ctx)
	if err != nil {
		return fmt.Errorf("webhooksout.Start: %w", err)
	}
	defer func() { _ = whStop() }()

	id := identity.New(st, mk)
	aud := audience.New(st)
	au := authoring.New(st, eng, authoring.WithSecretBox(secrets))

	// Re-register every persisted workflow with the freshly-started engine.
	// dagnats holds its workflow registry in memory and loses it on restart;
	// without this, existing workflows silently stop firing (the engine runs
	// them with task_count=0) until each is re-saved. Resilient by design: a
	// workflow that fails to compile is logged and skipped, never aborting boot.
	if n, err := au.RepublishAll(ctx); err != nil {
		slog.Default().Error("authoring: republish all workflows", "err", err)
	} else {
		slog.Default().Info("authoring: re-registered persisted workflows", "count", n)
	}

	trg := trigger.New(aud, st, eng).WithDigester(bus).WithDigestWindows(prefs)
	inbox := inboxapi.New(st)
	rt := realtime.New(bus)
	feed := feedapi.New(bus)
	// Inbound webhook-push: the source service (per-source secret lifecycle,
	// under the same master key as identity/connectivity) plus the ingest
	// gateway that authenticates SourceKeys, limits, normalizes, and publishes
	// batches into the durable feed.
	srcSvc := streamsource.New(st, mk)
	srcIngest := feedingest.New(srcSvc, bus)
	inb := inbound.New(trg)
	ag := agentsapi.New(ags).WithSecretBox(secrets).WithLoopbackMCP(cfg.dev)

	ev, err := envvars.New(st, mk)
	if err != nil {
		return fmt.Errorf("envvars.New: %w", err)
	}

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

	handler := httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trg, Inbox: inbox, Realtime: rt, Feed: feed, Inbound: inb, Analytics: an, Agents: ag, Ingress: ing, Connectivity: conn, Store: st, EnvVars: ev, EmailLoop: eml, SourceIngest: srcIngest, Authoring: au, Preferences: prefs, InboundMXHost: cfg.inboundMXHost, Secrets: secrets, Blobs: blobs})

	// Operator console: same listener, mounted at /console ahead of the
	// public API. All console reads go through the same Scope-carrying
	// services in-process.
	consoleDeps := console.Deps{
		Logger:           slog.Default(),
		DevMode:          cfg.dev,
		Identity:         id,
		Authoring:        au,
		Analytics:        an,
		WorkflowVersions: st.WorkflowVersions(),
		Notifications:    st.Notifications(),
		Messages:         st.Messages(),
		Integrations:     st.Integrations(),
		Connectivity:     conn,
		// S5: inbound webhook-push source management (same service the ingest
		// gateway authenticates against).
		StreamSources:  srcSvc,
		Audience:       aud,
		Webhooks:       webhooksout.NewManagement(st, secrets),
		Contexts:       contexts.New(st),
		Layouts:        st.Layouts(),
		Translations:   st.Translations(),
		Environments:   st.Environments(),
		EnvVars:        ev,
		InboundDomains: inbounddomains.New(st, cfg.inboundMXHost),
		// Inbound email console page: read-only thread log + endpoint status.
		EmailThreads:            st.EmailThreads(),
		InboundSecretConfigured: cfg.inboundSecret != "",
		Trigger:                 trg,
		CredVerifier:            credVerify,
		Agents:                  ags,
		Composer:                consoleComposer{ing},
		// #106: read-only chat inbound status page. The loop resolves its
		// default agent in the chat default scope (NOTIFY_CHAT_DEFAULT_ORG/ENV),
		// not the console operator's scope, so pin chatScope here.
		ChatStatus:        chatStatus{chat: chat, scope: chatScope},
		MasterKey:         mk,
		DagnatsConsoleURL: cfg.dagnatsConsoleURL,
		// AI workflow builder key (Guided create tab). The console uses non-empty
		// to decide display only; actual provider selection is done above via
		// NewClientFromEnv. Prefer NOTIFY_AI_API_KEY; fall back to ANTHROPIC_API_KEY
		// (resolved once in loadConfig).
		AnthropicKey:    cfg.aiConsoleKey,
		BuilderSessions: st.BuilderSessions(),
		ReadOnly:        cfg.consoleReadOnly,
		HTTPAddr:        cfg.addr,
		// #104: public origin used to render ready-to-register inbound webhook
		// URLs on the agent channels tab. Empty falls back to a "{base_url}"
		// placeholder the operator fills in.
		PublicBaseURL: cfg.publicBaseURL,
		// W4-S1 #40: subscriber preferences tab.
		Preferences: prefs,
		// W4-S2 #41: topic activity tab.
		TopicTriggers: st.TopicTriggers(),
		// W4-W2: SSE live-tail for the activity views. The event bus is the
		// delivery-status publisher the live-tail subscribes through.
		LiveTail: bus,
		// B3: the ai-generate handler subscribes to the durable build run's live
		// Events (stream.<runID>.build) through the event bus.
		BuildStream: bus,
	}
	// B3: the durable AI workflow builder drives the Guided create tab. Assign the
	// dispatcher only when it is wired (ANTHROPIC_API_KEY set) — a nil
	// *aibuilder.AgentLoopBuilder boxed into the interface would read as non-nil and
	// defeat the console's graceful not-configured fallback.
	if aiLoop != nil {
		consoleDeps.AIBuilder = aiLoop
	}
	consoleRoutes := console.New(consoleDeps).Routes()
	root := http.NewServeMux()
	root.Handle("/console", consoleRoutes)
	root.Handle("/console/", consoleRoutes)
	// Readiness probe (notify hardening #6), distinct from /healthz's
	// liveness-only check: reports 503 until both the dagnats bridge and the
	// writer DB pool answer live, 200 once both do.
	writer, _ := st.Pools()
	root.HandleFunc("/readyz", readyzHandler(cfg.bridgeURL, bridgeClient, writer))
	// Ops metrics (notify hardening #8): JSON of delivery outcomes and NATS link
	// status, mounted ahead of the "/" catch-all. Unauthenticated, same trust
	// posture as /healthz — an operator gates it at the ingress if needed.
	// metrics.Handler() (not expvar.Handler()) exposes only notify_ vars, never
	// expvar's default cmdline (os.Args) / memstats — /metrics is public via the
	// tunnel, so argv must never surface there.
	root.Handle("/metrics", metrics.Handler())
	root.Handle("/", handler)

	// Embedded SMTP receiver (#340): opt-in, so the inbound MX host can be
	// notify itself. It shares ParseMessage/Accept with the webhook, and its
	// banner names the MX host the operator told subscribers to use.
	if cfg.smtpAddr != "" {
		eml.EnableListener()
		bannerHost := cfg.inboundMXHost
		if bannerHost == "" {
			bannerHost, _ = os.Hostname()
		}
		receiver := smtpin.New(eml, bannerHost, slog.Default())
		if cfg.smtpCert != "" {
			// Asked-for TLS that cannot load is a refusal to start, not a
			// receiver that quietly offers none.
			certs, err := smtpin.LoadCertificate(cfg.smtpCert, cfg.smtpKey, slog.Default())
			if err != nil {
				return fmt.Errorf("smtp STARTTLS: %w", err)
			}
			receiver.WithTLS(certs)
		}
		go func() {
			if err := receiver.ListenAndServe(ctx, cfg.smtpAddr); err != nil {
				slog.Default().Error("smtpin: receiver stopped", "addr", cfg.smtpAddr, "err", err)
			}
		}()
	}

	srv := &http.Server{Addr: cfg.addr, Handler: root}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	fmt.Printf("notify %s serving on %s (nats=%s bridge=%s db=%s)\n",
		version.Version, cfg.addr, cfg.natsURL, cfg.bridgeURL, cfg.dbPath)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("ListenAndServe: %w", err)
	}
	return nil
}

// runBootstrap provisions a tenant (org + env + API key) and prints the key and
// scope so an operator can begin triggering.
// runRotateKey mints a fresh API key for an EXISTING org/environment.
//
// It exists because the raw key is only ever shown once (identity stores a
// hash), and the console's regenerate button is reachable only by an operator
// who is already logged in — which requires the very key they have lost. That
// is a genuine lockout: bootstrap cannot help, because it provisions a NEW
// tenant rather than re-keying an existing one, leaving the real workflows and
// subscribers behind.
//
// Org and environment are resolved by name so an operator does not have to dig
// ULIDs out of SQLite. Rotation revokes every existing key for the scope (see
// identity.RotateKey), so any client still holding the old key breaks.
func runRotateKey(args []string) error {
	fs := flag.NewFlagSet("rotate-key", flag.ExitOnError)
	org := fs.String("org", "", "organization name (or id)")
	env := fs.String("env", "", "environment name (or id)")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow rotating without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}

	// The master key must match the one serve uses: identity encrypts the
	// stored key material with it, so rotating under a different key mints
	// something serve cannot decrypt.
	mk, err := masterKey(*masterKeyHex, *dev)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer func() { _ = st.Close() }()

	scope, err := resolveScope(ctx, st, *org, *env)
	if err != nil {
		return err
	}

	key, err := identity.New(st, mk).RotateKey(ctx, scope)
	if err != nil {
		return fmt.Errorf("rotate: %w", err)
	}

	out, _ := json.MarshalIndent(struct {
		APIKey string `json:"apiKey"`
		OrgID  string `json:"orgId"`
		EnvID  string `json:"envId"`
	}{key, scope.OrgID, scope.EnvID}, "", "  ")
	fmt.Println(string(out))
	fmt.Fprintln(os.Stderr, "note: previous keys for this scope are now revoked; the value above is shown once")
	return nil
}

// resolveScope turns human org/env names into a domain.Scope, accepting an id
// for either so the command still works when names are ambiguous. An ambiguous
// name is an error rather than a silent first-match, because rotating the wrong
// environment revokes a key the operator may still be relying on.
func resolveScope(ctx context.Context, st *store.Store, org, env string) (domain.Scope, error) {
	_, reader := st.Pools()

	var orgID string
	rows, err := reader.QueryContext(ctx,
		`SELECT id FROM organizations WHERE id = ? OR name = ?`, org, org)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup org: %w", err)
	}
	orgIDs, err := collectIDs(rows)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup org: %w", err)
	}
	switch len(orgIDs) {
	case 0:
		return domain.Scope{}, fmt.Errorf("no organization named %q", org)
	case 1:
		orgID = orgIDs[0]
	default:
		return domain.Scope{}, fmt.Errorf("organization %q is ambiguous (%d matches); pass its id instead", org, len(orgIDs))
	}

	rows, err = reader.QueryContext(ctx,
		`SELECT id FROM environments WHERE org_id = ? AND (id = ? OR name = ?)`, orgID, env, env)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup env: %w", err)
	}
	envIDs, err := collectIDs(rows)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("lookup env: %w", err)
	}
	switch len(envIDs) {
	case 0:
		return domain.Scope{}, fmt.Errorf("organization %q has no environment named %q", org, env)
	case 1:
		return domain.Scope{OrgID: orgID, EnvID: envIDs[0]}, nil
	default:
		return domain.Scope{}, fmt.Errorf("environment %q is ambiguous (%d matches); pass its id instead", env, len(envIDs))
	}
}

func collectIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	org := fs.String("org", "", "organization name")
	env := fs.String("env", "", "environment identifier")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow bootstrapping without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *org == "" || *env == "" {
		return fmt.Errorf("--org and --env are required")
	}

	mk, err := masterKey(*masterKeyHex, *dev)
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
