package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/danmestas/notify/internal/domain"
)

// SessionTTL is how long a minted subscriber session token stays valid. Short
// because the consumer's backend re-mints on demand for a logged-in user and
// there is no revocation list: the token is stateless, so expiry is the only
// thing that ends it early.
const SessionTTL = time.Hour

// sessionMACLen is the number of HMAC bytes carried in the token. 16 bytes
// (128 bits) is far past any practical forgery margin, matching the same
// truncation emailloop's reply tokens use.
const sessionMACLen = 16

// SubscriberClaims is the verified content of a session token: the tenancy it
// was minted in and the single subscriber it authenticates.
type SubscriberClaims struct {
	Scope        domain.Scope
	SubscriberID string
	ExpiresAt    time.Time
}

// MintSubscriberSession returns an opaque token authenticating exactly one
// subscriber's inbox within scope, valid for SessionTTL.
//
// This is the credential a browser may hold. The API key must never be: it is
// tenant-wide, so a page holding it could read every subscriber's
// notifications by varying a query parameter.
func (svc *Service) MintSubscriberSession(scope domain.Scope, subscriberID string) (token string, expiresAt time.Time) {
	expiresAt = time.Now().Add(SessionTTL).UTC()
	payload := encodeSessionPayload(scope.EnvID, scope.OrgID, subscriberID, expiresAt)
	mac := svc.signSession(payload)
	return b64session(payload) + "." + b64session(mac[:sessionMACLen]), expiresAt
}

// VerifySubscriberSession authenticates a token and returns its claims. A
// malformed, tampered, wrong-key or expired token returns an error; callers
// MUST treat any error as "unauthenticated" and answer 401 rather than acting
// on partial data.
func (svc *Service) VerifySubscriberSession(token string) (SubscriberClaims, error) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return SubscriberClaims{}, fmt.Errorf("identity: malformed session token")
	}
	payload, err := unb64session(token[:dot])
	if err != nil {
		return SubscriberClaims{}, fmt.Errorf("identity: decode session payload: %w", err)
	}
	gotMAC, err := unb64session(token[dot+1:])
	if err != nil {
		return SubscriberClaims{}, fmt.Errorf("identity: decode session mac: %w", err)
	}
	want := svc.signSession(payload)
	if len(gotMAC) != sessionMACLen || !hmac.Equal(gotMAC, want[:sessionMACLen]) {
		return SubscriberClaims{}, fmt.Errorf("identity: session token signature mismatch")
	}
	env, org, sub, exp, err := decodeSessionPayload(payload)
	if err != nil {
		return SubscriberClaims{}, err
	}
	// Expiry is checked only after the signature: an unauthenticated token's
	// expiry field is attacker-controlled and means nothing.
	if !time.Now().Before(exp) {
		return SubscriberClaims{}, fmt.Errorf("identity: session token expired")
	}
	return SubscriberClaims{
		Scope:        domain.Scope{OrgID: org, EnvID: env},
		SubscriberID: sub,
		ExpiresAt:    exp,
	}, nil
}

// SubscriberMiddleware authenticates EITHER a tenant-wide `Authorization:
// ApiKey <key>` (a backend caller, which may address any subscriber in its
// scope exactly as before) OR a `Authorization: Bearer <token>` subscriber
// session, which additionally pins the request to that one subscriber.
//
// Only the inbox routes are mounted behind this. That is the whole enforcement
// model and it is deliberately structural: a session token is not a valid
// credential anywhere else in the API because no other route sits behind a
// middleware that would accept it. Nothing has to remember to opt out, so a
// route added tomorrow cannot accidentally become reachable with a browser
// token.
func (svc *Service) SubscriberMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(header, "ApiKey "):
			key := strings.TrimSpace(strings.TrimPrefix(header, "ApiKey "))
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
		case strings.HasPrefix(header, "Bearer "):
			tok := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
			claims, err := svc.VerifySubscriberSession(tok)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := domain.ContextWithScope(r.Context(), claims.Scope)
			ctx = domain.ContextWithPinnedSubscriber(ctx, claims.SubscriberID)
			next.ServeHTTP(w, r.WithContext(ctx))
		default:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	})
}

// PostSubscriberSession handles POST /v1/subscribers/{subscriberId}/session.
// It is mounted behind the API-key Middleware: minting is a backend operation,
// performed for the user a consumer has already authenticated themselves.
func (svc *Service) PostSubscriberSession(w http.ResponseWriter, r *http.Request) {
	scope := ScopeFrom(r.Context())
	subscriberID := chi.URLParam(r, "subscriberId")
	if subscriberID == "" {
		http.Error(w, "subscriberId is required", http.StatusBadRequest)
		return
	}
	// A token for a subscriber that does not exist would authenticate a
	// permanently empty inbox, which reads as "notify is broken" rather than
	// "that id is wrong". Fail at mint instead.
	if _, err := svc.store.Subscribers().GetByExternalID(r.Context(), scope, subscriberID); err != nil {
		http.Error(w, "subscriber not found", http.StatusNotFound)
		return
	}
	token, expiresAt := svc.MintSubscriberSession(scope, subscriberID)
	w.Header().Set("Content-Type", "application/json")
	// A credential must never be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}{Token: token, ExpiresAt: expiresAt.Format(time.RFC3339)})
}

// signSession computes the full HMAC-SHA256 of payload under the session
// subkey.
func (svc *Service) signSession(payload []byte) []byte {
	h := hmac.New(sha256.New, svc.sessionKey)
	h.Write(payload)
	return h.Sum(nil)
}

// encodeSessionPayload joins the fields with a control byte that cannot appear
// in an id, so the payload parses unambiguously whatever the ids contain.
func encodeSessionPayload(env, org, sub string, exp time.Time) []byte {
	return []byte(strings.Join([]string{env, org, sub, strconv.FormatInt(exp.Unix(), 10)}, "\x1f"))
}

func decodeSessionPayload(payload []byte) (env, org, sub string, exp time.Time, err error) {
	parts := strings.Split(string(payload), "\x1f")
	if len(parts) != 4 {
		return "", "", "", time.Time{}, fmt.Errorf("identity: session payload has %d fields, want 4", len(parts))
	}
	unix, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("identity: session payload expiry: %w", err)
	}
	return parts[0], parts[1], parts[2], time.Unix(unix, 0).UTC(), nil
}

func b64session(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64session(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
