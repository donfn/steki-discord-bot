package events

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/moderation"
)

// The registration card. Each register channel holds one message that shows
// the three steps, marks where the member is, and says the single thing to do
// next. As they go, the bot edits that message instead of posting new ones,
// so the channel never fills up with text to scroll through.

// cardState is what the card is showing.
type cardState int

const (
	cardAsk     cardState = iota // waiting for the address
	cardSending                  // address received, email being sent
	cardSent                     // email sent, waiting for the link to be opened
	cardInvalid                  // not a student address
	cardInUse                    // address already registered
	cardTooMany                  // too many emails requested
	cardFailed                   // something broke; try again
	cardExpired                  // the link expired
)

// card is everything one rendering of the card needs.
type card struct {
	userID string
	state  cardState
	email  string // what the member entered, once they have
}

const cardColor = 0xFFC900

func (c card) step() int {
	switch c.state {
	case cardSending, cardSent:
		return 2
	default:
		return 1
	}
}

// steps renders the checklist, ticking what is done and pointing at what is
// next.
func (c card) steps() string {
	lines := []string{
		"Γράψε το ακαδημαϊκό σου email",
		"Άνοιξε το email που θα σου στείλω",
		"Πάτα **Σύνδεση λογαριασμού** και μετά **Αποδοχή**",
	}
	var b strings.Builder
	for i, l := range lines {
		n := i + 1
		switch {
		case n < c.step():
			fmt.Fprintf(&b, "✅ ~~%s~~\n", stripBold(l))
		case n == c.step():
			fmt.Fprintf(&b, "👉 **%s**\n", stripBold(l))
		default:
			fmt.Fprintf(&b, "%d. %s\n", n, l)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func stripBold(s string) string { return strings.ReplaceAll(s, "**", "") }

// status is the block under the checklist: what happened and what to do now.
func (c card) status() string {
	entered := ""
	if c.email != "" {
		entered = fmt.Sprintf("Έστειλες το `%s` για email.\n", moderation.Quote(c.email))
	}
	switch c.state {
	case cardSending:
		return entered + "Στέλνω το email…"
	case cardSent:
		s := entered + "📬 **Σου έστειλα email.** Άνοιξέ το και πάτα **Σύνδεση λογαριασμού**.\n" +
			fmt.Sprintf("-# Ο σύνδεσμος λήγει σε %d λεπτά. Αν δεν το βρίσκεις, κοίτα και στα spam.", int(linkTTL.Minutes()))
		if csdEmailRegex.MatchString(c.email) {
			s += "\n-# Είσαι από άλλο τμήμα, οπότε θα μπεις ως Verified Outsider."
		}
		return s
	case cardInvalid:
		return entered + "❌ **Αυτό δεν είναι φοιτητικό email.**\n" +
			"Πρέπει να μοιάζει με `th12345@edu.hmu.gr` ή `csd1234@csd.uoc.gr`."
	case cardInUse:
		return entered + "❌ **Αυτό το email έχει ήδη χρησιμοποιηθεί.**\n" +
			"Αν είναι δικό σου, πάτα **Χρειάζομαι βοήθεια**."
	case cardTooMany:
		return entered + "✋ **Έχεις ζητήσει πολλά email.**\n" +
			"Πάτα **Χρειάζομαι βοήθεια** και θα σε βοηθήσουμε."
	case cardFailed:
		return entered + "⚠️ **Κάτι πήγε στραβά.** Δοκίμασε ξανά σε λίγο."
	case cardExpired:
		return "⌛ **Ο σύνδεσμος έληξε.** Γράψε ξανά το email σου για να σου στείλω νέο."
	default:
		return "Πάτα το κουμπί και γράψε το email που σου έδωσε η γραμματεία.\n" +
			"-# Π.χ. `th12345@edu.hmu.gr` ή, για άλλα τμήματα, `csd1234@csd.uoc.gr`"
	}
}

// buttons are the card's actions: one main action for the current step, and
// the restart.
func (c card) buttons() discordgo.ActionsRow {
	var row []discordgo.MessageComponent
	email := func(label string, style discordgo.ButtonStyle) discordgo.Button {
		return discordgo.Button{
			Label: label, Style: style, CustomID: emailButtonID,
			Emoji: &discordgo.ComponentEmoji{Name: "📧"},
		}
	}
	switch c.state {
	case cardAsk:
		row = append(row, email("Ολοκλήρωση Εγγραφής", discordgo.PrimaryButton))
	case cardSending:
		b := email("Ολοκλήρωση Εγγραφής", discordgo.PrimaryButton)
		b.Disabled = true
		row = append(row, b)
	case cardSent:
		if hmuEmailRegex.MatchString(c.email) {
			row = append(row, discordgo.Button{
				Label: "Άνοιγμα webmail", Style: discordgo.LinkButton, URL: "https://webmail.edu.hmu.gr",
			})
		}
		row = append(row, email("Αλλαγή email", discordgo.SecondaryButton))
	case cardTooMany:
	case cardExpired:
		row = append(row, email("Στείλε μου νέο email", discordgo.PrimaryButton))
	default:
		row = append(row, email("Δοκίμασε ξανά", discordgo.PrimaryButton))
	}
	row = append(row,
		discordgo.Button{
			Label: "Χρειάζομαι βοήθεια", Style: discordgo.SecondaryButton, CustomID: helpButtonID,
			Emoji: &discordgo.ComponentEmoji{Name: "🙋"},
		},
		discordgo.Button{
			Label: "Από την αρχή", Style: discordgo.SecondaryButton, CustomID: restartButtonID,
			Disabled: c.state == cardSending,
		})
	return discordgo.ActionsRow{Components: row}
}

// components renders the whole card.
func (c card) components() []discordgo.MessageComponent {
	color := cardColor
	small := discordgo.SeparatorSpacingSizeSmall
	return []discordgo.MessageComponent{discordgo.Container{
		AccentColor: &color,
		Components: []discordgo.MessageComponent{
			discordgo.TextDisplay{Content: fmt.Sprintf("## Καλώς ήρθες στο Steki 👋\n"+
				"<@%s>, για να μπεις πρέπει να επιβεβαιώσουμε ότι είσαι φοιτητής. Είναι 3 βήματα:", c.userID)},
			discordgo.TextDisplay{Content: c.steps()},
			discordgo.Separator{Spacing: &small},
			discordgo.TextDisplay{Content: c.status()},
			c.buttons(),
			discordgo.Separator{Spacing: &small},
			discordgo.TextDisplay{Content: "-# Κόλλησες; Πάτα **Χρειάζομαι βοήθεια** για να μιλήσεις με την ομάδα, " +
				"ή **Από την αρχή** για να ξεκινήσεις ξανά."},
		},
	}}
}

// postCard sends a new card. Only the first one pings the member.
func (h *Handler) postCard(s *discordgo.Session, channelID string, c card) {
	_, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Components:      c.components(),
		Flags:           discordgo.MessageFlagsIsComponentsV2,
		AllowedMentions: moderation.SafeMentions,
	})
	if err != nil {
		h.log.Warn("post registration card failed", "channel_id", channelID, "error", err)
	}
}

// editCard redraws an existing card.
func (h *Handler) editCard(s *discordgo.Session, channelID, messageID string, c card) {
	rows := c.components()
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         messageID,
		Components: &rows,
		Flags:      discordgo.MessageFlagsIsComponentsV2,
	})
	if err != nil {
		h.log.Warn("edit registration card failed", "channel_id", channelID, "error", err)
	}
}
