package events

import (
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"steki-bot/analytics"
)

// SetAnalytics turns on reporting to PostHog. Without it every track call
// does nothing. It must be called before Register.
func (h *Handler) SetAnalytics(c *analytics.Client) { h.an = c }

// track reports one interaction of the member userID. Message text and email
// addresses never go in props: only their shape (length, count, domain).
func (h *Handler) track(userID, event string, props map[string]any) {
	h.an.Capture(userID, event, props)
}

// messageProps describes a message without quoting it.
func messageProps(m *discordgo.Message) map[string]any {
	types := make([]string, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		types = append(types, a.ContentType)
	}
	return map[string]any{
		"guild_id":         m.GuildID,
		"channel_id":       m.ChannelID,
		"message_id":       m.ID,
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
func interactionProps(i *discordgo.InteractionCreate) map[string]any {
	props := map[string]any{
		"guild_id":   i.GuildID,
		"channel_id": i.ChannelID,
		"kind":       i.Type.String(),
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		props["command"] = commandPath(i.ApplicationCommandData())
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

// auditProps turns an audit detail into event properties. Anything that holds
// an email address is reduced to its domain.
func auditProps(regID, actorID string, detail map[string]any) map[string]any {
	props := make(map[string]any, len(detail)+2)
	for k, v := range detail {
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(k), "email") {
			if _, domain, found := strings.Cut(s, "@"); found {
				props[k+"_domain"] = domain
			}
			continue
		}
		props[k] = v
	}
	if regID != "" {
		props["registration_id"] = regID
	}
	if actorID != "" {
		props["actor_id"] = actorID
	}
	return props
}
