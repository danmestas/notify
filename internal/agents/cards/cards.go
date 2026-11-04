// Package cards serves the A2A agent cards runtime mesh agents publish.
// Each agent writes its own card, a strict A2A v0.3 AgentCard JSON, to the
// chat_cards KV bucket under the tail of its prompt subject (agents.prompt.
// cc.dmestas.claude is keyed cc.dmestas.claude). notify only reads: Source
// watches the bucket and keeps the latest card per key in memory.
package cards

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Bucket is the KV bucket the chat program owns and agents write their cards
// to. notify lists it in its manifest Reads.
const Bucket = "chat_cards"

// PromptPrefix is the subject root a mesh agent's cards are keyed under.
const PromptPrefix = "agents.prompt."

// Key returns the card key for a mesh subject, or "" when the subject is not
// a prompt subject.
func Key(meshSubject string) string {
	if !strings.HasPrefix(meshSubject, PromptPrefix) {
		return ""
	}
	return strings.TrimPrefix(meshSubject, PromptPrefix)
}

// Card is one agent's card as last written.
type Card struct {
	// Raw is the card JSON exactly as the agent wrote it.
	Raw []byte
	// UpdatedAt is when that revision was written.
	UpdatedAt time.Time
}

// Lookup answers card queries. A nil Lookup is valid wherever one is optional.
type Lookup interface {
	Card(key string) (Card, bool)
}

// Source watches the card bucket and answers lookups from memory.
type Source struct {
	js  jetstream.JetStream
	log *slog.Logger

	mu    sync.RWMutex
	cards map[string]Card

	// retryMin and retryMax bound the backoff between watch attempts.
	retryMin, retryMax time.Duration
}

// New returns a Source reading the bucket through js. Run starts it.
func New(js jetstream.JetStream, log *slog.Logger) *Source {
	if log == nil {
		log = slog.Default()
	}
	return &Source{
		js: js, log: log, cards: map[string]Card{},
		retryMin: time.Second, retryMax: 30 * time.Second,
	}
}

// Card returns the card stored under key.
func (s *Source) Card(key string) (Card, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.cards[key]
	return c, ok
}

// Run watches the bucket until ctx ends. A bucket that does not exist yet, or
// a watch that breaks, is retried with backoff; the first failure of a run of
// failures is logged and the rest are quiet.
func (s *Source) Run(ctx context.Context) {
	delay := s.retryMin
	logged := false
	for ctx.Err() == nil {
		started, err := s.watch(ctx)
		if started {
			delay, logged = s.retryMin, false
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !logged {
			s.log.Warn("agent cards unavailable; retrying", "bucket", Bucket, "err", err)
			logged = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, s.retryMax)
	}
}

// watch runs one watch to its end. started reports that the watch was
// established, so the caller resets its backoff.
func (s *Source) watch(ctx context.Context) (started bool, err error) {
	kv, err := s.js.KeyValue(ctx, Bucket)
	if err != nil {
		return false, err
	}
	w, err := kv.WatchAll(ctx)
	if err != nil {
		return false, err
	}
	defer func() { go func() { _ = w.Stop() }() }()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case e, ok := <-w.Updates():
			if !ok {
				return true, errors.New("card watch closed")
			}
			if e != nil {
				s.apply(e)
			}
		}
	}
}

func (s *Source) apply(e jetstream.KeyValueEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		delete(s.cards, e.Key())
	default:
		s.cards[e.Key()] = Card{Raw: append([]byte(nil), e.Value()...), UpdatedAt: e.Created().UTC()}
	}
}
