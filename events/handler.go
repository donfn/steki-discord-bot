package events

import (
	"sync"
	"sync/atomic"

	"steki-bot/config"
	"steki-bot/db"
	"steki-bot/registration"
	"steki-bot/utils"
)

type Handler struct {
	cfg     *config.Config
	logger  *utils.BotLogger
	linkChk *utils.LinkChecker
	mailer  *utils.Mailer
	regRepo *registration.Repository

	processedMessages int64
	blockedMessages   int64

	startOnce sync.Once
}

func NewHandler(cfg *config.Config, database *db.DB) *Handler {
	logger := utils.NewBotLogger(cfg.LogChannelID)
	return &Handler{
		cfg:     cfg,
		logger:  logger,
		linkChk: utils.NewLinkChecker(cfg.URLBlacklist, logger),
		mailer:  utils.NewMailer(cfg.MailgunAPIToken),
		regRepo: registration.NewRepository(database),
	}
}

func (h *Handler) incrProcessed() { atomic.AddInt64(&h.processedMessages, 1) }
func (h *Handler) incrBlocked()   { atomic.AddInt64(&h.blockedMessages, 1) }

func (h *Handler) getProcessed() int64 { return atomic.LoadInt64(&h.processedMessages) }
func (h *Handler) getBlocked() int64   { return atomic.LoadInt64(&h.blockedMessages) }
