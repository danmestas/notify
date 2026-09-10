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
// The token names an email_threads row by id and carries an HMAC over that id
// under the master key: <row id>.<mac>. Every claim -- tenant, thread,
// subscriber -- lives on the row, so the address stays well inside the 64
// octets RFC 5321 allows a local part (#350), a token cannot be forged without
// the key, and a token for a row that no longer exists is refused at RCPT.
package emailloop

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// macLen is the number of HMAC bytes carried in the token. 16 bytes (128 bits)
// is well past any practical forgery margin for an unguessable mailbox token
// while keeping the address short.
const macLen = 16

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

// Mint returns the token for an email_threads row id. The id is a ULID, so
// the token is address safe as-is and 49 characters long.
func (m *Minter) Mint(threadRowID string) string {
	mac := m.sign(threadRowID)
	return threadRowID + "." + b64(mac[:macLen])
}

// Verify authenticates a token and returns the email_threads row id it names.
// A malformed token or a wrong-key signature returns an error; callers MUST
// treat any error as "unknown mailbox" and drop the inbound mail.
func (m *Minter) Verify(token string) (string, error) {
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return "", fmt.Errorf("emailloop: malformed token")
	}
	id := token[:dot]
	gotMAC, err := unb64(token[dot+1:])
	if err != nil {
		return "", fmt.Errorf("emailloop: decode mac: %w", err)
	}
	want := m.sign(id)
	// Constant-time compare on the truncated MAC.
	if len(gotMAC) != macLen || !hmac.Equal(gotMAC, want[:macLen]) {
		return "", fmt.Errorf("emailloop: token signature mismatch")
	}
	return id, nil
}

// sign computes the full HMAC-SHA256 of id under the master key.
func (m *Minter) sign(id string) []byte {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(id))
	return h.Sum(nil)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
