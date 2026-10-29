package verify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Consent is what the link page shows the member before they accept: who they
// are on Discord and which address they are about to connect.
type Consent struct {
	Username   string
	AvatarURL  string
	Email      string
	University string // a key of Universities
}

// University is a school whose students can register.
type University struct {
	Name string
	Logo string // path under /verify/uni/
}

// Universities are keyed by the value of Consent.University.
var Universities = map[string]University{
	"hmu": {Name: "Ελληνικό Μεσογειακό Πανεπιστήμιο", Logo: "hmu.png"},
	"uoc": {Name: "Πανεπιστήμιο Κρήτης", Logo: "uoc.png"},
}

// Consenter is the bot side of the registration link: it describes a pending
// link and, once the member accepts, binds the address and lets them in.
type Consenter interface {
	Pending(ctx context.Context, c *Claims) (*Consent, error)
	Accept(ctx context.Context, c *Claims) error
}

// IssueLink signs a registration email link for one registration attempt.
func (t *Tokens) IssueLink(userID, guildID, linkID string, expires time.Time) (string, error) {
	return t.Issue(userID, guildID, ReasonLink, linkID, expires)
}

// linkClaims parses a registration link token; media guard tokens are refused.
func (v *Server) linkClaims(raw string) (*Claims, bool) {
	c, err := v.tokens.Parse(raw)
	if err != nil || c.Reason != ReasonLink {
		return nil, false
	}
	return c, true
}

// pending parses a link and asks the bot whether it is still the member's
// outstanding one.
func (v *Server) pending(ctx context.Context, raw string) (*Claims, *Consent, string) {
	c, ok := v.linkClaims(raw)
	if !ok {
		return nil, nil, stateInvalid
	}
	info, err := v.consent.Pending(ctx, c)
	switch {
	case errors.Is(err, ErrLinkUsed):
		return nil, nil, stateInvalid
	case err != nil:
		v.log.Error("link: lookup failed", "user_id", c.Subject, "error", err)
		return nil, nil, stateError
	}
	return c, info, stateReady
}

func (v *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	raw := r.URL.Query().Get("t")
	_, info, state := v.pending(ctx, raw)
	data := map[string]any{"State": state}
	if state == stateReady {
		uni := Universities[info.University]
		data["Token"] = raw
		data["Username"] = info.Username
		data["AvatarURL"] = info.AvatarURL
		data["Email"] = info.Email
		// Split at the @ so a narrow tile wraps the address there.
		data["EmailUser"], data["EmailDomain"], _ = strings.Cut(info.Email, "@")
		data["UniName"] = uni.Name
		data["UniLogo"] = uni.Logo
	}
	v.render(w, linkTmpl, data)
}

func (v *Server) linkChallenge(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	c, _, state := v.pending(ctx, r.URL.Query().Get("t"))
	if state != stateReady {
		writeJSON(w, statusFor(state), map[string]string{"error": state})
		return
	}
	v.writeChallenge(w, c.ID)
}

func (v *Server) linkAccept(w http.ResponseWriter, r *http.Request) {
	secure(w)
	// Accepting grants a role and deletes a channel on Discord, each retried,
	// so it gets longer than a page load.
	ctx, cancel := context.WithTimeout(r.Context(), 2*requestTimeout)
	defer cancel()

	c, ok := v.readSolve(w, r, v.linkClaims)
	if !ok {
		return
	}
	switch err := v.consent.Accept(ctx, c); {
	case errors.Is(err, ErrLinkUsed):
		writeJSON(w, http.StatusGone, map[string]string{"error": stateInvalid})
	case errors.Is(err, ErrEmailTaken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "taken"})
	case err != nil:
		v.log.Error("link: accept failed", "user_id", c.Subject, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": stateError})
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}
