package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

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

// primaryEntryPointCommand is the App Launcher's Launch command, and
// entryPointAppHandler the handler that has the app answer it.
const (
	primaryEntryPointCommand discordgo.ApplicationCommandType = 4
	entryPointAppHandler                                      = 1
)

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

// Discord creates the Launch command with the DISCORD_LAUNCH_ACTIVITY
// handler, which launches the Activity itself and posts a "Game Invitation"
// in the channel. quietLaunchCommand switches it to APP_HANDLER, so the bot
// answers it the way it answers the sticky's button: the Activity opens and
// the channel stays as it was. It runs when the bot connects, and only with
// the Activity's button on.
func (h *Handler) quietLaunchCommand(s *discordgo.Session) {
	if !h.activityEnabled() || s.State.User == nil {
		return
	}
	ctx, cancel := h.ctx()
	defer cancel()
	endpoint := discordgo.EndpointApplicationGlobalCommands(s.State.User.ID)
	body, err := s.RequestWithBucketID(http.MethodGet, endpoint, nil, endpoint, discordgo.WithContext(ctx))
	if err != nil {
		h.log.Warn("activity: list global commands", "error", err)
		return
	}
	var cmds []struct {
		ID      string                           `json:"id"`
		Type    discordgo.ApplicationCommandType `json:"type"`
		Handler int                              `json:"handler"`
	}
	if err := json.Unmarshal(body, &cmds); err != nil {
		h.log.Warn("activity: read global commands", "error", err)
		return
	}
	for _, c := range cmds {
		if c.Type != primaryEntryPointCommand || c.Handler == entryPointAppHandler {
			continue
		}
		cmd := discordgo.EndpointApplicationGlobalCommand(s.State.User.ID, c.ID)
		_, err := s.RequestWithBucketID(http.MethodPatch, cmd, map[string]int{"handler": entryPointAppHandler},
			endpoint, discordgo.WithContext(ctx))
		if err != nil {
			h.log.Warn("activity: switch the Launch command to the app", "error", err)
			return
		}
		h.log.Info("activity: the Launch command no longer posts in the channel", "command_id", c.ID)
	}
}

// launchFromEntryPoint answers the App Launcher's Launch command. Wherever it
// is used the Activity opens; outside a course channel it shows the study
// space's index.
func (h *Handler) launchFromEntryPoint(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !h.mcpEnabled() {
		h.respondNow(s, i, "Ο χώρος μελέτης δεν είναι διαθέσιμος αυτή τη στιγμή.")
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: interactionLaunchActivity}); err != nil {
		h.log.Error("activity: launch from the app launcher failed", "error", err)
		h.respondNow(s, i, msgGenericError)
	}
}
