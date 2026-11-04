package mesh

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// protocol is the wire dialect an agent's prompt subject speaks.
type protocol int

const (
	// protoA2A is the A2A v2 envelope (the default).
	protoA2A protocol = iota
	// protoSynadia is the Synadia Agent Protocol v0.3: {prompt} requests and
	// a typed chunk stream on the reply inbox.
	protoSynadia
)

const (
	// DefaultDiscoveryWindow is how long detection listens for $SRV.INFO
	// replies. Replies arrive within milliseconds on a hub; the window only
	// bounds the wait when no v0.3 agent answers.
	DefaultDiscoveryWindow = 500 * time.Millisecond
	// discoveryTTL bounds how long a detected protocol is trusted, so an
	// agent that restarts as a different dialect is noticed.
	discoveryTTL = 60 * time.Second
	// serviceName is the micro service every Synadia agent registers as.
	serviceName = "agents"
	// infoSubject lists every micro service named "agents" (spec §4.1).
	infoSubject = "$SRV.INFO." + serviceName
	// synadiaVersion is the protocol_version metadata value of v0.3 agents.
	synadiaVersion = "0.3"
)

// serviceInfo is the part of a $SRV.INFO response detection reads.
type serviceInfo struct {
	Metadata  map[string]string `json:"metadata"`
	Endpoints []struct {
		Name    string `json:"name"`
		Subject string `json:"subject"`
	} `json:"endpoints"`
}

// discovery detects and caches the protocol per prompt subject. One scan of
// $SRV.INFO.agents answers for every agent on the hub, so it fills the cache
// for all v0.3 subjects it sees; a subject absent from the scan is cached as
// a2a. A scan that cannot even be sent degrades to a2a without caching, so
// the next turn tries again.
type discovery struct {
	window time.Duration
	ttl    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	scans  int // INFO scans issued, for tests of caching
	cached map[string]cacheEntry
}

type cacheEntry struct {
	proto   protocol
	expires time.Time
}

func newDiscovery(window, ttl time.Duration) discovery {
	return discovery{window: window, ttl: ttl, now: time.Now, cached: map[string]cacheEntry{}}
}

// protocolFor returns the protocol subj speaks, scanning $SRV.INFO.agents on a
// cache miss.
func (d *discovery) protocolFor(ctx context.Context, nc *nats.Conn, subj string) protocol {
	d.mu.Lock()
	if e, ok := d.cached[subj]; ok && d.now().Before(e.expires) {
		d.mu.Unlock()
		return e.proto
	}
	d.mu.Unlock()

	found, ok := d.scan(ctx, nc)
	if !ok {
		return protoA2A
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	exp := d.now().Add(d.ttl)
	for s := range found {
		d.cached[s] = cacheEntry{proto: protoSynadia, expires: exp}
	}
	if _, ok := found[subj]; !ok {
		d.cached[subj] = cacheEntry{proto: protoA2A, expires: exp}
	}
	return d.cached[subj].proto
}

// scan multicasts $SRV.INFO.agents and returns the prompt subjects of
// instances declaring protocol_version "0.3". ok is false when the request
// could not be sent.
func (d *discovery) scan(ctx context.Context, nc *nats.Conn) (map[string]struct{}, bool) {
	d.mu.Lock()
	d.scans++
	d.mu.Unlock()

	inbox := replyRoot + ".info." + randToken()
	replies := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(inbox, replies)
	if err != nil {
		return nil, false
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.PublishRequest(infoSubject, inbox, nil); err != nil {
		return nil, false
	}
	found := map[string]struct{}{}
	timer := time.NewTimer(d.window)
	defer timer.Stop()
	for {
		select {
		case m := <-replies:
			var info serviceInfo
			if json.Unmarshal(m.Data, &info) != nil || info.Metadata["protocol_version"] != synadiaVersion {
				continue
			}
			for _, ep := range info.Endpoints {
				if ep.Name == "prompt" {
					found[ep.Subject] = struct{}{}
				}
			}
		case <-timer.C:
			return found, true
		case <-ctx.Done():
			return found, true
		}
	}
}
