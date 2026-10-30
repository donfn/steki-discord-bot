// Package shield watches the public HTTP surface address by address. An
// address that scans (404s) or floods is challenged at Railway's edge for an
// hour; one that keeps at it after passing the challenge is blocked there for
// six hours. Blocks are stored, so a redeploy does not lift them, and written
// to the service's edge rules (edge.go), so the edge stops them before they
// reach the bot. Railway's Under Attack Mode, which challenges every visitor,
// is kept for floods spread over many addresses, which no single block stops.
package shield

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"steki-bot/store"
)

// MaterialFilesPrefix is the path of the material page's file downloads,
// which are exempt from the counts when they succeed.
const MaterialFilesPrefix = "/s/f/"

// MCPPath is the MCP endpoint, whose successful calls are exempt too.
const MCPPath = "/mcp"

const (
	// rateSeconds is the window request rates are averaged over, so one page
	// load (about four requests) never looks like a flood.
	rateSeconds = 10

	// An address is challenged for challengeFor when it gets challengeNotFound
	// 404s within notFoundWindow, or sends more than challengeRate requests a
	// second over rateSeconds.
	challengeNotFound = 3
	challengeRate     = 10
	challengeFor      = time.Hour
	notFoundWindow    = time.Hour

	// A challenged address is banned for banFor when it reaches banNotFound
	// 404s within notFoundWindow, or keeps up banRate requests a second for
	// longer than banSustain after the challenge began. Only requests that
	// passed the challenge reach the bot to be counted.
	banNotFound = 20
	banRate     = 10
	banSustain  = 20 * time.Minute
	banFor      = 6 * time.Hour

	// floodRate is the request rate, from addresses not already challenged or
	// banned, above which Under Attack Mode goes on for the whole service. One
	// address is challenged long before it could reach this alone.
	floodRate = 50

	// cooldown matches the duration Under Attack Mode is switched on for, so
	// the detector stays quiet while the protection is still active.
	cooldown = time.Hour
	// retryAfter is how long to wait after a failed attempt to enable it.
	retryAfter = 5 * time.Minute
	// enableTimeout bounds one call to Railway.
	enableTimeout = 30 * time.Second

	// maxTracked caps the addresses held in memory. Past it, new addresses
	// are not counted until idle ones are swept.
	maxTracked = 50000
	// idleAfter is how long an address without a block is remembered.
	idleAfter = notFoundWindow

	// syncDelay gathers the blocks of a burst into one edge rules write;
	// syncEvery is how often blocks that ended are taken off the edge.
	syncDelay = 10 * time.Second
	syncEvery = time.Minute
	// syncTimeout bounds one write of the edge rules.
	syncTimeout = 30 * time.Second
)

// Enabler turns Under Attack Mode on.
type Enabler func(ctx context.Context) error

// Store keeps the blocks across restarts.
type Store interface {
	RecordIPBan(ctx context.Context, b store.IPBan) error
	ActiveIPBans(ctx context.Context, now time.Time) ([]store.IPBan, error)
}

// Edge enforces the blocks before requests reach the bot.
type Edge interface {
	Sync(ctx context.Context, challenged, banned []string) error
}

// Event is something the moderators are told about.
type Event struct {
	// Kind is one of the Event constants.
	Kind   string
	Reason string
	// IP and Until describe a ban.
	IP    string
	Until time.Time
	// Err is set for a failure.
	Err error
}

// Kinds of Event.
const (
	EventUnderAttack = "under_attack" // Under Attack Mode switched on, or Err
	EventBan         = "ban"          // an address was banned
	EventEdgeFailed  = "edge_failed"  // the edge rules could not be written
)

// Options configures a Detector. Every field but ClientIP may be left empty.
type Options struct {
	// ClientIP is the address a request came from.
	ClientIP func(*http.Request) string
	Store    Store
	Edge     Edge
	// Enable switches on Under Attack Mode; nil leaves it off.
	Enable Enabler
	Notify func(Event)
	Logger *slog.Logger
}

// Detector counts requests per address and blocks the ones that misbehave.
type Detector struct {
	o Options

	mu    sync.Mutex
	ips   map[string]*ipState
	swept time.Time
	full  bool
	// secs counts requests from addresses without a block, for floods.
	secs       ring
	quietUntil time.Time

	// dirty asks the sync loop to write the edge rules.
	dirty chan struct{}
	// edgeFailed is when a failed write was last reported.
	edgeFailed time.Time
}

type second struct {
	at    int64
	count int
}

// ipState is what is known about one address.
type ipState struct {
	secs     ring
	notFound []time.Time
	// hotSince is when the address began holding banRate while challenged.
	hotSince                       time.Time
	challengeSince, challengeUntil time.Time
	bannedUntil                    time.Time
	seen                           time.Time
}

// New returns a Detector.
func New(o Options) *Detector {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Detector{o: o, ips: map[string]*ipState{}, dirty: make(chan struct{}, 1)}
}

// Load restores the blocks still running from the store.
func (d *Detector) Load(ctx context.Context) error {
	if d.o.Store == nil {
		return nil
	}
	now := time.Now()
	blocks, err := d.o.Store.ActiveIPBans(ctx, now)
	if err != nil {
		return err
	}
	d.mu.Lock()
	for _, b := range blocks {
		st := d.ips[b.IP]
		if st == nil {
			st = &ipState{seen: now}
			d.ips[b.IP] = st
		}
		switch b.Kind {
		case store.IPBanned:
			st.bannedUntil = b.Until
		case store.IPChallenge:
			st.challengeSince, st.challengeUntil = b.StartedAt, b.Until
		}
	}
	d.mu.Unlock()
	d.o.Logger.Info("shield: blocks restored", "count", len(blocks))
	d.markDirty()
	return nil
}

// Run keeps the edge rules in step with the blocks until ctx ends: soon after
// a block is added, and every syncEvery so blocks that ended come off.
func (d *Detector) Run(ctx context.Context) {
	if d.o.Edge == nil {
		return
	}
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	var last [2][]string
	synced := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.dirty:
			select {
			case <-ctx.Done():
				return
			case <-time.After(syncDelay):
			}
		case <-tick.C:
		}
		challenged, banned := d.edgeSets(time.Now())
		if synced && slices.Equal(challenged, last[0]) && slices.Equal(banned, last[1]) {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, syncTimeout)
		err := d.o.Edge.Sync(sctx, challenged, banned)
		cancel()
		if err != nil {
			d.o.Logger.Error("shield: write edge rules", "error", err)
			// Tell the moderators at most hourly; the next tick retries.
			if time.Since(d.edgeFailed) > time.Hour {
				d.edgeFailed = time.Now()
				d.notify(Event{Kind: EventEdgeFailed, Err: err})
			}
			synced = false
			continue
		}
		d.o.Logger.Info("shield: edge rules written", "challenged", len(challenged), "banned", len(banned))
		last, synced = [2][]string{challenged, banned}, true
	}
}

// edgeSets lists the IPv4 addresses to challenge and to block at the edge,
// newest blocks first when there are more than a rule holds, then sorted so
// an unchanged set compares equal.
func (d *Detector) edgeSets(now time.Time) (challenged, banned []string) {
	type entry struct {
		ip    string
		until time.Time
	}
	var ch, bn []entry
	d.mu.Lock()
	for ip, st := range d.ips {
		if p := net.ParseIP(ip); p == nil || p.To4() == nil {
			continue
		}
		switch {
		case st.bannedUntil.After(now):
			bn = append(bn, entry{ip, st.bannedUntil})
		case st.challengeUntil.After(now):
			ch = append(ch, entry{ip, st.challengeUntil})
		}
	}
	d.mu.Unlock()
	pick := func(es []entry) []string {
		slices.SortFunc(es, func(a, b entry) int { return b.until.Compare(a.until) })
		es = es[:min(len(es), maxEdgeIPs)]
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = e.ip
		}
		slices.Sort(out)
		return out
	}
	return pick(ch), pick(bn)
}

func (d *Detector) markDirty() {
	select {
	case d.dirty <- struct{}{}:
	default:
	}
}

func (d *Detector) notify(e Event) {
	if d.o.Notify != nil {
		d.o.Notify(e)
	}
}

// key is the address a request is counted under. An IPv6 host usually holds a
// whole /64, so that is what is counted and blocked.
func key(ip string) string {
	p := net.ParseIP(ip)
	if p == nil || p.To4() != nil {
		return ip
	}
	return (&net.IPNet{IP: p.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
}

// browserProbes are paths browsers and crawlers ask for on their own. A 404
// for one says nothing about the visitor.
var browserProbes = []string{"/favicon.ico", "/robots.txt", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png"}

// probe reports whether a 404 for path is one a well-behaved client gets. MCP
// clients look for several /.well-known documents this server does not have,
// and the hosted assistants share their addresses between many members.
func probe(path string) bool {
	return slices.Contains(browserProbes, path) || strings.HasPrefix(path, "/.well-known/")
}

// banned reports until when an address is banned, if it is.
func (d *Detector) banned(ip string, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st := d.ips[ip]; st != nil && st.bannedUntil.After(now) {
		return st.bannedUntil, true
	}
	return time.Time{}, false
}

// state returns the address's state, creating it if there is room.
func (d *Detector) state(ip string, now time.Time) *ipState {
	if st := d.ips[ip]; st != nil {
		st.seen = now
		return st
	}
	if now.Sub(d.swept) > time.Minute || (len(d.ips) >= maxTracked && now.Sub(d.swept) > time.Second) {
		for k, st := range d.ips {
			if now.Sub(st.seen) > idleAfter && !st.bannedUntil.After(now) && !st.challengeUntil.After(now) {
				delete(d.ips, k)
			}
		}
		d.swept = now
	}
	if len(d.ips) >= maxTracked {
		if !d.full {
			d.full = true
			d.o.Logger.Warn("shield: too many addresses to track; new ones are not counted", "limit", maxTracked)
		}
		return nil
	}
	d.full = false
	st := &ipState{seen: now}
	d.ips[ip] = st
	return st
}

// ring counts requests per second over the last rateSeconds, plus the second
// under way.
type ring [rateSeconds + 1]second

// tally counts one request. It returns the total over the last rateSeconds
// up to now, and over the rateSeconds whole seconds before this one, which is
// what a steady rate is judged by: the second under way is still filling.
func (r *ring) tally(now time.Time) (recent, whole int) {
	sec := now.Unix()
	slot := &r[sec%int64(len(r))]
	if slot.at != sec {
		*slot = second{at: sec}
	}
	slot.count++
	for _, s := range r {
		switch age := sec - s.at; {
		case age < rateSeconds:
			recent += s.count
			if age > 0 {
				whole += s.count
			}
		case age == rateSeconds:
			whole += s.count
		}
	}
	return recent, whole
}

// observe records one finished request from ip. It returns the block to put
// on the address, if one is due, and why Under Attack Mode should go on, or "".
func (d *Detector) observe(ip string, status int, path string, now time.Time) (*store.IPBan, string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := d.state(ip, now)
	if st == nil {
		return nil, d.flood(now)
	}
	if st.bannedUntil.After(now) {
		return nil, ""
	}
	if !st.challengeUntil.IsZero() && !st.challengeUntil.After(now) {
		st.challengeSince, st.challengeUntil, st.hotSince = time.Time{}, time.Time{}, time.Time{}
	}
	challenged := !st.challengeUntil.IsZero()

	total, steady := st.secs.tally(now)
	if status == http.StatusNotFound && !probe(path) {
		st.notFound = append(st.notFound, now)
	}
	cut := 0
	for cut < len(st.notFound) && now.Sub(st.notFound[cut]) > notFoundWindow {
		cut++
	}
	st.notFound = st.notFound[cut:]
	if len(st.notFound) > banNotFound {
		st.notFound = st.notFound[len(st.notFound)-banNotFound:]
	}

	if challenged {
		if steady >= banRate*rateSeconds {
			if st.hotSince.IsZero() {
				st.hotSince = now.Add(-rateSeconds * time.Second)
				if st.hotSince.Before(st.challengeSince) {
					st.hotSince = st.challengeSince
				}
			}
		} else {
			st.hotSince = time.Time{}
		}
		reason := ""
		switch {
		case len(st.notFound) >= banNotFound:
			reason = fmt.Sprintf("%d απαντήσεις 404 μέσα σε μία ώρα, μετά από έλεγχο", len(st.notFound))
		case !st.hotSince.IsZero() && now.Sub(st.hotSince) > banSustain:
			reason = fmt.Sprintf("τουλάχιστον %d αιτήματα/δευτερόλεπτο για πάνω από %d λεπτά, μετά από έλεγχο", banRate, int(banSustain.Minutes()))
		default:
			return nil, ""
		}
		*st = ipState{seen: now, bannedUntil: now.Add(banFor)}
		return &store.IPBan{IP: ip, Kind: store.IPBanned, Reason: reason, StartedAt: now, Until: st.bannedUntil}, ""
	}

	reason := ""
	switch {
	case len(st.notFound) >= challengeNotFound:
		reason = fmt.Sprintf("%d απαντήσεις 404 μέσα σε μία ώρα", len(st.notFound))
	case total > challengeRate*rateSeconds:
		reason = fmt.Sprintf("πάνω από %d αιτήματα/δευτερόλεπτο για %d δευτερόλεπτα", challengeRate, rateSeconds)
	default:
		return nil, d.flood(now)
	}
	st.challengeSince, st.challengeUntil, st.hotSince = now, now.Add(challengeFor), time.Time{}
	return &store.IPBan{IP: ip, Kind: store.IPChallenge, Reason: reason, StartedAt: now, Until: st.challengeUntil}, ""
}

// flood counts a request from an address without a block towards the whole
// service's rate and returns why Under Attack Mode should go on, or "". The
// caller holds the lock.
func (d *Detector) flood(now time.Time) string {
	if d.o.Enable == nil || now.Before(d.quietUntil) {
		return ""
	}
	total, _ := d.secs.tally(now)
	if total <= floodRate*rateSeconds {
		return ""
	}
	d.quietUntil = now.Add(cooldown)
	d.secs = ring{}
	return fmt.Sprintf("πάνω από %d αιτήματα/δευτερόλεπτο για %d δευτερόλεπτα από πολλές διευθύνσεις (%d αιτήματα)",
		floodRate, rateSeconds, total)
}

// block stores a new block, schedules the edge write and reports a ban.
func (d *Detector) block(b *store.IPBan) {
	log := d.o.Logger.With("ip", b.IP, "until", b.Until, "reason", b.Reason)
	if b.Kind == store.IPBanned {
		log.Warn("shield: address banned")
		d.notify(Event{Kind: EventBan, IP: b.IP, Reason: b.Reason, Until: b.Until})
	} else {
		log.Info("shield: address challenged")
	}
	d.markDirty()
	if d.o.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.o.Store.RecordIPBan(ctx, *b); err != nil {
		log.Error("shield: store block", "error", err)
	}
}

func (d *Detector) fire(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), enableTimeout)
	defer cancel()
	err := d.o.Enable(ctx)
	if err != nil {
		d.mu.Lock()
		d.quietUntil = time.Now().Add(retryAfter)
		d.mu.Unlock()
		d.o.Logger.Error("under attack mode: enable failed", "reason", reason, "error", err)
	} else {
		d.o.Logger.Warn("under attack mode enabled", "reason", reason)
	}
	d.notify(Event{Kind: EventUnderAttack, Reason: reason, Err: err})
}

// recorder captures the status a handler wrote.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Middleware refuses banned addresses itself, as the edge would (this covers
// the seconds before the edge rules are written, and IPv6), and counts every
// other request except the platform's health probe and successful material
// downloads and MCP calls. Wrap it outside the rate limiter, so requests that
// were turned away still count.
//
// One material page fetches every file of its channel at once, and an
// assistant makes several MCP calls a second. Those only succeed with a valid
// link or token, which a flood cannot forge; refused ones are still counted.
func (d *Detector) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ip := key(d.o.ClientIP(r))
		if until, ok := d.banned(ip, time.Now()); ok {
			refuse(w, until)
			return
		}
		rec := &recorder{ResponseWriter: w}
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			if status == http.StatusOK && strings.HasPrefix(r.URL.Path, MaterialFilesPrefix) {
				return
			}
			if (status == http.StatusOK || status == http.StatusAccepted) && r.URL.Path == MCPPath {
				return
			}
			b, reason := d.observe(ip, status, r.URL.Path, time.Now())
			if b != nil {
				go d.block(b)
			}
			if reason != "" {
				go d.fire(reason)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// refuse answers a banned address the way the edge rule does.
func refuse(w http.ResponseWriter, until time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if s := int(time.Until(until).Seconds()) + 1; s > 0 {
		h.Set("Retry-After", fmt.Sprint(s))
	}
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(banBody))
}
