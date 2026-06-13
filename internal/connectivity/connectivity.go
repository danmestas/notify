// Package connectivity manages channel integrations: it stores provider
// credentials encrypted at rest (AES-256-GCM) and resolves the active provider
// for a Scope. Credentials are sealed with the deploy master key and never
// persisted in the clear; ResolveEmail decrypts them on demand and constructs a
// ready-to-send providers.EmailProvider.
package connectivity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
	"github.com/danmestas/notify/internal/providers/cloudflareemail"
	"github.com/danmestas/notify/internal/providers/ntfy"
	"github.com/danmestas/notify/internal/store"
)

// ErrPrimaryConflict is returned by Update when the caller promotes an
// integration to primary and another row for the same channel already holds
// primary_provider=1. The HTTP handler converts this to 409 Conflict.
type ErrPrimaryConflict struct {
	ExistingID string // ID of the row that is already primary
}

func (e ErrPrimaryConflict) Error() string {
	return "connectivity: primary conflict: existing primary id " + e.ExistingID
}

// UpdateInput carries the fields that callers may change on an existing
// integration. Credential fields that are the sentinel value "••••••"
// (CredentialMaskSentinel) are treated as "leave unchanged"; only fields
// with a different, non-empty value are re-encrypted and stored.
type UpdateInput struct {
	Identifier  string
	Name        string // display label; stored in configurations JSON
	Primary     bool
	Priority    int
	Credentials map[string]string // keyed by config JSON field name
}

// CredentialMaskSentinel is the placeholder value the edit form sends when the
// operator has not changed a secret field. The server treats this as "keep the
// stored encrypted value unchanged."
const CredentialMaskSentinel = "••••••"

// SMTPConfig is the credential payload for an SMTP email integration. It is
// JSON-encoded and AES-256-GCM-sealed into Integration.EncCredentials.
type SMTPConfig struct {
	Addr     string `json:"addr"` // host:port
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

// NtfyConfig is the credential payload for an ntfy push integration: the
// server base URL and access token, JSON-encoded and AES-256-GCM-sealed into
// Integration.EncCredentials. The per-subscriber topic lives in
// subscriber_channels, not here.
type NtfyConfig struct {
	BaseURL string `json:"base_url"`
	Token   string `json:"token"`
}

// CloudflareEmailConfig is the credential payload for a Cloudflare Email
// Sending integration. It is JSON-encoded and AES-256-GCM-sealed into
// Integration.EncCredentials.
type CloudflareEmailConfig struct {
	AccountID string `json:"account_id"`
	APIToken  string `json:"api_token"`
	From      string `json:"from"`
}

// Service stores integrations and resolves active providers per Scope.
type Service struct {
	store *store.Store
	gcm   cipher.AEAD
}

// New builds a Service. masterKey must be exactly 32 bytes (AES-256-GCM); New
// panics otherwise, since a misconfigured key is a deploy-time programmer error
// rather than a runtime condition.
func New(s *store.Store, masterKey []byte) *Service {
	if len(masterKey) != 32 {
		panic(fmt.Sprintf("connectivity: masterKey must be 32 bytes for AES-256-GCM, got %d", len(masterKey)))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(fmt.Sprintf("connectivity: aes.NewCipher: %v", err))
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("connectivity: cipher.NewGCM: %v", err))
	}
	return &Service{store: s, gcm: gcm}
}

// CreateSMTP stores an SMTP email integration for the Scope: it AES-GCM-encrypts
// the JSON-encoded cfg into EncCredentials, with ProviderID="smtp" and
// Channel="email". When primary is set the integration becomes the preferred
// provider for the channel.
func (svc *Service) CreateSMTP(ctx context.Context, scope domain.Scope, identifier string, cfg SMTPConfig, primary bool) (domain.Integration, error) {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: marshal smtp config: %w", err)
	}
	enc, err := svc.encrypt(plaintext)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: encrypt smtp config: %w", err)
	}
	return svc.store.Integrations().Create(ctx, scope, domain.Integration{
		ProviderID:     "smtp",
		Channel:        "email",
		EncCredentials: enc,
		Active:         true,
		Primary:        primary,
		Identifier:     identifier,
	})
}

// CreateCloudflareEmail stores a Cloudflare Email Sending integration for the
// Scope: it AES-GCM-encrypts the JSON-encoded cfg into EncCredentials, with
// ProviderID="cloudflare-email" and Channel="email". When primary is set the
// integration becomes the preferred provider for the channel.
func (svc *Service) CreateCloudflareEmail(ctx context.Context, scope domain.Scope, identifier string, cfg CloudflareEmailConfig, primary bool) (domain.Integration, error) {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: marshal cloudflare email config: %w", err)
	}
	enc, err := svc.encrypt(plaintext)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: encrypt cloudflare email config: %w", err)
	}
	return svc.store.Integrations().Create(ctx, scope, domain.Integration{
		ProviderID:     "cloudflare-email",
		Channel:        "email",
		EncCredentials: enc,
		Active:         true,
		Primary:        primary,
		Identifier:     identifier,
	})
}

// ResolveEmail returns the active email provider, From address, provider ID,
// and integration ID for the Scope. It loads the active email integration,
// decrypts its credentials, and builds the appropriate provider based on the
// integration's ProviderID:
//
//   - "smtp"             → *providers.SMTP (existing path, unchanged)
//   - "cloudflare-email" → *cloudflareemail.Client
//
// Return order: (provider, from, providerID, integrationID, error).
// The integration ID is the row's primary key; callers use it to populate
// domain.Message.IntegrationID (BP-13b). The providerID is the actual provider
// string from the integration row, fixing the hardcoded "smtp" bug (#20).
func (svc *Service) ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, string, string, error) {
	in, err := svc.store.Integrations().GetActiveByChannel(ctx, scope, "email")
	if err != nil {
		return nil, "", "", "", err
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("connectivity: decrypt email config: %w", err)
	}

	providerID := in.ProviderID
	if providerID == "" {
		providerID = "smtp" // normalise legacy rows that predate the field
	}

	switch in.ProviderID {
	case "cloudflare-email":
		var cfg CloudflareEmailConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return nil, "", "", "", fmt.Errorf("connectivity: unmarshal cloudflare email config: %w", err)
		}
		return &cloudflareemail.Client{
			AccountID: cfg.AccountID,
			APIToken:  cfg.APIToken,
			From:      cfg.From,
		}, cfg.From, providerID, in.ID, nil
	case "smtp", "": // empty ProviderID covers legacy rows created before the field was enforced.
		var cfg SMTPConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return nil, "", "", "", fmt.Errorf("connectivity: unmarshal smtp config: %w", err)
		}
		return &providers.SMTP{
			Addr:     cfg.Addr,
			Host:     cfg.Host,
			Username: cfg.Username,
			Password: cfg.Password,
		}, cfg.From, providerID, in.ID, nil
	default:
		// Fail closed: decoding unknown-provider credentials as SMTPConfig
		// would surface as a confusing empty-Addr dial failure at send time.
		return nil, "", "", "", fmt.Errorf("connectivity: unknown email provider %q", in.ProviderID)
	}
}

// CreateNtfy stores an ntfy push integration for the Scope: it AES-GCM-encrypts
// the JSON-encoded cfg into EncCredentials, with ProviderID="ntfy" and
// Channel="push". When primary is set the integration becomes the preferred
// provider for the channel.
func (svc *Service) CreateNtfy(ctx context.Context, scope domain.Scope, identifier string, cfg NtfyConfig, primary bool) (domain.Integration, error) {
	plaintext, err := json.Marshal(cfg)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: marshal ntfy config: %w", err)
	}
	enc, err := svc.encrypt(plaintext)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: encrypt ntfy config: %w", err)
	}
	return svc.store.Integrations().Create(ctx, scope, domain.Integration{
		ProviderID:     "ntfy",
		Channel:        "push",
		EncCredentials: enc,
		Active:         true,
		Primary:        primary,
		Identifier:     identifier,
	})
}

// ResolvePush returns the active push provider, its provider id, and
// integration ID for the Scope. It loads the active push integration,
// decrypts its credentials into an NtfyConfig, and builds a *ntfy.Client.
// The integration ID (third return) is used by delivery/push.go to populate
// domain.Message.IntegrationID (BP-13b).
func (svc *Service) ResolvePush(ctx context.Context, scope domain.Scope) (providers.PushProvider, string, string, error) {
	in, err := svc.store.Integrations().GetActiveByChannel(ctx, scope, "push")
	if err != nil {
		return nil, "", "", err
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return nil, "", "", fmt.Errorf("connectivity: decrypt ntfy config: %w", err)
	}
	var cfg NtfyConfig
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return nil, "", "", fmt.Errorf("connectivity: unmarshal ntfy config: %w", err)
	}
	return &ntfy.Client{BaseURL: cfg.BaseURL, Token: cfg.Token}, in.ProviderID, in.ID, nil
}

// Store returns the underlying store.Store so callers (e.g. httpapi) can
// invoke repo methods (List, Delete, SetActive) that do not require credential
// handling in the connectivity layer.
func (svc *Service) Store() *store.Store { return svc.store }

// DecryptCredentials exposes the AES-GCM decryption for httpapi so it can
// build masked credential maps for GET responses without re-implementing the
// cipher. The caller must not log or return the plaintext directly.
func (svc *Service) DecryptCredentials(enc []byte) ([]byte, error) {
	return svc.decrypt(enc)
}

// Update applies mutable changes to an existing integration. Credential fields
// in inp.Credentials that equal CredentialMaskSentinel (or are empty) are kept
// as-is from the stored encrypted blob; only fields with new values are
// re-encrypted. If inp.Primary is true and another row for the same channel
// already holds primary_provider=1, Update returns ErrPrimaryConflict before
// touching the store. The caller may retry with force=true by first clearing
// the conflict row's primary flag externally, or by passing a different
// primary value.
func (svc *Service) Update(ctx context.Context, scope domain.Scope, id string, inp UpdateInput) (domain.Integration, error) {
	// Load the existing row so we can merge credentials.
	existing, err := svc.store.Integrations().GetByID(ctx, scope, id)
	if err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: update: load existing: %w", err)
	}

	// Primary-conflict check: if we are promoting this row to primary, verify
	// no other row in the same channel already holds primary_provider=1.
	if inp.Primary && !existing.Primary {
		all, err := svc.store.Integrations().List(ctx, scope)
		if err != nil {
			return domain.Integration{}, fmt.Errorf("connectivity: update: list for primary check: %w", err)
		}
		for _, row := range all {
			if row.Channel == existing.Channel && row.Primary && row.ID != id {
				return domain.Integration{}, ErrPrimaryConflict{ExistingID: row.ID}
			}
		}
	}

	// Credential merge: decrypt the stored blob, overlay new non-sentinel fields,
	// re-encrypt.
	newEnc := existing.EncCredentials
	if len(inp.Credentials) > 0 {
		plaintext, err := svc.decrypt(existing.EncCredentials)
		if err != nil {
			return domain.Integration{}, fmt.Errorf("connectivity: update: decrypt existing: %w", err)
		}

		// Unmarshal into a raw map so we can overlay any provider's fields
		// without knowing the concrete struct type.
		var fields map[string]any
		if err := json.Unmarshal(plaintext, &fields); err != nil {
			return domain.Integration{}, fmt.Errorf("connectivity: update: unmarshal existing credentials: %w", err)
		}

		changed := false
		for k, v := range inp.Credentials {
			if v == CredentialMaskSentinel || v == "" {
				// Caller left this field unchanged; keep the stored value.
				continue
			}
			fields[k] = v
			changed = true
		}

		if changed {
			merged, err := json.Marshal(fields)
			if err != nil {
				return domain.Integration{}, fmt.Errorf("connectivity: update: marshal merged credentials: %w", err)
			}
			enc, err := svc.encrypt(merged)
			if err != nil {
				return domain.Integration{}, fmt.Errorf("connectivity: update: encrypt merged credentials: %w", err)
			}
			newEnc = enc
		}
	}

	updated := domain.Integration{
		ID:             id,
		OrgID:          scope.OrgID,
		EnvID:          scope.EnvID,
		ProviderID:     existing.ProviderID,
		Channel:        existing.Channel,
		EncCredentials: newEnc,
		Configurations: existing.Configurations,
		Active:         existing.Active,
		Primary:        inp.Primary,
		Priority:       inp.Priority,
		Identifier:     inp.Identifier,
	}
	if err := svc.store.Integrations().Update(ctx, scope, updated); err != nil {
		return domain.Integration{}, fmt.Errorf("connectivity: update: store: %w", err)
	}
	// Return without enc_credentials so callers get a safe DTO.
	updated.EncCredentials = nil
	return updated, nil
}

// VerifyResult is the outcome of a credential verification probe (W4 ADR-013
// credential-verify durable action). OK reports whether the probe succeeded;
// Detail carries a short human message (provider id, endpoint reached, or the
// failure cause) suitable for surfacing in the console and the engine run
// output. No secret is ever included.
type VerifyResult struct {
	OK         bool   `json:"ok"`
	ProviderID string `json:"providerId"`
	Channel    string `json:"channel"`
	Detail     string `json:"detail"`
}

// VerifyByID performs a real credentials probe against the provider configured
// on integration id, scoped to scope. It resolves the integration, decrypts its
// stored credentials, and attempts a provider-appropriate handshake that
// validates the credentials WITHOUT delivering a message to a subscriber:
//
//   - smtp: dial the server and run AUTH (no MAIL/DATA);
//   - cloudflare-email: an authenticated GET against the account email scope;
//   - ntfy: an authenticated GET against the server root.
//
// A nil error with VerifyResult.OK=false means "reached the provider, but the
// credentials were rejected" — a definite negative. A non-nil error means the
// probe could not be completed (integration missing, decrypt failure, malformed
// config); the caller decides whether that is retryable.
func (svc *Service) VerifyByID(ctx context.Context, scope domain.Scope, id string) (VerifyResult, error) {
	in, err := svc.store.Integrations().GetByID(ctx, scope, id)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("connectivity: verify: load integration: %w", err)
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("connectivity: verify: decrypt: %w", err)
	}

	providerID := in.ProviderID
	if providerID == "" {
		providerID = "smtp"
	}
	res := VerifyResult{ProviderID: providerID, Channel: in.Channel}

	switch providerID {
	case "smtp", "":
		var cfg SMTPConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return VerifyResult{}, fmt.Errorf("connectivity: verify: unmarshal smtp config: %w", err)
		}
		ok, detail := probeSMTP(cfg)
		res.OK, res.Detail = ok, detail
		return res, nil
	case "cloudflare-email":
		var cfg CloudflareEmailConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return VerifyResult{}, fmt.Errorf("connectivity: verify: unmarshal cloudflare config: %w", err)
		}
		ok, detail := probeCloudflareEmail(ctx, cfg)
		res.OK, res.Detail = ok, detail
		return res, nil
	case "ntfy":
		var cfg NtfyConfig
		if err := json.Unmarshal(plaintext, &cfg); err != nil {
			return VerifyResult{}, fmt.Errorf("connectivity: verify: unmarshal ntfy config: %w", err)
		}
		ok, detail := probeNtfy(ctx, cfg)
		res.OK, res.Detail = ok, detail
		return res, nil
	default:
		res.OK = false
		res.Detail = "unsupported provider for verification: " + providerID
		return res, nil
	}
}

// encrypt seals plaintext with AES-256-GCM, prepending the random 12-byte nonce
// to the ciphertext.
func (svc *Service) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, svc.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return svc.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt reverses encrypt: it splits the prepended nonce and opens the
// ciphertext.
func (svc *Service) decrypt(enc []byte) ([]byte, error) {
	ns := svc.gcm.NonceSize()
	if len(enc) < ns {
		return nil, fmt.Errorf("connectivity: ciphertext shorter than nonce")
	}
	return svc.gcm.Open(nil, enc[:ns], enc[ns:], nil)
}
