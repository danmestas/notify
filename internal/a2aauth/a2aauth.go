// Package a2aauth authenticates callers of notify's A2A channel and manages the
// machine credential they may present: the A2A client key.
//
// Two credentials are accepted, both as `Authorization: Bearer <token>`:
//
//   - a subscriber session token (a person; minted by the consumer's backend,
//     verified by identity), and
//   - an A2A client key (a machine; minted here, per environment, optionally
//     bound to one subscriber so the caller speaks as that person).
//
// Authenticate resolves either to one Principal, so the handlers behind it do
// not need to know which kind it was. Tenant API keys are NOT accepted: the
// scheme is `Bearer`, never `ApiKey`, and a tenant key is not a valid bearer
// token. Like the inbox routes, that is structural -- only routes mounted
// behind Authenticate can be reached with these credentials, and no other
// middleware accepts a client key.
//
// A client key is a recognisable-prefix token (`a2ack_<64 hex>`). The prefix
// lets the middleware tell it from a session token without a database round
// trip for the wrong kind. Only the SHA-256 hash of the secret is stored, so a
// key is shown once at mint and cannot be recovered afterwards.
package a2aauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/timestamp"
)

// TokenPrefix marks a token as an A2A client key.
const TokenPrefix = "a2ack_"

// secretLen is the number of random bytes in a client key secret.
const secretLen = 32

// maxNameLen bounds a key name.
const maxNameLen = 64

// Kind says which credential authenticated a request.
type Kind string

const (
	// KindClientKey is a machine credential minted by this package.
	KindClientKey Kind = "client_key"
	// KindSession is a subscriber session token.
	KindSession Kind = "session"
)

// touchEvery bounds how often a key's last-used time is written.
const touchEvery = time.Minute

// ErrInvalidName is returned by Mint for an empty or over-long name.
var ErrInvalidName = fmt.Errorf("a2aauth: name must be 1-%d characters", maxNameLen)

// ErrSubscriberNotFound is returned by Mint when the bound subscriber does not
// exist in the scope.
var ErrSubscriberNotFound = errors.New("a2aauth: subscriber not found")

// Principal is the authenticated caller of an A2A request.
type Principal struct {
	// Scope is the org and environment the credential belongs to. Handlers
	// must resolve every agent and task inside it.
	Scope domain.Scope
	// SubscriberID is the external id the caller speaks as. It is empty for
	// an unbound client key, which speaks as no subscriber (an anonymous
	// caller), never as any subscriber it names.
	SubscriberID string
	// CredentialID identifies the credential for audit: the key's ULID, or
	// "session:<subscriber>" for a session token (which has no stored row).
	CredentialID string
	Kind         Kind
}

// Service mints, lists, revokes and authenticates A2A credentials.
type Service struct {
	store    *store.Store
	sessions *identity.Service
	log      *slog.Logger
}

// New builds a Service. log receives the audit records (credential ids, never
// tokens); nil uses slog.Default.
func New(s *store.Store, sessions *identity.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: s, sessions: sessions, log: log}
}

// Mint creates a client key and returns the stored row plus the raw token --
// the only time the secret exists outside the caller's hands. A non-empty
// subscriberID binds the key to that subscriber, which must exist.
func (svc *Service) Mint(ctx context.Context, scope domain.Scope, name, subscriberID string) (domain.A2AClientKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxNameLen {
		return domain.A2AClientKey{}, "", ErrInvalidName
	}
	if subscriberID != "" {
		if _, err := svc.store.Subscribers().GetByExternalID(ctx, scope, subscriberID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return domain.A2AClientKey{}, "", ErrSubscriberNotFound
			}
			return domain.A2AClientKey{}, "", fmt.Errorf("a2aauth: check subscriber: %w", err)
		}
	}
	raw := make([]byte, secretLen)
	if _, err := rand.Read(raw); err != nil {
		return domain.A2AClientKey{}, "", fmt.Errorf("a2aauth: mint secret: %w", err)
	}
	key, err := svc.store.A2AClientKeys().Create(ctx, scope, domain.A2AClientKey{
		Name:         name,
		SubscriberID: subscriberID,
		SecretHash:   hashSecret(raw),
		CreatedAt:    timestamp.Now(),
	})
	if err != nil {
		return domain.A2AClientKey{}, "", fmt.Errorf("a2aauth: create: %w", err)
	}
	svc.audit(ctx, "a2a.client_key.mint", key.ID, scope, "name", key.Name, "subscriber", key.SubscriberID)
	return key, TokenPrefix + hex.EncodeToString(raw), nil
}

// List returns the scope's keys, revoked included. The secret hash is cleared
// so it never leaves the service.
func (svc *Service) List(ctx context.Context, scope domain.Scope) ([]domain.A2AClientKey, error) {
	keys, err := svc.store.A2AClientKeys().List(ctx, scope)
	if err != nil {
		return nil, err
	}
	for i := range keys {
		keys[i].SecretHash = ""
	}
	return keys, nil
}

// Revoke ends a key immediately. store.ErrNotFound when the id is absent in the
// scope or already revoked.
func (svc *Service) Revoke(ctx context.Context, scope domain.Scope, id string) error {
	if err := svc.store.A2AClientKeys().Revoke(ctx, scope, id, timestamp.Now()); err != nil {
		return err
	}
	svc.audit(ctx, "a2a.client_key.revoke", id, scope)
	return nil
}

// Authenticate is the middleware for A2A routes. It accepts `Authorization:
// Bearer <token>` where the token is a client key or a subscriber session
// token, attaches the Principal to the request context, and answers a uniform
// 401 for anything else (including a revoked key and a tenant API key).
func (svc *Service) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const scheme = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, scheme) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tok := strings.TrimSpace(strings.TrimPrefix(header, scheme))
		p, err := svc.resolve(r.Context(), tok)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		svc.audit(r.Context(), "a2a.credential.use", p.CredentialID, p.Scope, "kind", string(p.Kind))
		// The scope and pinned subscriber go in the same slots the other auth
		// middlewares use, so handlers built on those helpers work unchanged.
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = domain.ContextWithScope(ctx, p.Scope)
		if p.SubscriberID != "" {
			ctx = domain.ContextWithPinnedSubscriber(ctx, p.SubscriberID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (svc *Service) resolve(ctx context.Context, tok string) (Principal, error) {
	if tok == "" {
		return Principal{}, errors.New("empty token")
	}
	if rest, ok := strings.CutPrefix(tok, TokenPrefix); ok {
		return svc.resolveKey(ctx, rest)
	}
	claims, err := svc.sessions.VerifySubscriberSession(tok)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		Scope:        claims.Scope,
		SubscriberID: claims.SubscriberID,
		CredentialID: "session:" + claims.SubscriberID,
		Kind:         KindSession,
	}, nil
}

func (svc *Service) resolveKey(ctx context.Context, secretHex string) (Principal, error) {
	raw, err := hex.DecodeString(secretHex)
	if err != nil || len(raw) != secretLen {
		return Principal{}, store.ErrNotFound
	}
	key, err := svc.store.A2AClientKeys().GetBySecretHash(ctx, hashSecret(raw))
	if err != nil {
		return Principal{}, err
	}
	if key.RevokedAt != "" {
		return Principal{}, errors.New("revoked")
	}
	// Best-effort and throttled: a failed activity write must never fail the
	// request, and SQLite's single writer shouldn't take a write per call.
	if last, err := timestamp.Parse(key.LastUsedAt); err != nil || time.Since(last) >= touchEvery {
		_ = svc.store.A2AClientKeys().Touch(ctx, key.ID, timestamp.Now())
	}
	return Principal{
		Scope:        domain.Scope{OrgID: key.OrgID, EnvID: key.EnvID},
		SubscriberID: key.SubscriberID,
		CredentialID: key.ID,
		Kind:         KindClientKey,
	}, nil
}

func (svc *Service) audit(ctx context.Context, event, credentialID string, scope domain.Scope, kv ...any) {
	attrs := append([]any{
		"audit", true, "credential", credentialID, "org", scope.OrgID, "env", scope.EnvID,
	}, kv...)
	svc.log.InfoContext(ctx, event, attrs...)
}

type principalKey struct{}

// FromContext returns the Principal set by Authenticate.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// hashSecret returns the hex SHA-256 of the raw secret bytes (identity's
// hashKey semantics).
func hashSecret(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
