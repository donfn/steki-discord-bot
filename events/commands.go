package events

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// The commands themselves live beside the feature they serve: the registration
// and email commands in cmd_reg.go and cmd_email.go, /bot stats in cmd_bot.go,
// the material commands in review.go, sticky.go and links.go, /mcp in mcp.go,
// /guard in guardcmds.go, /member in bans.go and /rules in rules.go. This file
// holds what they share: the definitions Discord is given, the routing table,
// the dispatcher with its staff check, and the reply and option helpers.

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
				Description: "Ακύρωση επαλήθευσης: ο χρήστης πρέπει να ξαναγραφτεί από την αρχή",
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
			{
				Name:        "reconcile",
				Description: "Συγχρονίζει τις εγγραφές με τον server (μέλη που έφυγαν, κανάλια που λείπουν)",
				Type:        discordgo.ApplicationCommandOptionSubCommand,
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
						Description: "Αφαίρεση κατόχου: το email μένει δεσμευμένο, χωρίς κάτοχο",
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
		}, {
			Name:        "refresh",
			Description: "Δίνει το τρέχον κουμπί στις παλιές ανακοινώσεις του MCP",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}},
	},
	{
		Name:                     "materials",
		Description:              "Έλεγχος του υλικού που έχει ανέβει",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "rescan",
			Description: "Ελέγχει με AI όσες εικόνες έχουν ήδη ανέβει και αφαιρεί όσες δεν είναι υλικό μαθήματος",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}, {
			Name:        "scan",
			Description: "Ψάχνει νέο υλικό στο ιστορικό των καναλιών μαθημάτων και ανανεώνει τα sticky",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}, {
			Name:        "stickies",
			Description: "Ανανεώνει τα κουμπιά του sticky σε όλα τα κανάλια μαθημάτων, επί τόπου και σιωπηλά",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}, {
			Name:        "cards",
			Description: "Στέλνει τις κάρτες ελέγχου εικόνων και αναφορών που λείπουν από το κανάλι των logs",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
		}, {
			Name:        "view-as",
			Description: "Ανοίγει τον χώρο μελέτης ενός μαθήματος όπως τον βλέπει ένα μέλος (μόνο ανάγνωση)",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "channel", Description: "Το κανάλι του μαθήματος",
				Type: discordgo.ApplicationCommandOptionChannel, Required: true,
				ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
			}, {
				Name: "user", Description: "Το μέλος",
				Type: discordgo.ApplicationCommandOptionUser, Required: true,
			}},
		}, {
			Name:        "remove",
			Description: "Αφαιρεί οριστικά υλικό από τη σελίδα και το bucket",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "message", Description: "Το link ή το ID του μηνύματος με το αρχείο",
				Type: discordgo.ApplicationCommandOptionString, Required: true,
			}, {
				Name: "file", Description: "Μέρος του ονόματος αρχείου, αν το μήνυμα έχει πολλά",
				Type: discordgo.ApplicationCommandOptionString,
			}},
		}},
	},
	{
		Name:                     "member",
		Description:              "Αποκλεισμός μελών",
		DefaultMemberPermissions: &staffOnly,
		Options: []*discordgo.ApplicationCommandOption{{
			Name:        "ban",
			Description: "Αποκλείει τον χρήστη από τον server και τα email του από νέα εγγραφή",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "user", Description: "Ο χρήστης",
				Type: discordgo.ApplicationCommandOptionUser, Required: true,
			}, {
				Name: "reason", Description: "Ο λόγος (φαίνεται στο audit log)",
				Type: discordgo.ApplicationCommandOptionString,
			}, {
				Name: "email", Description: "Επιπλέον email για αποκλεισμό",
				Type: discordgo.ApplicationCommandOptionString,
			}},
		}, {
			Name:        "unban",
			Description: "Άρση αποκλεισμού του χρήστη και των email του",
			Type:        discordgo.ApplicationCommandOptionSubCommand,
			Options: []*discordgo.ApplicationCommandOption{{
				Name: "user", Description: "Ο χρήστης",
				Type: discordgo.ApplicationCommandOptionUser, Required: true,
			}, {
				Name: "email", Description: "Επιπλέον email για αποδέσμευση",
				Type: discordgo.ApplicationCommandOptionString,
			}},
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

// textLimits bounds every free-text option by its name. Discord itself allows
// 6,000 characters, far more than any of these needs, and the text lands in the
// database, the audit log or a reply. A text option with no entry here fails
// TestEveryTextOptionIsBounded, so a new one has to be given a limit.
var textLimits = map[string]int{
	"email":     254, // an address, or a 64-character hash
	"new_email": 254,
	"reason":    512, // what Discord's audit log keeps
	"message":   200, // a message link or ID
	"file":      100, // part of a file name
}

func init() {
	var limit func(opts []*discordgo.ApplicationCommandOption)
	limit = func(opts []*discordgo.ApplicationCommandOption) {
		for _, o := range opts {
			if o.Type == discordgo.ApplicationCommandOptionString {
				o.MaxLength = textLimits[o.Name]
			}
			limit(o.Options)
		}
	}
	for _, cmd := range commandDefs {
		limit(cmd.Options)
	}
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
	"reg status":         (*Handler).cmdRegStatus,
	"reg history":        (*Handler).cmdRegHistory,
	"reg stuck":          (*Handler).cmdRegStuck,
	"reg restart":        (*Handler).cmdRegRestart,
	"reg verify":         (*Handler).cmdRegVerify,
	"reg reregister":     (*Handler).cmdRegReregister,
	"reg forget":         (*Handler).cmdRegForget,
	"reg reconcile":      (*Handler).cmdRegReconcile,
	"email whois":        (*Handler).cmdEmailWhois,
	"email list":         (*Handler).cmdEmailList,
	"email add":          (*Handler).cmdEmailAdd,
	"email update":       (*Handler).cmdEmailUpdate,
	"email remove":       (*Handler).cmdEmailRemove,
	"bot stats":          (*Handler).cmdBotStats,
	"member ban":         (*Handler).cmdMemberBan,
	"member unban":       (*Handler).cmdMemberUnban,
	"mcp announce":       (*Handler).cmdMCPAnnounce,
	"mcp refresh":        (*Handler).cmdMCPRefresh,
	"rules post":         (*Handler).cmdRulesPost,
	"materials rescan":   (*Handler).cmdMaterialsRescan,
	"materials remove":   (*Handler).cmdMaterialsRemove,
	"materials scan":     (*Handler).cmdMaterialsScan,
	"materials stickies": (*Handler).cmdMaterialsStickies,
	"materials cards":    (*Handler).cmdMaterialsCards,
	"materials view-as":  (*Handler).cmdMaterialsViewAs,
	"guard status":       (*Handler).cmdGuardStatus,
	"guard restrict":     (*Handler).cmdGuardRestrict,
	"guard ungrace":      (*Handler).cmdGuardUngrace,
}

// onInteractionCreate dispatches moderator slash commands.
//
// Every command is logged on arrival and every refusal says why, because a
// command Discord shows as "didn't respond" is otherwise indistinguishable
// from one that never reached the bot.
func (h *Handler) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h.inHomeGuild(i.GuildID) {
		h.track("discord_interaction", interactionProps(i))
	}
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
