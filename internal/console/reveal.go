package console

// reveal.go implements a small in-memory, one-shot flash store for freshly
// minted secrets (API keys, stream-source push secrets).
//
// Before this existed, the mint/rotate handlers redirected straight to
// "?newKey=<raw>" / "?newSecret=<raw>" — the raw secret rode in the redirect
// Location header, which means it landed in the browser's history AND in
// any reverse-proxy/CDN access log sitting in front of the console (this
// product is fronted by a Cloudflare Tunnel). Reloading the reveal URL also
// re-rendered the secret indefinitely.
//
// Console sessions are a stateless signed cookie (see auth.go: the payload
// is sealed client-side, there is no server-side session store keyed by the
// cookie), so there is no existing server-side store to flash a value
// through. This file is that missing piece, scoped narrowly to reveal-once
// secrets: the mutation handler calls stashReveal(secret) and redirects
// with the returned opaque token instead of the secret; the next GET calls
// takeReveal(token) exactly once — found+valid returns the secret and
// deletes the entry, so a reload of the same URL (or a second tab holding
// the same link) renders nothing. Entries also expire after revealTTL so an
// unclaimed stash doesn't live forever in memory.

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// revealTTL bounds how long a stashed secret waits for its one GET before
// it expires unclaimed.
const revealTTL = 2 * time.Minute

// revealTokenBytes is the amount of randomness in a reveal token (32 hex
// chars once encoded) — unguessable, and a different length than the 64-hex
// secrets/CSRF tokens already riding in these pages, so the two can never be
// mistaken for one another.
const revealTokenBytes = 16

// revealEntry is one stashed secret awaiting its single GET.
type revealEntry struct {
	secret string
	expiry time.Time
}

// revealStash is a small in-memory, one-shot flash store for freshly minted
// secrets, guarded by a mutex. Zero value is ready to use.
type revealStash struct {
	mu      sync.Mutex
	entries map[string]revealEntry
}

// stash stores secret under a fresh random token and returns the token.
// Expired entries are swept opportunistically on every call so the map
// can't grow unbounded across a long-lived process.
func (s *revealStash) stash(secret string) (string, error) {
	b := make([]byte, revealTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]revealEntry)
	}
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.expiry) {
			delete(s.entries, k)
		}
	}
	s.entries[token] = revealEntry{secret: secret, expiry: now.Add(revealTTL)}
	return token, nil
}

// take consumes (deletes) the entry for token, returning its secret. ok is
// false when the token is empty, unknown, already consumed, or expired —
// callers must render no secret in that case.
func (s *revealStash) take(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[token]
	if !ok {
		return "", false
	}
	delete(s.entries, token)
	if time.Now().After(e.expiry) {
		return "", false
	}
	return e.secret, true
}

// stashReveal stores secret in the console's one-shot reveal flash and
// returns an opaque token to embed in a redirect Location — never the
// secret itself.
func (c *Console) stashReveal(secret string) (string, error) {
	if c == nil {
		panic("stashReveal: c is nil")
	}
	return c.reveals.stash(secret)
}

// takeReveal consumes the one-shot reveal flash for token. ok is false when
// there is nothing to reveal (missing, already-consumed, or expired token).
func (c *Console) takeReveal(token string) (string, bool) {
	if c == nil {
		panic("takeReveal: c is nil")
	}
	return c.reveals.take(token)
}
