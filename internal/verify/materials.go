package verify

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"steki-bot/internal/analytics"
)

// ErrNoMaterial is returned by a MaterialSource for a channel that has no
// material page, or a file the channel's page does not list.
var ErrNoMaterial = errors.New("verify: no such material")

// MaterialFile is one file on a material page.
type MaterialFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is application/pdf or image/jpeg.
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Caption  string    `json:"caption,omitempty"`
	PostedAt time.Time `json:"posted"`
	// Uploader is the member's display name, when the bot knows it.
	Uploader string `json:"by,omitempty"`
	// Thread names the thread the file was posted in, if not the channel.
	Thread string `json:"thread,omitempty"`
}

// MaterialChannel is what one course channel's page lists.
type MaterialChannel struct {
	Name  string
	Files []MaterialFile
}

// MaterialSource is the bot side of the material page: it knows which files
// a course channel's page lists and reads them back from the backup bucket.
type MaterialSource interface {
	Channel(ctx context.Context, channelID string) (*MaterialChannel, error)
	Open(ctx context.Context, channelID, fileID string) (*MaterialBody, error)
}

// MaterialBody is an open file of a material page. The caller closes Body.
type MaterialBody struct {
	Body io.ReadCloser
	Size int64
	Type string
}

// profileTimeout bounds how long the material page waits for a member's profile.
const profileTimeout = 2 * time.Second

const (
	// entryPath is where the links on the stickies point. It only turns a
	// valid link into a session; see session.go.
	entryPath = "/materials"

	// fileTimeout bounds one file download, far past the server-wide write
	// timeout, which is sized for pages, not PDFs on a slow phone.
	fileTimeout = 10 * time.Minute
)

// materialCSP is pageCSP widened for the viewer: pdf.js is fetched from the
// pinned CDN and runs its worker from a blob, and PDF pages render to
// canvases. Files are fetched as blobs, so images and the fallback PDF frame
// load from blob: URLs. Notes draw formulas with KaTeX's stylesheet, from the
// same CDN, and keep their images as data: URLs.
const materialCSP = "default-src 'none'; script-src 'nonce-%s' https://cdn.jsdelivr.net; " +
	"style-src 'unsafe-inline' https://fonts.googleapis.com https://cdn.jsdelivr.net; font-src https://fonts.gstatic.com https://cdn.jsdelivr.net data:; " +
	"img-src 'self' data: blob: https://cdn.discordapp.com; connect-src 'self' https://cdn.jsdelivr.net; worker-src blob:; " +
	"frame-src blob: https://www.youtube-nocookie.com; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// materialClaims parses a material page token; every other kind is refused.
func (v *Server) materialClaims(raw string) (*Claims, bool) {
	c, err := v.tokens.Parse(raw)
	if err != nil || c.Reason != ReasonMaterials || c.ChannelID == "" {
		return nil, false
	}
	return c, true
}

// sessionID is the visitor's session cookie, or "" without one.
func sessionID(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// isHTTPS reports whether the visitor reached the server over TLS, directly
// or through the platform proxy.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// materialsPage is the link's landing: a valid link, followed for the first
// time, opens a session and the visitor is sent on to the page, so the link
// itself never stays in the address bar, the history or a Referer. Anything
// else gets the same answer as a page that has run out.
func (v *Server) materialsPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	analytics.Tag(r.Context(), "page", "materials")

	c, ok := v.materialClaims(r.URL.Query().Get("t"))
	if !ok || !v.spendLink(c) {
		v.expired(w, r)
		return
	}
	analytics.Tag(r.Context(), "channel_id", c.ChannelID)
	id, ok := v.sessions.open(c.ChannelID, c.Subject, c.GuildID, c.Viewer, time.Now())
	if !ok {
		v.expired(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     pagePath,
		MaxAge:   int(studyTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		// Lax, not Strict: the visit starts on another site (the chat), and a
		// Strict cookie would be withheld from the redirect that follows.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, pagePath, http.StatusSeeOther)
}

// sessionPage serves the page once per session. A second request, which is
// what a refresh is, revokes the session and finds it expired.
func (v *Server) sessionPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	id := sessionID(r)
	channelID, subject, ok := v.sessions.claimPage(id, time.Now())
	if !ok {
		v.expired(w, r)
		return
	}
	analytics.Tag(ctx, "page", "materials", "channel_id", channelID)

	// The link only got the visitor a session. Whether the member may still
	// read the channel (still in the server, still registered, can still see
	// it) is checked again here, not taken from a link that may be seconds old
	// or was issued by a button anyone who can see the card can press.
	switch err := v.checkAccess(ctx, subject, channelID); {
	case errors.Is(err, ErrNoAccess):
		v.sessions.revoke(id)
		v.expired(w, r)
		return
	case err != nil:
		v.log.Error("verify: access check", "user_id", subject, "channel_id", channelID, "error", err)
		v.sessions.unclaimPage(id)
		v.renderMaterials(w, http.StatusServiceUnavailable, map[string]any{"State": stateError})
		return
	}

	ch, err := v.materials.Channel(ctx, channelID)
	switch {
	case errors.Is(err, ErrNoMaterial):
		v.sessions.revoke(id)
		v.expired(w, r)
		return
	case err != nil:
		v.log.Error("verify: material list failed", "channel_id", channelID, "error", err)
		// Nothing was shown, so the visitor may try again.
		v.sessions.unclaimPage(id)
		v.renderMaterials(w, http.StatusServiceUnavailable, map[string]any{"State": stateError})
		return
	}

	files := ch.Files
	if files == nil {
		files = []MaterialFile{}
	}
	ids := make([]string, len(files))
	var total int64
	for i, f := range files {
		ids[i] = f.ID
		total += f.Size
	}
	v.sessions.setFiles(id, ids)
	notes := v.notesFor(ctx, subject, ids)
	guildID, viewer := v.sessions.owner(id)
	who := v.memberProfile(ctx, guildID, subject)
	who.Viewer = viewer
	if who.Viewer != "" {
		// A staff member viewing as the member gets no socket: nothing
		// saves and they are not counted.
		guildID = ""
		analytics.Tag(ctx, "staff_view", true)
	}
	socket, socketExp, err := v.socketToken(subject, guildID, channelID)
	if err != nil {
		v.log.Error("verify: issue socket token", "error", err)
	}
	v.renderMaterials(w, http.StatusOK, map[string]any{
		"Socket":    socket,
		"SocketExp": socketExp.UnixMilli(),
		"Who":       who,
		"ViewOnly":  who.Viewer != "",
		"Avatar":    avatarURL(who.Avatar),
		"State":     stateReady,
		"Channel":   ch.Name,
		"Course":    courseName(ch.Name),
		"Files":     files,
		"Total":     total,
		"Notes":     notes,
	})
}

// expired answers a visit that has no (or no longer a) session. The page is
// the same whatever went wrong and says nothing of what it was for.
func (v *Server) expired(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Path: pagePath, MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	v.renderMaterials(w, http.StatusGone, map[string]any{"State": stateInvalid})
}

// renderMaterials writes the page template under a fresh CSP nonce.
func (v *Server) renderMaterials(w http.ResponseWriter, status int, data map[string]any) {
	nonce := newNonce()
	data["Nonce"] = nonce
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(materialCSP, nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := matTmpl.Execute(w, data); err != nil {
		v.log.Warn("verify: render material page", "error", err)
	}
}

// materialFile streams one file of the session's channel, if the session's
// page listed it. Once the last of those files has gone out in full the
// session is revoked.
func (v *Server) materialFile(w http.ResponseWriter, r *http.Request) {
	secure(w)
	sid := sessionID(r)
	id := r.PathValue("id")
	if !isSnowflake(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	channelID, subject, ok := v.sessions.authorize(sid, id, time.Now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fileTimeout)
	defer cancel()
	analytics.Tag(ctx, "channel_id", channelID)
	// A member who left, lost their role or can no longer see the channel
	// stops downloading at once, not when the session runs out.
	switch err := v.checkAccess(ctx, subject, channelID); {
	case errors.Is(err, ErrNoAccess):
		v.sessions.revoke(sid)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
		return
	case err != nil:
		v.log.Error("verify: access check", "user_id", subject, "channel_id", channelID, "error", err)
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": stateError})
		return
	}
	f, err := v.materials.Open(ctx, channelID, id)
	switch {
	// Gone rather than Not Found: 404s count towards the flood shield, and
	// a file deleted while the page was open is not a scanner.
	case errors.Is(err, ErrNoMaterial):
		writeJSON(w, http.StatusGone, map[string]string{"error": "gone"})
		return
	case err != nil:
		v.log.Error("verify: open material", "channel_id", channelID, "file_id", id, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": stateError})
		return
	}
	defer f.Body.Close()

	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fileTimeout)); err != nil {
		v.log.Debug("verify: extend write deadline", "error", err)
	}
	h := w.Header()
	ctype := "application/octet-stream"
	if f.Type == "application/pdf" || f.Type == "image/jpeg" {
		ctype = f.Type
	}
	h.Set("Content-Type", ctype)
	// A file is somebody else's upload. Should one ever be opened as a page of
	// its own, it runs nothing and loads nothing.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Length", fmt.Sprint(f.Size))
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": id}))
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, f.Body)
	if err != nil || n != f.Size {
		v.log.Debug("verify: material download interrupted", "file_id", id, "error", err)
		return
	}
	if v.sessions.complete(sid, id) {
		analytics.Tag(ctx, "outcome", "material_session_completed")
	}
}

// checkAccess asks the material source whether the member may still read the
// channel. A source that cannot tell admits everyone its session already
// admitted. It returns ErrNoAccess for a member who may not.
func (v *Server) checkAccess(ctx context.Context, userID, channelID string) error {
	ac, ok := v.materials.(AccessChecker)
	if !ok {
		return nil
	}
	return ac.CanRead(ctx, userID, channelID)
}

// memberProfile looks up how the material page shows a member. The page works
// without it, so a failed or slow lookup only costs the name and picture.
func (v *Server) memberProfile(ctx context.Context, guildID, userID string) Profile {
	if v.profiles == nil || guildID == "" {
		return Profile{}
	}
	ctx, cancel := context.WithTimeout(ctx, profileTimeout)
	defer cancel()
	who, err := v.profiles.MemberProfile(ctx, guildID, userID)
	if err != nil {
		v.log.Debug("verify: profile lookup failed", "user_id", userID, "error", err)
		return Profile{}
	}
	return who
}

// avatarURL returns an avatar's URL if it is on Discord's CDN, the only
// image host the page's CSP allows, or "" otherwise.
func avatarURL(u string) template.URL {
	if !strings.HasPrefix(u, "https://cdn.discordapp.com/") {
		return ""
	}
	return template.URL(u)
}

// isSnowflake reports whether s looks like a Discord ID.
func isSnowflake(s string) bool {
	if len(s) == 0 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// courseName turns a channel name such as "δομές-δεδομένων" into the course's
// name as a sentence: "Δομές δεδομένων".
func courseName(channel string) string {
	name := strings.Join(strings.Fields(strings.ReplaceAll(channel, "-", " ")), " ")
	r, size := utf8.DecodeRuneInString(name)
	if r == utf8.RuneError {
		return name
	}
	return string(unicode.ToUpper(r)) + name[size:]
}
