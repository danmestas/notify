// Package identity mints and authenticates API keys, binding each presented
// key to the tenancy Scope it authorises. Keys are stored as a SHA-256 hash
// (the lookup PK) plus an AES-256-GCM encryption of the raw key bytes, so the
// plaintext key is recoverable for display/rotation but never persisted in the
// clear.
package identity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/store"
)

// keyLen is the number of random bytes in a freshly minted API key.
const keyLen = 32

// Service issues and verifies API keys against the backing store.
type Service struct {
	store *store.Store
	gcm   cipher.AEAD
}

// New builds a Service. masterKey must be exactly 32 bytes (AES-256-GCM); New
// panics otherwise, since a misconfigured key is a deploy-time programmer
// error rather than a runtime condition.
func New(s *store.Store, masterKey []byte) *Service {
	if len(masterKey) != 32 {
		panic(fmt.Sprintf("identity: masterKey must be 32 bytes for AES-256-GCM, got %d", len(masterKey)))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		panic(fmt.Sprintf("identity: aes.NewCipher: %v", err))
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("identity: cipher.NewGCM: %v", err))
	}
	return &Service{store: s, gcm: gcm}
}

// Bootstrap provisions a fresh tenant: an Organization, an Environment (which
// mints the Scope), and an ApiKey authorising that Scope. It returns the raw
// hex key (shown once to the caller) and the minted Scope.
func (svc *Service) Bootstrap(ctx context.Context, orgName, envIdentifier string) (string, domain.Scope, error) {
	org, err := svc.store.Organizations().Create(ctx, orgName)
	if err != nil {
		return "", domain.Scope{}, fmt.Errorf("identity: create org: %w", err)
	}

	_, scope, err := svc.store.Environments().Create(ctx, org.ID, domain.Environment{
		Name:       envIdentifier,
		Identifier: envIdentifier,
	})
	if err != nil {
		return "", domain.Scope{}, fmt.Errorf("identity: create env: %w", err)
	}

	raw := make([]byte, keyLen)
	if _, err := rand.Read(raw); err != nil {
		return "", domain.Scope{}, fmt.Errorf("identity: mint key: %w", err)
	}

	enc, err := svc.encrypt(raw)
	if err != nil {
		return "", domain.Scope{}, fmt.Errorf("identity: encrypt key: %w", err)
	}

	if _, err := svc.store.ApiKeys().Create(ctx, scope, domain.APIKey{
		Hash:   hashKey(raw),
		EncKey: enc,
	}); err != nil {
		return "", domain.Scope{}, fmt.Errorf("identity: create api key: %w", err)
	}

	return hex.EncodeToString(raw), scope, nil
}

// Authenticate resolves a presented hex key back to its Scope, hashing it and
// looking up the stored row. A miss returns the store's not-found error.
func (svc *Service) Authenticate(ctx context.Context, presentedKey string) (domain.Scope, error) {
	raw, err := hex.DecodeString(presentedKey)
	if err != nil {
		return domain.Scope{}, fmt.Errorf("identity: decode key: %w", err)
	}
	_, scope, err := svc.store.ApiKeys().GetByHash(ctx, hashKey(raw))
	if err != nil {
		return domain.Scope{}, err
	}
	return scope, nil
}

// Middleware authenticates the `Authorization: ApiKey <key>` header, attaching
// the resolved Scope to the request context on success and answering 401 on a
// missing, malformed, or unrecognised key.
func (svc *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "ApiKey "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		key := strings.TrimSpace(strings.TrimPrefix(header, prefix))
		if key == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		scope, err := svc.Authenticate(r.Context(), key)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(domain.ContextWithScope(r.Context(), scope)))
	})
}

// ScopeFrom extracts the request Scope set by Middleware. It panics when the
// Scope is absent: that means the route was not mounted behind Middleware, a
// programmer error rather than a recoverable runtime condition.
func ScopeFrom(ctx context.Context) domain.Scope {
	scope, ok := domain.ScopeFromContext(ctx)
	if !ok {
		panic("identity: no scope in context (route not behind Middleware)")
	}
	return scope
}

// encrypt seals plaintext with AES-256-GCM, prepending the random 12-byte
// nonce to the ciphertext.
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
		return nil, fmt.Errorf("identity: ciphertext shorter than nonce")
	}
	return svc.gcm.Open(nil, enc[:ns], enc[ns:], nil)
}

// hashKey returns the hex SHA-256 of the raw key bytes, used as the api_keys PK.
func hashKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
