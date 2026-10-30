package events

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/mcp"
	"steki-bot/store"
	"steki-bot/verify"
)

// The MCP server lets members read the course material from an AI assistant.
// This file is the bot's side of it: who may read what (mcpLibrary), and the
// announcement moderators post to explain the feature, whose button hands each
// member a personal link to the setup page, like the stickies' (links.go).

// mcpEnabled reports whether the MCP server is on: it needs the Discord login
// and the material backup.
func (h *Handler) mcpEnabled() bool {
	return h.cfg.MCPEnabled() && h.mat != nil
}

// MCPLibrary returns the mcp.Library that serves the course material, or nil
// when the MCP server is off.
func (h *Handler) MCPLibrary(s *discordgo.Session) mcp.Library {
	if !h.mcpEnabled() {
		return nil
	}
	return &mcpLibrary{h: h, s: s, src: &materialSource{h: h, s: s}}
}

type mcpLibrary struct {
	h   *Handler
	s   *discordgo.Session
	src *materialSource
}

// Reader admits verified members and verified outsiders, the two roles that
// mean registration is complete. Everyone else, staff without either role
// included, is turned away, as are members still in a register channel and
// people who have left. It runs on every request, so losing the role ends
// access at once whatever the token says.
func (l *mcpLibrary) Reader(ctx context.Context, userID string) (mcp.Reader, error) {
	guildID := l.h.guildID()
	if guildID == "" {
		return nil, errors.New("mcp: guild not known yet")
	}
	mem, err := l.member(ctx, guildID, userID)
	if dc.IsMissingResource(err) {
		return nil, mcp.ErrForbidden
	}
	if err != nil {
		return nil, err
	}
	if !hasAnyRole(mem, verifiedRoleIDs) {
		return nil, mcp.ErrForbidden
	}
	return &mcpReader{l: l, guildID: guildID, userID: userID}, nil
}

// member reads a member from the cache, falling back to Discord and caching
// the answer.
func (l *mcpLibrary) member(ctx context.Context, guildID, userID string) (*discordgo.Member, error) {
	if m, err := l.s.State.Member(guildID, userID); err == nil {
		return m, nil
	}
	m, err := l.s.GuildMember(guildID, userID, discordgo.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	m.GuildID = guildID
	_ = l.s.State.MemberAdd(m)
	return m, nil
}

type mcpReader struct {
	l       *mcpLibrary
	guildID string
	userID  string
}

// canView reports whether the member can see a channel in Discord. The tools
// never show a course the member could not open themselves.
func (r *mcpReader) canView(channelID string) bool {
	p, err := r.l.s.State.UserChannelPermissions(r.userID, channelID)
	return err == nil && p&discordgo.PermissionViewChannel != 0
}

func (r *mcpReader) Courses(ctx context.Context) ([]mcp.Course, error) {
	channels, err := dc.GuildChannels(ctx, r.l.s, r.l.h.log, r.guildID)
	if err != nil {
		return nil, err
	}
	stats, err := r.l.h.st.CourseMaterialStats(ctx)
	if err != nil {
		return nil, err
	}
	categories := map[string]*discordgo.Channel{}
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildCategory {
			categories[ch.ID] = ch
		}
	}

	type ranked struct {
		course   mcp.Course
		catPos   int
		position int
	}
	var list []ranked
	for _, ch := range channels {
		if !isCourseChannel(ch) || !r.canView(ch.ID) {
			continue
		}
		cat := categories[ch.ParentID]
		st := stats[ch.ID]
		c := mcp.Course{ID: ch.ID, Name: ch.Name, Files: st.Files, Bytes: st.Bytes, Latest: st.Latest}
		pos := 0
		if cat != nil {
			c.Category, pos = cat.Name, cat.Position
		}
		list = append(list, ranked{c, pos, ch.Position})
	}
	sort.SliceStable(list, func(a, b int) bool {
		x, y := list[a], list[b]
		if x.catPos != y.catPos {
			return x.catPos < y.catPos
		}
		if x.course.Category != y.course.Category {
			return x.course.Category < y.course.Category
		}
		return x.position < y.position
	})
	out := make([]mcp.Course, len(list))
	for i, c := range list {
		out[i] = c.course
	}
	return out, nil
}

// check resolves a course channel the member may read.
func (r *mcpReader) check(ctx context.Context, channelID string) (*discordgo.Channel, error) {
	ch, err := r.l.h.channel(ctx, r.l.s, channelID)
	if err != nil {
		if dc.IsMissingResource(err) {
			return nil, mcp.ErrNotFound
		}
		return nil, err
	}
	if !isCourseChannel(ch) || !r.canView(ch.ID) {
		return nil, mcp.ErrNotFound
	}
	return ch, nil
}

func (r *mcpReader) Index(ctx context.Context, channelID string) ([]mcp.File, error) {
	if _, err := r.check(ctx, channelID); err != nil {
		return nil, err
	}
	page, err := r.l.src.Channel(ctx, channelID)
	if errors.Is(err, verify.ErrNoMaterial) {
		return nil, mcp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]mcp.File, len(page.Files))
	for i, f := range page.Files {
		out[i] = mcp.File{
			ID: f.ID, ChannelID: channelID, Name: f.Name, Type: f.Type, Size: f.Size,
			Caption: f.Caption, Posted: f.PostedAt, Uploader: f.Uploader, Thread: f.Thread,
		}
	}
	return out, nil
}

func (r *mcpReader) Open(ctx context.Context, channelID, fileID string) (*mcp.Download, error) {
	if _, err := r.check(ctx, channelID); err != nil {
		return nil, err
	}
	m, err := r.l.h.st.ChannelMaterial(ctx, channelID, fileID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, mcp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	body, size, err := r.l.h.mat.Bucket().Open(ctx, m.ObjectKey)
	if err != nil {
		return nil, err
	}
	return &mcp.Download{
		File: mcp.File{
			ID: m.AttachmentID, ChannelID: channelID, Name: m.Filename, Type: m.ContentType, Size: size,
			Caption: m.Caption, Posted: m.PostedAt, Uploader: r.l.src.memberName(m.GuildID, m.UploaderID),
		},
		Body: body,
	}, nil
}

// ── The announcement ────────────────────────────────────────────────────────

// mcpGuideButtonID asks for a personal link to the setup page.
const mcpGuideButtonID = "mcp:guide"

// mcpGuideLink returns one member's personal link to the setup page, and the
// link's ID.
func (h *Handler) mcpGuideLink(userID string) (link, tokenID string, err error) {
	guildID := h.guildID()
	if guildID == "" {
		return "", "", errors.New("mcp: guild not known yet")
	}
	if tokenID, err = newTokenID(); err != nil {
		return "", "", err
	}
	token, err := h.tokens.IssueGuide(userID, guildID, tokenID, time.Now().Add(personalLinkTTL))
	if err != nil {
		return "", "", err
	}
	return h.cfg.PublicURL + verify.MCPGuidePath + "?t=" + token, tokenID, nil
}

// mcpAnnouncementCard is the message explaining the feature and its button.
func mcpAnnouncementCard() ([]*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	embeds := []*discordgo.MessageEmbed{{
		Title: "🤖 Το υλικό του Steki, στο AI σου",
		Description: "Μπορείς πλέον να συνδέσεις το **Claude** ή το **ChatGPT** με το υλικό των μαθημάτων. " +
			"Ζήτα τους να βρουν σημειώσεις και θέματα, να διαβάσουν ένα PDF ή να σου κάνουν περίληψη, " +
			"χωρίς να ψάχνεις κανάλι κανάλι.",
		Color: stickyColor,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Τι μπορεί να κάνει", Value: "• Να βρίσκει τα κανάλια των μαθημάτων\n" +
				"• Να δείχνει το υλικό κάθε καναλιού με περιγραφές και ημερομηνίες\n" +
				"• Να κατεβάζει και να διαβάζει PDF και φωτογραφίες"},
			{Name: "Ασφάλεια", Value: "• Συνδέεσαι μία φορά με τον λογαριασμό σου στο Discord\n" +
				"• Μπορεί μόνο να διαβάζει, και μόνο από τα κανάλια που βλέπεις κι εσύ\n" +
				"• Η σύνδεση ισχύει 30 ημέρες"},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "Πάτα το κουμπί για οδηγίες βήμα προς βήμα"},
	}}
	components := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Οδηγίες σύνδεσης",
			Style:    discordgo.PrimaryButton,
			CustomID: mcpGuideButtonID,
			Emoji:    &discordgo.ComponentEmoji{Name: "🔌"},
		},
	}}}
	return embeds, components
}

// cmdMCPAnnounce posts the announcement in a channel.
func (h *Handler) cmdMCPAnnounce(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if !h.mcpEnabled() {
		h.reply(s, i, "Ο MCP server είναι απενεργοποιημένος: χρειάζονται τα `DISCORD_CLIENT_ID` και `DISCORD_CLIENT_SECRET` και το bucket υλικού.")
		return
	}
	opt := findOpt(sub.Options, "channel")
	if opt == nil {
		h.reply(s, i, "Διάλεξε κανάλι.")
		return
	}
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	ch, err := h.channel(ctx, s, opt.ChannelValue(nil).ID)
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Δεν βρήκα το κανάλι: %v", err))
		return
	}
	if !h.inHomeGuild(ch.GuildID) || (ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews) {
		h.reply(s, i, "Διάλεξε ένα κανάλι κειμένου αυτού του server.")
		return
	}
	embeds, components := mcpAnnouncementCard()
	msg, err := s.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Embeds: embeds, Components: components},
		discordgo.WithContext(ctx))
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Δεν μπόρεσα να στείλω το μήνυμα στο <#%s>: %v", ch.ID, err))
		return
	}
	h.log.Info("mcp: announcement posted", "channel_id", ch.ID, "message_id", msg.ID, "user_id", actorID(i))
	h.reply(s, i, fmt.Sprintf("✅ Το μήνυμα στάλθηκε στο <#%s>. Το κουμπί δίνει σε κάθε μέλος δικό του σύνδεσμο.", ch.ID))
}
