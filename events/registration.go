package events

import (
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"steki-bot/registration"
)

var (
	hmuEmailRegex = regexp.MustCompile(`^th[0-9]+@edu\.hmu\.gr$`)
	csdEmailRegex = regexp.MustCompile(`^csd[0-9]+@csd\.uoc\.gr$`)
)

func (h *Handler) handleRegistration(s *discordgo.Session, m *discordgo.MessageCreate, ch *discordgo.Channel) {
	reg, err := h.regRepo.FindByUser(m.Author.ID)
	if err != nil {
		log.Println("handleRegistration:", err)
		return
	}
	if reg == nil {
		reg = &registration.Registration{
			UserID: m.Author.ID,
			Step:   registration.StepSendEmail,
		}
	}

	switch reg.Step {
	case registration.StepSendEmail:
		h.stepSendEmail(s, m, reg)
	case registration.StepVerifyEmail:
		h.stepVerifyEmail(s, m, ch, reg)
	}
}

func (h *Handler) stepSendEmail(s *discordgo.Session, m *discordgo.MessageCreate, reg *registration.Registration) {
	email := strings.TrimSpace(m.Content)

	if !hmuEmailRegex.MatchString(email) && !csdEmailRegex.MatchString(email) {
		h.logger.Log(s, fmt.Sprintf("Ο χρήστης <@%s> έστειλε κάτι που δεν μοιάζει με email.", m.Author.ID))
		s.ChannelMessageSend(m.ChannelID, ":see_no_evil: Αυτό που μου έστειλες δεν μοιάζει με φοιτητικό email από το H.M.M.Y. ΕΛ.ΜΕ.ΠΑ.")
		time.AfterFunc(2*time.Second, func() {
			s.ChannelMessageSend(m.ChannelID, "Ένα ακαδημαϊκό, φοιτητικό email έχει την μορφή thXXXXX@edu.hmu.gr.")
		})
		return
	}

	hash := hashSHA256(email)
	h.logger.Log(s, fmt.Sprintf("Ο χρήστης <@%s> έστειλε mail για εγγραφή: ```Hash: %s```", m.Author.ID, hash))

	exists, err := h.regRepo.EmailExists(hash)
	if err != nil {
		log.Println("stepSendEmail: emailExists:", err)
		return
	}
	if exists {
		h.logger.Log(s, fmt.Sprintf("Το %s έχει ήδη χρησιμοποιηθεί. Hash: %s", email, hash))
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf(
			"Αυτό το email έχει ήδη χρησιμοποιηθεί για εγγραφή στο Steki. "+
				"Αν θεωρείς ότι έχει γίνει κάποιο λάθος, στείλε ένα μήνυμα στο <#939177847933780018>.\n`Αναγνωριστικό Email: %s`",
			hash,
		))
		return
	}

	s.ChannelMessageSend(m.ChannelID, "Δώσε μου μισό λεπτάκι...")

	code := generateCode()
	if err := h.mailer.SendVerificationCode(email, code); err != nil {
		log.Println("stepSendEmail: send email:", err)
		s.ChannelMessageSend(m.ChannelID, "Υπήρξε ένα σφάλμα. Παρακαλώ προσπάθησε αργότερα...")
		h.logger.Log(s, fmt.Sprintf("Κατά την αποστολή email στον <@%s> προέκυψε σφάλμα: ```%v```", m.Author.ID, err))
		return
	}

	reg.Step = registration.StepVerifyEmail
	reg.Email = email
	reg.EncryptedVerificationCode = hashSHA256(code + h.cfg.Salt)

	if err := h.regRepo.Upsert(reg); err != nil {
		log.Println("stepSendEmail: upsert:", err)
		return
	}

	if hmuEmailRegex.MatchString(email) {
		s.ChannelMessageSend(m.ChannelID, "Τέλεια! Τσέκαρε τα εισερχόμενά σου (https://webmail.edu.hmu.gr) για ένα μήνυμα με θέμα *\"Εγγραφή στο Steki\"*")
	} else {
		s.ChannelMessageSend(m.ChannelID, "Το email που έστειλες αντιστοιχεί σε εξωτερικό τμήμα. Γι' αυτό, θα γραφτείς ως Verified Outsider. Τσέκαρε το ακαδημαϊκό σου email.")
	}

	h.logger.Log(s, fmt.Sprintf("Ο χρήστης <@%s> έλαβε τον κωδικό εγγραφής ```Hash: %s```.", m.Author.ID, reg.EncryptedVerificationCode))
	time.AfterFunc(500*time.Millisecond, func() {
		s.ChannelMessageSend(m.ChannelID, "Όταν το λάβεις, στείλε μου τον κωδικό εγγραφής εδώ.")
	})
}

func (h *Handler) stepVerifyEmail(s *discordgo.Session, m *discordgo.MessageCreate, ch *discordgo.Channel, reg *registration.Registration) {
	input := strings.TrimSpace(m.Content)
	submitted := hashSHA256(input + h.cfg.Salt)

	if reg.EncryptedVerificationCode != "" && submitted == reg.EncryptedVerificationCode {
		if reg.Email != "" {
			if err := h.regRepo.AddEmail(hashSHA256(reg.Email)); err != nil {
				log.Println("stepVerifyEmail: addEmail:", err)
			}
		}

		var roleID string
		switch {
		case hmuEmailRegex.MatchString(reg.Email):
			roleID = hmuRoleID
		case csdEmailRegex.MatchString(reg.Email):
			roleID = csdRoleID
		default:
			h.logger.Log(s, fmt.Sprintf("Κατά την εγγραφή του <@%s> το mail δεν πληροί τις προϋποθέσεις. Please check.", m.Author.ID))
			s.ChannelMessageSend(m.ChannelID, "Κάτι πήγε στραβά. Δώσε λίγη ώρα για την Ομάδα Διαχείρισης να το ελέγξει...")
			return
		}

		if err := s.GuildMemberRoleAdd(m.GuildID, m.Author.ID, roleID); err != nil {
			log.Println("stepVerifyEmail: add role:", err)
		}
		h.completeRegistration(s, m.ChannelID, reg)
		h.logger.Log(s, fmt.Sprintf("Ο χρήστης <@%s> ολοκλήρωσε με επιτυχία την εγγραφή του.", m.Author.ID))
		return
	}

	// Wrong code path
	if reg.FailedAttempts > 2 || reg.EncryptedVerificationCode == "" {
		reg.EncryptedVerificationCode = ""
		reg.FailedAttempts = 0
		_ = h.regRepo.Upsert(reg)
		h.logger.Log(s, fmt.Sprintf("Ο χρήστης <@%s> έβαλε 4 φορές λάθος κωδικό. Η διαδικασία πρέπει να επανακινηθεί.", m.Author.ID))
		s.ChannelMessageSend(m.ChannelID, "Για λόγους ασφαλείας ο κωδικός εγγραφής σου έχει καταστραφεί. Παρακαλώ επανεκκίνησε τη διαδικασία, πατώντας το :arrows_counterclockwise: που βρίσκεται παραπάνω.")
		return
	}

	s.ChannelMessageSend(m.ChannelID, "Χμμμ... Αυτή δεν ήταν η απάντηση που περίμενα. Χρειάζομαι *μόνο* τον 6-ψήφιο κωδικό εγγραφής που αναγράφει το mail :woozy_face:.")
	s.ChannelMessageSend(m.ChannelID, "*Ενημέρωση από την Ομάδα Διαχείρισης, λόγω πρόσφατων συμβάντων:*\n```Οποιαδήποτε προσπάθεια tampering με το bot καταγράφεται, ενώ επαναλαμβανόμενοι παραβάτες θα γίνονται ban από τον σέρβερ οριστικά.```")
	reg.FailedAttempts++
	_ = h.regRepo.Upsert(reg)
}
