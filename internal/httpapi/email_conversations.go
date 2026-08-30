package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/agents/ingress"
	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/emailloop"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
)

// emailConversationRequest is the POST
// /v1/agents/{identifier}/conversations/email body.
type emailConversationRequest struct {
	SubscriberID string `json:"subscriberId"`
	Text         string `json:"text"`
}

// emailConversationResponse reports the new conversation and the reply-to
// mailbox the loop is now armed with, so the caller can see the loop is live
// without a separate lookup.
type emailConversationResponse struct {
	ConversationID string `json:"conversationId"`
	ReplyTo        string `json:"replyTo"`
}

// postAgentEmailConversation handles POST
// /v1/agents/{identifier}/conversations/email: it starts an email-origin
// agent conversation on behalf of a subscriber and arms the email two-way
// loop so the subscriber's reply threads back to the agent.
//
// This is the missing link closing #317: ingress.ComposeMessage alone creates
// a conversation and its first turn, but nothing ever wrote the
// email_threads binding a reply needs to resolve back to it. This handler
// arms THEN composes: the turn is dispatched as engine work and may finish
// (firing emailloop.ReplyHook) before this handler function returns, so the
// binding must already exist by the time ComposeMessage starts the run — an
// arm-after-compose ordering can lose the agent's first reply to a
// best-effort, no-retry lookup miss. A binding armed for a conversation that
// never composes is a harmless orphan; a running turn with no binding is not.
func postAgentEmailConversation(ing *ingress.Service, eml *emailloop.Service, aud *audience.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req emailConversationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if req.SubscriberID == "" {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "subscriberId is required")
			return
		}
		if req.Text == "" {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "text is required")
			return
		}

		// Refuse rather than hand back a reply-to nobody can reply to: with no
		// inbound shared secret configured, HandleInbound 503s every message,
		// so the returned mailbox would be decorative (#317 is precisely about
		// a loop that looks live and is not).
		if !eml.InboundEnabled() {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity",
				"inbound email is not configured (no inbound shared secret), so a reply-to cannot be honoured")
			return
		}

		scope := identity.ScopeFrom(r.Context())
		agentIdentifier := chi.URLParam(r, "identifier")

		sub, err := aud.Get(r.Context(), scope, req.SubscriberID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeJSONError(w, http.StatusNotFound, "not_found", "subscriber not found")
				return
			}
			writeInternalError(w, r, err)
			return
		}
		if sub.Email == "" {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "subscriber has no email address")
			return
		}

		inboundDomain, ok := eml.VerifiedInboundDomain(r.Context(), scope)
		if !ok {
			writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", "no verified inbound domain for this scope")
			return
		}

		threadID := store.NewULID()
		if err := eml.ArmEmailThread(r.Context(), scope, agentIdentifier, threadID, req.SubscriberID, sub.Email, inboundDomain); err != nil {
			writeInternalError(w, r, err)
			return
		}

		res, err := ing.ComposeMessage(r.Context(), scope, agentIdentifier, threadID, req.SubscriberID, req.Text)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "not_found", "agent not found")
			case errors.Is(err, ingress.ErrUnknownRuntime):
				writeJSONError(w, http.StatusUnprocessableEntity, "unprocessable_entity", err.Error())
			default:
				writeInternalError(w, r, err)
			}
			return
		}

		replyTo, ok := eml.ReplyToAddress(r.Context(), scope, res.ConversationID, req.SubscriberID)
		if !ok {
			writeInternalError(w, r, errors.New("reply-to address unavailable after arming"))
			return
		}

		writeJSON(w, http.StatusCreated, emailConversationResponse{
			ConversationID: res.ConversationID,
			ReplyTo:        replyTo,
		})
	}
}
