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
	"github.com/danmestas/notify/internal/providers/ntfy"
	"github.com/danmestas/notify/internal/store"
)

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

// ResolveEmail returns the active email provider and From address for the Scope.
// It loads the active email integration, decrypts its credentials into an
// SMTPConfig, and builds a *providers.SMTP from them.
func (svc *Service) ResolveEmail(ctx context.Context, scope domain.Scope) (providers.EmailProvider, string, error) {
	in, err := svc.store.Integrations().GetActiveByChannel(ctx, scope, "email")
	if err != nil {
		return nil, "", err
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return nil, "", fmt.Errorf("connectivity: decrypt smtp config: %w", err)
	}
	var cfg SMTPConfig
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return nil, "", fmt.Errorf("connectivity: unmarshal smtp config: %w", err)
	}
	return &providers.SMTP{
		Addr:     cfg.Addr,
		Host:     cfg.Host,
		Username: cfg.Username,
		Password: cfg.Password,
	}, cfg.From, nil
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

// ResolvePush returns the active push provider and its provider id for the
// Scope. It loads the active push integration, decrypts its credentials into
// an NtfyConfig, and builds a *ntfy.Client from them.
func (svc *Service) ResolvePush(ctx context.Context, scope domain.Scope) (providers.PushProvider, string, error) {
	in, err := svc.store.Integrations().GetActiveByChannel(ctx, scope, "push")
	if err != nil {
		return nil, "", err
	}
	plaintext, err := svc.decrypt(in.EncCredentials)
	if err != nil {
		return nil, "", fmt.Errorf("connectivity: decrypt ntfy config: %w", err)
	}
	var cfg NtfyConfig
	if err := json.Unmarshal(plaintext, &cfg); err != nil {
		return nil, "", fmt.Errorf("connectivity: unmarshal ntfy config: %w", err)
	}
	return &ntfy.Client{BaseURL: cfg.BaseURL, Token: cfg.Token}, in.ProviderID, nil
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
