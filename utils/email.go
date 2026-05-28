package utils

import (
	"context"
	"fmt"
	"time"

	"github.com/mailgun/mailgun-go/v4"
)

type Mailer struct {
	mg *mailgun.MailgunImpl
}

func NewMailer(apiToken string) *Mailer {
	mg := mailgun.NewMailgun("poiw.org", apiToken)
	mg.SetAPIBase("https://api.eu.mailgun.net/v3")
	return &Mailer{mg: mg}
}

func (m *Mailer) SendVerificationCode(to, code string) error {
	body := fmt.Sprintf(
		"Ο κωδικός εγγραφής σου στο Steki είναι: %s\n\n** Μην δώσεις τον παραπάνω κωδικό σε κανένα **",
		code,
	)
	msg := m.mg.NewMessage("Steki <steki@poiw.org>", "Εγγραφή στο Steki", body, to)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := m.mg.Send(ctx, msg)
	return err
}
