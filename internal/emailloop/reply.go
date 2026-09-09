package emailloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/notify/internal/domain"
	corestore "github.com/danmestas/notify/internal/store"
)

// Thread kinds: who answers a reply mailbox. Stored in email_threads.kind.
const (
	// KindAgent threads the reply onto an agent conversation and runs the
	// agent's turn. Every binding that predates the column is this kind.
	KindAgent = "agent"
	// KindWebhook hands the reply back to the environment's outbound webhook
	// endpoints as an email.reply event. It exists for the caller a forge
	// actually has: a notification went out, a person replied, and the thing
	// that sent the notification wants the text back (#345).
	KindWebhook = "webhook"
)

// ReplyEventType is the "type" field of the event published for a webhook-kind
// reply. Delivery-status events on the same stream carry a Channel and Status
// instead; a consumer filters on this.
const ReplyEventType = "email.reply"

// EventPublisher is the slice of the event bus a webhook-kind reply needs: the
// per-environment delivery stream that webhooksout fans out to endpoints.
type EventPublisher interface {
	PublishDelivery(environmentID string, data []byte) error
}

// WithEventPublisher attaches the bus that carries webhook-kind replies. A
// Service without one cannot honour a webhook binding and says so at Accept.
func (s *Service) WithEventPublisher(p EventPublisher) *Service {
	s.events = p
	return s
}

// ErrNoEventPublisher reports a webhook-kind binding on a Service that was
// built without a bus. It is a wiring fault, surfaced as a retryable error so
// the transport asks the sender to try again rather than acknowledging a reply
// that went nowhere.
var ErrNoEventPublisher = errors.New("emailloop: webhook-kind reply but no event publisher configured")

// ArmReply arms a webhook-kind reply thread for one recipient of one
// notification and returns the tokenized reply mailbox to put on the send.
// ok is false, with no error, when the loop cannot honour a reply for this
// scope -- inbound is not enabled, or no inbound domain is verified -- so the
// caller sends without a token rather than promising a reply it cannot take.
//
// The thread is keyed on notificationID, which is minted per recipient. The
// transaction id is not usable as the key: a multicast trigger shares one
// across every recipient, and email_threads is unique per thread.
//
// This is the delivery.ReplyArmer contract; the parameter list is primitives
// so delivery does not import this package.
func (s *Service) ArmReply(ctx context.Context, scope domain.Scope, notificationID, transactionID, workflowIdentifier, subscriberID, subscriberEmail string) (replyTo string, ok bool, err error) {
	if notificationID == "" || subscriberID == "" || subscriberEmail == "" {
		return "", false, nil
	}
	if !s.InboundEnabled() {
		return "", false, nil
	}
	dom, ok := s.verifiedInboundDomain(ctx, scope)
	if !ok {
		return "", false, nil
	}
	if _, err := s.core.EmailThreads().Upsert(ctx, scope, corestore.EmailThread{
		Kind:               KindWebhook,
		PlatformThreadID:   notificationID,
		SubscriberID:       subscriberID,
		SubscriberEmail:    subscriberEmail,
		InboundDomain:      dom,
		TransactionID:      transactionID,
		WorkflowIdentifier: workflowIdentifier,
	}); err != nil {
		return "", false, fmt.Errorf("emailloop: arm reply thread: %w", err)
	}
	return "reply+" + s.minter.Mint(scope, notificationID, subscriberID) + "@" + dom, true, nil
}

// ReplyEvent is the email.reply event published for a webhook-kind reply.
//
// OrgID and EnvID are PascalCase because that is what webhooksout reads to
// route the event -- delivery-status events are raw domain.Message JSON, which
// has no tags. The rest is the shape the consumer asked for.
type ReplyEvent struct {
	OrgID string `json:"OrgID"`
	EnvID string `json:"EnvID"`

	Type           string `json:"type"`
	SubscriberID   string `json:"subscriberId"`
	TransactionID  string `json:"transactionId"`
	Workflow       string `json:"workflow"`
	NotificationID string `json:"notificationId"`
	Text           string `json:"text"`
	From           string `json:"from,omitempty"`
	MessageID      string `json:"messageId,omitempty"`
	InReplyTo      string `json:"inReplyTo,omitempty"`
	ReceivedAt     string `json:"receivedAt"`
}

// publishReply hands a webhook-kind reply to the delivery stream. The turn's
// text is already the quoted-history-stripped body Accept selected.
func (s *Service) publishReply(rcpt Recipient, payload InboundEmail, text string) error {
	if s.events == nil {
		return ErrNoEventPublisher
	}
	ev := ReplyEvent{
		OrgID:          rcpt.Scope.OrgID,
		EnvID:          rcpt.Scope.EnvID,
		Type:           ReplyEventType,
		SubscriberID:   rcpt.Binding.SubscriberID,
		TransactionID:  rcpt.Binding.TransactionID,
		Workflow:       rcpt.Binding.WorkflowIdentifier,
		NotificationID: rcpt.Claims.ThreadID,
		Text:           text,
		From:           payload.From,
		MessageID:      payload.Headers["Message-Id"],
		InReplyTo:      payload.InReplyTo,
		ReceivedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if ev.MessageID == "" {
		ev.MessageID = payload.Headers["Message-ID"]
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := s.events.PublishDelivery(rcpt.Scope.EnvID, data); err != nil {
		return fmt.Errorf("emailloop: publish reply event: %w", err)
	}
	s.log.Info("emailloop: reply handed to webhook endpoints",
		"notification", ev.NotificationID, "transaction", ev.TransactionID,
		"workflow", ev.Workflow, "subscriber", ev.SubscriberID)
	return nil
}
