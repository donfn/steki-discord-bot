package verify

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

const (
	// peerRefresh is how often a blocked service's addresses are looked up
	// again: a redeploy can move it.
	peerRefresh = 30 * time.Second
	peerLookup  = 5 * time.Second
	// peerLogEvery bounds the log lines a refused service can cause.
	peerLogEvery = time.Minute
)

// PeerBlock refuses connections from another service on the private network.
// The blastdoor service parses strangers' files and never needs to call the
// bot, so a request from it means it has been taken over. Such a request skips
// the platform proxy, so its X-Forwarded-For is whatever it says, and ClientIP
// would believe it: the shield could be made to ban anyone's address.
type PeerBlock struct {
	host   string
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	log    *slog.Logger

	mu     sync.RWMutex
	addrs  map[netip.Addr]bool
	logged time.Time
}

// NewPeerBlock returns a block on the host of serviceURL. An empty URL blocks
// nothing.
func NewPeerBlock(serviceURL string, log *slog.Logger) *PeerBlock {
	host := ""
	if u, err := url.Parse(serviceURL); err == nil {
		host = u.Hostname()
	}
	return &PeerBlock{
		host: host,
		log:  log,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
	}
}

// Run keeps the addresses current until ctx ends.
func (p *PeerBlock) Run(ctx context.Context) {
	if p.host == "" {
		return
	}
	failing := false
	tick := time.NewTicker(peerRefresh)
	defer tick.Stop()
	for {
		lookupCtx, cancel := context.WithTimeout(ctx, peerLookup)
		err := p.refresh(lookupCtx)
		cancel()
		if err != nil && !failing && ctx.Err() == nil {
			p.log.Warn("peer block: lookup failed, keeping the last addresses", "host", p.host, "error", err)
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// refresh replaces the addresses. A failed lookup keeps the old ones. Loopback
// is left out so that a converter run on a developer's machine does not lock
// them out of their own bot.
func (p *PeerBlock) refresh(ctx context.Context) error {
	found, err := p.lookup(ctx, p.host)
	if err != nil {
		return err
	}
	addrs := make(map[netip.Addr]bool, len(found))
	for _, a := range found {
		if a = normalize(a); !a.IsLoopback() {
			addrs[a] = true
		}
	}
	p.mu.Lock()
	p.addrs = addrs
	p.mu.Unlock()
	return nil
}

func normalize(a netip.Addr) netip.Addr { return a.Unmap().WithZone("") }

// blocked reports whether a connection from remoteAddr is refused.
func (p *PeerBlock) blocked(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.addrs[normalize(ap.Addr())]
}

// shouldLog reports whether a refusal is due a log line.
func (p *PeerBlock) shouldLog(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.logged) < peerLogEvery {
		return false
	}
	p.logged = now
	return true
}

// Middleware refuses the blocked service before anything counts the request.
func (p *PeerBlock) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.blocked(r.RemoteAddr) {
			if p.shouldLog(time.Now()) {
				p.log.Error("peer block: request from a service that never calls the bot; it may be compromised",
					"host", p.host, "path", r.URL.Path)
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
