// Package notify is the notify server: its store, its dagnats-backed engine,
// every service and every HTTP route, behind a small lifecycle. The process
// that runs it (cmd/notify, or an embedding application) supplies a Host and
// a Config, serves Handler on a listener of its own, and calls Start and Stop.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	agentsapi "github.com/danmestas/notify/internal/agents/api"
	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/agents/runtime/managed"
	"github.com/danmestas/notify/internal/agents/runtime/mesh"
	agentstore "github.com/danmestas/notify/internal/agents/store"
	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/analytics"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/chatloop"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/console"
	"github.com/danmestas/notify/internal/contexts"
	"github.com/danmestas/notify/internal/credverify"
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
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/realtime"
	"github.com/danmestas/notify/internal/replyhook"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/smtpin"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/streamsource"
	"github.com/danmestas/notify/internal/trigger"
	"github.com/danmestas/notify/internal/webhooksout"
)

// Notify is a wired notify server. Build it with New; it does nothing until
// Start.
type Notify struct {
	log *slog.Logger
	st  *store.Store
	eng *engine.Engine
	ags *agentstore.Store
	au  *authoring.Service

	credVerify *credverify.Service
	aiLoop     *aibuilder.AgentLoopBuilder
	chat       *chatloop.Service
	chatScope  domain.Scope
	webhooks   *webhooksout.Consumer
	an         *analytics.Service
	smtp       *smtpin.Server
	smtpAddr   string

	handler http.Handler

	// cleanup runs last-in first-out on Stop, so a resource is released
	// after everything started on top of it. Start pushes the engine's drain
	// last, so it runs first: no dependency closes under a handler that is
	// still finishing.
	cleanup []func() error

	// life bounds every loop Start launches; Stop cancels it.
	life    context.Context
	endLife context.CancelFunc
	loops   sync.WaitGroup

	startOnce sync.Once
	stopOnce  sync.Once
	stopErr   error
}

// New opens the store and wires every subsystem against h, registering the
// engine's task handlers and building the HTTP routes. It starts no
// goroutines: Start does. On error everything New opened is closed again.
func New(ctx context.Context, h Host, cfg Config) (_ *Notify, err error) {
	if h.NC == nil {
		return nil, errors.New("notify: Host.NC is required")
	}
	base, err := cleanBasePath(h.BasePath)
	if err != nil {
		return nil, err
	}
	log := h.Logger
	if log == nil {
		log = slog.Default()
	}
	meshConn := h.Mesh
	if meshConn == nil {
		meshConn = h.NC
	}
	n := &Notify{log: log}
	defer func() {
		if err != nil {
			_ = n.unwind()
		}
	}()
	mk := cfg.MasterKey

	// One secret box for at-rest encryption of declared secret control fields
	// (http_request headers). Shared by authoring (seal on write) and the
	// http_request handler (open at run time).
	secrets, err := secretbox.New(mk)
	if err != nil {
		return nil, fmt.Errorf("secretbox.New: %w", err)
	}

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	n.st = st
	n.push(st.Close)

	eng := engine.FromConn(h.NC, engine.WithLogger(log))
	n.eng = eng
	n.push(eng.Stop)

	bus, err := eventbus.FromConn(ctx, h.NC)
	if err != nil {
		return nil, fmt.Errorf("eventbus: %w", err)
	}
	n.push(bus.Close)

	prefs := preferences.New(st)
	conn := connectivity.New(st, mk,
		connectivity.WithSendmailCommand(cfg.SendmailCommand),
		connectivity.WithPublicBaseURL(cfg.PublicBaseURL))

	// W4 ADR-013 (#55): durable credential-verify action. The verify task runs
	// in this worker pool (registered before StartWorkers); the workflow is
	// declared with the engine in Start.
	n.credVerify = credverify.New(eng, conn)
	handle(eng, credverify.TaskVerify, n.credVerify.Handler)

	meshClient := mesh.FromConn(meshConn, mesh.WithAllowedSubjects(cfg.MeshAllow))
	n.push(func() error { meshClient.Close(); return nil })
	ags := agentstore.New(st)
	n.ags = ags
	ing := ingress.New(ags, st, eng, bus).
		WithRuntime(agentdomain.RuntimeMesh, meshClient).
		// Dev mode lets managed agents reach MCP servers on loopback over http.
		WithRuntime(agentdomain.RuntimeManaged, managed.New(secrets, managed.DefaultClient).WithMCP(cfg.Dev))

	// Email two-way loop: inbound email replies route back to the agent and the
	// agent's reply mails back out. The responder defaults to the conversation's
	// agent (ing.ComposeMessage); conn resolves the outbound provider. The reply
	// hook closes the loop. InboundSecret authenticates the mail forwarder's
	// POST to /v1/inbound/email; an empty secret disables inbound.
	compose := consoleComposer{ing}.ComposeMessage
	blobs, err := openBlobStore(cfg)
	if err != nil {
		return nil, err
	}
	eml := emailloop.New(
		st, ags, emailloop.NewMinter(mk), conn,
		emailloop.NewAgentResponder(compose),
		cfg.InboundSecret, log,
	).WithEventPublisher(bus). // webhook-kind replies ride the delivery stream (#345)
					WithBlobStore(blobs)

	// All step-task handlers register in one place so the set can be compared
	// mechanically against steptype.Registry.TaskTypes() -- see the coverage
	// test. Registered after the email loop exists, because send.email takes
	// it as its reply armer.
	registerStepTaskHandlers(eng, stepTaskDeps{
		store: st, conn: conn, prefs: prefs, bus: bus, secrets: secrets, replyArmer: eml,
		composer: ing, drafts: ags.Drafts(),
	})

	// Chat two-way loop: inbound Telegram (getUpdates long-poll) and Discord
	// (gateway WS) messages route to the agent and the agent's reply is sent
	// back into the originating chat. Inbound dials OUT (no public tunnel).
	// Cold inbound (a chat with no binding) routes to ChatDefaultAgent in scope
	// (ChatDefaultOrg, ChatDefaultEnv).
	chat := chatloop.New(
		st,
		chatloop.NewAgentResponder(compose),
		cfg.TelegramBotToken,
		cfg.DiscordBotToken,
		cfg.ChatDefaultAgent,
		log,
	)
	// DiscordRespondAll makes the Discord gateway reply to every message in a
	// guild channel (for a dedicated single-purpose channel). Otherwise the bot
	// must be @-mentioned in guild channels; DMs always route.
	if cfg.DiscordRespondAll {
		chat.SetDiscordRespondAll(true)
	}
	chatScope := domain.Scope{OrgID: cfg.ChatDefaultOrg, EnvID: cfg.ChatDefaultEnv}
	n.chat, n.chatScope = chat, chatScope
	// Fediverse inbound (mastodon/bluesky mention polling, #296) reads its
	// credentials from the integrations table, not configuration -- unlike
	// Telegram and Discord above. conn.DecryptedCredentials resolves the active
	// integration for that provider id in chatScope; chatloop.Start disables
	// the corresponding poller when it comes back empty.
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
	ing = ing.WithReplyHook(replyhook.Combine(log, eml.ReplyHook, chat.ReplyHook))
	handle(eng, ingress.TaskMesh, ing.HandleTurn)
	// A managed turn with tools runs up to managed.MaxTurnDuration; past that
	// plus a margin it is hung, and dagnats stops keeping its claim alive.
	handle(eng, ingress.TaskManaged, ing.HandleTurn, engine.WithMaxRunTime(managed.MaxTurnDuration+runTimeMargin))

	// B3: durable AI workflow-builder loop, hosted as a dagnats agent-loop
	// handler alongside the synchronous console path (console.Builder.Run).
	// A nil cfg.AI leaves the builder disabled (the Guided tab shows a note).
	// The handler must be registered before StartWorkers; the workflow is
	// declared in Start, like credverify.
	if cfg.AI != nil {
		n.aiLoop = aibuilder.NewAgentLoopBuilder(eng, cfg.AI, st.Integrations(), st.BuilderSessions())
		n.aiLoop.Register()
		registerLegacyAgentLoop(n.aiLoop)
	}

	n.webhooks = webhooksout.New(bus, st).WithSecrets(secrets)

	id := identity.New(st, mk)
	aud := audience.New(st)
	n.au = authoring.New(st, eng, authoring.WithSecretBox(secrets))
	trg := trigger.New(aud, st, eng).WithDigester(bus).WithDigestWindows(prefs)
	// Inbound webhook-push: the source service (per-source secret lifecycle,
	// under the same master key as identity/connectivity) plus the ingest
	// gateway that authenticates SourceKeys, limits, normalizes, and publishes
	// batches into the durable feed.
	srcSvc := streamsource.New(st, mk)
	ag := agentsapi.New(ags).WithSecretBox(secrets).WithLoopbackMCP(cfg.Dev)

	ev, err := envvars.New(st, mk)
	if err != nil {
		return nil, fmt.Errorf("envvars.New: %w", err)
	}

	an, anClose, err := analyticsBackend(ctx, st, bus, cfg.DuckDBPath)
	if err != nil {
		return nil, fmt.Errorf("analyticsBackend: %w", err)
	}
	n.an = an
	n.push(anClose)

	api := httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trg, Inbox: inboxapi.New(st),
		Realtime: realtime.New(bus), Feed: feedapi.New(bus), Inbound: inbound.New(trg),
		Analytics: an, Agents: ag, Ingress: ing, Connectivity: conn, Store: st, EnvVars: ev,
		EmailLoop: eml, SourceIngest: feedingest.New(srcSvc, bus), Authoring: n.au,
		Preferences: prefs, InboundMXHost: cfg.InboundMXHost, Secrets: secrets, Blobs: blobs,
	})

	// Operator console: same listener, mounted at /console ahead of the
	// public API. All console reads go through the same Scope-carrying
	// services in-process.
	consoleDeps := console.Deps{
		Logger:           log,
		DevMode:          cfg.Dev,
		Identity:         id,
		Authoring:        n.au,
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
		InboundDomains: inbounddomains.New(st, cfg.InboundMXHost),
		// Inbound email console page: read-only thread log + endpoint status.
		EmailThreads:            st.EmailThreads(),
		InboundSecretConfigured: cfg.InboundSecret != "",
		Trigger:                 trg,
		CredVerifier:            n.credVerify,
		Agents:                  ags,
		Composer:                consoleComposer{ing},
		// #106: read-only chat inbound status page. The loop resolves its
		// default agent in the chat default scope, not the console operator's
		// scope, so pin chatScope here.
		ChatStatus:        chatStatus{chat: chat, scope: chatScope},
		MasterKey:         mk,
		DagnatsConsoleURL: cfg.DagnatsConsoleURL,
		// AI workflow builder key (Guided create tab). The console uses
		// non-empty to decide display only; the provider is cfg.AI.
		AnthropicKey:    cfg.AIConsoleKey,
		BuilderSessions: st.BuilderSessions(),
		ReadOnly:        cfg.ConsoleReadOnly,
		HTTPAddr:        cfg.HTTPAddr,
		// #104: public origin used to render ready-to-register inbound webhook
		// URLs on the agent channels tab. Empty falls back to a "{base_url}"
		// placeholder the operator fills in.
		PublicBaseURL: cfg.PublicBaseURL,
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
	// B3: assign the dispatcher only when it is wired — a nil
	// *aibuilder.AgentLoopBuilder boxed into the interface would read as
	// non-nil and defeat the console's graceful not-configured fallback.
	if n.aiLoop != nil {
		consoleDeps.AIBuilder = n.aiLoop
	}
	writer, _ := st.Pools()
	n.handler = mount(base, routes(console.New(consoleDeps).Routes(), api, readyzHandler(eng.Ready, writer)))

	// Embedded SMTP receiver (#340): opt-in, so the inbound MX host can be
	// notify itself. It shares ParseMessage/Accept with the webhook, and its
	// banner names the MX host the operator told subscribers to use.
	if cfg.SMTPAddr != "" {
		eml.EnableListener()
		bannerHost := cfg.InboundMXHost
		if bannerHost == "" {
			bannerHost, _ = os.Hostname()
		}
		n.smtp = smtpin.New(eml, bannerHost, log)
		n.smtpAddr = cfg.SMTPAddr
		if cfg.SMTPCert != "" {
			// Asked-for TLS that cannot load is a refusal to start, not a
			// receiver that quietly offers none.
			certs, err := smtpin.LoadCertificate(cfg.SMTPCert, cfg.SMTPKey, log)
			if err != nil {
				return nil, fmt.Errorf("smtp STARTTLS: %w", err)
			}
			n.smtp.WithTLS(certs)
		}
	}
	return n, nil
}

// routes is notify's root mux.
func routes(consoleRoutes, api http.Handler, readyz http.HandlerFunc) *http.ServeMux {
	root := http.NewServeMux()
	root.Handle("/console", consoleRoutes)
	root.Handle("/console/", consoleRoutes)
	// Readiness probe (notify hardening #6), distinct from /healthz's
	// liveness-only check: reports 503 until both the engine's dagnats
	// connection and the writer DB pool are live, 200 once both are.
	root.HandleFunc("/readyz", readyz)
	// Ops metrics (notify hardening #8): JSON of delivery outcomes and NATS link
	// status, mounted ahead of the "/" catch-all. Unauthenticated, same trust
	// posture as /healthz — an operator gates it at the ingress if needed.
	// metrics.Handler() (not expvar.Handler()) exposes only notify_ vars, never
	// expvar's default cmdline (os.Args) / memstats — /metrics is public via the
	// tunnel, so argv must never surface there.
	root.Handle("/metrics", metrics.Handler())
	root.Handle("/", api)
	return root
}

// cleanBasePath accepts "" or an absolute path, and drops a trailing slash.
func cleanBasePath(p string) (string, error) {
	p = strings.TrimRight(p, "/")
	if p != "" && !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("notify: Host.BasePath %q must start with /", p)
	}
	return p, nil
}

// mount serves h under base, so a request outside base is a 404.
func mount(base string, h http.Handler) http.Handler {
	if base == "" {
		return h
	}
	outer := http.NewServeMux()
	outer.Handle(base+"/", http.StripPrefix(base, h))
	return outer
}

// Handler serves every notify route under Host.BasePath.
func (n *Notify) Handler() http.Handler { return n.handler }

// Start launches notify's background work: the retention purges, the engine's
// task workers, the chat pollers, the outbound webhook and analytics
// consumers and the SMTP receiver. It declares the workflows notify owns and
// re-registers every persisted one with dagnats. Cancelling ctx, or calling
// Stop, ends the background work. Start runs once; a second call is an error.
func (n *Notify) Start(ctx context.Context) error {
	err := errors.New("notify: already started")
	n.startOnce.Do(func() { err = n.start(ctx) })
	return err
}

func (n *Notify) start(ctx context.Context) error {
	n.life, n.endLife = context.WithCancel(ctx)
	ctx = n.life

	// step_outputs grows without bound otherwise: every http_request action
	// writes a row holding the captured response body and nothing removed them.
	n.goLoop(func() { runStepOutputPurge(ctx, n.log, n.st) })
	// delay_deadlines grows without bound otherwise once a deployment stops
	// creating new relative dynamic delays: Save no longer sweeps, so nothing
	// else would ever reclaim an abandoned deadline row.
	n.goLoop(func() { runDelayDeadlinePurge(ctx, n.log, n.st) })
	// chat_inbound_handled would otherwise keep one row per inbound chat message forever.
	n.goLoop(func() { runChatInboundPurge(ctx, n.log, n.st) })
	// agent_drafts would otherwise keep every draft no one fetched.
	n.goLoop(func() { runDraftPurge(ctx, n.log, n.ags) })

	if err := n.eng.StartWorkers(ctx); err != nil {
		return err
	}

	// Launch the chat inbound pollers now that the engine worker pool is live.
	// Each poller no-ops when its bot token is unset.
	n.chat.Start(ctx, n.chatScope)

	// Register the credential-verify workflow now that the engine is live.
	// A failure here is non-fatal: the console renders the Verify button
	// disabled if the run later cannot start, rather than refusing to boot.
	if err := n.credVerify.Register(ctx); err != nil {
		n.log.Error("credverify: register workflow", "err", err)
	}

	// Declare the AI-builder agent-loop workflow now that the engine is live. A
	// failure here is non-fatal: the durable builder simply cannot be triggered
	// until a later boot succeeds, rather than refusing to start the binary.
	if n.aiLoop != nil {
		if err := n.aiLoop.RegisterWorkflow(ctx); err != nil {
			n.log.Error("aibuilder: register agent-loop workflow", "err", err)
		}
	}

	whStop, err := n.webhooks.Start(ctx)
	if err != nil {
		return fmt.Errorf("webhooksout.Start: %w", err)
	}
	n.push(whStop)

	// Re-register every persisted workflow with the freshly-started engine.
	// dagnats holds its workflow registry in memory and loses it on restart;
	// without this, existing workflows silently stop firing (the engine runs
	// them with task_count=0) until each is re-saved. Resilient by design: a
	// workflow that fails to compile is logged and skipped, never aborting boot.
	if count, err := n.au.RepublishAll(ctx); err != nil {
		n.log.Error("authoring: republish all workflows", "err", err)
	} else {
		n.log.Info("authoring: re-registered persisted workflows", "count", count)
	}

	anStop, err := n.an.Start(ctx)
	if err != nil {
		return fmt.Errorf("analytics.Start: %w", err)
	}
	n.push(anStop)

	if n.smtp != nil {
		n.goLoop(func() {
			if err := n.smtp.ListenAndServe(ctx, n.smtpAddr); err != nil {
				n.log.Error("smtpin: receiver stopped", "addr", n.smtpAddr, "err", err)
			}
		})
	}

	// Pushed last so Stop drains the engine first, before any dependency
	// pushed above (bus, mesh, webhooks, analytics, store) closes under a
	// handler that is still finishing. Engine.Stop is safe to call twice.
	n.push(n.eng.Stop)
	return nil
}

// Stop ends the background work and releases what New opened, in reverse
// order of start: the engine drains first and the store closes last. It never
// closes the host's connections and never calls dagnats' control plane. ctx
// bounds the wait for notify's own loops. Stop is safe to call more than once,
// and without Start.
func (n *Notify) Stop(ctx context.Context) error {
	n.stopOnce.Do(func() {
		if n.endLife != nil {
			n.endLife()
		}
		var waitErr error
		done := make(chan struct{})
		go func() { n.loops.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			waitErr = fmt.Errorf("notify: waiting for background loops: %w", ctx.Err())
		}
		n.stopErr = errors.Join(waitErr, n.unwind())
	})
	return n.stopErr
}

// Backup writes a consistent copy of the store's database to path with
// VACUUM INTO. It refuses a path that already exists rather than overwrite
// it.
func (n *Notify) Backup(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("notify: backup %q: %w", path, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("notify: backup %q: %w", path, err)
	}
	writer, _ := n.st.Pools()
	if _, err := writer.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("notify: backup %q: %w", path, err)
	}
	return nil
}

// push records a release step for Stop.
func (n *Notify) push(f func() error) { n.cleanup = append(n.cleanup, f) }

// unwind runs the release steps last-in first-out.
func (n *Notify) unwind() error {
	var errs []error
	for i := len(n.cleanup) - 1; i >= 0; i-- {
		errs = append(errs, n.cleanup[i]())
	}
	n.cleanup = nil
	return errors.Join(errs...)
}

// goLoop runs f on a goroutine Stop waits for.
func (n *Notify) goLoop(f func()) {
	n.loops.Add(1)
	go func() {
		defer n.loops.Done()
		f()
	}()
}

// consoleComposer adapts *ingress.Service to console.ConversationComposer and
// to the two-way loops' compose primitive: they need only the conversation
// thread handle back, so this flattens ingress.ComposeResult to its
// ConversationID.
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
// the chat default scope the loop resolves its default agent in — the console
// operator's request scope is irrelevant to the chat config (#106).
type chatStatus struct {
	chat  *chatloop.Service
	scope domain.Scope
}

func (c chatStatus) Status(ctx context.Context, _ domain.Scope) chatloop.Snapshot {
	return c.chat.Status(ctx, c.scope)
}

// openBlobStore builds the configured raw-mail store, or nil when it is off.
func openBlobStore(cfg Config) (blobstore.Store, error) {
	switch cfg.BlobDriver {
	case "":
		return nil, nil
	case "fs":
		return blobstore.NewFS(cfg.BlobDir)
	case "s3":
		return blobstore.NewS3(cfg.BlobS3)
	default:
		return nil, fmt.Errorf("NOTIFY_BLOB_DRIVER %q is not one of fs, s3 (or empty for off)", cfg.BlobDriver)
	}
}
