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

// headerCID names the inline header picture in the HTML.
const headerCID = "steki-header"

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
	Logo       []byte        // the Steki logo PNG, drawn into the header
	UniLogo    []byte        // the university logo PNG, drawn into the header
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
	}
	header, err := renderHeader(ctx, l)
	if err != nil {
		m.log.Warn("email header not drawn; sending the HTML one", "error", err)
		req.Html = htmlBody(l, false)
	} else {
		req.Html = htmlBody(l, true)
		req.Attachments = []*resend.Attachment{{
			Content: header, Filename: "steki.png", ContentType: "image/png", ContentId: headerCID,
		}}
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
		"Γεια σου %s,\n\n"+
			"Για να μπεις στο Steki, επιβεβαίωσε με ένα κλικ ότι ο λογαριασμός σου στο Discord "+
			"και το %s είναι δικά σου:\n\n%s\n\n"+
			"Ο σύνδεσμος ισχύει για %d λεπτά και ανοίγει μόνο μία φορά.\n\n"+
			"Δεν το ζήτησες εσύ; Αγνόησε αυτό το email. Χωρίς το κλικ σου κανείς δεν μπορεί να χρησιμοποιήσει το email σου.\n",
		l.Username, l.Email, l.URL, minutes(l.ValidFor))
}

// htmlBody renders the email in the style of the pages: warm paper ground, a
// plain sheet with a hairline border, a yellow button. Mail clients ignore
// stylesheets and most modern CSS, so it is built from tables with inline
// styles. It pins the light scheme: clients that recolour emails for dark mode
// would turn the brand yellow into mud. With picture set, the
// top is the inline header PNG (see renderHeader); without, it is HTML.
func htmlBody(l Link, picture bool) string {
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
	header := fallbackHeader
	if picture {
		header = htmlHeader
	}
	return r.Replace(strings.Replace(htmlTemplate, "{{HEADER}}", header, 1))
}

// htmlHeader is the top of the email when it is sent as a picture: the PNG
// carries the logo, headline and Discord ↔ university pair, and its alt text
// says the same for clients that block images.
const htmlHeader = `      <img src="cid:` + headerCID + `" width="384" alt="Σύνδεσε τον λογαριασμό σου: {{USERNAME}} ↔ {{EMAIL}}" style="display:block;width:100%;max-width:384px;height:auto;margin:0 0 20px;border:0;">
`

// fallbackHeader is the same top in HTML, for when the picture cannot be drawn.
const fallbackHeader = `      <img src="{{LOGO}}" width="52" height="52" alt="στέκι" style="display:block;width:52px;height:52px;border-radius:12px;">

      <h1 style="margin:22px 0 10px;font-family:'Commissioner',-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;font-size:28px;line-height:1.2;font-weight:700;color:#1B1A17;">Σύνδεσε τον λογαριασμό σου</h1>

      <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 22px;"><tr>
        <td width="43%" align="center" valign="top" style="background:#F8F6EF;border:1px solid #D9D5C8;border-radius:8px;padding:16px 8px 12px;">
          <img src="{{AVATAR}}" width="64" height="64" alt="" style="display:block;width:64px;height:64px;border-radius:34px;">
          <div style="margin-top:10px;font-size:14px;font-weight:700;color:#1B1A17;word-break:break-word;">{{USERNAME}}</div>
          <div style="font-size:12px;color:#5A5648;">Discord</div>
        </td>
        <td width="14%" align="center" valign="middle">
          <div style="width:34px;height:34px;line-height:34px;font-size:18px;font-weight:700;text-align:center;color:#1B1A17;">&#8596;</div>
        </td>
        <td width="43%" align="center" valign="top" style="background:#F8F6EF;border:1px solid #D9D5C8;border-radius:8px;padding:16px 8px 12px;">
          <img src="{{UNILOGO}}" width="64" height="64" alt="" style="display:block;width:64px;height:64px;border-radius:34px;background:#FFFFFF;">
          <div style="margin-top:10px;font-size:13px;font-weight:700;color:#1B1A17;">{{EMAILUSER}}<wbr>@{{EMAILDOMAIN}}</div>
          <div style="font-size:12px;color:#5A5648;">{{UNI}}</div>
        </td>
      </tr></table>
`

const htmlTemplate = `<!doctype html>
<html lang="el">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light only">
<meta name="supported-color-schemes" content="light">
<title>Εγγραφή στο Steki</title>
</head>
<body style="margin:0;padding:0;background:#F8F6EF;">
<div style="display:none;max-height:0;overflow:hidden;opacity:0;">Επιβεβαίωσε την εγγραφή σου στο Steki. Ο σύνδεσμος ισχύει για {{MINUTES}} λεπτά.</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#F8F6EF;">
<tr><td align="center" style="padding:32px 16px;">
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:446px;">
  <tr><td>
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#FEFDF8;border:1px solid #D9D5C8;border-radius:8px;font-family:'Commissioner',-apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1B1A17;">
    <tr><td style="padding:32px 26px 26px;">

{{HEADER}}
      <p style="margin:0 0 22px;font-size:16px;line-height:1.55;color:#5A5648;">Γεια σου <strong style="color:#1B1A17;">{{USERNAME}}</strong>. Πάτα το κουμπί για να επιβεβαιώσεις ότι ο λογαριασμός σου στο Discord και αυτό το ακαδημαϊκό email είναι δικά σου.</p>


      <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0"><tr>
        <td align="center" style="background:#FFC900;border-radius:8px;">
          <a href="{{URL}}" style="display:block;padding:16px 20px;font-size:17px;font-weight:700;color:#1B1A17;text-decoration:none;">Σύνδεση λογαριασμού</a>
        </td>
      </tr></table>

      <p style="margin:18px 0 0;font-size:13px;line-height:1.5;color:#5A5648;text-align:center;">Ο σύνδεσμος ισχύει για {{MINUTES}} λεπτά και ανοίγει μόνο μία φορά.</p>

      <p style="margin:24px 0 0;padding-top:18px;border-top:1px solid #D9D5C8;font-size:13px;line-height:1.5;color:#5A5648;"><strong style="color:#1B1A17;">Δεν το ζήτησες εσύ;</strong> Αγνόησε αυτό το email. Χωρίς το κλικ σου κανείς δεν μπορεί να μπει στο Steki με το email σου.</p>
      <p style="margin:12px 0 0;font-size:12px;line-height:1.5;color:#7A7562;">Αν το κουμπί δεν δουλεύει, αντίγραψε αυτόν τον σύνδεσμο στον browser σου:<br><a href="{{URL}}" style="color:#5A5648;word-break:break-all;">{{URL}}</a></p>

    </td></tr>
    </table>
  </td></tr>
  </table>
</td></tr>
</table>
</body>
</html>`
