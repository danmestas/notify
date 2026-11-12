package connectivity

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/store"
)

// SMSCallback is what a provider's inbound or status callback needs from the
// SMS integration its URL names.
type SMSCallback struct {
	// Scope is the integration's org and environment: the callback acts there
	// and nowhere else.
	Scope         domain.Scope
	IntegrationID string
	// ProviderID is "twilio" or "signalwire"; it picks the signature scheme.
	ProviderID string
	// SigningKey signs the provider's callbacks: Twilio's auth token, or
	// SignalWire's Signing Key. Empty means no callback can be verified.
	SigningKey string
	// From is the integration's sender number.
	From string
	// HelpText is the configured reply to HELP; empty means the default.
	HelpText string
	// Sender sends through the integration, for the HELP reply.
	Sender providers.SMSProvider
}

// SMSCallback resolves integrationID, in whatever scope it lives, for a
// provider callback. It is store.ErrNotFound unless the id names an active
// Twilio or SignalWire SMS integration. The caller must verify the callback's signature
// with SigningKey before acting in Scope.
func (svc *Service) SMSCallback(ctx context.Context, integrationID string) (SMSCallback, error) {
	in, err := svc.store.Integrations().LookupUnscoped(ctx, integrationID)
	if err != nil {
		return SMSCallback{}, err
	}
	// An inactive integration answers nothing and sends nothing.
	if !in.Active || in.Channel != "sms" || (in.ProviderID != "twilio" && in.ProviderID != "signalwire") {
		return SMSCallback{}, store.ErrNotFound
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return SMSCallback{}, fmt.Errorf("connectivity: decrypt sms config: %w", err)
	}
	cb := SMSCallback{
		Scope:         domain.Scope{OrgID: in.OrgID, EnvID: in.EnvID},
		IntegrationID: in.ID,
		ProviderID:    in.ProviderID,
	}
	switch in.ProviderID {
	case "twilio":
		var cfg TwilioConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return SMSCallback{}, fmt.Errorf("connectivity: unmarshal twilio config: %w", err)
		}
		cb.SigningKey, cb.From, cb.HelpText = cfg.AuthToken, cfg.From, cfg.HelpText
	case "signalwire":
		var cfg SignalWireConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return SMSCallback{}, fmt.Errorf("connectivity: unmarshal signalwire config: %w", err)
		}
		cb.SigningKey, cb.From, cb.HelpText = cfg.SigningKey, cfg.From, cfg.HelpText
	}
	cb.Sender, err = svc.SMSProviderFromIntegration(in)
	if err != nil {
		return SMSCallback{}, err
	}
	return cb, nil
}
