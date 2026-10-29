package events

import (
	"sync"
	"time"
)

// rateGates counts events per key in a sliding window. The zero value is
// ready to use. State is in memory, so a restart resets it; it exists to
// stop abuse within a process lifetime, not to be an audit trail.
type rateGates struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	swep time.Time
}

// allow records one event for key and reports whether it is within limit
// events per window. A refused event is not recorded.
func (g *rateGates) allow(key string, limit int, window time.Duration) bool {
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

const (
	// maxMailsPerUserHour, maxMailsPerAddrHour and maxMailsPerHour bound the
	// verification emails one member, one address and the whole bot may
	// send, so leaving and rejoining cannot reset the per-registration cap
	// and the bot cannot be used to flood an address or burn the quota.
	maxMailsPerUserHour = 6
	maxMailsPerAddrHour = 3
	maxMailsPerHour     = 150

	// alertEvery is how often one kind of alert about one member may reach
	// the mod log.
	alertEvery = 10 * time.Minute
)

// A join rate above joinsPerWindow per joinWindow alerts the moderators; it
// does not block, so a genuine rush of students is never turned away.
const (
	joinsPerWindow = 40
	joinWindow     = 10 * time.Minute
)
