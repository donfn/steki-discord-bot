package events

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/store"
)

// replyLimit keeps replies under Discord's 2000-character message cap.
const replyLimit = 1900

// staffOnly gates the commands at the Discord level. The explicit role check
// in requireStaff is still performed, because this permission can be
// overridden by a server administrator.
var staffOnly = int64(discordgo.PermissionManageGuild)

// commandDefs is the full moderator surface. It replaces the bare-text
// addEmail / removeEmail / removeHash commands that were parsed out of the log
// channel, where anyone able to post could see them.
var commandDefs = []*discordgo.ApplicationCommand{
	{
		Name:                     "reg",
		Description:              "Διαχείριση εγγραφών",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "status",
				Description: "Κατάσταση εγγραφής ενός χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "history",
				Description: "Πλήρες ιστορικό ενεργειών ενός χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "stuck",
				Description: "Ποιοι έχουν κολλήσει στην εγγραφή",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
			},
			{
				Name:        "restart",
				Description: "Επανεκκίνηση της εγγραφής ενός χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "reregister",
				Description: "Αφαίρεση επαλήθευσης: ο χρήστης πρέπει να ξαναγραφτεί από την αρχή",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{
					{
						Name: "user", Description: "Ο χρήστης",
						Type: discordgo.ApplicationCommandOptionUser, Required: true,
					},
					{
						Name:        "release_email",
						Description: "Αποδέσμευση του email του, ώστε να μπορεί να γραφτεί με άλλο (προεπιλογή: όχι)",
						Type:        discordgo.ApplicationCommandOptionBoolean,
					},
				},
			},
			{
				Name:        "verify",
				Description: "Χειροκίνητη επαλήθευση χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{
					{
						Name: "user", Description: "Ο χρήστης",
						Type: discordgo.ApplicationCommandOptionUser, Required: true,
					},
					{
						Name: "email", Description: "Το ακαδημαϊκό email",
						Type: discordgo.ApplicationCommandOptionString, Required: true,
					},
				},
			},
			{
				Name:        "forget",
				Description: "Διαγραφή όλων των δεδομένων ενός χρήστη που έφυγε από τον server",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
		},
	},
	{
		Name:                     "email",
		Description:              "Διαχείριση καταχωρημένων email",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "whois",
				Description: "Σε ποιον ανήκει ένα email",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "email", Description: "Το email ή το hash",
					Type: discordgo.ApplicationCommandOptionString, Required: true,
				}},
			},
			{
				Name:        "list",
				Description: "Όλα τα email και τα hashes ενός χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "add",
				Description: "Προσθήκη email στη βάση",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{
					{
						Name: "email", Description: "Το email ή το hash",
						Type: discordgo.ApplicationCommandOptionString, Required: true,
					},
					{
						Name: "user", Description: "Ο κάτοχος (προαιρετικό)",
						Type: discordgo.ApplicationCommandOptionUser,
					},
				},
			},
			{
				Name:        "update",
				Description: "Αλλαγή κατόχου ή διόρθωση ενός email της βάσης",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{
					{
						Name: "email", Description: "Το email ή το hash",
						Type: discordgo.ApplicationCommandOptionString, Required: true,
					},
					{
						Name: "user", Description: "Ο νέος κάτοχος",
						Type: discordgo.ApplicationCommandOptionUser,
					},
					{
						Name:        "unlink",
						Description: "Αφαίρεση του κατόχου: το email μένει δεσμευμένο χωρίς κάτοχο",
						Type:        discordgo.ApplicationCommandOptionBoolean,
					},
					{
						Name:        "new_email",
						Description: "Η σωστή διεύθυνση, αν το email είχε καταχωρηθεί λάθος",
						Type:        discordgo.ApplicationCommandOptionString,
					},
				},
			},
			{
				Name:        "remove",
				Description: "Αφαίρεση email από τη βάση",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "email", Description: "Το email ή το hash",
					Type: discordgo.ApplicationCommandOptionString, Required: true,
				}},
			},
		},
	},
	{
		Name:                     "guard",
		Description:              "Προστασία από παραβιασμένους λογαριασμούς (captcha)",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{
			{
				Name:        "status",
				Description: "Κατάσταση προστασίας ενός χρήστη",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "restrict",
				Description: "Timeout 24 ωρών που αφαιρείται όταν ο χρήστης λύσει captcha",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
			{
				Name:        "ungrace",
				Description: "Αφαίρεση της περιόδου χάριτος 24 ωρών μετά το captcha",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Options: []*discordgo.ApplicationCommandOption{{
					Name: "user", Description: "Ο χρήστης",
					Type: discordgo.ApplicationCommandOptionUser, Required: true,
				}},
			},
		},
	},
	{
		Name:                     "bot",
		Description:              "Κατάσταση του bot",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "stats",
			Description: "Στατιστικά και υγεία συστήματος",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}},
	},
	{
		Name:                     "mcp",
		Description:              "Σύνδεση του υλικού με Claude και ChatGPT (MCP)",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "announce",
			Description: "Στέλνει σε ένα κανάλι μήνυμα που εξηγεί τον MCP server, με κουμπί για τις οδηγίες",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "channel", Description: "Το κανάλι όπου θα σταλεί το μήνυμα",
				Type:         discordgo.ApplicationCommandOptionChannel,
				ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews},
				Required:     true,
			}},
		}},
	},
	{
		Name:                     "materials",
		Description:              "Έλεγχος του υλικού που έχει ανέβει",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "rescan",
			Description: "Ελέγχει με AI όλες τις εικόνες που έχουν ήδη ανέβει και βγάζει όσες δεν είναι υλικό μαθήματος",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}},
	},
	{
		Name:                     "rules",
		Description:              "Κανόνες και ρόλοι του server",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "post",
			Description: "Στέλνει σε ένα κανάλι τους κανόνες και τη λίστα των ρόλων",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "channel", Description: "Το κανάλι όπου θα σταλούν",
				Type:         discordgo.ApplicationCommandOptionChannel,
				ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews},
				Required:     true,
			}},
		}},
	},
}

// registerCommands installs the command set on the guild. Guild commands
// update instantly, unlike global ones.
func (h *Handler) registerCommands(s *discordgo.Session) error {
	guildID := h.guildID()
	if guildID == "" || s.State.User == nil {
		return nil
	}
	ctx, cancel := h.ctx()
	defer cancel()

	return h.retry(ctx, "commands.overwrite", func() error {
		_, err := s.ApplicationCommandBulkOverwrite(
			s.State.User.ID, guildID, commandDefs, discordgo.WithContext(ctx))
		return err
	})
}

// commandFunc handles one subcommand. The reply has already been deferred;
// the handler fills it in with reply.
type commandFunc func(h *Handler, s *discordgo.Session, i *discordgo.InteractionCreate,
	sub *discordgo.ApplicationCommandInteractionDataOption)

// commandRoutes maps "command subcommand" to its handler.
var commandRoutes = map[string]commandFunc{
	"reg status":       (*Handler).cmdRegStatus,
	"reg history":      (*Handler).cmdRegHistory,
	"reg stuck":        (*Handler).cmdRegStuck,
	"reg restart":      (*Handler).cmdRegRestart,
	"reg verify":       (*Handler).cmdRegVerify,
	"reg reregister":   (*Handler).cmdRegReregister,
	"reg forget":       (*Handler).cmdRegForget,
	"email whois":      (*Handler).cmdEmailWhois,
	"email list":       (*Handler).cmdEmailList,
	"email add":        (*Handler).cmdEmailAdd,
	"email update":     (*Handler).cmdEmailUpdate,
	"email remove":     (*Handler).cmdEmailRemove,
	"bot stats":        (*Handler).cmdBotStats,
	"mcp announce":     (*Handler).cmdMCPAnnounce,
	"rules post":       (*Handler).cmdRulesPost,
	"materials rescan": (*Handler).cmdMaterialsRescan,
	"guard status":     (*Handler).cmdGuardStatus,
	"guard restrict":   (*Handler).cmdGuardRestrict,
	"guard ungrace":    (*Handler).cmdGuardUngrace,
}

// onInteractionCreate dispatches moderator slash commands.
//
// Every command is logged on arrival and every refusal says why, because a
// command Discord shows as "didn't respond" is otherwise indistinguishable
// from one that never reached the bot.
func (h *Handler) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		h.onComponent(s, i)
		return
	case discordgo.InteractionModalSubmit:
		// A form discordgo cannot decode (the upload form) arrives without
		// data; onRawEvent handles it.
		if _, ok := i.Data.(discordgo.ModalSubmitInteractionData); ok {
			h.onEmailSubmit(s, i)
		}
		return
	case discordgo.InteractionApplicationCommand:
	default:
		return
	}
	data := i.ApplicationCommandData()
	log := h.log.With("command", commandPath(data), "guild_id", i.GuildID, "user_id", actorID(i))
	log.Info("slash command received")

	if !h.inHomeGuild(i.GuildID) {
		log.Warn("slash command ignored: not the Steki guild", "home_guild_id", h.guildID())
		return
	}

	// The Discord-level permission can be overridden by a server
	// administrator, so the role check is repeated here.
	if i.Member == nil || !hasAnyRole(i.Member, []string{staffRoleID, modRoleID}) {
		log.Warn("slash command refused: caller lacks the staff and moderator roles")
		h.respondNow(s, i, "⛔ Η εντολή αυτή είναι μόνο για την Ομάδα Διαχείρισης.")
		return
	}

	route, ok := commandRoutes[commandPath(data)]
	if !ok {
		log.Warn("slash command ignored: no handler")
		h.respondNow(s, i, "Άγνωστη εντολή.")
		return
	}
	sub := data.Options[0]

	// Acknowledge first: Discord drops an interaction not answered within
	// three seconds, and several commands make more round trips than that
	// reliably allows.
	if !h.deferReply(s, i) {
		return
	}
	route(h, s, i, sub)
}

// commandPath returns "command subcommand", the key into commandRoutes, or
// just the command name when it has no subcommand.
func commandPath(data discordgo.ApplicationCommandInteractionData) string {
	if len(data.Options) == 0 || data.Options[0].Type != discordgo.ApplicationCommandOptionSubCommand {
		return data.Name
	}
	return data.Name + " " + data.Options[0].Name
}

// respondNow sends an immediate ephemeral reply, for answers that need no work.
func (h *Handler) respondNow(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: truncate(content, replyLimit),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		h.log.Error("interaction respond failed", "error", err)
	}
}

// deferReply acknowledges the command with an ephemeral "thinking" state, so
// moderator output never leaks into a channel members can read.
func (h *Handler) deferReply(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		h.log.Error("interaction defer failed", "error", err)
		return false
	}
	return true
}

// reply fills in a deferred response.
func (h *Handler) reply(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	content = truncate(content, replyLimit)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		h.log.Error("interaction reply failed", "error", err)
	}
}

// dbError reports a database failure to the moderator.
func (h *Handler) dbError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	h.reply(s, i, fmt.Sprintf("Σφάλμα βάσης: %v", err))
}

// findOpt pulls a named option out of a subcommand's option list.
func findOpt(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) *discordgo.ApplicationCommandInteractionDataOption {
	for _, o := range opts {
		if o.Name == name {
			return o
		}
	}
	return nil
}

// optUser resolves a user option. Discord ships the full user in the
// interaction's resolved data, so no REST lookup is needed. It returns nil
// when the option is absent.
func optUser(i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption,
	name string) *discordgo.User {

	opt := findOpt(opts, name)
	if opt == nil {
		return nil
	}
	user := opt.UserValue(nil) // ID only
	if r := i.ApplicationCommandData().Resolved; r != nil {
		if full, ok := r.Users[user.ID]; ok {
			return full
		}
	}
	return user
}

// optString returns a string option, or "" when it is absent.
func optString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	if opt := findOpt(opts, name); opt != nil {
		return opt.StringValue()
	}
	return ""
}

// actorID is the moderator who ran the command.
func actorID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	return ""
}

func (h *Handler) cmdRegStatus(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "**Κατάσταση για <@%s>**\n", user.ID)

	member, err := h.st.GetMember(ctx, user.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b.WriteString("Δεν υπάρχει εγγραφή στη βάση.\n")
	case err != nil:
		h.dbError(s, i, err)
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
		h.dbError(s, i, err)
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

	h.reply(s, i, b.String())
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
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	events, err := h.st.History(ctx, user.ID, 25)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if len(events) == 0 {
		h.reply(s, i, fmt.Sprintf("Δεν υπάρχει ιστορικό για τον <@%s>.", user.ID))
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
	h.reply(s, i, b.String())
}

func (h *Handler) cmdRegStuck(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	ctx, cancel := h.ctx()
	defer cancel()

	open, err := h.st.ListOpen(ctx) // oldest activity first
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if len(open) == 0 {
		h.reply(s, i, "✅ Κανείς δεν έχει κολλήσει σε εγγραφή.")
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
	h.reply(s, i, b.String())
}

func (h *Handler) cmdRegRestart(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	ctx, cancel := h.ctx()
	defer cancel()

	reg, err := h.st.OpenRegistration(ctx, user.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.reply(s, i, fmt.Sprintf("Δεν βρέθηκε ανοιχτή εγγραφή για τον <@%s>.", user.ID))
		return
	}
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if reg.ChannelID == nil {
		h.reply(s, i, "Η εγγραφή δεν έχει κανάλι. Θα το φτιάξει ο reconciler.")
		return
	}

	if err := h.restartRegistration(s, *reg.ChannelID, user.ID, i.GuildID); err != nil {
		h.reply(s, i, fmt.Sprintf("Η επανεκκίνηση απέτυχε: %v", err))
		return
	}
	h.audit(user.ID, reg.ID, store.EventModAction, map[string]any{"action": "restart"}, actorID(i))
	h.reply(s, i, fmt.Sprintf("🔄 Η εγγραφή του <@%s> επανεκκινήθηκε.", user.ID))
}

// cmdRegReregister sends a verified member back to the start: the database
// forgets their verification, the verified roles come off, and they get a
// fresh registration channel. Unless release_email is set they keep the claim
// on their address, so they can re-register with it but nobody else can.
func (h *Handler) cmdRegReregister(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	release := false
	if o := findOpt(sub.Options, "release_email"); o != nil {
		release = o.BoolValue()
	}
	if user.Bot {
		h.reply(s, i, "Δεν γίνεται επανεγγραφή σε bot.")
		return
	}

	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	if _, err := h.guildMember(ctx, s, i.GuildID, user.ID); err != nil {
		h.reply(s, i, fmt.Sprintf("Ο <@%s> δεν είναι μέλος του server.", user.ID))
		return
	}

	released, err := func() (int64, error) {
		defer h.lockUser(user.ID)()
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
			if err := h.deleteChannel(ctx, s, *open.ChannelID); err != nil {
				h.log.Warn("reregister: delete old channel failed", "channel_id", *open.ChannelID, "error", err)
			}
		}
		return n, nil
	}()
	if err != nil {
		h.dbError(s, i, err)
		return
	}

	var kept []string
	for _, roleID := range verifiedRoleIDs {
		err := h.retry(ctx, "member.role.remove", func() error {
			return s.GuildMemberRoleRemove(i.GuildID, user.ID, roleID, discordgo.WithContext(ctx))
		})
		if err != nil && !dc.IsMissingResource(err) {
			h.log.Warn("reregister: remove role failed", "user_id", user.ID, "role_id", roleID, "error", err)
			kept = append(kept, "<@&"+roleID+">")
		}
	}

	h.revokeHelp(ctx, s, user.ID)
	h.ensureRegisterChannel(s, i.GuildID, user)

	h.audit(user.ID, "", store.EventModAction,
		map[string]any{"action": "reregister", "release_email": release, "released": released}, actorID(i))
	note := "Κράτησε το email του, οπότε μπορεί να ξαναγραφτεί μόνο με αυτό."
	if release {
		note = fmt.Sprintf("Αποδεσμεύτηκαν %d email.", released)
	}
	h.alertMods(s, "🔁 Ο <@%s> έστειλε τον <@%s> να ξαναγραφτεί. %s", actorID(i), user.ID, note)

	msg := fmt.Sprintf("🔁 Ο <@%s> πρέπει πλέον να ξαναγραφτεί και πήρε νέο κανάλι εγγραφής. %s", user.ID, note)
	if len(kept) > 0 {
		msg += fmt.Sprintf("\n⚠️ Δεν μπόρεσα να αφαιρέσω: %s. Αφαίρεσέ τους χειροκίνητα.", strings.Join(kept, ", "))
	}
	h.reply(s, i, msg)
}

func (h *Handler) cmdRegVerify(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	email := store.NormalizeEmail(optString(sub.Options, "email"))

	if _, ok := classifyEmail(email); !ok {
		h.reply(s, i, fmt.Sprintf("Το `%s` δεν είναι αποδεκτό ακαδημαϊκό email.", email))
		return
	}

	defer h.lockUser(user.ID)()
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	if err := h.st.EnsureMember(ctx, user.ID, user.Username); err != nil {
		h.dbError(s, i, err)
		return
	}
	reg, _, err := h.st.EnsureOpenRegistration(ctx, user.ID, i.GuildID)
	if err != nil {
		h.dbError(s, i, err)
		return
	}

	err = h.grantVerification(ctx, s, i.GuildID, reg, email)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrEmailTaken):
		h.reply(s, i, fmt.Sprintf("Το `%s` ανήκει ήδη σε άλλον λογαριασμό. Δες `/email whois`.", email))
		return
	case errors.Is(err, errRoleNotGranted):
		h.reply(s, i, fmt.Sprintf("Η βάση ενημερώθηκε αλλά ο ρόλος δεν δόθηκε: %v", err))
		return
	default:
		h.dbError(s, i, err)
		return
	}

	h.revokeHelp(ctx, s, user.ID)
	if reg.ChannelID != nil {
		if err := h.deleteChannel(ctx, s, *reg.ChannelID); err != nil {
			h.log.Warn("verify: delete channel failed", "channel_id", *reg.ChannelID, "error", err)
		}
	}

	h.audit(user.ID, reg.ID, store.EventModAction,
		map[string]any{"action": "verify", "email": email}, actorID(i))
	h.alertMods(s, "🛠️ Ο <@%s> επαλήθευσε χειροκίνητα τον <@%s> με `%s`.", actorID(i), user.ID, email)
	h.reply(s, i, fmt.Sprintf("✅ Ο <@%s> επαληθεύτηκε με `%s`.", user.ID, email))
}

// cmdRegForget erases a former member from the database. It is limited to
// people who are no longer on the server: for someone still here the database
// has to keep matching their roles, and /reg reregister is the way to reset them.
func (h *Handler) cmdRegForget(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	user := optUser(i, sub.Options, "user")
	if user.Bot {
		h.reply(s, i, "Τα bots δεν έχουν εγγραφή.")
		return
	}

	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	_, err := h.guildMember(ctx, s, i.GuildID, user.ID)
	switch {
	case err == nil:
		h.reply(s, i, fmt.Sprintf(
			"Ο <@%s> είναι ακόμα στον server. Η διαγραφή γίνεται μόνο για όσους έφυγαν· για επανεκκίνηση χρησιμοποίησε `/reg reregister`.", user.ID))
		return
	case !dc.IsMissingResource(err):
		h.reply(s, i, fmt.Sprintf("Δεν μπόρεσα να ελέγξω αν ο <@%s> είναι στον server: %v", user.ID, err))
		return
	}

	defer h.lockUser(user.ID)()

	// A member who left mid-registration may still have a channel to clean up.
	open, err := h.st.OpenRegistration(ctx, user.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.dbError(s, i, err)
		return
	}
	if open != nil && open.ChannelID != nil {
		if err := h.deleteChannel(ctx, s, *open.ChannelID); err != nil {
			h.log.Warn("forget: delete channel failed", "channel_id", *open.ChannelID, "error", err)
		}
	}
	h.revokeHelp(ctx, s, user.ID)

	res, err := h.st.ForgetMember(ctx, user.ID)
	if err != nil {
		h.dbError(s, i, err)
		return
	}
	if !res.Found() {
		h.reply(s, i, fmt.Sprintf("Δεν υπάρχουν δεδομένα για τον <@%s>.", user.ID))
		return
	}

	// The trail the command erased must not be rebuilt around the member, so the
	// record of the erasure itself carries counts only.
	h.audit("", "", store.EventModAction, map[string]any{
		"action": "forget", "registrations": res.Registrations,
		"events": res.Events, "emails_unlinked": res.EmailsUnlinked,
	}, actorID(i))
	h.alertMods(s, "🗑️ Ο <@%s> διέγραψε από τη βάση τον <@%s>.", actorID(i), user.ID)
	h.reply(s, i, fmt.Sprintf(
		"🗑️ Διαγράφηκαν τα δεδομένα του <@%s>: %d εγγραφές, %d γεγονότα ιστορικού. "+
			"Τα email του (%d) μένουν δεσμευμένα, χωρίς κάτοχο.",
		user.ID, res.Registrations, res.Events, res.EmailsUnlinked))
}

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
				"Το `%s` υπάρχει μόνο ως παλιό hash (πριν τη μετάβαση σε Postgres). Κάτοχος άγνωστος.", email))
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
		h.reply(s, i, "Δώσε τι να αλλάξει: `user`, `unlink` ή `new_email`.")
		return
	case user != nil && unlink:
		h.reply(s, i, "Δώσε είτε `user` είτε `unlink`, όχι και τα δύο.")
		return
	case newEmail != "" && isHash:
		h.reply(s, i, "Ένα hash δεν έχει διεύθυνση να διορθωθεί. Πρόσθεσε το σωστό email με `/email add`.")
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

func (h *Handler) cmdBotStats(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "**Κατάσταση bot**\n• Uptime: %s\n", formatUptime(time.Since(h.bootTime)))
	fmt.Fprintf(&b, "• Μηνύματα: %d · Μπλοκαρισμένα: %d\n", h.processed.Load(), h.blocked.Load())

	if err := h.st.Ping(ctx); err != nil {
		fmt.Fprintf(&b, "• 🔴 Βάση: %v\n", err)
	} else {
		stat := h.st.DBStats()
		fmt.Fprintf(&b, "• 🟢 Βάση: %.1f MB · %d/%d συνδέσεις\n",
			float64(h.st.FileSize())/(1<<20), stat.InUse, stat.MaxOpenConnections)
	}

	if open, err := h.st.ListOpen(ctx); err == nil {
		fmt.Fprintf(&b, "• Ανοιχτές εγγραφές: %d\n", len(open))
	}
	if es, err := h.st.EmailStats(ctx); err == nil {
		fmt.Fprintf(&b, "• Emails: %d (με κάτοχο: %d) · παλιά hashes: %d\n",
			es.Verified, es.VerifiedLinked, es.LegacyHashes)
	}

	if occ, err := h.placer.Occupancies(ctx, s, h.guildID()); err == nil {
		b.WriteString("\n**Κατηγορίες εγγραφής**\n")
		for _, o := range occ {
			warn := ""
			if o.Used >= o.Capacity-5 {
				warn = " ⚠️"
			}
			fmt.Fprintf(&b, "• `%s`: %d/%d%s\n", o.Name, o.Used, o.Capacity, warn)
		}
	}

	if counts, err := h.st.CountEventsByKind(ctx, time.Now().Add(-24*time.Hour)); err == nil && len(counts) > 0 {
		b.WriteString("\n**Τελευταίο 24ωρο**\n")
		kinds := make([]string, 0, len(counts))
		for k := range counts {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for _, k := range kinds {
			fmt.Fprintf(&b, "• `%s`: %d\n", k, counts[k])
		}
	}

	h.reply(s, i, b.String())
}
