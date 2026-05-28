package utils

import (
	"log"

	"github.com/bwmarrin/discordgo"
)

type BotLogger struct {
	channelID string
}

func NewBotLogger(channelID string) *BotLogger {
	return &BotLogger{channelID: channelID}
}

func (b *BotLogger) Log(s *discordgo.Session, message string) {
	if _, err := s.ChannelMessageSend(b.channelID, message); err != nil {
		log.Println("botLogger:", err)
	}
}
