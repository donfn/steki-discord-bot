// Package mail sends the registration email over Resend.
//
// The email carries a single link to the bot's consent page rather than a
// code: the member opens it, confirms that their Discord account and academic
// address belong together, and is let in. Its design follows that page, so
// the email and the page read as one step.
package mail

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/resend/resend-go/v3"
)

const subject = "Εγγραφή στο Steki"

// Mailer sends registration emails.
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

// Link is everything one registration email shows.
type Link struct {
	URL        string        // the consent page link
	Username   string        // the member's Discord name
	AvatarURL  string        // the member's Discord avatar
	Email      string        // the address the email is sent to
	UniName    string        // the university the address belongs to
	UniLogoURL string        // absolute URL of the university's logo
	LogoURL    string        // absolute URL of the Steki logo
	ValidFor   time.Duration // how long the link works
}

// SendVerificationLink emails the consent link. It retries once on failure,
// because a transient provider error should not cost the member their attempt.
func (m *Mailer) SendVerificationLink(ctx context.Context, l Link) error {
	req := &resend.SendEmailRequest{
		From:    m.from,
		To:      []string{l.Email},
		Subject: subject,
		Text:    plainBody(l),
		Html:    htmlBody(l),
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
				return fmt.Errorf("send verification link: %w", errors.Join(lastErr, ctx.Err()))
			case <-time.After(time.Second):
			}
		}
	}
	return fmt.Errorf("send verification link: %w", lastErr)
}

func minutes(d time.Duration) int {
	return int(d.Round(time.Minute) / time.Minute)
}

func plainBody(l Link) string {
	return fmt.Sprintf(
		"Γειά σου %s,\n\n"+
			"Για να μπεις στο Steki, άνοιξε τον παρακάτω σύνδεσμο και επιβεβαίωσε ότι ο λογαριασμός σου "+
			"στο Discord συνδέεται με το %s:\n\n%s\n\n"+
			"Ο σύνδεσμος ισχύει για %d λεπτά και μόνο μία φορά.\n\n"+
			"Δεν ζήτησες εσύ αυτή την εγγραφή; Αγνόησε αυτό το μήνυμα, κανείς δεν θα μπει με το email σου.\n",
		l.Username, l.Email, l.URL, minutes(l.ValidFor))
}

// htmlBody renders the email. Mail clients ignore stylesheets and most modern
// CSS, so it is built from tables with inline styles, and it pins the light
// scheme: clients that recolour emails for dark mode would turn the brand
// yellow into mud.
func htmlBody(l Link) string {
	e := html.EscapeString
	// The address breaks only at the @ on a narrow screen: thXXXXX / @edu.hmu.gr.
	user, domain, _ := strings.Cut(l.Email, "@")
	r := strings.NewReplacer(
		"{{EMAILUSER}}", e(user),
		"{{EMAILDOMAIN}}", e(domain),
		"{{URL}}", e(l.URL),
		"{{USERNAME}}", e(l.Username),
		"{{AVATAR}}", e(l.AvatarURL),
		"{{EMAIL}}", e(l.Email),
		"{{UNI}}", e(l.UniName),
		"{{UNILOGO}}", e(l.UniLogoURL),
		"{{LOGO}}", e(l.LogoURL),
		"{{MINUTES}}", fmt.Sprint(minutes(l.ValidFor)),
	)
	return r.Replace(htmlTemplate)
}

const htmlTemplate = `<!doctype html>
<html lang="el">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light only">
<meta name="supported-color-schemes" content="light">
<title>Εγγραφή στο Steki</title>
</head>
<body style="margin:0;padding:0;background:#FFC900;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">Πάτα τον σύνδεσμο για να μπεις στο Steki. Ισχύει για {{MINUTES}} λεπτά.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#FFC900;">
<tr><td align="center" style="padding:32px 16px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:440px;background:#FFFFFF;border-radius:28px;font-family:Manrope,-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#0B0B0C;">
  <tr><td style="padding:32px 28px 28px;">

    <table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
      <td style="padding-right:10px;"><img src="{{LOGO}}" width="36" height="36" alt="" style="display:block;border-radius:10px;"></td>
      <td style="font-size:16px;font-weight:800;">στέκι</td>
    </tr></table>

    <h1 style="margin:24px 0 10px;font-size:28px;line-height:1.15;font-weight:800;letter-spacing:-0.5px;">Σύνδεσε τον λογαριασμό σου</h1>
    <p style="margin:0 0 24px;font-size:16px;line-height:1.55;color:#5C5C5E;">Γειά σου <strong style="color:#0B0B0C;">{{USERNAME}}</strong>! Για να μπεις στο Steki, επιβεβαίωσε ότι ο λογαριασμός σου στο Discord συνδέεται με αυτό το ακαδημαϊκό email.</p>

    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 24px;"><tr>
      <td width="44%" align="center" valign="top" style="background:#F6F5F1;border-radius:20px;padding:18px 8px 14px;">
        <img src="{{AVATAR}}" width="64" height="64" alt="" style="display:block;width:64px;height:64px;border-radius:32px;">
        <div style="margin-top:10px;font-size:14px;font-weight:700;color:#0B0B0C;word-break:break-word;">{{USERNAME}}</div>
        <div style="font-size:12px;font-weight:600;color:#5C5C5E;">Discord</div>
      </td>
      <td width="12%" align="center" valign="middle">
        <div style="width:36px;height:36px;line-height:36px;border-radius:18px;background:#FFC900;font-size:18px;text-align:center;">&#128279;</div>
      </td>
      <td width="44%" align="center" valign="top" style="background:#F6F5F1;border-radius:20px;padding:18px 8px 14px;">
        <img src="{{UNILOGO}}" width="64" height="64" alt="" style="display:block;width:64px;height:64px;border-radius:32px;background:#FFFFFF;">
        <div style="margin-top:10px;font-size:13px;font-weight:700;color:#0B0B0C;">{{EMAILUSER}}<wbr>@{{EMAILDOMAIN}}</div>
        <div style="font-size:12px;font-weight:600;color:#5C5C5E;">{{UNI}}</div>
      </td>
    </tr></table>

    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
      <td align="center" style="background:#0B0B0C;border-radius:16px;">
        <a href="{{URL}}" style="display:block;padding:17px 20px;font-size:16px;font-weight:800;color:#FFFFFF;text-decoration:none;">Σύνδεση λογαριασμού</a>
      </td>
    </tr></table>

    <p style="margin:16px 0 0;font-size:13px;line-height:1.5;color:#5C5C5E;text-align:center;">Ο σύνδεσμος ισχύει για {{MINUTES}} λεπτά και μόνο μία φορά.</p>

    <p style="margin:24px 0 0;padding-top:20px;border-top:1px solid #E7E7E7;font-size:13px;line-height:1.5;color:#5C5C5E;"><strong style="color:#0B0B0C;">Δεν ζήτησες εσύ αυτή την εγγραφή;</strong> Αγνόησε αυτό το μήνυμα. Κανείς δεν θα μπει στο Steki με το email σου χωρίς να πατήσει αποδοχή στη σελίδα.</p>
    <p style="margin:12px 0 0;font-size:12px;line-height:1.5;color:#8A8A8C;">Αν το κουμπί δεν ανοίγει, αντέγραψε αυτόν τον σύνδεσμο στον browser σου:<br><a href="{{URL}}" style="color:#5C5C5E;word-break:break-all;">{{URL}}</a></p>

  </td></tr>
  </table>
</td></tr>
</table>
</body>
</html>`
