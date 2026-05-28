package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"steki-bot/config"
	"steki-bot/db"
	"steki-bot/events"
)

func main() {
	cfg := config.Load()

	database, err := db.Connect(cfg.MongoURL)
	if err != nil {
		log.Fatal("mongodb connect:", err)
	}
	defer database.Disconnect()

	token := cfg.Token
	if cfg.Env == "development" {
		token = cfg.DevToken
	}

	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("discordgo.New:", err)
	}

	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildEmojis |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildMessageReactions

	h := events.NewHandler(cfg, database)
	sess.AddHandler(h.OnReady)
	sess.AddHandler(h.OnMessage)
	sess.AddHandler(h.OnMemberAdd)
	sess.AddHandler(h.OnMemberRemove)
	sess.AddHandler(h.OnReactionAdd)
	sess.AddHandler(h.OnMessageDelete)
	sess.AddHandler(h.OnMessageUpdate)

	if err := sess.Open(); err != nil {
		log.Fatal("session open:", err)
	}
	defer sess.Close()

	log.Println("Bot is running. CTRL-C to exit.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop
}
