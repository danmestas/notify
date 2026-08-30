// Package chatloop implements notify's chat two-way loop for Telegram and
// Discord: an inbound chat message routes into notify by chat_id, threads onto
// (or mints) a conversation, and is handed to the configured responder
// (default: the conversation's agent). The agent's reply is sent back into the
// originating chat, closing the loop.
//
// The loop has three legs, mirroring emailloop but keyed on chat_id rather than
// a token:
//
//  1. Inbound — notify dials OUT (Telegram getUpdates long-poll; Discord
//     gateway WS), so no public tunnel is needed. A message on a chat with an
//     existing binding continues its conversation; a cold message on an unbound
//     chat mints a new conversation against NOTIFY_CHAT_DEFAULT_AGENT (or is
//     dropped when no default agent is configured).
//  2. Turn — the responder threads the message onto the conversation and the
//     agent runtime generates the reply (engine work, never in-request).
//  3. Reply — ingress fires ReplyHook after the turn; chatloop looks up the
//     chat_thread by the conversation's platform_thread_id and sends the reply
//     back to the chat_id via the provider (Telegram sendMessage / Discord bot
//     REST).
package chatloop

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers/bluesky"
	corestore "github.com/danmestas/notify/internal/store"
)

// Provider names stored in chat_threads.provider and used to pick the reply
// sender. They double as the platform tag passed to handleInbound.
const (
	providerTelegram = "telegram"
	providerDiscord  = "discord"
	providerMastodon = "mastodon"
	providerBluesky  = "bluesky"
)

// telegramDefaultBaseURL is the Telegram Bot API host (poll + reply send).
const telegramDefaultBaseURL = "https://api.telegram.org"

// discordAPIBaseURL is the Discord REST API host (gateway bootstrap + reply
// send). The gateway WS URL is fetched from {discordAPIBaseURL}/gateway.
const discordAPIBaseURL = "https://discord.com/api/v10"

// Service is the chat two-way loop. It owns the chat-thread correlation store,
// the responder seam, the inbound pollers (Telegram long-poll, Discord
// gateway), and the outbound reply senders that close the loop.
type Service struct {
	core         *corestore.Store
	responder    Responder
	defaultAgent string
	log          *slog.Logger

	telegramToken string
	discordToken  string

	// discordRespondAll, when true, makes the Discord gateway respond to every
	// message in a guild channel instead of only when the bot is @-mentioned.
	// Set from NOTIFY_DISCORD_RESPOND_ALL for a dedicated single-purpose channel;
	// default (false) requires a direct mention in guild channels. DMs always
	// route regardless of this flag.
	discordRespondAll bool

	// Overridable in tests. Empty values fall back to the public hosts.
	telegramBaseURL string
	discordAPIBase  string
	httpClient      *http.Client

	telegram *telegramSender
	discord  *discordSender
	mastodon *mastodonSender
	bluesky  *blueskySender

	// mastodonMu guards mastodonPending, the in-memory map from a mention
	// author's acct (the chat_thread's chat_id) to the most recent mention's
	// status id + visibility. chat_threads has no column for either, so the
	// reply path (ReplyHook, generic across providers) cannot recover them from
	// the store; runMastodonPoll records them here on every inbound mention and
	// ReplyHook reads them back to address and thread the reply correctly. Not
	// persisted: a reply whose pending entry was lost to a restart falls back
	// to an untargeted (non-threaded, default-visibility) reply -- the same
	// replay-on-restart tradeoff runTelegramPoll's in-memory offset already
	// makes.
	mastodonMu      sync.Mutex
	mastodonPending map[string]mastodonPendingReply

	// mastodonPollInterval overrides the gap between notifications polls.
	// Zero (the default) uses mastodonPollInterval's package constant. Test
	// seam only -- production always uses the default.
	mastodonPollInterval time.Duration

	// blueskyMu guards blueskyPending, the in-memory map from a mention
	// author's DID (the chat_thread's chat_id) to the most recent
	// mention/reply's threading refs (parent + root uri/cid). chat_threads
	// has no column for either, so the reply path (ReplyHook, generic across
	// providers) cannot recover them from the store; runBlueskyPoll records
	// them here on every inbound mention and ReplyHook reads them back to
	// thread the reply correctly. Not persisted: a reply whose pending entry
	// was lost to a restart falls back to an unthreaded post -- the same
	// replay-on-restart tradeoff mastodonPending already makes.
	blueskyMu      sync.Mutex
	blueskyPending map[string]blueskyPendingReply

	// blueskyPollInterval overrides the gap between notifications polls.
	// Zero (the default) uses blueskyPollInterval's package constant. Test
	// seam only -- production always uses the default.
	blueskyPollInterval time.Duration

	// discordDialer lets tests point the gateway at an in-process WS server.
	// nil uses the default dialer that connects to Discord's real gateway URL.
	discordDialer gatewayDialer

	// credentialSource resolves the active integration credentials for
	// providers whose creds live in the integrations table rather than an env
	// var (mastodon, bluesky mention polling). nil (the default) leaves those
	// providers' pollers disabled. See SetCredentialSource.
	credentialSource CredentialSource
}

// CredentialSource resolves the ACTIVE integration's decrypted credential map
// for providerID within the loop's scope (production wires this from
// connectivity.Service.DecryptedCredentials). A nil map with a nil error means
// "no active integration for this provider" — that is not a failure, it just
// means the provider is unconfigured and Start leaves its poller disabled. A
// non-nil error means the lookup itself failed (store or decrypt error); Start
// logs it and leaves the poller disabled either way.
type CredentialSource func(ctx context.Context, providerID string) (map[string]string, error)

// New builds the Service. responder is the default turn handler (typically an
// AgentResponder). telegramToken/discordToken are the bot credentials read from
// env; an empty token disables that platform's poller (Start no-ops it) so the
// loop builds and runs without creds. defaultAgent is the agent cold inbound
// (a chat with no binding) routes to; empty drops cold inbound with a log line.
func New(
	core *corestore.Store,
	responder Responder,
	telegramToken, discordToken, defaultAgent string,
	log *slog.Logger,
) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		core:          core,
		responder:     responder,
		defaultAgent:  defaultAgent,
		log:           log,
		telegramToken: telegramToken,
		discordToken:  discordToken,
	}
	return s
}

// withTelegramBaseURL overrides the Telegram host (poll + reply). Test seam.
func (s *Service) withTelegramBaseURL(u string) *Service { s.telegramBaseURL = u; return s }

// withDiscordAPIBase overrides the Discord REST host (reply send + gateway
// bootstrap). Test seam.
func (s *Service) withDiscordAPIBase(u string) *Service { s.discordAPIBase = u; return s }

// SetDiscordRespondAll toggles guild respond-to-all (true) vs mention-required
// (false, the default). Production sets this from NOTIFY_DISCORD_RESPOND_ALL.
func (s *Service) SetDiscordRespondAll(v bool) { s.discordRespondAll = v }

// withDiscordRespondAll is the chained test seam over SetDiscordRespondAll.
func (s *Service) withDiscordRespondAll(v bool) *Service { s.SetDiscordRespondAll(v); return s }

// SetCredentialSource injects the lookup Start uses to resolve the active
// integration credentials for providers that read creds from the
// integrations table instead of an env-var token (mastodon, bluesky). A nil
// source (the default from New) leaves those pollers disabled. Production
// wires this from main.go with a small closure over
// connectivity.Service.DecryptedCredentials.
func (s *Service) SetCredentialSource(src CredentialSource) { s.credentialSource = src }

// withCredentialSource is the chained test seam over SetCredentialSource.
func (s *Service) withCredentialSource(src CredentialSource) *Service {
	s.SetCredentialSource(src)
	return s
}

// withHTTPClient overrides the HTTP client used by pollers and senders. Test
// seam.
func (s *Service) withHTTPClient(hc *http.Client) *Service { s.httpClient = hc; return s }

// withMastodonPollInterval overrides the gap between mastodon notifications
// polls. Test seam.
func (s *Service) withMastodonPollInterval(d time.Duration) *Service {
	s.mastodonPollInterval = d
	return s
}

// withBlueskyPollInterval overrides the gap between bluesky notifications
// polls. Test seam.
func (s *Service) withBlueskyPollInterval(d time.Duration) *Service {
	s.blueskyPollInterval = d
	return s
}

// senders lazily builds the per-provider reply senders from the loop's tokens.
func (s *Service) telegramSender() *telegramSender {
	if s.telegram == nil {
		s.telegram = newTelegramSender(s.telegramToken, s.telegramBaseURL, s.httpClient)
	}
	return s.telegram
}

func (s *Service) discordSender() *discordSender {
	if s.discord == nil {
		s.discord = newDiscordSender(s.discordToken, s.discordAPIBase, s.httpClient)
	}
	return s.discord
}

// handleInbound routes one inbound chat message. It resolves the chat binding
// by (provider, chatID): a bound chat continues its conversation with the bound
// agent; an unbound chat mints a new conversation against defaultAgent, or is
// dropped when no defaultAgent is configured (mirrors email's "no_binding"
// silent drop). For a cold chat the conversation id is minted here and the
// chat_thread binding is committed BEFORE the turn is dispatched: the turn
// runs as engine work, so its reply can fire ReplyHook at any moment after
// dispatch, and ReplyHook resolves the binding by that id -- binding after
// dispatch left a window where a fast reply found no binding and was dropped.
//
// scope is the tenancy the loop operates in. A single-tenant deploy routes all
// inbound to one scope; multi-tenant routing by bot identity is a later slice.
func (s *Service) handleInbound(ctx context.Context, scope domain.Scope, provider, chatID, subscriberID, text string) {
	if text == "" {
		return
	}

	agentIdentifier := s.defaultAgent
	conversationID := ""

	binding, err := s.core.ChatThreads().GetByChat(ctx, scope, provider, chatID)
	switch {
	case err == nil:
		// Existing binding: continue its conversation with its bound agent.
		agentIdentifier = binding.AgentIdentifier
		conversationID = binding.PlatformThreadID
	case s.defaultAgent == "":
		// Cold inbound with no default agent: drop quietly (mirror email's
		// no_binding silent drop) so an unconfigured loop does not error-loop.
		s.log.Info("chatloop: inbound on unbound chat and no default agent, dropped",
			"provider", provider, "chat_id", chatID)
		return
	default:
		// Cold inbound with a default agent: mint the conversation id and
		// bind the chat to it before dispatching, so the binding is visible
		// to ReplyHook by the time the turn can produce a reply.
		conversationID = corestore.NewULID()
		s.log.Info("chatloop: cold inbound, minting conversation",
			"provider", provider, "chat_id", chatID, "agent", agentIdentifier, "conversation", conversationID)
		if _, err := s.core.ChatThreads().Upsert(ctx, scope, corestore.ChatThread{
			AgentIdentifier:  agentIdentifier,
			PlatformThreadID: conversationID,
			SubscriberID:     subscriberID,
			Provider:         provider,
			ChatID:           chatID,
		}); err != nil {
			s.log.Error("chatloop: bind chat_thread", "err", err,
				"provider", provider, "chat_id", chatID, "conversation", conversationID)
			return
		}
	}

	turn := InboundTurn{
		Scope:           scope,
		ConversationID:  conversationID,
		SubscriberID:    subscriberID,
		AgentIdentifier: agentIdentifier,
		Text:            text,
	}
	convID, err := s.responder.Respond(ctx, turn)
	if err != nil {
		s.log.Error("chatloop: responder dispatch failed",
			"err", err, "provider", provider, "chat_id", chatID)
		return
	}
	if convID != "" && convID != conversationID {
		// The binding is already committed on conversationID; a responder
		// that lands the turn elsewhere would strand replies, so make it loud.
		s.log.Warn("chatloop: responder landed turn on a different conversation than bound",
			"provider", provider, "chat_id", chatID, "bound", conversationID, "landed", convID)
	}
}

// ReplyHook is the close-the-loop seam ingress calls after persisting an agent
// reply. When the conversation thread has a chat-origin binding, the reply is
// sent back into the originating chat (Telegram sendMessage to chat_id / Discord
// bot REST to channel_id). A thread with no chat binding (a pure inbox or
// email-origin conversation) is a no-op.
//
// It is best-effort: a send failure is logged but never propagated, because the
// reply is already durably persisted as the in_app activity — failing the turn
// would re-run the agent and duplicate the answer.
func (s *Service) ReplyHook(ctx context.Context, scope domain.Scope, conv agentdomain.Conversation, text string) {
	binding, err := s.core.ChatThreads().GetByThread(ctx, scope, conv.PlatformThreadID)
	if err != nil {
		// No chat binding for this thread: not a chat-origin conversation.
		return
	}
	if text == "" {
		return
	}
	var sendErr error
	switch binding.Provider {
	case providerTelegram:
		sendErr = s.telegramSender().send(ctx, binding.ChatID, text)
	case providerDiscord:
		sendErr = s.discordSender().send(ctx, binding.ChatID, text)
	case providerMastodon:
		// Sender pointer is read under the same mutex that guards its write in
		// resolveCredentialPoller: ReplyHook can fire (via StartWorkers) while
		// Start is still wiring pollers.
		s.mastodonMu.Lock()
		sender := s.mastodon
		pending := s.mastodonPending[binding.ChatID]
		s.mastodonMu.Unlock()
		if sender == nil {
			s.log.Warn("chatloop: mastodon reply hook fired but poller inactive, dropped",
				"thread", conv.PlatformThreadID, "chat_id", binding.ChatID)
			return
		}
		sendErr = sender.send(ctx, binding.ChatID, pending.statusID, pending.visibility, text)
	case providerBluesky:
		// Same write/read discipline as the mastodon case above.
		s.blueskyMu.Lock()
		sender := s.bluesky
		pending := s.blueskyPending[binding.ChatID]
		s.blueskyMu.Unlock()
		if sender == nil {
			s.log.Warn("chatloop: bluesky reply hook fired but poller inactive, dropped",
				"thread", conv.PlatformThreadID, "chat_id", binding.ChatID)
			return
		}
		sendErr = sender.send(ctx, text, pending)
	default:
		s.log.Warn("chatloop: reply hook hit unknown provider, dropped",
			"provider", binding.Provider, "thread", conv.PlatformThreadID)
		return
	}
	if sendErr != nil {
		s.log.Error("chatloop: send agent reply", "err", sendErr,
			"provider", binding.Provider, "thread", conv.PlatformThreadID, "chat_id", binding.ChatID)
		return
	}
	s.log.Info("chatloop: agent reply sent back",
		"provider", binding.Provider, "thread", conv.PlatformThreadID, "chat_id", binding.ChatID)
}

// Start launches the inbound pollers for any platform whose token is set. Each
// runs in its own goroutine until ctx is cancelled. A platform with an empty
// token is skipped (no-op), so a deploy with only one bot configured runs only
// that poller. scope is the tenancy inbound routes to.
func (s *Service) Start(ctx context.Context, scope domain.Scope) {
	if s.telegramToken != "" {
		go s.runTelegramPoll(ctx, scope)
	} else {
		s.log.Info("chatloop: telegram poller disabled (no token)")
	}
	if s.discordToken != "" {
		go s.runDiscordGateway(ctx, scope)
	} else {
		s.log.Info("chatloop: discord gateway disabled (no token)")
	}

	s.resolveCredentialPoller(ctx, scope, providerMastodon)
	s.resolveCredentialPoller(ctx, scope, providerBluesky)
}

// resolveCredentialPoller resolves providerID's active integration
// credentials via the injected CredentialSource and logs the enable/disable
// decision exactly like the token-based platforms above. A nil source, a
// no-integration result, or a lookup error all disable the provider (logged
// distinctly so an operator can tell "not configured" from "misconfigured"
// apart). Providers whose mention-poll loop is implemented are launched here;
// the rest (see #296) only surface the resolved-but-not-yet-wired decision.
func (s *Service) resolveCredentialPoller(ctx context.Context, scope domain.Scope, providerID string) {
	if s.credentialSource == nil {
		s.log.Info("chatloop: poller disabled (no credential source)", "provider", providerID)
		return
	}
	creds, err := s.credentialSource(ctx, providerID)
	if err != nil {
		s.log.Error("chatloop: resolve credentials failed", "err", err, "provider", providerID)
		return
	}
	if len(creds) == 0 {
		s.log.Info("chatloop: poller disabled (no active integration)", "provider", providerID)
		return
	}
	switch providerID {
	case providerMastodon:
		baseURL, token := creds["base_url"], creds["access_token"]
		if baseURL == "" || token == "" {
			s.log.Error("chatloop: mastodon integration missing base_url or access_token, poller disabled",
				"provider", providerID)
			return
		}
		s.mastodonMu.Lock()
		s.mastodon = newMastodonSender(token, baseURL, s.httpClient)
		s.mastodonMu.Unlock()
		s.log.Info("chatloop: credentials resolved, launching mention poller", "provider", providerID)
		go s.runMastodonPoll(ctx, scope, baseURL, token)
	case providerBluesky:
		pdsURL, identifier, appPassword := creds["pds_url"], creds["identifier"], creds["app_password"]
		if pdsURL == "" || identifier == "" || appPassword == "" {
			s.log.Error("chatloop: bluesky integration missing pds_url, identifier, or app_password, poller disabled",
				"provider", providerID)
			return
		}
		blueskyHC := s.httpClient
		if blueskyHC == nil {
			// pds_url is operator-supplied (integrations table); guard the
			// fallback client against SSRF like connectivity.deliveryClient()
			// does for the outbound path, since bluesky.Client would
			// otherwise fall back to a raw, unguarded http.Client.
			blueskyHC = httpaction.NewSSRFSafeClientWithTimeout(10 * time.Second)
		}
		client := &bluesky.Client{
			PDSURL:      pdsURL,
			Identifier:  identifier,
			AppPassword: appPassword,
			HTTPClient:  blueskyHC,
		}
		s.blueskyMu.Lock()
		s.bluesky = newBlueskySender(client)
		s.blueskyMu.Unlock()
		s.log.Info("chatloop: credentials resolved, launching mention poller", "provider", providerID)
		go s.runBlueskyPoll(ctx, scope, client)
	default:
		// TODO: launch the mention-poll loop once implemented (see #296).
		s.log.Info("chatloop: credentials resolved, poller not yet implemented", "provider", providerID)
	}
}
