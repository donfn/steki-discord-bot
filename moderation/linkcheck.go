package moderation

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// timeoutDuration is how long a member is muted after posting a blacklisted
// link.
const timeoutDuration = 24 * time.Hour

var urlRegex = regexp.MustCompile(
	`https?://[a-zA-Z0-9][a-zA-Z0-9\-]+[a-zA-Z0-9]\.[^\s]{2,}` +
		`|www\.[a-zA-Z0-9][a-zA-Z0-9\-]+[a-zA-Z0-9]\.[^\s]{2,}` +
		`|https?://[a-zA-Z0-9]+\.[^\s]{2,}` +
		`|www\.[a-zA-Z0-9]+\.[^\s]{2,}`)

// ContainsURL reports whether text contains anything that looks like a link.
func ContainsURL(text string) bool {
	return urlRegex.MatchString(text)
}

// LinkChecker deletes messages containing blacklisted link fragments.
//
// Google Web Risk scanning was removed: it required application default
// credentials that were never present in the deployment, so the client failed
// to initialise on every boot and no message was ever actually scanned.
type LinkChecker struct {
	blacklist []string
	modlog    *Log
	log       *slog.Logger
}

// NewLinkChecker returns a checker for the given lowercase link fragments.
func NewLinkChecker(blacklist []string, modlog *Log, logger *slog.Logger) *LinkChecker {
	return &LinkChecker{blacklist: blacklist, modlog: modlog, log: logger}
}

// Check reports whether the message was removed.
func (l *LinkChecker) Check(s *discordgo.Session, m *discordgo.Message) bool {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" || len(l.blacklist) == 0 {
		return false
	}

	links := urlRegex.FindAllString(m.Content, -1)
	if len(links) == 0 {
		return false
	}

	for _, link := range links {
		lower := strings.ToLower(link)
		for _, banned := range l.blacklist {
			if strings.Contains(lower, banned) {
				l.punish(s, m, banned)
				return true
			}
		}
	}
	return false
}

// punish warns the member, times them out and removes the message.
func (l *LinkChecker) punish(s *discordgo.Session, m *discordgo.Message, matched string) {
	warning := fmt.Sprintf(
		"<@%s> Το τελευταίο σου μήνυμα:``` %s```περιέχει συνδέσμους με μη επιτρεπτές λέξεις. "+
			"Γι' αυτό, το μήνυμά σου διαγράφτηκε και έγινε καταγραφή από την Ομάδα Διαχείρισης. "+
			"Λόγω εξάρσεων επιθέσεων spam σε servers του Discord, σου εφαρμόστηκε προσωρινά timeout.\n"+
			"**Αν δεν έστειλες εσύ το μήνυμα, άλλαξε κωδικό άμεσα και ενεργοποίησε 2FA!**",
		m.Author.ID, m.Content)

	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		if _, err := s.ChannelMessageSend(dm.ID, warning); err != nil {
			l.log.Debug("blacklist DM failed", "user_id", m.Author.ID, "error", err)
		}
	}

	l.modlog.Post(s, fmt.Sprintf(
		"Στο κανάλι <#%s>, ο χρήστης <@%s> έστειλε σύνδεσμο με μη επιτρεπτή λέξη (`%s`): ``` %s```",
		m.ChannelID, m.Author.ID, matched, m.Content))

	until := time.Now().Add(timeoutDuration)
	if err := s.GuildMemberTimeout(m.GuildID, m.Author.ID, &until); err != nil {
		l.log.Warn("timeout failed", "user_id", m.Author.ID, "error", err)
	}
	if err := s.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		l.log.Warn("delete blacklisted message failed", "message_id", m.ID, "error", err)
	}
	l.log.Info("removed blacklisted link",
		"user_id", m.Author.ID, "channel_id", m.ChannelID, "matched", matched)
}
