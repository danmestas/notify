// Package envvars provides AES-GCM-encrypted environment variable storage.
package envvars

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"regexp"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// ValueMaskSentinel is returned in API responses when reveal=false.
const ValueMaskSentinel = "••••••"

// keyPattern is the URL-safe slug allowlist for env var keys: letters
// (upper or lower case — the "API_KEY"-style convention is uppercase),
// digits, hyphens, and underscores, starting with a letter or digit. A key
// is used verbatim as a console URL path segment (the variable detail
// route); a key containing '/' (path separator) or other reserved/URL-unsafe
// characters produces a var every console route 404s on, permanently
// orphaning it (same class as H2, see authoring.triggerIdentifierPattern).
// Unlike the trigger-identifier/topic-key allowlist, uppercase is allowed
// here because it is the established convention for env var keys.
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ErrInvalidKey is returned by Set when key fails keyPattern. Checked before
// any store write.
var ErrInvalidKey = fmt.Errorf("envvars: key may only contain letters, numbers, hyphens, and underscores")

// Service wraps EnvVarRepo with AES-GCM encrypt/decrypt for values.
type Service struct {
	repo store.EnvVarRepo
	gcm  cipher.AEAD
}

// New creates a Service using the provided store and 32-byte AES-256-GCM master key.
func New(st *store.Store, masterKey []byte) (*Service, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("envvars: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("envvars: new GCM: %w", err)
	}
	return &Service{repo: st.EnvVars(), gcm: gcm}, nil
}

// Set encrypts value and upserts the env var.
func (svc *Service) Set(ctx context.Context, scope domain.Scope, key string, isSecret bool, value string) (domain.EnvVar, error) {
	if !keyPattern.MatchString(key) {
		return domain.EnvVar{}, ErrInvalidKey
	}
	enc, err := svc.encrypt([]byte(value))
	if err != nil {
		return domain.EnvVar{}, fmt.Errorf("envvars: encrypt: %w", err)
	}
	return svc.repo.Upsert(ctx, scope, key, isSecret, enc)
}

// Get returns the env var metadata (no value decrypted).
func (svc *Service) Get(ctx context.Context, scope domain.Scope, key string) (domain.EnvVar, error) {
	return svc.repo.Get(ctx, scope, key)
}

// GetValue decrypts and returns the plaintext value.
func (svc *Service) GetValue(ctx context.Context, scope domain.Scope, key string) (string, error) {
	enc, err := svc.repo.GetValue(ctx, scope, key)
	if err != nil {
		return "", err
	}
	plain, err := svc.decrypt(enc)
	if err != nil {
		return "", fmt.Errorf("envvars: decrypt: %w", err)
	}
	return string(plain), nil
}

// List returns env vars. Values are not included in domain.EnvVar; callers
// must call GetValue for each item if they need plaintext.
func (svc *Service) List(ctx context.Context, scope domain.Scope, search, after, before string, limit int) ([]domain.EnvVar, string, string, error) {
	return svc.repo.List(ctx, scope, search, after, before, limit)
}

// Delete removes the env var.
func (svc *Service) Delete(ctx context.Context, scope domain.Scope, key string) error {
	return svc.repo.Delete(ctx, scope, key)
}

func (svc *Service) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, svc.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return svc.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (svc *Service) decrypt(enc []byte) ([]byte, error) {
	ns := svc.gcm.NonceSize()
	if len(enc) < ns {
		return nil, fmt.Errorf("envvars: ciphertext shorter than nonce")
	}
	return svc.gcm.Open(nil, enc[:ns], enc[ns:], nil)
}
