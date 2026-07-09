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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
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
	"github.com/danmestas/notify/internal/delivery"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/emailloop"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/envvars"
	"github.com/danmestas/notify/internal/eventbus"
	"github.com/danmestas/notify/internal/feedapi"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inbound"
	"github.com/danmestas/notify/internal/inbounddomains"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/replyhook"
	"github.com/danmestas/notify/internal/store"
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
// is decoded and length-checked. An empty flag is only accepted when dev is
// true: it then derives the exact same fixed dev key as before (byte-for-byte
// unchanged) so a local operator can keep booting without ceremony and
// existing local users are not re-keyed. Outside dev, an empty hex key is a
// fatal misconfiguration — silently deriving a fixed, publicly-computable key
// would encrypt every credential and cookie under a value anyone can compute.
func masterKey(hexKey string, dev bool) ([]byte, error) {
	if hexKey == "" {
		if !dev {
			return nil, fmt.Errorf("master-key-hex is required outside -dev/NOTIFY_DEV=1 (an empty key would derive a fixed, publicly-known encryption key)")
		}
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

// envBool reports whether the environment variable key is set to "1" or
// "true" (matches the existing NOTIFY_DISCORD_RESPOND_ALL convention below).
func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true":
		return true
	default:
		return false
	}
}

// runServe wires the full stack and serves the HTTP API until interrupted.
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	natsURL := fs.String("nats-url", envOr("NOTIFY_NATS_URL", "nats://127.0.0.1:4222"), "dagnats NATS client URL")
	bridgeURL := fs.String("bridge-url", envOr("NOTIFY_BRIDGE_URL", "http://127.0.0.1:8080"), "dagnats worker bridge URL")
	dbPath := fs.String("db", envOr("NOTIFY_DB", "notify.db"), "SQLite database path")
	addr := fs.String("addr", envOr("NOTIFY_ADDR", "127.0.0.1:3000"), "HTTP listen address")
	masterKeyHex := fs.String("master-key-hex", os.Getenv("NOTIFY_MASTER_KEY_HEX"), "32-byte AES master key as hex (required unless -dev)")
	dev := fs.Bool("dev", envBool("NOTIFY_DEV"), "allow booting without -master-key-hex/NOTIFY_MASTER_KEY_HEX by deriving a fixed, insecure dev key (development only)")
	meshAllow := fs.String("mesh-allow", envOr("NOTIFY_MESH_ALLOW", mesh.DefaultAllowedSubjects), "allowlist pattern for mesh agent dispatch subjects")
	meshNATSURL := fs.String("mesh-nats-url", envOr("NOTIFY_MESH_NATS_URL", ""), "NATS URL for mesh agent dispatch; empty reuses -nats-url (set when agents live on a different hub than the engine)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mk, err := masterKey(*masterKeyHex, *dev)
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

	// Boot-time dagnats bridge reachability probe (notify hardening #6):
	// engine.New only validates the NATS control-plane connection; the HTTP
	// worker bridge is otherwise first touched lazily on the first poll (see
	// engine.go's poll), so a misconfigured -bridge-url/NOTIFY_BRIDGE_URL
	// would silently boot and then have every send.* task fail forever. A
	// transport-level failure (nothing listening) is fatal here; see
	// probeBridgeReachable's doc comment for why a reachable-but-not-2xx
	// bridge is logged and swallowed instead.
	bridgeClient := &http.Client{Timeout: bridgeProbeTimeout}
	if err := probeBridgeReachable(ctx, bridgeClient, *bridgeURL); err != nil {
		return fmt.Errorf("probeBridgeReachable: %w", err)
	}

	bus, err := eventbus.Connect(ctx, *natsURL)
	if err != nil {
		return fmt.Errorf("eventbus.Connect: %w", err)
	}
	defer func() { _ = bus.Close() }()

	prefs := preferences.New(st)
	conn := connectivity.New(st, mk)
	registerDeliveryHandlers(eng, st, conn, prefs, bus)
	// Scheduled (cron) digest window: a digest.wait task sleeps until the next
	// cron tick, then the downstream channel step self-sources and drains the
	// accumulated events. Regular (amount+unit) digests use the engine's native
	// sleep and need no handler. Registration is explicit (the step-type registry
	// is import-light and carries no runtime delivery wiring).
	eng.Handle("digest.wait", delivery.NewDigestWait(st).HandleDigestWait)
	// Scheduled (calendar) and dynamic (payload-sourced) delays: like digest.wait
	// the wait duration depends on wall-clock time or the trigger payload, so it
	// cannot be baked into a static sleep StepDef. The handler computes the wait
	// at dispatch and blocks context-aware; on shutdown it returns a
	// RetryableError so dagnats requeues (per the StepDef's RetryPolicy) and the
	// wait resumes after restart. Regular delays (amount+unit, incl. weeks/months)
	// use the engine's native sleep and need no handler.
	dw := delivery.NewDelayWait(st)
	eng.Handle("delay.scheduled", dw.HandleScheduled)
	eng.Handle("delay.dynamic", dw.HandleDynamic)
	// check.throttle: gate a run by a fixed-window counter; must be registered
	// whenever steptype.Default() includes "throttle" (see delivery.TaskThrottle).
	eng.Handle(delivery.TaskThrottle, delivery.NewThrottleHandler(st, bus).Handle)
	// http_request: outbound HTTP action step. Makes the interpolated call, then
	// captures statusCode/body/headers into step_outputs for downstream steps;
	// must be registered whenever steptype.Default() includes "http_request".
	eng.Handle("http_request", httpaction.New(st).Handle)

	// W4 ADR-013 (#55): durable credential-verify action. The verify task runs
	// in this worker pool (registered before StartWorkers); the workflow is
	// declared with the engine just after the workers start (below).
	credVerify := credverify.New(eng, conn)
	eng.Handle(credverify.TaskVerify, credVerify.Handler)

	meshHub := *meshNATSURL
	if meshHub == "" {
		meshHub = *natsURL
	}
	meshClient, err := mesh.Dial(meshHub, mesh.WithAllowedSubjects(*meshAllow))
	if err != nil {
		return fmt.Errorf("mesh.Dial: %w", err)
	}
	defer meshClient.Close()
	ags := agentstore.New(st)
	ing := ingress.New(ags, st, eng, bus).WithRuntime(agentdomain.RuntimeMesh, meshClient)

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
	eml := emailloop.New(
		st, ags, emailloop.NewMinter(mk), conn,
		emailloop.NewAgentResponder(emlCompose),
		os.Getenv("NOTIFY_INBOUND_SECRET"), slog.Default(),
	)

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
		os.Getenv("TELEGRAM_BOT_TOKEN"),
		os.Getenv("DISCORD_BOT_TOKEN"),
		os.Getenv("NOTIFY_CHAT_DEFAULT_AGENT"),
		slog.Default(),
	)
	// NOTIFY_DISCORD_RESPOND_ALL=1/true makes the Discord gateway reply to every
	// message in a guild channel (for a dedicated single-purpose channel).
	// Default (unset) requires the bot to be @-mentioned in guild channels; DMs
	// always route. See chatloop.Service.discordRespondAll.
	switch os.Getenv("NOTIFY_DISCORD_RESPOND_ALL") {
	case "1", "true":
		chat.SetDiscordRespondAll(true)
	}
	chatScope := domain.Scope{
		OrgID: os.Getenv("NOTIFY_CHAT_DEFAULT_ORG"),
		EnvID: os.Getenv("NOTIFY_CHAT_DEFAULT_ENV"),
	}

	// ingress has a single reply-hook slot but both two-way loops need to act on
	// an agent reply, so compose them: each loop's hook no-ops when the
	// conversation is not its binding, and a panic in one is recovered so it
	// never fails the turn or blocks the other.
	ing = ing.WithReplyHook(replyhook.Combine(slog.Default(), eml.ReplyHook, chat.ReplyHook))
	eng.Handle(ingress.TaskMesh, ing.HandleTurn)

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

	whStop, err := webhooksout.New(bus, st).Start(ctx)
	if err != nil {
		return fmt.Errorf("webhooksout.Start: %w", err)
	}
	defer func() { _ = whStop() }()

	id := identity.New(st, mk)
	aud := audience.New(st)
	au := authoring.New(st, eng)

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

	trg := trigger.New(aud, st, eng).WithDigester(bus)
	inbox := inboxapi.New(st)
	rt := realtime.New(bus)
	feed := feedapi.New(bus)
	inb := inbound.New(trg)
	ag := agentsapi.New(ags)

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

	handler := httpapi.New(id, aud, trg, inbox, rt, feed, inb, an, ag, ing, conn, st, ev, eml, au)

	// Operator console: same listener, mounted at /console ahead of the
	// public API. All console reads go through the same Scope-carrying
	// services in-process.
	consoleDeps := console.Deps{
		Logger:         slog.Default(),
		Identity:       id,
		Authoring:      au,
		Analytics:      an,
		Notifications:  st.Notifications(),
		Messages:       st.Messages(),
		Integrations:   st.Integrations(),
		Connectivity:   conn,
		Audience:       aud,
		Webhooks:       webhooksout.NewManagement(st),
		Contexts:       contexts.New(st),
		Layouts:        st.Layouts(),
		Translations:   st.Translations(),
		Environments:   st.Environments(),
		EnvVars:        ev,
		InboundDomains: inbounddomains.New(st),
		// Inbound email console page: read-only thread log + endpoint status.
		EmailThreads:            st.EmailThreads(),
		InboundSecretConfigured: os.Getenv("NOTIFY_INBOUND_SECRET") != "",
		Trigger:                 trg,
		CredVerifier:            credVerify,
		Agents:                  ags,
		Composer:                consoleComposer{ing},
		// #106: read-only chat inbound status page. The loop resolves its
		// default agent in the chat default scope (NOTIFY_CHAT_DEFAULT_ORG/ENV),
		// not the console operator's scope, so pin chatScope here.
		ChatStatus:        chatStatus{chat: chat, scope: chatScope},
		MasterKey:         mk,
		DagnatsConsoleURL: os.Getenv("NOTIFY_DAGNATS_CONSOLE_URL"),
		// AI workflow builder key (Guided create tab). The console uses non-empty
		// to decide display only; actual provider selection is done above via
		// NewClientFromEnv. Prefer NOTIFY_AI_API_KEY; fall back to ANTHROPIC_API_KEY.
		AnthropicKey: func() string {
			if k := os.Getenv("NOTIFY_AI_API_KEY"); k != "" {
				return k
			}
			return os.Getenv("ANTHROPIC_API_KEY")
		}(),
		BuilderSessions: st.BuilderSessions(),
		ReadOnly:        os.Getenv("NOTIFY_CONSOLE_READ_ONLY") == "1",
		HTTPAddr:        *addr,
		// #104: public origin used to render ready-to-register inbound webhook
		// URLs on the agent channels tab. Empty falls back to a "{base_url}"
		// placeholder the operator fills in.
		PublicBaseURL: os.Getenv("NOTIFY_PUBLIC_BASE_URL"),
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
	root.HandleFunc("/readyz", readyzHandler(*bridgeURL, bridgeClient, writer))
	root.Handle("/", handler)

	srv := &http.Server{Addr: *addr, Handler: root}

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
