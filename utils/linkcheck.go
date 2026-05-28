package utils

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	webrisk "cloud.google.com/go/webrisk/apiv1"
	"cloud.google.com/go/webrisk/apiv1/webriskpb"
	"github.com/bwmarrin/discordgo"
)

var urlRegex = regexp.MustCompile(
	`https?://[a-zA-Z0-9][a-zA-Z0-9\-]+[a-zA-Z0-9]\.[^\s]{2,}` +
		`|www\.[a-zA-Z0-9][a-zA-Z0-9\-]+[a-zA-Z0-9]\.[^\s]{2,}` +
		`|https?://[a-zA-Z0-9]+\.[^\s]{2,}` +
		`|www\.[a-zA-Z0-9]+\.[^\s]{2,}`)

var threatTypes = []webriskpb.ThreatType{
	webriskpb.ThreatType_MALWARE,
	webriskpb.ThreatType_SOCIAL_ENGINEERING,
	webriskpb.ThreatType_UNWANTED_SOFTWARE,
}

type Threat struct {
	Link        string
	ThreatTypes []string
}

type LinkChecker struct {
	blacklist []string
	client    *webrisk.Client
	logger    *BotLogger
}

func NewLinkChecker(blacklist []string, logger *BotLogger) *LinkChecker {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := webrisk.NewClient(ctx)
	if err != nil {
		log.Println("webrisk client init failed (link checking degraded):", err)
		return &LinkChecker{blacklist: blacklist, logger: logger}
	}
	return &LinkChecker{blacklist: blacklist, client: client, logger: logger}
}

// Check inspects all URLs in the message. Returns true if the message was deleted.
func (l *LinkChecker) Check(s *discordgo.Session, m *discordgo.Message) (bool, error) {
	if m.Author == nil || m.Author.Bot {
		return false, nil
	}

	links := urlRegex.FindAllString(m.Content, -1)
	if len(links) == 0 {
		return false, nil
	}

	for _, link := range links {
		for _, bl := range l.blacklist {
			if strings.Contains(link, bl) {
				l.handleBlacklisted(s, m)
				return true, nil
			}
		}
	}

	if l.client != nil {
		threats := l.checkWebRisk(links)
		if len(threats) > 0 {
			l.handleMalicious(s, m, threats)
			return true, nil
		}
	}

	return false, nil
}

func (l *LinkChecker) checkWebRisk(links []string) []Threat {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var threats []Threat
	for _, link := range links {
		resp, err := l.client.SearchUris(ctx, &webriskpb.SearchUrisRequest{
			Uri:         link,
			ThreatTypes: threatTypes,
		})
		if err != nil || resp.Threat == nil {
			continue
		}
		t := Threat{Link: link}
		for _, tt := range resp.Threat.ThreatTypes {
			t.ThreatTypes = append(t.ThreatTypes, tt.String())
		}
		threats = append(threats, t)
	}
	return threats
}

func (l *LinkChecker) handleBlacklisted(s *discordgo.Session, m *discordgo.Message) {
	msg := fmt.Sprintf(
		"<@%s> Το τελευταίο σου μήνυμα:``` %s```περιέχει συνδέσμους με μη επιτρεπτές λέξεις. "+
			"Γι' αυτό, το μήνυμά σου διαγράφτηκε και έγινε καταγραφή από την Ομάδα Διαχείρισης. "+
			"Λόγω εξάρσεων επιθέσεων spam σε servers του Discord, σου εφαρμόστηκε προσωρινά timeout.\n"+
			"**Αν δεν έστειλες εσύ το μήνυμα, άλλαξε κωδικό άμεσα και ενεργοποίησε 2FA!**",
		m.Author.ID, m.Content,
	)
	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		s.ChannelMessageSend(dm.ID, msg)
	}
	if l.logger != nil {
		l.logger.Log(s, fmt.Sprintf(
			"Στο κανάλι <#%s>, ο χρήστης <@%s> έστειλε σύνδεσμο με μη επιτρεπτές λέξεις: ``` %s```",
			m.ChannelID, m.Author.ID, m.Content,
		))
	}
	until := timeoutUntil()
	s.GuildMemberTimeout(m.GuildID, m.Author.ID, &until)
	s.ChannelMessageDelete(m.ChannelID, m.ID)
}

func (l *LinkChecker) handleMalicious(s *discordgo.Session, m *discordgo.Message, threats []Threat) {
	msg := fmt.Sprintf(
		"<@%s> Στο τελευταίο σου μήνυμα:``` %s```εντοπίσαμε γνωστούς κακόβουλους συνδέσμους. "+
			"Διαγράψαμε το μήνυμά σου αυτόματα και σε κάναμε προσωρινά timeout.\n"+
			"**Αν δεν έστειλες εσύ το μήνυμα, άλλαξε κωδικό άμεσα και ενεργοποίησε 2FA!**",
		m.Author.ID, m.Content,
	)
	if dm, err := s.UserChannelCreate(m.Author.ID); err == nil {
		s.ChannelMessageSend(dm.ID, msg)
	}
	if l.logger != nil {
		var lines []string
		for _, t := range threats {
			tt := ""
			if len(t.ThreatTypes) > 0 {
				tt = t.ThreatTypes[0]
			}
			lines = append(lines, fmt.Sprintf("**%s** => ``%s``\n", t.Link, tt))
		}
		l.logger.Log(s, fmt.Sprintf(
			"Εντοπίστηκε κακόβουλος σύνδεσμος από <@%s> στο <#%s>\n\n Κατηγορίες απειλής: \n %s",
			m.Author.ID, m.ChannelID, strings.Join(lines, ""),
		))
	}
	until := timeoutUntil()
	s.GuildMemberTimeout(m.GuildID, m.Author.ID, &until)
	s.ChannelMessageDelete(m.ChannelID, m.ID)
}

func timeoutUntil() time.Time {
	return time.Now().Add(1440 * time.Minute)
}
