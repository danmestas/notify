// Package smsloop receives what an SMS provider (Twilio, SignalWire) sends
// back to notify: inbound texts and delivery-status callbacks, at
//
//	POST /v1/inbound/sms/{integrationId}
//	POST /v1/inbound/sms/{integrationId}/status
//
// The routes carry no API key. The integration id in the URL names the tenant,
// and the provider's signature over the public URL, made with that
// integration's own key, is the only authentication: a request that does not
// verify is refused with 403 before anything is read or written.
//
// An inbound text is kept in the inbound message store and handed to the
// tenant's webhook endpoints as an sms.inbound event. STOP and its synonyms
// opt the number out (sms.opt_out); START and its synonyms opt it back in
// (sms.opt_in); HELP and INFO get a reply. A status callback moves the sent
// message to delivered or error, once, and never backwards.
package smsloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phonenum"
	"github.com/danmestas/notify/internal/preferences"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/signalwire"
	"github.com/danmestas/notify/internal/providers/twilio"
	"github.com/danmestas/notify/internal/store"
)

// Event types handed to the tenant's webhook endpoints.
const (
	EventInbound = "sms.inbound"
	EventOptOut  = "sms.opt_out"
	EventOptIn   = "sms.opt_in"
	EventStatus  = "sms.status"
)

// maxBody bounds a callback body. Provider callbacks are a few hundred bytes;
// a long text with media URLs stays far below this.
const maxBody = 64 << 10

// Callbacks resolves the integration a callback URL names, in any scope.
// connectivity.Service satisfies it.
type Callbacks interface {
	SMSCallback(ctx context.Context, integrationID string) (connectivity.SMSCallback, error)
}

// EventPublisher hands an event to the delivery stream the outbound webhooks
// consume. eventbus.Bus satisfies it.
type EventPublisher interface {
	PublishDelivery(environmentID string, data []byte) error
}

// Service handles the SMS provider callbacks.
type Service struct {
	st        *store.Store
	cbs       Callbacks
	prefs     *preferences.Service
	publicURL string
	events    EventPublisher
	log       *slog.Logger
}

// New builds the service. publicURL is the origin and mount path notify is
// reached at (NOTIFY_PUBLIC_BASE_URL plus any base path): providers sign the
// URL they call, so it is what every signature is checked against. With it
// empty no signature can be checked and every callback is refused with 503.
func New(st *store.Store, cbs Callbacks, prefs *preferences.Service, publicURL string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, cbs: cbs, prefs: prefs, publicURL: strings.TrimRight(publicURL, "/"), log: log}
}

// WithEventPublisher sets where tenant events go. Without one, texts are
// still stored and keywords still applied, but the tenant is not told.
func (s *Service) WithEventPublisher(p EventPublisher) *Service {
	s.events = p
	return s
}

// Routes mounts the two callback routes on r, outside any API-key group.
func (s *Service) Routes(r chi.Router) {
	r.Post("/v1/inbound/sms/{integrationId}", s.HandleInbound)
	r.Post("/v1/inbound/sms/{integrationId}/status", s.HandleStatus)
}

// Event is what the tenant's webhook endpoints receive for an inbound text,
// an opt-out or an opt-in. OrgID and EnvID route it, like every event on the
// delivery stream.
type Event struct {
	OrgID string `json:"OrgID"`
	EnvID string `json:"EnvID"`

	Type string `json:"type"`
	// SubscriberID is the subscriber with the sender's phone, absent when no
	// subscriber has it.
	SubscriberID string `json:"subscriberId,omitempty"`
	// PhoneVerified is false when the matched subscriber has not verified the
	// phone, or there is no match.
	PhoneVerified bool   `json:"phoneVerified"`
	From          string `json:"from"`
	To            string `json:"to"`
	Body          string `json:"body,omitempty"`
	// Keyword is the normalised keyword of an opt-out or opt-in.
	Keyword          string `json:"keyword,omitempty"`
	IntegrationID    string `json:"integrationId"`
	ProviderID       string `json:"providerId"`
	ProviderMessage  string `json:"providerMessageId"`
	InboundMessageID string `json:"inboundMessageId,omitempty"`
	ReceivedAt       string `json:"receivedAt"`
}

// StatusEvent is what the tenant's webhook endpoints receive when a status
// callback moves a sent message to delivered or error.
type StatusEvent struct {
	OrgID string `json:"OrgID"`
	EnvID string `json:"EnvID"`

	Type              string `json:"type"`
	MessageID         string `json:"messageId"`
	Status            string `json:"status"`
	ErrorClass        string `json:"errorClass,omitempty"`
	ErrorText         string `json:"errorText,omitempty"`
	SubscriberID      string `json:"subscriberId,omitempty"`
	NotificationID    string `json:"notificationId,omitempty"`
	TransactionID     string `json:"transactionId,omitempty"`
	WorkflowID        string `json:"workflowId,omitempty"`
	IntegrationID     string `json:"integrationId"`
	ProviderMessageID string `json:"providerMessageId"`
	UpdatedAt         string `json:"updatedAt"`
}

// authenticate resolves the integration the URL names and verifies the
// provider's signature over the public URL. On failure it has written the
// response and ok is false.
func (s *Service) authenticate(w http.ResponseWriter, r *http.Request) (cb connectivity.SMSCallback, form url.Values, ok bool) {
	if s.publicURL == "" {
		http.Error(w, "inbound sms needs NOTIFY_PUBLIC_BASE_URL: provider signatures cover the public URL", http.StatusServiceUnavailable)
		return cb, nil, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return cb, nil, false
	}
	cb, err = s.cbs.SMSCallback(r.Context(), chi.URLParam(r, "integrationId"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Indistinguishable from a bad signature, so the route cannot be
		// used to probe for integration ids.
		http.Error(w, "forbidden", http.StatusForbidden)
		return cb, nil, false
	case err != nil:
		s.log.Error("smsloop: resolve integration", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return cb, nil, false
	}
	full := s.publicURL + r.URL.Path
	if r.URL.RawQuery != "" {
		full += "?" + r.URL.RawQuery
	}
	form, err = url.ParseQuery(string(raw))
	if err != nil {
		form = nil
	}
	var valid bool
	switch cb.ProviderID {
	case "twilio":
		valid = form != nil && twilio.ValidSignature(cb.SigningKey, full, form, r.Header.Get(twilio.SignatureHeader))
	case "signalwire":
		sig := r.Header.Get(signalwire.SignatureHeader)
		if sig == "" {
			sig = r.Header.Get(twilio.SignatureHeader)
		}
		valid = signalwire.ValidSignature(cb.SigningKey, full, raw, sig)
	}
	if !valid {
		s.log.Warn("smsloop: refused a callback with a bad or missing signature", "integration", cb.IntegrationID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return cb, nil, false
	}
	if form == nil {
		http.Error(w, "malformed form body", http.StatusBadRequest)
		return cb, nil, false
	}
	return cb, form, true
}

// emptyTwiML is the reply to an inbound text: nothing more to send.
const emptyTwiML = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`

// HandleInbound handles POST /v1/inbound/sms/{integrationId}: one inbound
// text.
func (s *Service) HandleInbound(w http.ResponseWriter, r *http.Request) {
	cb, form, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sid := firstOf(form, "MessageSid", "SmsSid")
	if sid == "" {
		http.Error(w, "MessageSid is required", http.StatusBadRequest)
		return
	}
	from := form.Get("From")
	if n, ok := phonenum.Normalize(from); ok {
		from = n
	}
	to := form.Get("To")
	if n, ok := phonenum.Normalize(to); ok {
		to = n
	}
	body := form.Get("Body")

	subs, err := s.st.Subscribers().ListByPhone(ctx, cb.Scope, from)
	if err != nil {
		s.fail(w, "match sender", err)
		return
	}
	var primary domain.Subscriber
	if len(subs) > 0 {
		primary = subs[0]
	}
	verified := primary.SubscriberID != "" && primary.PhoneVerifiedAt != ""

	rec, created, err := s.st.InboundMessages().RecordSMS(ctx, cb.Scope, store.InboundMessage{
		IntegrationID: cb.IntegrationID, MessageID: sid, Sender: from, Recipient: to, Body: body,
		SubscriberID: primary.SubscriberID, PhoneVerified: verified,
	})
	if err != nil {
		s.fail(w, "record inbound text", err)
		return
	}
	if !created {
		// A provider retry: everything below already happened once.
		writeTwiML(w)
		return
	}

	base := Event{
		OrgID: cb.Scope.OrgID, EnvID: cb.Scope.EnvID,
		From: from, To: to, IntegrationID: cb.IntegrationID, ProviderID: cb.ProviderID,
		ProviderMessage: sid, InboundMessageID: rec.ID, ReceivedAt: rec.ReceivedAt,
	}
	in := base
	in.Type, in.SubscriberID, in.PhoneVerified, in.Body = EventInbound, primary.SubscriberID, verified, body
	s.publish(cb.Scope, in)

	switch kw := keyword(body); kind(kw) {
	case optOut:
		err = s.optOut(ctx, cb, from, subs, base, kw)
	case optIn:
		err = s.optIn(ctx, cb, from, subs, base, kw)
	case help:
		s.reply(ctx, cb, from, to, primary.SubscriberID)
	}
	if err != nil {
		s.fail(w, "apply keyword", err)
		return
	}
	writeTwiML(w)
}

// optOut records the number as opted out, turns sms off at every matched
// subscriber's global preference, and tells the tenant once per subscriber
// (once with no subscriber when none matched).
func (s *Service) optOut(ctx context.Context, cb connectivity.SMSCallback, from string, subs []domain.Subscriber, base Event, kw string) error {
	if err := s.st.SMSOptOuts().OptOut(ctx, cb.Scope, from, cb.IntegrationID); err != nil {
		return err
	}
	return s.setSMS(ctx, cb.Scope, subs, false, base, EventOptOut, kw)
}

// optIn clears the number's opt-out and turns sms back on at every matched
// subscriber's global preference.
func (s *Service) optIn(ctx context.Context, cb connectivity.SMSCallback, from string, subs []domain.Subscriber, base Event, kw string) error {
	if err := s.st.SMSOptOuts().OptIn(ctx, cb.Scope, from); err != nil {
		return err
	}
	return s.setSMS(ctx, cb.Scope, subs, true, base, EventOptIn, kw)
}

func (s *Service) setSMS(ctx context.Context, scope domain.Scope, subs []domain.Subscriber, on bool, base Event, typ, kw string) error {
	base.Type, base.Keyword = typ, kw
	if len(subs) == 0 {
		s.publish(scope, base)
		return nil
	}
	for _, sub := range subs {
		if err := s.prefs.SetGlobalChannel(ctx, scope, sub.SubscriberID, "sms", on); err != nil {
			return err
		}
		ev := base
		ev.SubscriberID, ev.PhoneVerified = sub.SubscriberID, sub.PhoneVerifiedAt != ""
		s.publish(scope, ev)
	}
	return nil
}

// reply answers HELP through the integration that received it. It goes to
// the sender as a direct answer, so the verified-phone gate does not apply.
// The send is recorded as an sms message; a failure is recorded, not
// returned: the inbound text itself was handled.
func (s *Service) reply(ctx context.Context, cb connectivity.SMSCallback, to, ourNumber, subscriberID string) {
	text := cb.HelpText
	if text == "" {
		sender := cb.From
		if sender == "" {
			sender = ourNumber
		}
		text = fmt.Sprintf("Messages from %s. Reply STOP to unsubscribe, START to resubscribe.", sender)
	}
	from := ourNumber
	if from == "" {
		from = cb.From
	}
	msg := domain.Message{
		SubscriberID: subscriberID, Channel: "sms", ProviderID: cb.ProviderID, IntegrationID: cb.IntegrationID,
		Content: text, Status: "sent", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	res, err := cb.Sender.Send(ctx, providers.SMSMessage{To: to, From: from, Body: text})
	if err != nil {
		msg.Status, msg.ErrorText, msg.ErrorClass = "error", err.Error(), string(providers.ClassifyError(err))
		s.log.Warn("smsloop: HELP reply failed", "integration", cb.IntegrationID, "err", err)
	}
	msg.ProviderMessageID = res.ProviderMessageID
	saved, err := s.st.Messages().Create(ctx, cb.Scope, msg)
	if err != nil {
		s.log.Error("smsloop: record HELP reply", "err", err)
		return
	}
	if data, err := json.Marshal(saved); err == nil && s.events != nil {
		_ = s.events.PublishDelivery(cb.Scope.EnvID, data)
	}
}

// HandleStatus handles POST /v1/inbound/sms/{integrationId}/status: a
// delivery-status callback for a message notify sent.
func (s *Service) HandleStatus(w http.ResponseWriter, r *http.Request) {
	cb, form, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sid := firstOf(form, "MessageSid", "SmsSid")
	next, class, text := mapStatus(cb.ProviderID, firstOf(form, "MessageStatus", "SmsStatus"), form.Get("ErrorCode"), form.Get("ErrorMessage"))
	if sid == "" || next == "" {
		// Not a final status (queued, sent, ...), or nothing to act on: the
		// message stays as it is.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m, err := s.st.Messages().GetByProviderMessageID(ctx, cb.Scope, cb.IntegrationID, sid)
	if errors.Is(err, store.ErrNotFound) {
		// Sent before ids were kept, or not by notify: nothing to update, and
		// a retry would not change that.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.fail(w, "find message", err)
		return
	}
	changed, err := s.st.Messages().AdvanceStatus(ctx, cb.Scope, m.ID, next, class, text)
	if err != nil {
		s.fail(w, "update message", err)
		return
	}
	if changed {
		if next == "error" {
			// A real delivery failure, reported once: the send itself already
			// counted as a success, and only this transition counts against
			// the integration.
			s.st.ReportFailedAttempt(ctx, cb.Scope, cb.IntegrationID, class, text)
		}
		s.publish(cb.Scope, StatusEvent{
			OrgID: cb.Scope.OrgID, EnvID: cb.Scope.EnvID, Type: EventStatus,
			MessageID: m.ID, Status: next, ErrorClass: class, ErrorText: text,
			SubscriberID: m.SubscriberID, NotificationID: m.NotificationID, TransactionID: m.TransactionID,
			WorkflowID: m.WorkflowID, IntegrationID: cb.IntegrationID, ProviderMessageID: sid,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// mapStatus maps a provider MessageStatus to the final notify status it
// moves a sent message to: delivered, or error with its class. Every other
// status (accepted, queued, sending, sent, ...) maps to "" and changes
// nothing.
func mapStatus(providerID, status, code, detail string) (next, class, text string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "delivered":
		return "delivered", "", ""
	case "undelivered", "failed":
		text = fmt.Sprintf("%s: %s", providerID, strings.ToLower(status))
		if code != "" {
			text += " (error " + code + ")"
		}
		if detail != "" {
			text += ": " + detail
		}
		return "error", string(classifyCode(code)), text
	}
	return "", "", ""
}

// classifyCode maps a Twilio-compatible messaging error code (SignalWire uses
// the same codes) to notify's error class. Unknown or missing codes are
// rejected_message, like any failure the provider did not explain.
func classifyCode(code string) providers.ErrorClass {
	switch code {
	case "30003", "30004", "30005", "30006", "21610", "21614", "21211", "21612":
		// Unreachable, blocked by or unsubscribed recipient, unknown or
		// landline number: about the recipient, not the integration.
		return providers.ClassRecipient
	case "30001":
		return providers.ClassRateLimited // queue overflow
	case "30002":
		return providers.ClassAuth // account suspended
	case "30008", "30500":
		return providers.ClassProviderDown
	}
	return providers.ClassRejectedMessage
}

func (s *Service) publish(scope domain.Scope, ev any) {
	if s.events == nil {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := s.events.PublishDelivery(scope.EnvID, data); err != nil {
		s.log.Error("smsloop: publish event", "err", err)
	}
}

func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("smsloop: "+what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeTwiML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, emptyTwiML)
}

func firstOf(form url.Values, names ...string) string {
	for _, n := range names {
		if v := form.Get(n); v != "" {
			return v
		}
	}
	return ""
}

type keywordKind int

const (
	notKeyword keywordKind = iota
	optOut
	optIn
	help
)

// keyword is body as a candidate keyword: trimmed and upper-cased. Only a
// whole-message match counts, so "please stop by" is just a text.
func keyword(body string) string { return strings.ToUpper(strings.TrimSpace(body)) }

func kind(kw string) keywordKind {
	switch kw {
	case "STOP", "STOPALL", "UNSUBSCRIBE", "CANCEL", "END", "QUIT":
		return optOut
	case "START", "YES", "UNSTOP":
		return optIn
	case "HELP", "INFO":
		return help
	}
	return notKeyword
}
