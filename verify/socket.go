package verify

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"steki-bot/analytics"
)

// The material page keeps one socket open while it is up, and sends the
// member's saves (notes, titles, studied marks, ink) over it.
//
// The socket is opened with a token of its own, signed like the links but
// good only here, for one member and one course channel, and only once. The
// page gets the first with the page itself; the server sends a fresh one as
// soon as the socket is open and again every socketReissue, so if the
// connection drops the page always holds an unused token to reconnect with,
// even across a restart. The token goes in the first message, not the URL, so
// it never reaches an access log.
//
// Saves travel in batches the page numbers and the server acknowledges. The
// page merges changes to the same file and kind, skips values the server
// already has, and resends unacknowledged batches after a reconnect. Frames
// are deflated (permessage-deflate), which suits JSON ink strokes well.
//
// Messages, as JSON:
//
//	page → server  {"op":"auth","t":token}
//	               {"op":"save","seq":n,"items":[{"f":fileID,"k":"study"|"ink","d":object|null}]}
//	server → page  {"op":"token","t":token,"exp":unixMillis}
//	               {"op":"ack","seq":n}                     every item stored
//	               {"op":"ack","seq":n,"retry":true}        nothing stored; send again
//	               {"op":"ack","seq":n,"drop":[{"f","k","code"}]}  those items refused
//
// The server closes with socketExpired when the page can no longer reconnect
// and should send the member back to Discord.
//
// Whether the member may still read the channel is checked when the socket
// opens, before every batch and at every reissue: a member who left the
// server, lost their role or can no longer see the channel is cut off at once
// and gets no new token.

const (
	socketTokenTTL = 30 * time.Minute
	socketReissue  = 5 * time.Minute

	// socketAuthWait is how long a new socket has to present its token.
	socketAuthWait = 10 * time.Second
	socketPing     = 30 * time.Second
	socketIdle     = 75 * time.Second
	socketWrite    = 10 * time.Second

	// maxSockets caps open sockets; they are outside Harden's in-flight cap.
	maxSockets = 4000
	// maxSocketFrame bounds one message: a batch can carry a few files' ink.
	maxSocketFrame = 4 << 20
	// socketFrames is how many saves a socket may send per minute. The page
	// sends at most one every second or so.
	socketFrames = 120

	socketExpired = 4001
)

// ErrNoAccess is returned by an AccessChecker for a member who may not read
// the channel.
var ErrNoAccess = errors.New("verify: no access to the channel")

// AccessChecker is implemented by a MaterialSource that can tell whether a
// member may still read a course channel.
type AccessChecker interface {
	CanRead(ctx context.Context, userID, channelID string) error
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:    4 << 10,
	WriteBufferSize:   4 << 10,
	EnableCompression: true,
	// The default origin check stands: the page's own origin only.
}

type socketItem struct {
	File string          `json:"f"`
	Kind string          `json:"k"`
	Data json.RawMessage `json:"d"`
}

type socketIn struct {
	Op    string       `json:"op"`
	Token string       `json:"t,omitempty"`
	Seq   int64        `json:"seq,omitempty"`
	Items []socketItem `json:"items,omitempty"`
}

type socketDrop struct {
	File string `json:"f"`
	Kind string `json:"k"`
	Code string `json:"code"`
}

// isWebSocket reports whether r asks to become a socket.
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// pageOrSocket serves the page, or its socket when the request asks for one.
func (v *Server) pageOrSocket(w http.ResponseWriter, r *http.Request) {
	if isWebSocket(r) {
		v.socket(w, r)
		return
	}
	v.sessionPage(w, r)
}

// socketToken signs a fresh socket token, or returns "" for a page whose
// session holds nothing to save. It also returns when the token expires.
func (v *Server) socketToken(userID, guildID, channelID string) (string, time.Time, error) {
	if guildID == "" {
		return "", time.Time{}, nil
	}
	id, err := randomID()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(socketTokenTTL)
	t, err := v.tokens.IssueSocket(userID, guildID, channelID, id, exp)
	return t, exp, err
}

// hijacker reaches the connection under the middleware's response recorders,
// which pass it on through Unwrap.
type hijacker struct{ http.ResponseWriter }

func (h hijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(h.ResponseWriter).Hijack()
}

// pageSocket is one open socket.
type pageSocket struct {
	v    *Server
	nb   Notebook
	conn *websocket.Conn

	writeMu sync.Mutex

	user, guild, channel string

	filesMu sync.Mutex
	files   map[string]bool
}

func (v *Server) socket(w http.ResponseWriter, r *http.Request) {
	nb, ok := v.materials.(Notebook)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if v.sockets.Add(1) > maxSockets {
		v.sockets.Add(-1)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	defer v.sockets.Add(-1)

	conn, err := upgrader.Upgrade(hijacker{w}, r, nil)
	if err != nil {
		return // the upgrader answered
	}
	defer conn.Close()
	conn.SetReadLimit(maxSocketFrame)
	conn.EnableWriteCompression(true)

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	s := &pageSocket{v: v, nb: nb, conn: conn}
	s.run(ctx)
}

// run authenticates the socket and serves it until it closes.
func (s *pageSocket) run(ctx context.Context) {
	var in socketIn
	_ = s.conn.SetReadDeadline(time.Now().Add(socketAuthWait))
	if err := s.conn.ReadJSON(&in); err != nil || in.Op != "auth" {
		return
	}
	c, err := s.v.tokens.Parse(in.Token)
	if err != nil || c.Reason != ReasonSocket || c.ChannelID == "" ||
		!s.v.tokens.spendUntil("socket:"+c.ID, c.ExpiresAt.Add(time.Minute)) {
		s.close(socketExpired, "expired")
		return
	}
	s.user, s.guild, s.channel = c.Subject, c.GuildID, c.ChannelID
	analytics.Tag(ctx, "page", "materials_socket", "channel_id", s.channel)
	if err := s.access(ctx); err != nil {
		s.closeFor(err)
		return
	}

	// The fresh token goes out first: whatever happens next, the page can
	// come back.
	if !s.reissue() {
		return
	}
	if err := s.loadFiles(ctx); err != nil {
		s.closeFor(err)
		return
	}

	done := make(chan struct{})
	defer close(done)
	go s.keepAlive(ctx, done)

	_ = s.conn.SetReadDeadline(time.Now().Add(socketIdle))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(socketIdle))
	})
	window, frames := time.Now(), 0
	for {
		in = socketIn{}
		if err := s.conn.ReadJSON(&in); err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(socketIdle))
		if in.Op != "save" {
			continue
		}
		if now := time.Now(); now.Sub(window) > time.Minute {
			window, frames = now, 0
		}
		if frames++; frames > socketFrames {
			s.close(websocket.ClosePolicyViolation, "too many saves")
			return
		}
		if !s.save(ctx, in) {
			return
		}
	}
}

// keepAlive pings the page and, every socketReissue, checks the channel
// still has a page and hands out a fresh token. A socket lives at most
// studyTTL.
func (s *pageSocket) keepAlive(ctx context.Context, done <-chan struct{}) {
	ping := time.NewTicker(socketPing)
	defer ping.Stop()
	reissue := time.NewTicker(socketReissue)
	defer reissue.Stop()
	end := time.NewTimer(studyTTL)
	defer end.Stop()
	for {
		select {
		case <-done:
			return
		case <-end.C:
			s.close(socketExpired, "expired")
			return
		case <-ping.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(socketWrite)); err != nil {
				return
			}
		case <-reissue.C:
			err := s.access(ctx)
			if err == nil {
				err = s.loadFiles(ctx)
			}
			if err != nil {
				s.closeFor(err)
				return
			}
			if !s.reissue() {
				return
			}
		}
	}
}

// access checks the member may still read the channel.
func (s *pageSocket) access(ctx context.Context) error {
	ac, ok := s.v.materials.(AccessChecker)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return ac.CanRead(ctx, s.user, s.channel)
}

// loadFiles reads which files the channel's page lists: only those take
// saves.
func (s *pageSocket) loadFiles(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	ch, err := s.v.materials.Channel(ctx, s.channel)
	if err != nil {
		return err
	}
	files := make(map[string]bool, len(ch.Files))
	for _, f := range ch.Files {
		files[f.ID] = true
	}
	s.filesMu.Lock()
	s.files = files
	s.filesMu.Unlock()
	return nil
}

// closeFor closes the socket after a failed check: for good when the member
// may not read the channel or it has no page, or so the page reconnects later
// when the check itself failed.
func (s *pageSocket) closeFor(err error) {
	if errors.Is(err, ErrNoMaterial) || errors.Is(err, ErrNoAccess) {
		s.close(socketExpired, "expired")
		return
	}
	s.v.log.Error("verify: socket check", "user_id", s.user, "channel_id", s.channel, "error", err)
	s.close(websocket.CloseTryAgainLater, "unavailable")
}

// reissue sends the page a fresh token. It reports whether that went out.
func (s *pageSocket) reissue() bool {
	t, exp, err := s.v.socketToken(s.user, s.guild, s.channel)
	if err != nil {
		s.v.log.Error("verify: issue socket token", "error", err)
		return false
	}
	return s.send(map[string]any{"op": "token", "t": t, "exp": exp.UnixMilli()}) == nil
}

// save stores one batch and acknowledges it, or ends the socket when the
// member may no longer read the channel.
func (s *pageSocket) save(ctx context.Context, in socketIn) bool {
	if err := s.access(ctx); err != nil {
		if errors.Is(err, ErrNoAccess) {
			s.close(socketExpired, "expired")
			return false
		}
		s.v.log.Error("verify: socket access check", "user_id", s.user, "error", err)
		_ = s.send(map[string]any{"op": "ack", "seq": in.Seq, "retry": true})
		return true
	}
	var drops []socketDrop
	s.filesMu.Lock()
	files := s.files
	s.filesMu.Unlock()
	for _, it := range in.Items {
		if !isSnowflake(it.File) || !files[it.File] {
			drops = append(drops, socketDrop{it.File, it.Kind, "gone"})
			continue
		}
		data, code := cleanNote(it.Kind, it.Data)
		if code != "" {
			drops = append(drops, socketDrop{it.File, it.Kind, code})
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, requestTimeout)
		err := s.nb.SaveNote(sctx, s.user, it.File, it.Kind, data)
		cancel()
		if err != nil {
			// The batch is sent again whole; saves replace, so the items
			// already stored are only stored again.
			s.v.log.Error("verify: save note", "user_id", s.user, "file_id", it.File, "error", err)
			_ = s.send(map[string]any{"op": "ack", "seq": in.Seq, "retry": true})
			return true
		}
	}
	ack := map[string]any{"op": "ack", "seq": in.Seq}
	if len(drops) > 0 {
		ack["drop"] = drops
	}
	_ = s.send(ack)
	return true
}

func (s *pageSocket) send(msg any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(socketWrite))
	return s.conn.WriteJSON(msg)
}

func (s *pageSocket) close(code int, reason string) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(socketWrite))
}

// randomID returns a token ID.
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
