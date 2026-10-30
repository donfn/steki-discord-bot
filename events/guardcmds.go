package events

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/store"
	"steki-bot/verify"
)

// discordTime renders a timestamp the way Discord localises for each reader.
func discordTime(t time.Time) string {
	return fmt.Sprintf("<t:%d:f> (<t:%d:R>)", t.Unix(), t.Unix())
}

func (h *Handler) cmdGuardStatus(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	g, err := h.st.GuardStatus(ctx, user.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(s, i, fmt.Sprintf("Ο <@%s> δεν έχει στείλει ποτέ συνημμένο ή σύνδεσμο, οπότε δεν υπάρχουν στοιχεία προστασίας.", user.ID))
		return
	}
	if err != nil {
		h.dbError(s, i, err)
		return
	}

	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "**Προστασία για <@%s>**\n", user.ID)
	switch {
	case g.LinkPending && g.RestrictedAt != nil:
		fmt.Fprintf(&b, "🔒 Σε περιορισμό από %s, μέχρι να λύσει το captcha.\n", discordTime(*g.RestrictedAt))
	case g.LinkPending:
		b.WriteString("🔒 Του έχει σταλεί σύνδεσμος captcha που δεν έχει λυθεί ακόμα.\n")
	default:
		b.WriteString("✅ Κανένας ενεργός περιορισμός.\n")
	}
	if g.ExemptUntil != nil && g.ExemptUntil.After(now) {
		fmt.Fprintf(&b, "🕊️ Περίοδος χάριτος μέχρι %s. Αφαίρεση με `/guard ungrace`.\n", discordTime(*g.ExemptUntil))
	} else {
		b.WriteString("Χωρίς περίοδο χάριτος.\n")
	}
	if g.WindowStart != nil && now.Sub(*g.WindowStart) < guardWindow {
		fmt.Fprintf(&b, "⏱️ Έστειλε συνημμένο ή σύνδεσμο %s. Αν στείλει κι άλλο μέσα σε μία ώρα, μπαίνει σε περιορισμό.\n",
			discordTime(*g.WindowStart))
	}
	h.reply(s, i, b.String())
}

func (h *Handler) cmdGuardRestrict(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	if user.Bot {
		h.reply(s, i, "Δεν γίνεται περιορισμός σε bot.")
		return
	}
	if user.ID == actorID(i) {
		h.reply(s, i, "Δεν μπορείς να βάλεις τον εαυτό σου σε περιορισμό.")
		return
	}

	ctx, cancel := h.ctx()
	defer cancel()
	if _, err := h.guildMember(ctx, s, i.GuildID, user.ID); err != nil {
		h.reply(s, i, fmt.Sprintf("Ο <@%s> δεν είναι μέλος του server.", user.ID))
		return
	}

	tokenID, err := newTokenID()
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Σφάλμα: %v", err))
		return
	}
	if err := h.st.ForceRestriction(ctx, user.ID, i.GuildID, tokenID, time.Now()); err != nil {
		h.dbError(s, i, err)
		return
	}

	res, err := h.restrict(s, user.ID, i.GuildID, i.ChannelID, verify.ReasonManual, tokenID, actorID(i))
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Ο περιορισμός απέτυχε: %v\n"+
			"Το Discord δεν επιτρέπει timeout σε χρήστη με ρόλο ίσο ή υψηλότερο από του bot.", err))
		return
	}
	if !res.DMSent {
		h.reply(s, i, fmt.Sprintf("🔒 Ο <@%s> μπήκε σε timeout 24 ωρών, αλλά **δεν δέχεται DM**. "+
			"Στείλε του τον σύνδεσμο:\n<%s>", user.ID, res.Link))
		return
	}
	h.reply(s, i, fmt.Sprintf("🔒 Ο <@%s> μπήκε σε timeout 24 ωρών και πήρε DM με το captcha.", user.ID))
}

func (h *Handler) cmdGuardUngrace(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	cleared, err := h.st.ClearExemption(ctx, user.ID)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if !cleared {
		h.reply(s, i, fmt.Sprintf("Ο <@%s> δεν έχει ενεργή περίοδο χάριτος.", user.ID))
		return
	}
	h.audit(user.ID, "", store.EventModAction, map[string]any{"action": "guard_ungrace"}, actorID(i))
	h.alertMods(s, "🕊️ Ο <@%s> αφαίρεσε την περίοδο χάριτος του <@%s>· η προστασία ισχύει ξανά κανονικά.",
		actorID(i), user.ID)
	h.reply(s, i, fmt.Sprintf("🕊️ Η περίοδος χάριτος του <@%s> αφαιρέθηκε. "+
		"Από εδώ και πέρα η προστασία ισχύει κανονικά.", user.ID))
}
