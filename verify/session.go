package verify

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// A material link is shared with the whole channel and rotates every few
// minutes, so it only gets a visitor in the door. Following it opens a session
// of its own: the browser keeps an unguessable cookie, the link leaves the
// address bar, and everything after that is authorised by the session.
//
// A session is built to be used once. The page can be fetched a single time,
// so a refresh finds nothing; only the files that were on that page can be
// downloaded; and once every one of them has been sent in full the session is
// closed: no more downloads. A closed session can still file reports about
// the files that were on its page, since the page stays open and reading is
// when problems show up, until sessionTTL cuts it off. Sessions live in
// memory, so a restart revokes them all, which costs a visitor one more click.
const (
	// sessionCookie is the cookie's name. It says nothing about what it guards.
	sessionCookie = "s"

	// sessionTTL is the longest a session lives, from the moment it is opened.
	sessionTTL = 15 * time.Minute

	// maxSessions caps the sessions held at once, so opening them cannot be
	// used to exhaust memory.
	maxSessions = 5000

	// pagePath and filesPath are the session's page and its downloads. They
	// must match the edge rule and shield.MaterialFilesPrefix.
	pagePath  = "/s"
	filesPath = "/s/f/"

	// reportPath takes a report about one of the page's files.
	reportPath = "/s/report"
)

// session is one visitor's access to one course channel's files.
type session struct {
	channelID string
	subject   string
	opened    time.Time
	// served is set when the page has been handed out; it is never unset on a
	// success, which is what makes a refresh fail.
	served bool
	want   map[string]bool
	done   map[string]bool
	// closed is set once every listed file was sent: nothing more downloads.
	closed bool
}

// sessions is the set of open sessions.
type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

// open starts a session and returns its cookie value. It refuses, returning
// false, when too many are open.
func (s *sessions) open(channelID, subject string, now time.Time) (string, bool) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false
	}
	id := base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= maxSessions {
		for k, v := range s.m {
			if now.Sub(v.opened) > sessionTTL {
				delete(s.m, k)
			}
		}
		if len(s.m) >= maxSessions {
			return "", false
		}
	}
	s.m[id] = &session{channelID: channelID, subject: subject, opened: now}
	return id, true
}

// live returns the session if it exists and has not run out. The caller holds
// the lock.
func (s *sessions) live(id string, now time.Time) *session {
	sess := s.m[id]
	if sess == nil {
		return nil
	}
	if now.Sub(sess.opened) > sessionTTL {
		delete(s.m, id)
		return nil
	}
	return sess
}

// claimPage takes the session's one page view and returns its channel. A
// second claim revokes the session instead.
func (s *sessions) claimPage(id string, now time.Time) (channelID, subject string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.live(id, now)
	if sess == nil {
		return "", "", false
	}
	if sess.served {
		delete(s.m, id)
		return "", "", false
	}
	sess.served = true
	return sess.channelID, sess.subject, true
}

// unclaimPage gives the page view back, for a page that failed to load, so the
// visitor can try again.
func (s *sessions) unclaimPage(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.m[id]; sess != nil {
		sess.served = false
	}
}

// setFiles records which files the page listed. With none there is nothing to
// download, and the session ends here.
func (s *sessions) setFiles(id string, fileIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[id]
	if sess == nil {
		return
	}
	if len(fileIDs) == 0 {
		delete(s.m, id)
		return
	}
	sess.want = make(map[string]bool, len(fileIDs))
	sess.done = make(map[string]bool, len(fileIDs))
	for _, f := range fileIDs {
		sess.want[f] = true
	}
}

// revoke ends a session.
func (s *sessions) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// authorize checks that a session may download a file: it has shown its page
// and that page listed the file. It returns the session's channel.
func (s *sessions) authorize(id, fileID string, now time.Time) (channelID, subject string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.live(id, now)
	if sess == nil || sess.closed || !sess.served || !sess.want[fileID] {
		return "", "", false
	}
	return sess.channelID, sess.subject, true
}

// authorizeReport checks that a session may report a file: it has shown its
// page and that page listed the file. It works after the downloads are over.
func (s *sessions) authorizeReport(id, fileID string, now time.Time) (channelID, subject string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.live(id, now)
	if sess == nil || !sess.served || !sess.want[fileID] {
		return "", "", false
	}
	return sess.channelID, sess.subject, true
}

// complete notes that a file was sent in full and closes the session once
// every file on its page has been. It reports whether it did.
func (s *sessions) complete(id, fileID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[id]
	if sess == nil || sess.closed || !sess.want[fileID] {
		return false
	}
	sess.done[fileID] = true
	if len(sess.done) < len(sess.want) {
		return false
	}
	sess.closed = true
	return true
}
