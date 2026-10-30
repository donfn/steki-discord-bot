package verify

import (
	"net/http"
	"time"
)

// MCPGuidePath is the setup page of the MCP server. The announcement's button
// links to it, and it must match the edge rule's allowlist.
const MCPGuidePath = "/mcp/setup"

// MCPGuideLinkTTL is how long a link to the setup page works. The announcement
// is edited with a new link every half hour, like the material page's.
const MCPGuideLinkTTL = time.Hour

// EnableMCPGuide mounts the setup page, which shows mcpURL as the address to
// add to the assistant. It must be called before Register.
func (v *Server) EnableMCPGuide(mcpURL string) { v.mcpURL = mcpURL }

func (v *Server) mcpGuidePage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	data := map[string]any{"State": stateInvalid}
	if c, err := v.tokens.Parse(r.URL.Query().Get("t")); err == nil && c.Reason == ReasonMCPGuide {
		data = map[string]any{"State": stateReady, "URL": v.mcpURL}
	}
	v.render(w, mcpTmpl, data)
}
