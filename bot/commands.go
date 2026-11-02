package bot

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// The commands themselves live in the service they serve: /reg and /email in
// registration, /guard, /member, /rules and /bot in moderation, /materials and
// /mcp in web. This file holds what they share: the definitions Discord is given,
// the staff check, and the reply and option helpers.

// ReplyLimit keeps replies under Discord's 2000-character message cap.
const ReplyLimit = 1900

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
				Description: "Επανεκκίνηση της εγγραφής ενός χρήστη και μηδενισμός του ορίου email",
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

// TextLimits bounds every free-text option by its name. Discord itself allows
// 6,000 characters, far more than any of these needs, and the text lands in the
// database, the audit log or a reply. A text option with no entry here fails
// TestEveryTextOptionIsBounded, so a new one has to be given a limit.
var TextLimits = map[string]int{
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
				o.MaxLength = TextLimits[o.Name]
			}
			limit(o.Options)
		}
	}
	for _, cmd := range commandDefs {
		limit(cmd.Options)
	}
}

// Commands is every slash command, whichever service answers it. Discord replaces
// the whole set at once, so the moderation service alone installs it, and each
// service ignores the commands that are not its own.
func Commands() []*discordgo.ApplicationCommand { return commandDefs }

// BeginCommand decides whether a slash command is this service's and may run:
// it logs it, checks the home guild and the staff roles, and acknowledges it,
// since Discord drops an interaction not answered within three seconds.
// owned says which "command subcommand" paths this service routes.
//
// Every service sees every interaction, so a path that is not owned is silently
// left to its owner. Every refusal says why, because a command Discord shows as
// "didn't respond" is otherwise indistinguishable from one that never arrived.
func (c *Core) BeginCommand(s *discordgo.Session, i *discordgo.InteractionCreate, owned func(path string) bool) (
	sub *discordgo.ApplicationCommandInteractionDataOption, path string, ok bool) {

	data := i.ApplicationCommandData()
	path = CommandPath(data)
	if !owned(path) {
		return nil, path, false
	}
	log := c.Log.With("command", path, "guild_id", i.GuildID, "user_id", ActorID(i))
	log.Info("slash command received")

	if !c.InHomeGuild(i.GuildID) {
		log.Warn("slash command ignored: not the Steki guild", "home_guild_id", c.GuildID())
		return nil, path, false
	}
	c.Track("discord_interaction", InteractionProps(i))

	// The Discord-level permission can be overridden by a server
	// administrator, so the role check is repeated here.
	if i.Member == nil || !HasAnyRole(i.Member, []string{StaffRoleID, ModRoleID}) {
		log.Warn("slash command refused: caller lacks the staff and moderator roles")
		c.RespondNow(s, i, "⛔ Η εντολή αυτή είναι μόνο για την Ομάδα Διαχείρισης.")
		return nil, path, false
	}
	if !c.DeferReply(s, i) {
		return nil, path, false
	}
	return data.Options[0], path, true
}

// commandPath returns "command subcommand", the key into commandRoutes, or
// just the command name when it has no subcommand.
func CommandPath(data discordgo.ApplicationCommandInteractionData) string {
	if len(data.Options) == 0 || data.Options[0].Type != discordgo.ApplicationCommandOptionSubCommand {
		return data.Name
	}
	return data.Name + " " + data.Options[0].Name
}

// respondNow sends an immediate ephemeral reply, for answers that need no work.
func (h *Core) RespondNow(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: Truncate(content, ReplyLimit),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		h.Log.Error("interaction respond failed", "error", err)
	}
}

// deferReply acknowledges the command with an ephemeral "thinking" state, so
// moderator output never leaks into a channel members can read.
func (h *Core) DeferReply(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		h.Log.Error("interaction defer failed", "error", err)
		return false
	}
	return true
}

// reply fills in a deferred response.
func (h *Core) Reply(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	content = Truncate(content, ReplyLimit)
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		h.Log.Error("interaction reply failed", "error", err)
	}
}

// dbError reports a database failure to the moderator.
func (h *Core) DBError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	h.Reply(s, i, fmt.Sprintf("Σφάλμα βάσης: %v", err))
}

// findOpt pulls a named option out of a subcommand's option list.
func FindOpt(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) *discordgo.ApplicationCommandInteractionDataOption {
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
func OptUser(i *discordgo.InteractionCreate, opts []*discordgo.ApplicationCommandInteractionDataOption,
	name string) *discordgo.User {

	opt := FindOpt(opts, name)
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
func OptString(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	if opt := FindOpt(opts, name); opt != nil {
		return opt.StringValue()
	}
	return ""
}

// actorID is the moderator who ran the command.
func ActorID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	return ""
}
