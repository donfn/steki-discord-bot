package verify

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"steki-bot/analytics"
)

// The material page also runs as a Discord Activity, the study space
// («Χώρος μελέτης»). Launched from the sticky's button in a course channel it
// opens on that channel's page; launched anywhere else it opens on an index of
// every course page the member can read. Discord loads the page in an iframe
// under <app id>.discordsays.com and proxies every request to this server.
// There are no links to expire and no cookies: the page asks the Embedded App
// SDK for an OAuth code, trades it here for a short-lived Activity token, and
// sends that token as a Bearer header with every request. Each request then
// checks afresh that the member may read the channel, so losing the role or
// the channel ends access at once. Unlike a link session the Activity can be
// reopened and refreshed as often as the member likes.
//
// Inside the iframe only this origin is reachable, so the page loads pdf.js,
// the SDK and its fonts from /activity/static (see scripts/vendor-activity.sh)
// instead of the CDNs the web page uses. All its requests are relative: the
// Activity's root URL mapping points at /activity/.

// ErrForbidden is returned by an ActivityAccess for a member who may not use
// the Activity at all.
var ErrForbidden = errors.New("verify: not allowed")

// ActivityAccess is the bot side of the Activity: who may read which course
// channel.
type ActivityAccess interface {
	// GuildID is the server the Activity serves, or "" while the bot does not
	// know it yet.
	GuildID() string
	// CanRead returns nil when the member may read the channel's material
	// page, ErrForbidden when they may not use the Activity at all, and
	// ErrNoMaterial when the channel is not a course channel they can see.
	CanRead(ctx context.Context, userID, channelID string) error
	// Courses lists the course channels the member can see, in the server's
	// order, for the study space's index. It returns ErrForbidden like
	// CanRead.
	Courses(ctx context.Context, userID string) ([]ActivityCourse, error)
}

// ActivityCourse is one course on the study space's index.
type ActivityCourse struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Category string    `json:"category,omitempty"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
	Latest   time.Time `json:"latest,omitzero"`
}

// ActivityOptions configures the Activity.
type ActivityOptions struct {
	// ClientID and ClientSecret are the Discord application's OAuth2
	// credentials, the same ones the MCP login uses.
	ClientID     string
	ClientSecret string
	// PublicKey is the application's public key from the Developer Portal,
	// in hex. With it every Activity request must carry a valid signature
	// from Discord's proxy (proxy authentication), so the Activity cannot be
	// reached around Discord.
	PublicKey string
	Access    ActivityAccess

	// DiscordAPI and HTTPClient exist for tests.
	DiscordAPI string
	HTTPClient *http.Client
}

type activity struct {
	clientID, secret, api string
	access                ActivityAccess
	http                  *http.Client
	// proxyKey checks Discord's proxy signatures; nil skips the check.
	proxyKey ed25519.PublicKey
}

const (
	// ActivityPath is the page. The Activity's root URL mapping points at it.
	ActivityPath = "/activity"

	activityTokenPath   = "/activity/token"
	activityDataPath    = "/activity/data"
	activityCoursesPath = "/activity/courses"
	activityReportPath  = "/activity/report"

	// activityFilesPath and activityStaticPath must match the edge rule and
	// shield.ActivityFilesPrefix and shield.ActivityStaticPrefix.
	activityFilesPath  = "/activity/f/"
	activityStaticPath = "/activity/static/"

	// activityTokenTTL is how long one Activity token works. The page fetches
	// every file when it opens, and asks Discord for a new token without a
	// prompt when a later request finds this one expired.
	activityTokenTTL = 30 * time.Minute

	stateActivity = "activity"

	discordAPI = "https://discord.com/api/v10"
)

// activityCSP is materialCSP for the Activity: everything comes from this
// origin, which inside Discord is the Activity's proxy, and only Discord may
// frame the page. base-uri allows the page to set its own base, so that its
// relative URLs work whether Discord serves it at / or at /activity/.
const activityCSP = "default-src 'none'; script-src 'nonce-%s' 'self'; " +
	"style-src 'unsafe-inline' 'self'; font-src 'self' data:; " +
	"img-src 'self' data: blob:; connect-src 'self'; worker-src blob:; " +
	"frame-src blob:; base-uri 'self'; form-action 'none'; " +
	"frame-ancestors https://discord.com https://*.discord.com https://*.discordsays.com"

//go:embed static
var staticFS embed.FS

// EnableActivity mounts the Activity, which also needs the material source.
// Call it before Register. It fails on a malformed public key.
func (v *Server) EnableActivity(o ActivityOptions) error {
	var key ed25519.PublicKey
	if o.PublicKey != "" {
		b, err := hex.DecodeString(o.PublicKey)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return errors.New("verify: the Discord public key must be 64 hex characters")
		}
		key = b
	}
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	api := o.DiscordAPI
	if api == "" {
		api = discordAPI
	}
	v.activity = &activity{clientID: o.ClientID, secret: o.ClientSecret, api: api, access: o.Access, http: client,
		proxyKey: key}
	return nil
}

func (v *Server) registerActivity(mux *http.ServeMux) {
	handle := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, v.viaDiscord(h)) }
	handle("GET "+ActivityPath, v.activityPage)
	handle("GET "+ActivityPath+"/{$}", v.activityPage)
	handle("GET "+activityStaticPath+"{file...}", activityStatic)
	handle("POST "+activityTokenPath, v.activityToken)
	handle("GET "+activityDataPath, v.activityData)
	handle("GET "+activityCoursesPath, v.activityCourses)
	handle("GET "+activityFilesPath+"{id}", v.activityFile)
	handle("POST "+activityReportPath, v.activityReport)
}

// Discord's proxy signs every request it passes on when the application has
// proxy authentication turned on: X-Discord-Proxy-Payload is a base64 JSON
// payload, X-Signature-Ed25519 the signature of its decoded bytes under the
// application's public key, and X-Signature-Timestamp must equal the
// payload's created_at. The payload's expires_at bounds it.
const (
	proxyPayloadHeader   = "X-Discord-Proxy-Payload"
	proxySignatureHeader = "X-Signature-Ed25519"
	proxyTimestampHeader = "X-Signature-Timestamp"
)

// proxyPayload is the part of the proxy's payload the server reads.
type proxyPayload struct {
	CreatedAt json.Number `json:"created_at"`
	ExpiresAt json.Number `json:"expires_at"`
	UserID    string      `json:"user_id"`
}

// viaDiscord refuses requests that did not come through Discord's proxy,
// when the public key is set.
func (v *Server) viaDiscord(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := v.activity.proxyKey; key != nil {
			p, ok := checkProxy(key, r.Header, time.Now())
			if !ok {
				secure(w)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "proxy"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), proxyUserKey{}, p.UserID))
		}
		next(w, r)
	})
}

type proxyUserKey struct{}

// checkProxy verifies the proxy's signature headers. The docs show the
// signature both hex and base64 encoded, so either is accepted.
func checkProxy(key ed25519.PublicKey, h http.Header, now time.Time) (*proxyPayload, bool) {
	raw, err := decodeBase64(h.Get(proxyPayloadHeader))
	if err != nil || len(raw) == 0 || len(raw) > 4<<10 {
		return nil, false
	}
	sigText := h.Get(proxySignatureHeader)
	sig, err := hex.DecodeString(sigText)
	if err != nil {
		sig, err = decodeBase64(sigText)
	}
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, raw, sig) {
		return nil, false
	}
	var p proxyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, false
	}
	expires, err := p.ExpiresAt.Int64()
	if err != nil || p.CreatedAt.String() == "" || p.CreatedAt.String() != h.Get(proxyTimestampHeader) ||
		now.Unix() >= expires {
		return nil, false
	}
	return &p, true
}

// decodeBase64 reads standard or URL-safe base64, padded or not.
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// frameable undoes the parts of secure that would stop Discord from showing
// the page: the Activity lives in Discord's iframe, and is opened from there.
func frameable(w http.ResponseWriter) {
	h := w.Header()
	h.Del("X-Frame-Options")
	h.Del("Cross-Origin-Opener-Policy")
}

// activityPage serves the page shell. It carries no data: the page signs in
// through Discord and fetches the channel's files itself.
func (v *Server) activityPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	frameable(w)
	analytics.Tag(r.Context(), "page", "materials_activity")

	nonce := newNonce()
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(activityCSP, nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{"State": stateActivity, "Nonce": nonce, "ClientID": v.activity.clientID}
	if err := matTmpl.Execute(w, data); err != nil {
		v.log.Warn("verify: render activity page", "error", err)
	}
}

// activityStatic serves the vendored scripts and fonts. Every path names its
// version, so they are cached for good.
func activityStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	var b []byte
	if name == "logo.png" {
		b = logoPNG
	} else {
		var err error
		if b, err = staticFS.ReadFile("static/" + path.Clean(name)); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	h := w.Header()
	ctype := mime.TypeByExtension(path.Ext(name))
	switch path.Ext(name) {
	case ".mjs":
		ctype = "text/javascript; charset=utf-8"
	case "":
		ctype = "text/plain; charset=utf-8"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(b)
}

// activityToken trades the OAuth code the SDK handed the page for an Activity
// token. The token only says who the member is; what they may read is checked
// on every request that uses it.
func (v *Server) activityToken(w http.ResponseWriter, r *http.Request) {
	secure(w)
	var in struct {
		Code string `json:"code"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&in); err != nil || in.Code == "" || len(in.Code) > 256 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	guildID := v.activity.access.GuildID()
	if guildID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": stateError})
		return
	}
	userID, err := v.activity.discordUser(ctx, in.Code)
	if err != nil {
		v.log.Info("verify: activity login failed", "error", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login"})
		return
	}
	analytics.Tag(ctx, "outcome", "activity_login")
	tokenID, err := newActivityID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": stateError})
		return
	}
	expires := time.Now().Add(activityTokenTTL)
	tok, err := v.tokens.IssueActivity(userID, guildID, tokenID, expires)
	if err != nil {
		v.log.Error("verify: issue activity token", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": stateError})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires": expires.UnixMilli()})
}

func newActivityID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// discordUser trades an Activity's OAuth code for the member's ID. Like the
// MCP login, the Discord token is used once and revoked. An Activity's code
// needs no redirect URI.
func (a *activity) discordUser(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {a.clientID},
		"client_secret": {a.secret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := a.call(ctx, http.MethodPost, "/oauth2/token", form, "", &tok); err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("discord returned no access token")
	}
	defer func() {
		rev := url.Values{"token": {tok.AccessToken}, "client_id": {a.clientID}, "client_secret": {a.secret}}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = a.call(rctx, http.MethodPost, "/oauth2/token/revoke", rev, "", nil)
	}()

	var u struct {
		ID string `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, "/users/@me", nil, tok.AccessToken, &u); err != nil {
		return "", fmt.Errorf("read user: %w", err)
	}
	if !isSnowflake(u.ID) {
		return "", errors.New("discord returned no user id")
	}
	return u.ID, nil
}

func (a *activity) call(ctx context.Context, method, path string, form url.Values, bearer string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, a.api+path, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("User-Agent", "steki-bot (activity login)")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// The body can echo request parameters, so only the status is kept.
		return fmt.Errorf("discord answered %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// activityUser checks the request's Activity token and returns the member it
// was issued to. It writes the error response itself when the token fails.
func (v *Server) activityUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok {
		// When the proxy names the member, the token must be theirs.
		proxyUser, _ := r.Context().Value(proxyUserKey{}).(string)
		if c, err := v.tokens.Parse(raw); err == nil && c.Reason == ReasonActivity &&
			(proxyUser == "" || proxyUser == c.Subject) {
			return c.Subject, true
		}
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
	return "", false
}

// activityAuthorize checks the request's token and that its member may read
// channelID, and returns the member. It writes the error response itself.
func (v *Server) activityAuthorize(ctx context.Context, w http.ResponseWriter, r *http.Request, channelID string) (string, bool) {
	userID, ok := v.activityUser(w, r)
	if !ok {
		return "", false
	}
	if !isSnowflake(channelID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return "", false
	}
	// Forbidden rather than Not Found: 404s count towards the flood shield.
	switch err := v.activity.access.CanRead(ctx, userID, channelID); {
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	case errors.Is(err, ErrNoMaterial):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "channel"})
	case err != nil:
		v.log.Error("verify: activity access check", "user_id", userID, "channel_id", channelID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": stateError})
	default:
		return userID, true
	}
	return "", false
}

// activityCourses is the study space's index: every course page the member
// can open.
func (v *Server) activityCourses(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	userID, ok := v.activityUser(w, r)
	if !ok {
		return
	}
	analytics.Tag(ctx, "page", "materials_activity_index")
	courses, err := v.activity.access.Courses(ctx, userID)
	switch {
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	case err != nil:
		v.log.Error("verify: activity course list", "user_id", userID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": stateError})
		return
	}
	if courses == nil {
		courses = []ActivityCourse{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"courses": courses})
}

// activityData lists a channel's files, what the web page has rendered into
// it.
func (v *Server) activityData(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	channelID := r.URL.Query().Get("channel")
	_, ok := v.activityAuthorize(ctx, w, r, channelID)
	if !ok {
		return
	}
	analytics.Tag(ctx, "page", "materials_activity", "channel_id", channelID)

	ch, err := v.materials.Channel(ctx, channelID)
	switch {
	case errors.Is(err, ErrNoMaterial):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "channel"})
		return
	case err != nil:
		v.log.Error("verify: material list failed", "channel_id", channelID, "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": stateError})
		return
	}
	files := ch.Files
	if files == nil {
		files = []MaterialFile{}
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel": ch.Name,
		"course":  courseName(ch.Name),
		"files":   files,
		"total":   total,
	})
}

// activityFile streams one file of a channel the member may read.
func (v *Server) activityFile(w http.ResponseWriter, r *http.Request) {
	secure(w)
	id := r.PathValue("id")
	if !isSnowflake(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), fileTimeout)
	defer cancel()
	channelID := r.URL.Query().Get("channel")
	_, ok := v.activityAuthorize(ctx, w, r, channelID)
	if !ok {
		return
	}
	analytics.Tag(ctx, "channel_id", channelID)
	v.streamMaterial(ctx, w, channelID, id)
}

// activityReport checks one report about a file of a channel the member may
// read.
func (v *Server) activityReport(w http.ResponseWriter, r *http.Request) {
	secure(w)
	rep, ok := v.materials.(Reporter)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "gone"})
		return
	}
	in, ok := readReport(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), reportTimeout)
	defer cancel()
	userID, ok := v.activityAuthorize(ctx, w, r, in.Channel)
	if !ok {
		return
	}
	v.runReport(ctx, w, rep, userID, in.Channel, in.File, in.Reason)
}
