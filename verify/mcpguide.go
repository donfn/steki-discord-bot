package verify

import "net/http"

// MCPGuidePath is the setup page of the MCP server. The announcement's button
// hands out personal links to it, each good for one visit, and it must match
// the edge rule's allowlist.
const MCPGuidePath = "/mcp/setup"

// EnableMCPGuide mounts the setup page, which shows mcpURL as the address to
// add to the assistant. It must be called before Register.
func (v *Server) EnableMCPGuide(mcpURL string) { v.mcpURL = mcpURL }

func (v *Server) mcpGuidePage(w http.ResponseWriter, r *http.Request) {
	secure(w)
	data := map[string]any{"State": stateInvalid}
	if c, err := v.tokens.Parse(r.URL.Query().Get("t")); err == nil && c.Reason == ReasonMCPGuide && v.spendLink(c) {
		data = map[string]any{"State": stateReady, "URL": v.mcpURL}
	}
	v.render(w, mcpTmpl, data)
}
