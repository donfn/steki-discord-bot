package events

import (
	"context"
	"errors"

	"github.com/bwmarrin/discordgo"

	"steki-bot/mcp"
	"steki-bot/verify"
)

// The material page also runs as a Discord Activity (see verify/activity.go).
// The sticky's first button launches it from the course channel, so the page
// knows the channel from the SDK, and the member never sees a link expire.
// Who may read what is the MCP server's rule: a verified member, and only the
// course channels they can see.

// interactionLaunchActivity is the LAUNCH_ACTIVITY interaction callback, which
// discordgo v0.29.0 has no constant for.
const interactionLaunchActivity discordgo.InteractionResponseType = 12

// activityEnabled reports whether the stickies offer the Activity: it needs
// what the MCP server needs, the Activity's routes are then served, and the
// operator has switched the button on once the Developer Portal is set up.
func (h *Handler) activityEnabled() bool {
	return h.mcpEnabled() && h.cfg.ActivityButton
}

// ActivityAccess returns the verify.ActivityAccess the Activity checks members
// against, or nil when it cannot run: like the MCP server it needs the
// Discord login and the material backup.
func (h *Handler) ActivityAccess(s *discordgo.Session) verify.ActivityAccess {
	if !h.mcpEnabled() {
		return nil
	}
	return &activityAccess{lib: &mcpLibrary{h: h, s: s, src: &materialSource{h: h, s: s}}}
}

type activityAccess struct {
	lib *mcpLibrary
}

func (a *activityAccess) GuildID() string { return a.lib.h.guildID() }

// CanRead runs the MCP server's checks on every request: the member still
// holds a verified role, and the channel is a course channel they can see.
func (a *activityAccess) CanRead(ctx context.Context, userID, channelID string) error {
	r, err := a.lib.Reader(ctx, userID)
	if errors.Is(err, mcp.ErrForbidden) {
		return verify.ErrForbidden
	}
	if err != nil {
		return err
	}
	_, err = r.(*mcpReader).check(ctx, channelID)
	if errors.Is(err, mcp.ErrNotFound) {
		return verify.ErrNoMaterial
	}
	return err
}

// Courses lists the course channels the member can see, as the MCP server's
// course list does.
func (a *activityAccess) Courses(ctx context.Context, userID string) ([]verify.ActivityCourse, error) {
	r, err := a.lib.Reader(ctx, userID)
	if errors.Is(err, mcp.ErrForbidden) {
		return nil, verify.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	list, err := r.Courses(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]verify.ActivityCourse, len(list))
	for i, c := range list {
		out[i] = verify.ActivityCourse{ID: c.ID, Name: c.Name, Category: c.Category, Files: c.Files, Bytes: c.Bytes, Latest: c.Latest}
	}
	return out, nil
}

// launchActivity answers the sticky's button by opening the Activity in the
// channel. Should Discord refuse, the member gets the browser link instead.
func (h *Handler) launchActivity(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !h.activityEnabled() || !h.inHomeGuild(i.GuildID) || i.Member == nil {
		return
	}
	ctx, cancel := h.ctx()
	ch, err := h.channel(ctx, s, i.ChannelID)
	cancel()
	if err != nil || !isCourseChannel(ch) {
		h.respondNow(s, i, "Το υλικό ανοίγει μόνο από τα κανάλια των μαθημάτων.")
		return
	}
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: interactionLaunchActivity})
	if err == nil {
		return
	}
	h.log.Error("activity: launch failed", "channel_id", ch.ID, "error", err)
	link, lerr := h.materialLink(ch.ID)
	if lerr != nil {
		h.respondNow(s, i, msgGenericError)
		return
	}
	h.respondNow(s, i, "Η εφαρμογή δεν άνοιξε. Δες το υλικό στον browser: "+link)
}
