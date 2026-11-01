package registration

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/dc"
	"steki-bot/rpc"
	"steki-bot/store"
)

// historyLimit is how many events /reg history shows.
const historyLimit = 25

func (h *Handler) cmdRegStatus(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	ctx, cancel := h.Ctx()
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "**Κατάσταση για <@%s>**\n", user.ID)

	member, err := h.st.GetMember(ctx, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b.WriteString("Δεν υπάρχει εγγραφή στη βάση.\n")
	case err != nil:
		h.DBError(s, i, err)
		return
	default:
		fmt.Fprintf(&b, "• Πρώτη εμφάνιση: <t:%d:R>\n", member.FirstSeenAt.Unix())
		fmt.Fprintf(&b, "• Είσοδοι: %d · Αποβολές: %d\n", member.JoinCount, member.KickedCount)
		if member.VerifiedAt != nil {
			email := "—"
			if member.VerifiedEmail != nil {
				email = *member.VerifiedEmail
			}
			fmt.Fprintf(&b, "• ✅ Επαληθευμένος <t:%d:R> με `%s`\n", member.VerifiedAt.Unix(), email)
		}
		if member.LeftAt != nil {
			fmt.Fprintf(&b, "• Αποχώρησε <t:%d:R>\n", member.LeftAt.Unix())
		}
	}

	reg, err := h.st.OpenRegistration(ctx, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b.WriteString("\nΔεν υπάρχει ανοιχτή εγγραφή.")
	case err != nil:
		h.DBError(s, i, err)
		return
	default:
		idle := reg.IdleFor(time.Now())
		fmt.Fprintf(&b, "\n**Ανοιχτή εγγραφή**\n• Βήμα: `%s`\n", reg.Step)
		if reg.Email != nil {
			fmt.Fprintf(&b, "• Email: `%s`\n", *reg.Email)
		}
		fmt.Fprintf(&b, "• Λάθος προσπάθειες: %d · Κωδικοί: %d\n", reg.FailedAttempts, reg.CodeSentCount)
		fmt.Fprintf(&b, "• Αδράνεια: %s%s\n", idle.Round(time.Minute), reapHint(idle))
		if reg.ChannelID != nil {
			fmt.Fprintf(&b, "• Κανάλι: <#%s>\n", *reg.ChannelID)
		}
	}

	h.Reply(s, i, b.String())
}

// reapHint says what the reaper will do next, so a moderator can see at a
// glance whether someone is about to be removed.
func reapHint(idle time.Duration) string {
	switch {
	case idle >= kickAfter:
		return " ⚠️ (προς αποβολή)"
	case idle >= nudgeAfter:
		return fmt.Sprintf(" (αποβολή σε %s)", (kickAfter - idle).Round(time.Minute))
	default:
		return ""
	}
}

func (h *Handler) cmdRegHistory(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	ctx, cancel := h.Ctx()
	defer cancel()

	events, err := h.st.History(ctx, user.ID, historyLimit)
	if err != nil {
		h.DBError(s, i, err)
		return
	}
	// The other services keep what they did to the member themselves. One that
	// cannot answer costs its part of the trail, and the reply says so.
	missing := ""
	for name, peer := range map[string]*rpc.Client{"moderation": h.moderation, "web": h.web} {
		var ans rpc.EventsAnswer
		err := peer.Call(ctx, rpc.PathEvents, rpc.EventsRequest{UserID: user.ID, Limit: historyLimit}, &ans)
		if err != nil {
			h.Log.Warn("history: peer failed", "service", name, "error", err)
			missing += fmt.Sprintf("\n⚠️ Λείπει το ιστορικό της υπηρεσίας `%s`.", name)
			continue
		}
		events = append(events, ans.Events...)
	}
	slices.SortStableFunc(events, func(a, b *store.Event) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if len(events) > historyLimit {
		events = events[:historyLimit]
	}
	if len(events) == 0 {
		if missing != "" {
			h.Reply(s, i, "Δεν βρέθηκε ιστορικό."+missing)
			return
		}
		h.Reply(s, i, fmt.Sprintf("Δεν υπάρχει ιστορικό για τον <@%s>.", user.ID))
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Ιστορικό <@%s>** (τελευταία %d)\n", user.ID, len(events))
	for _, e := range events {
		fmt.Fprintf(&b, "<t:%d:f> · `%s`", e.CreatedAt.Unix(), e.Kind)
		if email, ok := e.Detail["email"].(string); ok {
			fmt.Fprintf(&b, " · %s", email)
		}
		if e.ActorID != nil {
			fmt.Fprintf(&b, " · από <@%s>", *e.ActorID)
		}
		b.WriteString("\n")
	}
	h.Reply(s, i, b.String())
}

func (h *Handler) cmdRegStuck(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	ctx, cancel := h.Ctx()
	defer cancel()

	open, err := h.st.ListOpen(ctx) // oldest activity first
	if err != nil {
		h.DBError(s, i, err)
		return
	}
	if len(open) == 0 {
		h.Reply(s, i, "✅ Κανείς δεν έχει κολλήσει σε εγγραφή.")
		return
	}

	const shown = 20
	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "**Ανοιχτές εγγραφές: %d**\n", len(open))
	for n, reg := range open {
		if n == shown {
			fmt.Fprintf(&b, "… και %d ακόμα\n", len(open)-shown)
			break
		}
		idle := reg.IdleFor(now)
		fmt.Fprintf(&b, "• <@%s> · `%s` · αδράνεια %s%s\n",
			reg.UserID, reg.Step, idle.Round(time.Minute), reapHint(idle))
	}
	h.Reply(s, i, b.String())
}

func (h *Handler) cmdRegRestart(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	ctx, cancel := h.Ctx()
	defer cancel()

	reg, err := h.st.OpenRegistration(ctx, user.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.Reply(s, i, fmt.Sprintf("Δεν βρέθηκε ανοιχτή εγγραφή για τον <@%s>.", user.ID))
		return
	}
	if err != nil {
		h.DBError(s, i, err)
		return
	}
	if reg.ChannelID == nil {
		h.Reply(s, i, "Η εγγραφή δεν έχει κανάλι. Τρέξε `/reg reconcile` για να δημιουργηθεί.")
		return
	}

	// A moderator's restart also lifts the email limits, which the member's own
	// restart button must not do. The address is only known past the email step.
	if err := h.st.ResetCodeSends(ctx, reg.ID); err != nil {
		h.DBError(s, i, err)
		return
	}
	h.Gate.Clear("mail-user:" + user.ID)
	if reg.Email != nil {
		h.Gate.Clear("mail-addr:" + *reg.Email)
	}

	if err := h.restartRegistration(s, *reg.ChannelID, user.ID, i.GuildID); err != nil {
		h.Reply(s, i, fmt.Sprintf("Η επανεκκίνηση απέτυχε: %v", err))
		return
	}
	h.Audit(user.ID, reg.ID, store.EventModAction, map[string]any{"action": "restart"}, bot.ActorID(i))
	h.Reply(s, i, fmt.Sprintf("🔄 Η εγγραφή του <@%s> επανεκκινήθηκε και το όριο των email μηδενίστηκε.", user.ID))
}

// cmdRegReregister sends a verified member back to the start: the database
// forgets their verification, the verified roles come off, and they get a
// fresh registration channel. Unless release_email is set they keep the claim
// on their address, so they can re-register with it but nobody else can.
func (h *Handler) cmdRegReregister(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	release := false
	if o := bot.FindOpt(sub.Options, "release_email"); o != nil {
		release = o.BoolValue()
	}
	if user.Bot {
		h.Reply(s, i, "Δεν γίνεται επανεγγραφή σε bot.")
		return
	}

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	if _, err := h.GuildMember(ctx, s, i.GuildID, user.ID); err != nil {
		h.Reply(s, i, fmt.Sprintf("Ο <@%s> δεν είναι μέλος του server.", user.ID))
		return
	}

	released, err := func() (int64, error) {
		defer h.LockUser(user.ID)()
		open, err := h.st.OpenRegistration(ctx, user.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
		n, err := h.st.ClearVerification(ctx, user.ID, release)
		if err != nil {
			return 0, err
		}
		// Any half-finished registration is replaced by the fresh one below.
		if open != nil && open.ChannelID != nil {
			if err := h.DeleteChannel(ctx, s, *open.ChannelID); err != nil {
				h.Log.Warn("reregister: delete old channel failed", "channel_id", *open.ChannelID, "error", err)
			}
		}
		return n, nil
	}()
	if err != nil {
		h.DBError(s, i, err)
		return
	}

	var kept []string
	for _, roleID := range bot.VerifiedRoleIDs {
		err := h.Retry(ctx, "member.role.remove", func() error {
			return s.GuildMemberRoleRemove(i.GuildID, user.ID, roleID, discordgo.WithContext(ctx))
		})
		if err != nil && !dc.IsMissingResource(err) {
			h.Log.Warn("reregister: remove role failed", "user_id", user.ID, "role_id", roleID, "error", err)
			kept = append(kept, "<@&"+roleID+">")
		}
	}

	h.revokeHelp(ctx, s, user.ID)
	h.ensureRegisterChannel(s, i.GuildID, user)

	h.Audit(user.ID, "", store.EventModAction,
		map[string]any{"action": "reregister", "release_email": release, "released": released}, bot.ActorID(i))
	note := "Το email του μένει δεσμευμένο, οπότε μπορεί να ξαναγραφτεί μόνο με αυτό."
	if release {
		note = fmt.Sprintf("Αποδεσμεύτηκαν %d email.", released)
	}
	h.AlertMods(s, "🔁 Ο <@%s> ακύρωσε την επαλήθευση του <@%s>, που πρέπει να ξαναγραφτεί. %s", bot.ActorID(i), user.ID, note)

	msg := fmt.Sprintf("🔁 Ο <@%s> πρέπει να ξαναγραφτεί και πήρε νέο κανάλι εγγραφής. %s", user.ID, note)
	if len(kept) > 0 {
		msg += fmt.Sprintf("\n⚠️ Δεν μπόρεσα να αφαιρέσω τους ρόλους: %s. Αφαίρεσέ τους χειροκίνητα.", strings.Join(kept, ", "))
	}
	h.Reply(s, i, msg)
}

func (h *Handler) cmdRegVerify(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	email := store.NormalizeEmail(bot.OptString(sub.Options, "email"))

	if _, ok := classifyEmail(email); !ok {
		h.Reply(s, i, fmt.Sprintf("Το `%s` δεν είναι αποδεκτό ακαδημαϊκό email.", email))
		return
	}

	defer h.LockUser(user.ID)()
	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	if err := h.st.EnsureMember(ctx, user.ID, user.Username); err != nil {
		h.DBError(s, i, err)
		return
	}
	reg, _, err := h.st.EnsureOpenRegistration(ctx, user.ID, i.GuildID)
	if err != nil {
		h.DBError(s, i, err)
		return
	}

	err = h.grantVerification(ctx, s, i.GuildID, reg, email)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrEmailTaken):
		h.Reply(s, i, fmt.Sprintf("Το `%s` ανήκει ήδη σε άλλον λογαριασμό. Δες `/email whois`.", email))
		return
	case errors.Is(err, errRoleNotGranted):
		h.Reply(s, i, fmt.Sprintf("Η βάση ενημερώθηκε αλλά ο ρόλος δεν δόθηκε: %v", err))
		return
	default:
		h.DBError(s, i, err)
		return
	}

	h.revokeHelp(ctx, s, user.ID)
	if reg.ChannelID != nil {
		if err := h.DeleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.Log.Warn("verify: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}

	h.Audit(user.ID, reg.ID, store.EventModAction,
		map[string]any{"action": "verify", "email": email}, bot.ActorID(i))
	h.AlertMods(s, "🛠️ Ο <@%s> επαλήθευσε χειροκίνητα τον <@%s> με `%s`.", bot.ActorID(i), user.ID, email)
	h.Reply(s, i, fmt.Sprintf("✅ Ο <@%s> επαληθεύτηκε με `%s`.", user.ID, email))
}

// cmdRegForget erases a former member from the database. It is limited to
// people who are no longer on the server: for someone still here the database
// has to keep matching their roles, and /reg reregister is the way to reset them.
func (h *Handler) cmdRegForget(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := bot.OptUser(i, sub.Options, "user")
	if user.Bot {
		h.Reply(s, i, "Τα bots δεν έχουν εγγραφή.")
		return
	}

	ctx, cancel := h.CtxTimeout(bot.FlowTimeout)
	defer cancel()

	_, err := h.GuildMember(ctx, s, i.GuildID, user.ID)
	switch {
	case err == nil:
		h.Reply(s, i, fmt.Sprintf(
			"Ο <@%s> είναι ακόμα στον server. Η διαγραφή είναι μόνο για όσους έχουν φύγει· για νέα εγγραφή χρησιμοποίησε `/reg reregister`.", user.ID))
		return
	case !dc.IsMissingResource(err):
		h.Reply(s, i, fmt.Sprintf("Δεν μπόρεσα να ελέγξω αν ο <@%s> είναι στον server: %v", user.ID, err))
		return
	}

	defer h.LockUser(user.ID)()

	// A member who left mid-registration may still have a channel to clean up.
	open, err := h.st.OpenRegistration(ctx, user.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.DBError(s, i, err)
		return
	}
	if open != nil && open.ChannelID != nil {
		if err := h.DeleteChannel(ctx, s, *open.ChannelID); err != nil {
			h.Log.Warn("forget: delete channel failed", "channel_id", *open.ChannelID, "error", err)
		}
	}
	h.revokeHelp(ctx, s, user.ID)

	// The other services first: if one cannot be reached the command can simply
	// be run again, with this service's records still there to find.
	for name, peer := range map[string]*rpc.Client{"moderation": h.moderation, "web": h.web} {
		if err := peer.Call(ctx, rpc.PathForget, rpc.UserRequest{UserID: user.ID}, nil); err != nil {
			h.Reply(s, i, fmt.Sprintf("⚠️ Η υπηρεσία `%s` δεν απάντησε, οπότε δεν διαγράφηκε τίποτα εδώ. Δοκίμασε ξανά: %v", name, err))
			return
		}
	}
	res, err := h.st.ForgetMember(ctx, user.ID)
	if err != nil {
		h.DBError(s, i, err)
		return
	}
	if !res.Found() {
		h.Reply(s, i, fmt.Sprintf("Δεν υπάρχουν δεδομένα για τον <@%s>.", user.ID))
		return
	}

	// The trail the command erased must not be rebuilt around the member, so the
	// record of the erasure itself carries counts only.
	h.Audit("", "", store.EventModAction, map[string]any{
		"action": "forget", "registrations": res.Registrations,
		"events": res.Events, "emails_unlinked": res.EmailsUnlinked,
	}, bot.ActorID(i))
	h.AlertMods(s, "🗑️ Ο <@%s> διέγραψε από τη βάση τον <@%s>.", bot.ActorID(i), user.ID)
	h.Reply(s, i, fmt.Sprintf(
		"🗑️ Διαγράφηκαν τα δεδομένα του <@%s>: %d εγγραφές, %d γεγονότα ιστορικού. "+
			"Τα email του (%d) μένουν δεσμευμένα, χωρίς κάτοχο.",
		user.ID, res.Registrations, res.Events, res.EmailsUnlinked))
}

// cmdRegReconcile repairs the drift between Discord and the database that
// builds up while the bot is away or a channel is deleted by hand: it closes
// the registrations of members who left, makes the channels that are missing
// (for joins the gateway dropped too), removes the ones nobody owns, and
// refreshes the member index behind the ghost role. Nothing runs it by
// itself: run it after the bot was down, or when /reg stuck shows a member
// without a channel.
func (h *Handler) cmdRegReconcile(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	h.Log.Info("reconcile requested", "user_id", bot.ActorID(i))
	rep, err := h.reconcile(s)
	switch {
	case errors.Is(err, errJobRunning):
		h.Reply(s, i, "Ο συγχρονισμός τρέχει ήδη. Δοκίμασε ξανά όταν τελειώσει.")
		return
	case errors.Is(err, bot.ErrGuildUnknown):
		h.Reply(s, i, "Δεν έχω συνδεθεί ακόμα με τον server. Δοκίμασε ξανά σε λίγο.")
		return
	case err != nil:
		h.Log.Error("reconcile failed", "error", err)
	}

	h.Audit("", "", store.EventModAction, map[string]any{
		"action": "reconcile", "members": rep.Members, "closed": rep.Closed, "recreated": rep.Recreated,
		"adopted": rep.Adopted, "orphans": rep.Orphans, "duplicates": rep.Duplicates,
		"waiting": rep.Waiting, "failed": err != nil,
	}, bot.ActorID(i))
	if rep.changed() > 0 {
		h.AlertMods(s, "🔄 Ο/Η <@%s> έτρεξε συγχρονισμό εγγραφών: %d αλλαγές.", bot.ActorID(i), rep.changed())
	}
	h.Reply(s, i, rep.summary(err))
}
