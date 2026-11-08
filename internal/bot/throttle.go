package bot

import (
	"sync"
	"time"
)

// RateGates counts events per key in a sliding window. The zero value is
// ready to use. State is in memory, so a restart resets it; it exists to
// stop abuse within a process lifetime, not to be an audit trail.
type RateGates struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	swep time.Time
}

// Allow records one event for key and reports whether it is within limit
// events per window. A refused event is not recorded.
func (g *RateGates) Allow(key string, limit int, window time.Duration) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hits == nil {
		g.hits = make(map[string][]time.Time)
	}
	if now.Sub(g.swep) > time.Hour {
		for k, ts := range g.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > 24*time.Hour {
				delete(g.hits, k)
			}
		}
		g.swep = now
	}
	recent := g.hits[key][:0]
	for _, t := range g.hits[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= limit {
		g.hits[key] = recent
		return false
	}
	g.hits[key] = append(recent, now)
	return true
}

// Clear forgets the events recorded for key.
func (g *RateGates) Clear(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hits, key)
}

// AlertEvery is how often one kind of alert about one member may reach the mod
// log.
const AlertEvery = 10 * time.Minute
