// Package mail sends the registration verification code over Resend.
//
// It replaces the previous Mailgun implementation. The copy is unchanged from
// what members are used to receiving.
package mail

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"time"

	"github.com/resend/resend-go/v2"
)

const subject = "Εγγραφή στο Steki"

// Mailer sends verification codes.
type Mailer struct {
	client *resend.Client
	from   string
	log    *slog.Logger
}

// New builds a Mailer. from is a full RFC 5322 address such as
// "Steki <steki@poiw.org>"; its domain must be verified in Resend.
func New(apiKey, from string, logger *slog.Logger) *Mailer {
	return &Mailer{
		client: resend.NewClient(apiKey),
		from:   from,
		log:    logger,
	}
}

// SendVerificationCode emails a six-digit code. It retries once on failure,
// because a transient provider error should not cost the member their attempt.
func (m *Mailer) SendVerificationCode(ctx context.Context, to, code string) error {
	req := &resend.SendEmailRequest{
		From:    m.from,
		To:      []string{to},
		Subject: subject,
		Text:    plainBody(code),
		Html:    htmlBody(code),
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		sent, err := m.client.Emails.SendWithContext(sendCtx, req)
		cancel()

		if err == nil {
			m.log.Info("verification email sent", "message_id", sent.Id, "attempt", attempt)
			return nil
		}
		lastErr = err
		m.log.Warn("verification email failed", "attempt", attempt, "error", err)

		if ctx.Err() != nil {
			break
		}
		if attempt == 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("send verification code: %w", errors.Join(lastErr, ctx.Err()))
			case <-time.After(time.Second):
			}
		}
	}
	return fmt.Errorf("send verification code: %w", lastErr)
}

func plainBody(code string) string {
	return fmt.Sprintf(
		"Ο κωδικός εγγραφής σου στο Steki είναι: %s\n\n"+
			"Ο κωδικός ισχύει για 15 λεπτά.\n\n"+
			"** Μην δώσεις τον παραπάνω κωδικό σε κανέναν **\n",
		code,
	)
}

func htmlBody(code string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="el">
<body style="font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;
             color:#1a1a1a;line-height:1.6;padding:24px">
  <p>Ο κωδικός εγγραφής σου στο Steki είναι:</p>
  <p style="font-size:30px;font-weight:700;letter-spacing:6px;margin:20px 0">%s</p>
  <p>Ο κωδικός ισχύει για 15 λεπτά.</p>
  <p style="color:#b00020"><strong>Μην δώσεις τον παραπάνω κωδικό σε κανέναν.</strong></p>
</body>
</html>`, html.EscapeString(code))
}
