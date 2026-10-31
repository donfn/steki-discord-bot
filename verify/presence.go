package verify

import (
	"sync"
	"time"
)

// Presence is who has the material page open, told to every open page: how
// many members are in the study space as a whole, and how many on the same
// course's page. A member counts once however many tabs they have open, and
// only while their page's socket is connected. Nobody is named.

// presenceDelay gathers joins and leaves into one update.
const presenceDelay = time.Second

type presence struct {
	mu sync.Mutex
	// users counts each member's open sockets per course channel.
	users map[string]map[string]int
	// online counts each member's open sockets across the study space.
	online  map[string]int
	sockets map[*pageSocket]struct{}
	timer   *time.Timer
}

func newPresence() *presence {
	return &presence{users: map[string]map[string]int{}, online: map[string]int{}, sockets: map[*pageSocket]struct{}{}}
}

// join counts a socket in, and schedules an update for everyone.
func (p *presence) join(s *pageSocket) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.users[s.channel] == nil {
		p.users[s.channel] = map[string]int{}
	}
	p.users[s.channel][s.user]++
	p.online[s.user]++
	p.sockets[s] = struct{}{}
	p.changed()
}

// leave counts a socket out, and schedules an update for everyone.
func (p *presence) leave(s *pageSocket) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sockets[s]; !ok {
		return
	}
	delete(p.sockets, s)
	if p.users[s.channel][s.user]--; p.users[s.channel][s.user] <= 0 {
		delete(p.users[s.channel], s.user)
		if len(p.users[s.channel]) == 0 {
			delete(p.users, s.channel)
		}
	}
	if p.online[s.user]--; p.online[s.user] <= 0 {
		delete(p.online, s.user)
	}
	p.changed()
}

// counts returns how many members are on a channel's page and in the study
// space as a whole.
func (p *presence) counts(channelID string) (here, all int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.users[channelID]), len(p.online)
}

// relay passes a save on to the member's other open sockets.
func (p *presence) relay(from *pageSocket, it socketItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for s := range p.sockets {
		if s != from && s.user == from.user {
			s.relay(it)
		}
	}
}

// changed schedules one update for every open socket. The caller holds the
// lock.
func (p *presence) changed() {
	if p.timer != nil {
		return
	}
	p.timer = time.AfterFunc(presenceDelay, func() {
		p.mu.Lock()
		p.timer = nil
		for s := range p.sockets {
			s.poke()
		}
		p.mu.Unlock()
	})
}
