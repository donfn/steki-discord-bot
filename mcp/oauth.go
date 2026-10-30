package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"

	"steki-bot/analytics"
)

// The server is its own OAuth 2.1 authorization server, and Discord is only the
// identity provider behind its login:
//
//	client            bot                                  Discord
//	  | register         |                                     |
//	  |----------------->| client_id (a signed token)          |
//	  | authorize+PKCE   |                                     |
//	  |----------------->|------- login (scope identify) ----->|
//	  |                  |<-------- code, user id -------------|
//	  |                  | member? verified? -> consent page   |
//	  |<-- code ---------|                                     |
//	  | token+verifier   |                                     |
//	  |----------------->| access token (JWT, 30 days)         |
//
// Nothing is stored. Client IDs, login state, approvals and codes are signed
// tokens that carry what they need, so a redeploy in the middle of a login
// costs nobody anything.

const (
	// Scope is the only scope there is: read access to the course material.
	Scope = "materials:read"

	// AccessTokenTTL is how long an access token works. There are no refresh
	// tokens: after a month the client logs in again.
	AccessTokenTTL = 30 * 24 * time.Hour

	pathRegister  = "/oauth/register"
	pathAuthorize = "/oauth/authorize"
	pathCallback  = "/oauth/callback"
	pathApprove   = "/oauth/approve"
	pathToken     = "/oauth/token"

	clientTTL   = 2 * 365 * 24 * time.Hour
	flowTTL     = 10 * time.Minute
	approvalTTL = 5 * time.Minute
	codeTTL     = time.Minute

	maxRedirects    = 8
	maxRedirectLen  = 512
	maxClientState  = 512
	maxClientNameLn = 64

	discordAuthorize = "https://discord.com/oauth2/authorize"
)

// Claims of the tokens other than the access token. ClientID, Redirect and
// Challenge are the authorization request being carried along.
type clientClaims struct {
	Name      string   `json:"cn,omitempty"`
	Redirects []string `json:"ru"`
	jwt.RegisteredClaims
}

type flowClaims struct {
	ClientID   string `json:"cid"`
	ClientName string `json:"cn,omitempty"`
	Redirect   string `json:"ru"`
	Challenge  string `json:"cc"`
	// State is the client's own state value, returned untouched.
	State string `json:"st,omitempty"`
	// Bind is the hash of the cookie that ties the login to the browser that
	// started it.
	Bind     string `json:"bd,omitempty"`
	UserName string `json:"un,omitempty"`
	jwt.RegisteredClaims
}

type accessClaims struct {
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
	jwt.RegisteredClaims
}

//go:embed consent.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// pageCSP allows the page's own styles, Google Fonts, the bot's logo and
// Discord avatars, and nothing else: it runs no script.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; img-src 'self' data: https://cdn.discordapp.com; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// pageData is what the consent and error pages show.
type pageData struct {
	State        string // consent, denied or error
	Message      string
	ClientName   string
	RedirectHost string
	UserName     string
	ApproveURL   string
	DenyURL      string
	Days         int
}

func (s *Server) page(w http.ResponseWriter, status int, d pageData) {
	secure(w)
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	d.Days = int(AccessTokenTTL / (24 * time.Hour))
	if err := pageTmpl.Execute(w, d); err != nil {
		s.log.Warn("mcp: render page", "error", err)
	}
}

func (s *Server) errorPage(w http.ResponseWriter, status int, msg string) {
	s.page(w, status, pageData{State: "error", Message: msg})
}

// claims returns the registered claims of a token for audience.
func (s *Server) claims(audience, subject string, ttl time.Duration) jwt.RegisteredClaims {
	now := time.Now()
	return jwt.RegisteredClaims{
		Issuer:    s.issuer,
		Subject:   subject,
		Audience:  jwt.ClaimStrings{audience},
		ID:        randomID(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
}

func randomID() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ── Discovery ───────────────────────────────────────────────────────────────

func (s *Server) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	s.metadata(w, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.issuer},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Steki – Υλικό μαθημάτων",
	})
}

func (s *Server) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	s.metadata(w, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + pathAuthorize,
		"token_endpoint":                                 s.issuer + pathToken,
		"registration_endpoint":                          s.issuer + pathRegister,
		"jwks_uri":                                       s.issuer + pathJWKS,
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) jwksDocument(w http.ResponseWriter, _ *http.Request) {
	s.metadata(w, s.sig.jwks())
}

func (s *Server) metadata(w http.ResponseWriter, doc any) {
	secure(w)
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, doc)
}

// ── Dynamic client registration (RFC 7591) ──────────────────────────────────

// register hands out a client ID. The ID is a signed token that carries the
// client's name and redirect URIs, so registering stores nothing. Every client
// is public and must use PKCE, so the ID is not a secret.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	secure(w)
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON object")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirects {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
			fmt.Sprintf("give between 1 and %d redirect_uris", maxRedirects))
		return
	}
	for _, u := range req.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect URIs must be https, http on localhost, or an app's own scheme")
			return
		}
	}
	name := cleanName(req.ClientName)
	id, err := s.sig.sign(typClient, clientClaims{
		Name:             name,
		Redirects:        req.RedirectURIs,
		RegisteredClaims: s.claims(s.issuer, "client", clientTTL),
	})
	if err != nil {
		s.log.Error("mcp: sign client id", "error", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not register")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	})
}

// blockedSchemes can never be a redirect: they run code or read files.
var blockedSchemes = []string{"javascript", "data", "file", "vbscript", "about", "blob", "ftp", "ws", "wss"}

// validRedirect accepts https URLs, http URLs on the loopback interface (the
// way desktop and command-line clients listen), and an app's private-use
// scheme such as cursor://.
func validRedirect(raw string) bool {
	if raw == "" || len(raw) > maxRedirectLen {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != "" && u.User == nil
	case "http":
		h := u.Hostname()
		return u.User == nil && (h == "localhost" || h == "127.0.0.1" || h == "::1")
	}
	return !slices.Contains(blockedSchemes, strings.ToLower(u.Scheme))
}

// cleanName makes a client's self-chosen name safe to show on the consent page.
func cleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > maxClientNameLn {
		out = string(r[:maxClientNameLn])
	}
	if out == "" {
		return "Εφαρμογή MCP"
	}
	return out
}

func (s *Server) client(id string) (*clientClaims, error) {
	var c clientClaims
	if err := s.sig.parse(typClient, s.issuer, id, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ── Authorization ───────────────────────────────────────────────────────────

var (
	challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	verifierRE  = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
)

func pkceOK(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func (s *Server) cookieName() string {
	if s.secureCookie {
		return "__Host-steki_mcp"
	}
	return "steki_mcp"
}

func (s *Server) setBind(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secureCookie, SameSite: http.SameSiteLaxMode,
	})
}

func bindHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// bound reports whether the request comes from the browser that started the
// login. Without this, anyone could start a login with their own client and
// trick a member into finishing it, which would hand the member's identity to
// the attacker's client.
func (s *Server) bound(r *http.Request, want string) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(bindHash(c.Value)), []byte(want)) == 1
}

// redirectBack sends the browser to the client's redirect URI with params.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, redirect string, params url.Values) {
	u, err := url.Parse(redirect)
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "Μη έγκυρη διεύθυνση επιστροφής.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusFound)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, desc string) {
	p := url.Values{"error": {code}, "error_description": {desc}}
	if state != "" {
		p.Set("state", state)
	}
	s.redirectBack(w, r, redirect, p)
}

// authorize starts a login: it checks the request and sends the member to
// Discord.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	secure(w)
	q := r.URL.Query()

	// Until the client and its redirect URI are known to match, errors are
	// shown to the member instead of sent anywhere.
	client, err := s.client(q.Get("client_id"))
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "Η εφαρμογή δεν είναι καταχωρημένη ή η καταχώρησή της έληξε. Σύνδεσέ την ξανά από την αρχή.")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.Redirects) == 1 {
		redirect = client.Redirects[0]
	}
	if !slices.Contains(client.Redirects, redirect) {
		s.errorPage(w, http.StatusBadRequest, "Η διεύθυνση επιστροφής δεν ταιριάζει με αυτή που δήλωσε η εφαρμογή.")
		return
	}

	state := q.Get("state")
	fail := func(code, desc string) { s.redirectError(w, r, redirect, state, code, desc) }
	switch {
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "only response_type=code is supported")
	case q.Get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Get("code_challenge")):
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
	case len(state) > maxClientState:
		fail("invalid_request", "state is too long")
	case q.Get("resource") != "" && strings.TrimRight(q.Get("resource"), "/") != s.resource:
		fail("invalid_target", "unknown resource; this server's resource is "+s.resource)
	default:
		s.startLogin(w, r, client, q.Get("client_id"), redirect, q.Get("code_challenge"), state)
	}
}

func (s *Server) startLogin(w http.ResponseWriter, r *http.Request, client *clientClaims, clientID, redirect, challenge, state string) {
	bind := randomID()
	tok, err := s.sig.sign(typState, flowClaims{
		ClientID: clientID, ClientName: client.Name, Redirect: redirect,
		Challenge: challenge, State: state, Bind: bindHash(bind),
		RegisteredClaims: s.claims(s.issuer, "login", flowTTL),
	})
	if err != nil {
		s.log.Error("mcp: sign login state", "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Κάτι πήγε στραβά. Δοκίμασε ξανά.")
		return
	}
	s.setBind(w, bind, int(flowTTL.Seconds()))
	q := url.Values{
		"client_id":     {s.discordID},
		"response_type": {"code"},
		"scope":         {"identify"},
		"redirect_uri":  {s.issuer + pathCallback},
		"state":         {tok},
		// Skip Discord's own screen for members who authorized the bot before.
		"prompt": {"none"},
	}
	w.Header().Set("Location", discordAuthorize+"?"+q.Encode())
	w.WriteHeader(http.StatusFound)
}

// callback is where Discord sends the member back. It learns who they are,
// checks they may read the material, and asks them to approve the client.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	secure(w)
	q := r.URL.Query()

	var st flowClaims
	if err := s.sig.parse(typState, s.issuer, q.Get("state"), &st); err != nil {
		s.errorPage(w, http.StatusBadRequest, "Η σύνδεση έληξε ή δεν είναι έγκυρη. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}
	if !s.bound(r, st.Bind) {
		s.errorPage(w, http.StatusBadRequest, "Η σύνδεση πρέπει να ολοκληρωθεί στον ίδιο browser που την ξεκίνησε. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}
	if q.Get("error") != "" {
		s.setBind(w, "", -1)
		s.redirectError(w, r, st.Redirect, st.State, "access_denied", "the login with Discord was cancelled")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.errorPage(w, http.StatusBadRequest, "Το Discord δεν επέστρεψε στοιχεία σύνδεσης. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	user, err := s.discordUser(ctx, code)
	if err != nil {
		s.log.Warn("mcp: discord login failed", "error", err)
		s.errorPage(w, http.StatusBadGateway, "Δεν μπόρεσα να επικοινωνήσω με το Discord. Δοκίμασε ξανά σε λίγο.")
		return
	}
	analytics.Tag(ctx, "page", "mcp_login")
	switch _, err := s.lib.Reader(ctx, user.ID); {
	case errors.Is(err, ErrForbidden):
		analytics.Tag(ctx, "outcome", "login_refused")
		s.log.Info("mcp: login refused", "user_id", user.ID)
		s.setBind(w, "", -1)
		s.page(w, http.StatusForbidden, pageData{State: "denied", UserName: user.display()})
		return
	case err != nil:
		s.log.Error("mcp: check member", "user_id", user.ID, "error", err)
		s.errorPage(w, http.StatusServiceUnavailable, "Δεν μπόρεσα να ελέγξω αν είσαι μέλος του Steki. Δοκίμασε ξανά σε λίγο.")
		return
	}

	ap := st
	ap.UserName = user.display()
	ap.RegisteredClaims = s.claims(s.issuer, user.ID, approvalTTL)
	tok, err := s.sig.sign(typApproval, ap)
	if err != nil {
		s.log.Error("mcp: sign approval", "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Κάτι πήγε στραβά. Δοκίμασε ξανά.")
		return
	}
	approve := s.issuer + pathApprove + "?a=" + url.QueryEscape(tok)
	s.page(w, http.StatusOK, pageData{
		State: "consent", ClientName: st.ClientName, RedirectHost: redirectHost(st.Redirect),
		UserName: user.display(), ApproveURL: approve, DenyURL: approve + "&deny=1",
	})
}

func redirectHost(redirect string) string {
	u, err := url.Parse(redirect)
	if err != nil {
		return redirect
	}
	if u.Host != "" {
		return u.Host
	}
	return u.Scheme + "://"
}

// approve is the member's answer on the consent page. It is a link, not a
// form, and is safe as one: it needs the login cookie of the browser that
// started the flow, and the signed approval, which only that browser was shown.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	secure(w)
	var ap flowClaims
	if err := s.sig.parse(typApproval, s.issuer, r.URL.Query().Get("a"), &ap); err != nil {
		s.errorPage(w, http.StatusBadRequest, "Η έγκριση έληξε. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}
	if !s.bound(r, ap.Bind) {
		s.errorPage(w, http.StatusBadRequest, "Η σύνδεση πρέπει να ολοκληρωθεί στον ίδιο browser που την ξεκίνησε. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}
	if !s.once(ap.ID, ap.ExpiresAt.Time) {
		s.errorPage(w, http.StatusGone, "Αυτή η έγκριση έχει ήδη χρησιμοποιηθεί. Ξεκίνα ξανά από την εφαρμογή σου.")
		return
	}
	s.setBind(w, "", -1)
	if r.URL.Query().Get("deny") != "" {
		s.redirectError(w, r, ap.Redirect, ap.State, "access_denied", "the member declined")
		return
	}

	code := ap
	code.RegisteredClaims = s.claims(s.issuer, ap.Subject, codeTTL)
	tok, err := s.sig.sign(typCode, code)
	if err != nil {
		s.log.Error("mcp: sign code", "error", err)
		s.redirectError(w, r, ap.Redirect, ap.State, "server_error", "could not issue a code")
		return
	}
	p := url.Values{"code": {tok}}
	if ap.State != "" {
		p.Set("state", ap.State)
	}
	analytics.Tag(r.Context(), "page", "mcp_consent", "outcome", "client_approved")
	s.log.Info("mcp: client approved", "user_id", ap.Subject, "client", ap.ClientName)
	s.redirectBack(w, r, ap.Redirect, p)
}

// ── Token endpoint ──────────────────────────────────────────────────────────

// token trades an authorization code and its PKCE verifier for an access
// token.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	secure(w)
	w.Header().Set("Pragma", "no-cache")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"only authorization_code is supported; after a month, log in again")
		return
	}

	var c flowClaims
	if err := s.sig.parse(typCode, s.issuer, r.PostForm.Get("code"), &c); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or has expired")
		return
	}
	switch {
	case r.PostForm.Get("client_id") != "" && r.PostForm.Get("client_id") != c.ClientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	case r.PostForm.Get("redirect_uri") != c.Redirect:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !verifierRE.MatchString(r.PostForm.Get("code_verifier")) || !pkceOK(r.PostForm.Get("code_verifier"), c.Challenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
		return
	case r.PostForm.Get("resource") != "" && strings.TrimRight(r.PostForm.Get("resource"), "/") != s.resource:
		oauthError(w, http.StatusBadRequest, "invalid_target", "unknown resource; this server's resource is "+s.resource)
		return
	}
	if !s.once(c.ID, c.ExpiresAt.Time) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch _, err := s.lib.Reader(ctx, c.Subject); {
	case errors.Is(err, ErrForbidden):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this member may no longer read the material")
		return
	case err != nil:
		s.log.Error("mcp: check member at token", "user_id", c.Subject, "error", err)
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "try again in a moment")
		return
	}

	tok, err := s.sig.sign(typAccess, accessClaims{
		Scope: Scope, ClientID: c.ClientID,
		RegisteredClaims: s.claims(s.resource, c.Subject, AccessTokenTTL),
	})
	if err != nil {
		s.log.Error("mcp: sign access token", "error", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue a token")
		return
	}
	s.log.Info("mcp: token issued", "user_id", c.Subject, "client", c.ClientName)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"token_type":   "Bearer",
		"expires_in":   int(AccessTokenTTL.Seconds()),
		"scope":        Scope,
	})
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// once records id as used until its token would have expired anyway, and
// reports false if it already was. This is what makes a code, and an approval,
// work once.
func (s *Server) once(id string, expires time.Time) bool {
	s.spentMu.Lock()
	defer s.spentMu.Unlock()
	now := time.Now()
	for k, t := range s.spent {
		if now.After(t) {
			delete(s.spent, k)
		}
	}
	if _, dup := s.spent[id]; dup {
		return false
	}
	s.spent[id] = expires.Add(time.Minute)
	return true
}

// ── Discord ─────────────────────────────────────────────────────────────────

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

func (u *discordUser) display() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

// discordUser trades the login code for the member's identity. The Discord
// token is used once and revoked: the bot needs to know who logged in, nothing
// more.
func (s *Server) discordUser(ctx context.Context, code string) (*discordUser, error) {
	form := url.Values{
		"client_id":     {s.discordID},
		"client_secret": {s.discordSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.issuer + pathCallback},
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := s.discordCall(ctx, http.MethodPost, "/oauth2/token", strings.NewReader(form.Encode()), "", &tok); err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("discord returned no access token")
	}
	defer func() {
		rev := url.Values{"token": {tok.AccessToken}, "client_id": {s.discordID}, "client_secret": {s.discordSecret}}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.discordCall(rctx, http.MethodPost, "/oauth2/token/revoke", strings.NewReader(rev.Encode()), "", nil); err != nil {
			s.log.Debug("mcp: revoke discord token", "error", err)
		}
	}()

	var u discordUser
	if err := s.discordCall(ctx, http.MethodGet, "/users/@me", nil, tok.AccessToken, &u); err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	if !isSnowflake(u.ID) {
		return nil, errors.New("discord returned no user id")
	}
	return &u, nil
}

func (s *Server) discordCall(ctx context.Context, method, path string, body io.Reader, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, s.discordAPI+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("User-Agent", "steki-bot (mcp login)")
	resp, err := s.http.Do(req)
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
