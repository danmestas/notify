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
// for all v0.3 subjects it sees.
//
// A turn never waits on a scan for a known subject: an expired entry is
// served as is while one background scan refreshes it. Only a subject never
// seen waits, for the scan in flight or one it starts; if that scan does not
// list it, it is cached as a2a. At most one scan runs at a time. A scan that
// cannot be sent leaves the cache alone, so the next turn tries again.
type discovery struct {
	window time.Duration
	ttl    time.Duration
	now    func() time.Time

	// ctx bounds every scan; Close cancels it and waits on wg.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	scans  int // INFO scans issued, for tests of caching
	cached map[string]cacheEntry
	flight *scanFlight // the scan in progress, nil when idle
	closed bool
}

type cacheEntry struct {
	proto   protocol
	expires time.Time
}

// scanFlight is one running scan. waiting holds the cold-miss subjects that
// joined it; done closes when the scan has updated the cache. notify closes
// (and is replaced) each time the scan caches a v0.3 subject early, so a miss
// for that subject need not wait out the window.
type scanFlight struct {
	done    chan struct{}
	notify  chan struct{}
	waiting map[string]struct{}
}

func newDiscovery(window, ttl time.Duration) *discovery {
	ctx, cancel := context.WithCancel(context.Background())
	return &discovery{
		window: window, ttl: ttl, now: time.Now,
		ctx: ctx, cancel: cancel, cached: map[string]cacheEntry{},
	}
}

// close stops the running scan and refuses new ones.
func (d *discovery) close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.cancel()
	d.wg.Wait()
}

// startLocked returns the scan in flight, starting one if none runs. It
// returns nil once the discovery is closed. d.mu must be held.
func (d *discovery) startLocked(nc *nats.Conn) *scanFlight {
	if d.flight != nil || d.closed {
		return d.flight
	}
	f := &scanFlight{done: make(chan struct{}), notify: make(chan struct{}), waiting: map[string]struct{}{}}
	d.flight = f
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		found, ok := d.scan(d.ctx, nc, func(subj string) {
			d.mu.Lock()
			d.cached[subj] = cacheEntry{proto: protoSynadia, expires: d.now().Add(d.ttl)}
			close(f.notify)
			f.notify = make(chan struct{})
			d.mu.Unlock()
		})
		d.mu.Lock()
		if ok {
			d.applyLocked(found, f.waiting)
		}
		d.flight = nil
		d.mu.Unlock()
		close(f.done)
	}()
	return f
}

// applyLocked records a completed scan: listed subjects are v0.3; cached
// subjects and waiting misses it did not list are a2a.
func (d *discovery) applyLocked(found, waiting map[string]struct{}) {
	exp := d.now().Add(d.ttl)
	for s := range d.cached {
		if _, ok := found[s]; !ok {
			d.cached[s] = cacheEntry{proto: protoA2A, expires: exp}
		}
	}
	for s := range waiting {
		if _, ok := found[s]; !ok {
			d.cached[s] = cacheEntry{proto: protoA2A, expires: exp}
		}
	}
	for s := range found {
		d.cached[s] = cacheEntry{proto: protoSynadia, expires: exp}
	}
}

// warm starts one background scan so subjects of agents already on the hub
// are cached before the first turn.
func (d *discovery) warm(nc *nats.Conn) {
	d.mu.Lock()
	d.startLocked(nc)
	d.mu.Unlock()
}

// protocolFor returns the protocol subj speaks. A cached entry answers at
// once, and an expired one also starts a background refresh. An unseen
// subject waits for a scan; a2a is the answer when none completes.
func (d *discovery) protocolFor(ctx context.Context, nc *nats.Conn, subj string) protocol {
	d.mu.Lock()
	if e, ok := d.cached[subj]; ok {
		if !d.now().Before(e.expires) {
			d.startLocked(nc)
		}
		d.mu.Unlock()
		return e.proto
	}
	f := d.startLocked(nc)
	if f == nil {
		d.mu.Unlock()
		return protoA2A
	}
	f.waiting[subj] = struct{}{}
	for {
		if e, ok := d.cached[subj]; ok {
			d.mu.Unlock()
			return e.proto
		}
		notify := f.notify
		d.mu.Unlock()
		select {
		case <-notify:
		case <-f.done:
		case <-ctx.Done():
			return protoA2A
		}
		d.mu.Lock()
		select {
		case <-f.done:
			e, ok := d.cached[subj]
			d.mu.Unlock()
			if ok {
				return e.proto
			}
			return protoA2A
		default:
		}
	}
}

// rescan drops subj's cached protocol and detects it afresh, joining the scan
// in flight or starting one. A turn calls it after an a2a dispatch showed the
// v0.3 shape, meaning the earlier scan missed a late INFO reply.
func (d *discovery) rescan(ctx context.Context, nc *nats.Conn, subj string) protocol {
	d.mu.Lock()
	delete(d.cached, subj)
	d.mu.Unlock()
	return d.protocolFor(ctx, nc, subj)
}

// scan multicasts $SRV.INFO.agents and returns the prompt subjects of
// instances declaring protocol_version "0.3". ok is false when the request
// could not be sent. onFound is called for each subject as its reply arrives.
func (d *discovery) scan(ctx context.Context, nc *nats.Conn, onFound func(string)) (map[string]struct{}, bool) {
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
					onFound(ep.Subject)
				}
			}
		case <-timer.C:
			return found, true
		case <-ctx.Done():
			return nil, false
		}
	}
}
