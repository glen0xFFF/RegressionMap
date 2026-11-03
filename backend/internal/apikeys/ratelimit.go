package apikeys

import (
	"sync"
	"time"
)

// limiter is a sliding-window counter per key id. It keeps only the
// timestamps inside the window (at most limit per key) and drops keys whose
// window is empty, so memory is bounded by limit × active keys.
type limiter struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	hits      map[string][]time.Time
	lastSweep time.Time
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

// prune drops timestamps at or before now-window. The caller holds l.mu.
func (l *limiter) prune(id string, now time.Time) []time.Time {
	ts := l.hits[id]
	cut := now.Add(-l.window)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.hits, id)
		return nil
	}
	l.hits[id] = ts
	return ts
}

// allow records a hit for id at now when it is under the limit. When it is
// not, retry is how long until the oldest hit leaves the window.
func (l *limiter) allow(id string, now time.Time) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) >= time.Minute {
		for k := range l.hits {
			l.prune(k, now)
		}
		l.lastSweep = now
	}
	ts := l.prune(id, now)
	if len(ts) >= l.limit {
		return false, ts[0].Add(l.window).Sub(now)
	}
	// Copy so the slice never grows past limit through re-slicing.
	next := make([]time.Time, len(ts), len(ts)+1)
	copy(next, ts)
	l.hits[id] = append(next, now)
	return true, 0
}

// size is the number of timestamps held, for tests.
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ts := range l.hits {
		n += len(ts)
	}
	return n
}
