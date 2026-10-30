package moderation

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

// timeoutDuration is how long a member is muted after posting a blacklisted
// link.
const timeoutDuration = 24 * time.Hour

// urlRegex matches anything Discord would turn into a link. It is
// deliberately loose: hosts may contain any letters, so lookalike domains
// written in Greek or Cyrillic ("dіscord.gift") are links too, and are checked
// against the phishing feed in their punycode form.
var urlRegex = regexp.MustCompile("(?i)\\bhttps?://[^\\s<>\"'`]+|\\bwww\\.[^\\s<>\"'`]+")

// bareRegex matches scheme-less domains ("evil.com/login"), which Discord
// also turns into links. It is used only for screening, not for counting
// links, because ordinary text like "e.g." or "main.go" can look like one.
var bareRegex = regexp.MustCompile("(?i)(?:[\\p{L}\\p{N}](?:[\\p{L}\\p{N}-]*[\\p{L}\\p{N}])?\\.)+\\p{L}{2,}(?:[/?#][^\\s<>\"'`]*)?")

// ContainsURL reports whether text contains anything that looks like a link.
func ContainsURL(text string) bool {
	return urlRegex.MatchString(text)
}

// SafeMentions lets the bot's messages ping the users they name but never
// @everyone, @here or a role, whatever member-written text they quote.
var SafeMentions = &discordgo.MessageAllowedMentions{
	Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers},
}

// Quote makes member text safe to embed in a ``` code block: backticks that
// would end the block early are replaced, and control and bidirectional
// characters that could reorder or hide the text are dropped.
func Quote(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "`", "ˋ")
}

// Text is everything a member authored in a message: its content and, for a
// forwarded message, the forwarded content, which Discord carries separately
// and which would otherwise slip past every check.
func Text(m *discordgo.Message) string {
	parts := []string{m.Content}
	for _, snap := range m.MessageSnapshots {
		if snap.Message != nil && snap.Message.Content != "" {
			parts = append(parts, snap.Message.Content)
		}
	}
	return strings.Join(parts, "\n")
}

// Attachments returns a message's attachments, including forwarded ones.
func Attachments(m *discordgo.Message) []*discordgo.MessageAttachment {
	out := m.Attachments
	for _, snap := range m.MessageSnapshots {
		if snap.Message != nil {
			out = append(out[:len(out):len(out)], snap.Message.Attachments...)
		}
	}
	return out
}

// LinkChecker deletes messages containing blacklisted link fragments or
// links to known phishing domains.
//
// Google Web Risk scanning was removed: it required application default
// credentials that were never present in the deployment, so the client failed
// to initialise on every boot and no message was ever actually scanned.
type LinkChecker struct {
	blacklist []string
	feed      *PhishFeed
	modlog    *Log
	log       *slog.Logger
}

// NewLinkChecker returns a checker for the given lowercase link fragments and
// phishing feed. feed may be nil.
func NewLinkChecker(blacklist []string, feed *PhishFeed, modlog *Log, logger *slog.Logger) *LinkChecker {
	return &LinkChecker{blacklist: blacklist, feed: feed, modlog: modlog, log: logger}
}

// Check reports whether the message was removed.
func (l *LinkChecker) Check(s *discordgo.Session, m *discordgo.Message) bool {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return false
	}

	text := Text(m)
	links := append(urlRegex.FindAllString(text, -1), bareRegex.FindAllString(text, -1)...)
	if len(links) == 0 {
		return false
	}

	for _, link := range links {
		lower := strings.ToLower(link)
		for _, banned := range l.blacklist {
			if strings.Contains(lower, banned) {
				l.punish(s, m, text, blacklistHit, banned)
				return true
			}
		}
		if l.feed != nil {
			if domain, ok := l.feed.Match(linkHost(link)); ok {
				l.punish(s, m, text, phishingHit, domain)
				return true
			}
		}
	}
	return false
}

type hitKind int

const (
	blacklistHit hitKind = iota
	phishingHit
)

// punish warns the member, times them out and removes the message.
func (l *LinkChecker) punish(s *discordgo.Session, m *discordgo.Message, text string, kind hitKind, matched string) {
	quoted := Quote(truncateRunes(text, 1000))
	reason, modReason := "περιέχει συνδέσμους με μη επιτρεπτές λέξεις",
		fmt.Sprintf("σύνδεσμο με μη επιτρεπτή λέξη (`%s`)", matched)
	if kind == phishingHit {
		reason, modReason = "περιέχει σύνδεσμο προς γνωστή σελίδα απάτης (phishing)",
			fmt.Sprintf("σύνδεσμο προς γνωστό domain απάτης (`%s`)", matched)
	}

	warning := fmt.Sprintf(
		"<@%s> Το τελευταίο σου μήνυμα:``` %s```%s. "+
			"Γι' αυτό διαγράφηκε και ενημερώθηκε η Ομάδα Διαχείρισης. "+
			"Επειδή τέτοια μηνύματα στέλνονται συνήθως από χακαρισμένους λογαριασμούς, μπήκες προσωρινά σε timeout.\n"+
			"**Αν δεν το έστειλες εσύ, άλλαξε αμέσως τον κωδικό σου και ενεργοποίησε το 2FA.**",
		m.Author.ID, quoted, reason)

	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		if _, err := s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
			Content: warning, AllowedMentions: SafeMentions,
		}); err != nil {
			l.log.Debug("link warning DM failed", "user_id", m.Author.ID, "error", err)
		}
	}

	l.modlog.Post(s, fmt.Sprintf(
		"Στο κανάλι <#%s>, ο χρήστης <@%s> έστειλε %s: ``` %s```",
		m.ChannelID, m.Author.ID, modReason, quoted))

	until := time.Now().Add(timeoutDuration)
	if err := s.GuildMemberTimeout(m.GuildID, m.Author.ID, &until); err != nil {
		l.log.Warn("timeout failed", "user_id", m.Author.ID, "error", err)
	}
	if err := s.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		l.log.Warn("delete flagged link message failed", "message_id", m.ID, "error", err)
	}
	l.log.Info("removed flagged link",
		"user_id", m.Author.ID, "channel_id", m.ChannelID, "kind", kind, "matched", matched)
}

// truncateRunes cuts s to at most n runes, so a quoted message always fits
// inside one log post and DM.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
