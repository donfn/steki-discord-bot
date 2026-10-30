package events

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"steki-bot/store"
)

func (h *Handler) cmdEmailWhois(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	input := strings.TrimSpace(optString(sub.Options, "email"))
	ctx, cancel := h.ctx()
	defer cancel()

	if store.IsLegacyHash(input) {
		rec, err := h.st.GetLegacyHash(ctx, input)
		switch {
		case errors.Is(err, store.ErrNotFound):
			h.reply(s, i, fmt.Sprintf("Το hash `%s` δεν υπάρχει στη βάση.", strings.ToLower(input)))
		case err != nil:
			h.dbError(s, i, err)
		default:
			owner := "άγνωστος"
			if rec.UserID != nil {
				owner = fmt.Sprintf("<@%s>", *rec.UserID)
			}
			h.reply(s, i, fmt.Sprintf(
				"**Hash** `%s`\n• Κάτοχος: %s\n• Πηγή: `%s`\n• Εισήχθη: <t:%d:f>",
				rec.Hash, owner, rec.Source, rec.ImportedAt.Unix()))
		}
		return
	}
	email := store.NormalizeEmail(input)

	rec, err := h.st.WhoIs(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		use, err := h.st.CheckEmail(ctx, email)
		switch {
		case err != nil:
			h.dbError(s, i, err)
		case use.Legacy:
			h.reply(s, i, fmt.Sprintf(
				"Το `%s` υπάρχει μόνο ως παλιό hash (από πριν τη μετάβαση σε Postgres) και ο κάτοχός του είναι άγνωστος.", email))
		default:
			h.reply(s, i, fmt.Sprintf("Το `%s` δεν έχει χρησιμοποιηθεί.", email))
		}
		return
	}
	if err != nil {
		h.dbError(s, i, err)
		return
	}

	owner := "άγνωστος"
	if rec.UserID != nil {
		owner = fmt.Sprintf("<@%s>", *rec.UserID)
	}
	h.reply(s, i, fmt.Sprintf(
		"**`%s`**\n• Κάτοχος: %s\n• Τμήμα: `%s`\n• Πηγή: `%s`\n• Καταχωρήθηκε: <t:%d:f>",
		rec.Email, owner, rec.Domain, rec.Source, rec.VerifiedAt.Unix()))
}

// cmdEmailList shows everything bound to one account: the read that goes with
// whois, which looks an address up rather than a person.
func (h *Handler) cmdEmailList(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	emails, err := h.st.EmailsForUser(ctx, user.ID)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	hashes, err := h.st.LegacyHashesForUser(ctx, user.ID)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if len(emails) == 0 && len(hashes) == 0 {
		h.reply(s, i, fmt.Sprintf("Δεν υπάρχει email στη βάση για τον <@%s>.", user.ID))
		return
	}

	const shown = 10
	var b strings.Builder
	fmt.Fprintf(&b, "**Email του <@%s>**\n", user.ID)
	for n, e := range emails {
		if n == shown {
			fmt.Fprintf(&b, "… και %d ακόμα\n", len(emails)-shown)
			break
		}
		fmt.Fprintf(&b, "• `%s` · `%s` · <t:%d:d>\n", e.Email, e.Source, e.VerifiedAt.Unix())
	}
	if len(hashes) > 0 {
		b.WriteString("\n**Παλιά hashes** (μόνο το hash είναι γνωστό)\n")
	}
	for n, r := range hashes {
		if n == shown {
			fmt.Fprintf(&b, "… και %d ακόμα\n", len(hashes)-shown)
			break
		}
		fmt.Fprintf(&b, "• `%s` · `%s`\n", r.Hash, r.Source)
	}
	h.reply(s, i, b.String())
}

// cmdEmailUpdate changes a recorded address: who owns it, or the address itself
// when it was recorded wrongly. A hash can change owner but has no address to
// correct. Nothing here touches the member's roles or verification.
func (h *Handler) cmdEmailUpdate(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	input := strings.TrimSpace(optString(sub.Options, "email"))
	user := optUser(i, sub.Options, "user")
	unlink := false
	if o := findOpt(sub.Options, "unlink"); o != nil {
		unlink = o.BoolValue()
	}
	newEmail := store.NormalizeEmail(optString(sub.Options, "new_email"))
	isHash := store.IsLegacyHash(input)

	switch {
	case user == nil && !unlink && newEmail == "":
		h.reply(s, i, "Πες τι να αλλάξει: `user`, `unlink` ή `new_email`.")
		return
	case user != nil && unlink:
		h.reply(s, i, "Δώσε είτε `user` είτε `unlink`, όχι και τα δύο.")
		return
	case newEmail != "" && isHash:
		h.reply(s, i, "Ένα hash δεν έχει διεύθυνση για διόρθωση. Πρόσθεσε το σωστό email με `/email add`.")
		return
	case newEmail != "" && store.EmailDomain(newEmail) == "":
		h.reply(s, i, fmt.Sprintf("Το `%s` δεν είναι διεύθυνση email.", newEmail))
		return
	}

	var owner *string
	ownerID := ""
	if user != nil {
		owner, ownerID = &user.ID, user.ID
	}

	ctx, cancel := h.ctx()
	defer cancel()

	var summary string
	if isHash {
		ok, err := h.st.SetLegacyHashOwner(ctx, input, owner)
		if err != nil {
			h.dbError(s, i, err)
			return
		}
		if !ok {
			h.reply(s, i, fmt.Sprintf("Το hash `%s` δεν υπάρχει στη βάση.", strings.ToLower(input)))
			return
		}
		summary = fmt.Sprintf("Το hash `%s` %s.", strings.ToLower(input), ownerPhrase(owner))
	} else {
		rec, err := h.st.UpdateVerifiedEmail(ctx, input, store.EmailEdit{NewEmail: newEmail, Owner: owner, Unlink: unlink})
		switch {
		case errors.Is(err, store.ErrEmailTaken):
			h.reply(s, i, fmt.Sprintf("Το `%s` υπάρχει ήδη στη βάση. Δες `/email whois`.", newEmail))
			return
		case errors.Is(err, store.ErrNotFound):
			// An address known only by its old hash can still change owner.
			if newEmail != "" {
				h.reply(s, i, fmt.Sprintf("Το `%s` δεν βρέθηκε στη βάση.", store.NormalizeEmail(input)))
				return
			}
			ok, herr := h.st.SetLegacyHashOwner(ctx, store.LegacyHash(input), owner)
			if herr != nil {
				h.dbError(s, i, herr)
				return
			}
			if !ok {
				h.reply(s, i, fmt.Sprintf("Το `%s` δεν βρέθηκε στη βάση.", store.NormalizeEmail(input)))
				return
			}
			summary = fmt.Sprintf("Το `%s` υπάρχει μόνο ως παλιό hash· %s.", store.NormalizeEmail(input), ownerPhrase(owner))
		case err != nil:
			h.dbError(s, i, err)
			return
		default:
			summary = fmt.Sprintf("Το `%s` %s.", rec.Email, ownerPhrase(rec.UserID))
			if newEmail != "" {
				summary = fmt.Sprintf("Το `%s` διορθώθηκε σε `%s`. ", store.NormalizeEmail(input), rec.Email) + summary
			}
		}
	}

	detail := map[string]any{"action": "email_update", "email": input, "unlink": unlink}
	if newEmail != "" {
		detail["new_email"] = newEmail
	}
	h.audit(ownerID, "", store.EventModAction, detail, actorID(i))
	h.reply(s, i, "✏️ "+summary)
}

// ownerPhrase says who an address now belongs to.
func ownerPhrase(owner *string) string {
	if owner == nil {
		return "δεν έχει κάτοχο (μένει δεσμευμένο)"
	}
	return fmt.Sprintf("ανήκει στον <@%s>", *owner)
}

func (h *Handler) cmdEmailAdd(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	input := strings.TrimSpace(optString(sub.Options, "email"))
	isHash := store.IsLegacyHash(input)
	email := store.NormalizeEmail(input)
	if !isHash && store.EmailDomain(email) == "" {
		h.reply(s, i, fmt.Sprintf("Το `%s` δεν είναι διεύθυνση email ούτε hash SHA-256.", email))
		return
	}

	var owner *string
	ownerID := ""
	if u := optUser(i, sub.Options, "user"); u != nil {
		owner, ownerID = &u.ID, u.ID
	}

	ctx, cancel := h.ctx()
	defer cancel()

	if isHash {
		if err := h.st.AddLegacyHash(ctx, input, owner, store.SourceModerator); err != nil {
			h.dbError(s, i, err)
			return
		}
		h.audit(ownerID, "", store.EventModAction,
			map[string]any{"action": "hash_add", "hash": strings.ToLower(input)}, actorID(i))
		h.reply(s, i, fmt.Sprintf("✅ Το hash `%s` προστέθηκε στη βάση.", strings.ToLower(input)))
		return
	}

	if err := h.st.AddVerifiedEmail(ctx, email, owner, store.SourceModerator); err != nil {
		h.dbError(s, i, err)
		return
	}
	h.audit(ownerID, "", store.EventModAction,
		map[string]any{"action": "email_add", "email": email}, actorID(i))
	h.reply(s, i, fmt.Sprintf("✅ Το `%s` προστέθηκε στη βάση.", email))
}

func (h *Handler) cmdEmailRemove(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	input := strings.TrimSpace(optString(sub.Options, "email"))
	ctx, cancel := h.ctx()
	defer cancel()

	removed, err := h.st.RemoveVerifiedEmail(ctx, input)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if !removed {
		// Perhaps a raw hash was supplied instead of an address.
		if removed, err = h.st.RemoveLegacyHash(ctx, input); err != nil {
			h.dbError(s, i, err)
			return
		}
	}
	if !removed {
		h.reply(s, i, fmt.Sprintf("Το `%s` δεν βρέθηκε στη βάση.", input))
		return
	}

	h.audit("", "", store.EventModAction,
		map[string]any{"action": "email_remove", "email": input}, actorID(i))
	h.reply(s, i, fmt.Sprintf("🗑️ Το `%s` αφαιρέθηκε από τη βάση.", input))
}
