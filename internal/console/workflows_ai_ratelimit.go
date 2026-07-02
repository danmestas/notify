package console

import (
	"sync"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

// aiGenerateRateLimit is the maximum number of AI generate/refine requests
// allowed per scope (org+env) per aiRateLimitWindow.
const aiGenerateRateLimit = 10

// aiRateLimitWindow is the fixed window over which aiGenerateRateLimit applies.
const aiRateLimitWindow = time.Minute

// aiPromptMaxLen is the maximum number of characters accepted in an AI prompt.
// Prompts longer than this are rejected with an inline message before dispatch.
const aiPromptMaxLen = 2000

// aiRateLimitSweepThreshold is the window-map size above which a new-window
// insert opportunistically evicts fully-expired entries. Below it the map is
// small enough that eviction is pointless; above it the sweep keeps a
// long-lived multi-tenant process from accumulating stale scope entries. It is
// a var (not const) only so tests can lower it. O(n) but amortised — it runs
// only when a new window is opened and the map is already large.
var aiRateLimitSweepThreshold = 1024

// aiRateLimiter is a simple in-memory fixed-window counter keyed by scope. Each
// scope gets an independent window so one env cannot exhaust another. The zero
// value is ready to use.
//
// The limiter is intentionally not persistent: restarting the server resets all
// counters. This is acceptable for abuse mitigation — we are protecting
// OpenRouter spend per-session, not across restarts.
type aiRateLimiter struct {
	mu      sync.Mutex
	windows map[string]*aiWindowState
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

type aiWindowState struct {
	count int
	start time.Time
}

func (l *aiRateLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Allow returns true if the scope is within its rate limit window and records
// the request. It returns false when the limit is exceeded and the request
// should be rejected.
func (l *aiRateLimiter) Allow(scope domain.Scope) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.windows == nil {
		l.windows = make(map[string]*aiWindowState)
	}
	key := scope.OrgID + ":" + scope.EnvID
	now := l.clock()

	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= aiRateLimitWindow {
		// New window. Opportunistically evict fully-expired entries when the map
		// has grown large, so scopes that go idle after one burst don't leave a
		// live entry forever. The common small-fleet case never enters this loop.
		if len(l.windows) >= aiRateLimitSweepThreshold {
			for k, ws := range l.windows {
				if now.Sub(ws.start) >= aiRateLimitWindow {
					delete(l.windows, k)
				}
			}
		}
		l.windows[key] = &aiWindowState{count: 1, start: now}
		return true
	}
	if w.count >= aiGenerateRateLimit {
		return false
	}
	w.count++
	return true
}
