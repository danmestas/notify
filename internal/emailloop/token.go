// Package emailloop implements notify's email two-way loop: an inbound email
// reply routes back into notify, threads onto the originating conversation, and
// is handed to the configured responder (default: the conversation's agent).
//
// The loop has three legs:
//
//  1. Outbound arming — when an email is sent in a conversation/two-way context,
//     a tokenized Reply-To (reply+<token>@<inbound-domain>) is set so a reply
//     can be correlated back to the conversation.
//  2. Inbound parse — POST /v1/inbound/email accepts a parsed-email webhook
//     (the reference platform inbound-email shape), extracts and verifies the token from the
//     recipient address, resolves the conversation, appends the inbound body as
//     a turn, and dispatches the responder.
//  3. Close — the responder (agent) reply is mailed back out with the same
//     tokenized Reply-To, so the thread continues.
//
// The token is a stateless HMAC-signed envelope: no store row is needed to
// verify it, it cannot be forged without the master key, and it embeds the
// org+env so a token minted for one tenant cannot thread into another.
package emailloop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

// macLen is the number of HMAC bytes carried in the token. 16 bytes (128 bits)
// is well past any practical forgery margin for an unguessable mailbox token
// while keeping the address short.
const macLen = 16

// Claims is the verified content of a reply token: the tenancy scope plus the
// conversation thread and subscriber it correlates to.
type Claims struct {
	OrgID        string
	EnvID        string
	ThreadID     string
	SubscriberID string
}

// Minter mints and verifies reply tokens with an HMAC keyed by the deployment
// master key (the same 32-byte key identity uses). A Minter is safe for
// concurrent use.
type Minter struct {
	key []byte
}

// NewMinter builds a Minter over masterKey. The key must be non-empty; an empty
// key would make every token forgeable, so NewMinter panics — a misconfigured
// key is a deploy-time programmer error, mirroring identity.New.
func NewMinter(masterKey []byte) *Minter {
	if len(masterKey) == 0 {
		panic("emailloop: masterKey must be non-empty")
	}
	// Copy so a caller mutating their slice cannot change our signing key.
	k := make([]byte, len(masterKey))
	copy(k, masterKey)
	return &Minter{key: k}
}

// Mint returns an opaque token correlating a conversation thread + subscriber
// within scope. The token is URL/address safe (base64url, no padding) so it
// drops straight into reply+<token>@<domain>.
func (m *Minter) Mint(scope domain.Scope, threadID, subscriberID string) string {
	payload := encodePayload(scope.EnvID, scope.OrgID, threadID, subscriberID)
	mac := m.sign(payload)
	return b64(payload) + "." + b64(mac[:macLen])
}

// Verify parses and authenticates a token, returning its Claims. A malformed
// token, a tampered payload, or a wrong-key signature returns an error; callers
// MUST treat any error as "unknown mailbox" and drop the inbound mail rather
// than acting on partial data.
func (m *Minter) Verify(token string) (Claims, error) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return Claims{}, fmt.Errorf("emailloop: malformed token")
	}
	payload, err := unb64(token[:dot])
	if err != nil {
		return Claims{}, fmt.Errorf("emailloop: decode payload: %w", err)
	}
	gotMAC, err := unb64(token[dot+1:])
	if err != nil {
		return Claims{}, fmt.Errorf("emailloop: decode mac: %w", err)
	}
	want := m.sign(payload)
	// Constant-time compare on the truncated MAC.
	if len(gotMAC) != macLen || !hmac.Equal(gotMAC, want[:macLen]) {
		return Claims{}, fmt.Errorf("emailloop: token signature mismatch")
	}
	env, org, thread, sub, err := decodePayload(payload)
	if err != nil {
		return Claims{}, err
	}
	return Claims{OrgID: org, EnvID: env, ThreadID: thread, SubscriberID: sub}, nil
}

// sign computes the full HMAC-SHA256 of payload under the master key.
func (m *Minter) sign(payload []byte) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write(payload)
	return h.Sum(nil)
}

// encodePayload joins the fields with a separator that cannot appear in the
// values (a control byte), so the payload parses unambiguously regardless of
// id contents.
func encodePayload(env, org, thread, sub string) []byte {
	return []byte(strings.Join([]string{env, org, thread, sub}, "\x1f"))
}

func decodePayload(payload []byte) (env, org, thread, sub string, err error) {
	parts := strings.Split(string(payload), "\x1f")
	if len(parts) != 4 {
		return "", "", "", "", fmt.Errorf("emailloop: token payload has %d fields, want 4", len(parts))
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
