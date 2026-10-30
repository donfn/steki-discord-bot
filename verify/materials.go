package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"steki-bot/analytics"
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

const (
	// materialFilesPath is where the page downloads its files. It must match
	// shield.MaterialFilesPrefix, which exempts the downloads from the flood
	// count, and the higher download budget in Harden.
	materialFilesPath = "/materials/files/"

	// fileTimeout bounds one file download, far past the server-wide write
	// timeout, which is sized for pages, not PDFs on a slow phone.
	fileTimeout = 10 * time.Minute
)

// materialCSP is pageCSP widened for the viewer: pdf.js is fetched from the
// pinned CDN and runs its worker from a blob, and PDF pages render to
// canvases. Files are fetched as blobs, so images and the fallback PDF frame
// load from blob: URLs.
const materialCSP = "default-src 'none'; script-src 'nonce-%s' https://cdn.jsdelivr.net; " +
	"style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com https://cdn.jsdelivr.net data:; " +
	"img-src 'self' data: blob:; connect-src 'self' https://cdn.jsdelivr.net; worker-src blob:; " +
	"frame-src blob:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// materialClaims parses a material page token; every other kind is refused.
func (v *Server) materialClaims(raw string) (*Claims, bool) {
	c, err := v.tokens.Parse(raw)
	if err != nil || c.Reason != ReasonMaterials || c.ChannelID == "" {
		return nil, false
	}
	return c, true
}

func (v *Server) materialsPage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	data := map[string]any{"State": stateInvalid}
	raw := r.URL.Query().Get("t")
	analytics.Tag(ctx, "page", "materials")
	if c, ok := v.materialClaims(raw); ok {
		analytics.Tag(ctx, "channel_id", c.ChannelID)
		ch, err := v.materials.Channel(ctx, c.ChannelID)
		switch {
		case errors.Is(err, ErrNoMaterial):
		case err != nil:
			v.log.Error("verify: material list failed", "channel_id", c.ChannelID, "error", err)
			data["State"] = stateError
		default:
			files := ch.Files
			if files == nil {
				files = []MaterialFile{}
			}
			var total int64
			for _, f := range files {
				total += f.Size
			}
			data = map[string]any{
				"State":   stateReady,
				"Token":   raw,
				"Channel": ch.Name,
				"Files":   files,
				"Total":   total,
				"Expires": c.ExpiresAt.Unix(),
			}
		}
	}
	nonce := newNonce()
	data["Nonce"] = nonce
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(materialCSP, nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := matTmpl.Execute(w, data); err != nil {
		v.log.Warn("verify: render material page", "error", err)
	}
}

// materialFile streams one file of the token's channel. The token travels in
// the Authorization header, not the URL, so it stays out of access logs.
func (v *Server) materialFile(w http.ResponseWriter, r *http.Request) {
	secure(w)
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, valid := v.materialClaims(raw)
	if !ok || !valid {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
		return
	}
	id := r.PathValue("id")
	if !isSnowflake(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fileTimeout)
	defer cancel()
	analytics.Tag(ctx, "channel_id", c.ChannelID)
	f, err := v.materials.Open(ctx, c.ChannelID, id)
	switch {
	// Gone rather than Not Found: 404s count towards the flood shield, and
	// a file deleted while the page was open is not a scanner.
	case errors.Is(err, ErrNoMaterial):
		writeJSON(w, http.StatusGone, map[string]string{"error": "gone"})
		return
	case err != nil:
		v.log.Error("verify: open material", "channel_id", c.ChannelID, "file_id", id, "error", err)
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
	h.Set("Content-Length", fmt.Sprint(f.Size))
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": id}))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Body); err != nil {
		v.log.Debug("verify: material download interrupted", "file_id", id, "error", err)
	}
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
