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

	agentdomain "github.com/danmestas/notify/internal/agents/domain"
	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// Provider names stored in chat_threads.provider and used to pick the reply
// sender. They double as the platform tag passed to handleInbound.
const (
	providerTelegram = "telegram"
	providerDiscord  = "discord"
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

	// Overridable in tests. Empty values fall back to the public hosts.
	telegramBaseURL string
	discordAPIBase  string
	httpClient      *http.Client

	telegram *telegramSender
	discord  *discordSender

	// discordDialer lets tests point the gateway at an in-process WS server.
	// nil uses the default dialer that connects to Discord's real gateway URL.
	discordDialer gatewayDialer
}

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

// withHTTPClient overrides the HTTP client used by pollers and senders. Test
// seam.
func (s *Service) withHTTPClient(hc *http.Client) *Service { s.httpClient = hc; return s }

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
// silent drop). After the responder threads the turn, the returned conversation
// id is upserted as the chat_thread's platform_thread_id so the ReplyHook can
// find it.
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
		// Cold inbound with a default agent: mint a new conversation.
		s.log.Info("chatloop: cold inbound, minting conversation",
			"provider", provider, "chat_id", chatID, "agent", agentIdentifier)
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

	if _, err := s.core.ChatThreads().Upsert(ctx, scope, corestore.ChatThread{
		AgentIdentifier:  agentIdentifier,
		PlatformThreadID: convID,
		SubscriberID:     subscriberID,
		Provider:         provider,
		ChatID:           chatID,
	}); err != nil {
		s.log.Error("chatloop: bind chat_thread", "err", err,
			"provider", provider, "chat_id", chatID, "conversation", convID)
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
}
