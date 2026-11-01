package bot

import (
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"steki-bot/analytics"
)

// SetAnalytics turns on reporting to PostHog. Without it every track call
// does nothing. It must be called before Register.
func (h *Core) SetAnalytics(c *analytics.Client) { h.An = c }

// Track reports one interaction. It says nothing of who: see the analytics
// package, which drops any property that could.
func (h *Core) Track(event string, props map[string]any) {
	h.An.Capture(event, props)
}

// messageProps describes a message without quoting it.
func MessageProps(m *discordgo.Message) map[string]any {
	types := make([]string, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		types = append(types, a.ContentType)
	}
	return map[string]any{
		"guild_id":         m.GuildID,
		"channel_id":       m.ChannelID,
		"content_length":   utf8.RuneCountInString(m.Content),
		"attachments":      len(m.Attachments),
		"attachment_types": types,
		"embeds":           len(m.Embeds),
		"mentions":         len(m.Mentions),
		"has_link":         strings.Contains(m.Content, "://"),
		"is_reply":         m.MessageReference != nil,
		"is_bot":           m.Author != nil && m.Author.Bot,
	}
}

// interactionProps describes a slash command, button or form submission.
func InteractionProps(i *discordgo.InteractionCreate) map[string]any {
	props := map[string]any{
		"guild_id":   i.GuildID,
		"channel_id": i.ChannelID,
		"kind":       i.Type.String(),
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		props["command"] = CommandPath(i.ApplicationCommandData())
	case discordgo.InteractionMessageComponent:
		props["custom_id"] = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		// The upload form's data is not decodable; its id is still on the wire.
		if d, ok := i.Data.(discordgo.ModalSubmitInteractionData); ok {
			props["custom_id"] = d.CustomID
		}
	}
	return props
}
