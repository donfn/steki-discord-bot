package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// "Ανέβασμα υλικού" opens a form with a title and a file picker. The bot
// posts the files to the channel under the title, credits the member, pins
// the message and records the files as theirs, so the sticky counts them as
// shared by a member even though the bot sent them.
//
// The form uses Discord's Label and File Upload components (types 18 and
// 19), which discordgo v0.29 cannot decode: the typed modal submit for it
// arrives with no data. It is therefore read from the raw gateway event.

const (
	uploadModalID = "materials:upload:form"
	uploadTitleID = "title"
	uploadFileID  = "files"

	// uploadMaxFiles is how many files one upload may carry, the most a
	// message can hold.
	uploadMaxFiles = 10

	// uploadMemoryCap bounds one upload's total size. discordgo assembles the
	// whole message in memory before sending it, and the bot runs under a
	// small memory limit.
	uploadMemoryCap = 25 << 20

	// uploadsPerHour limits one member's uploads.
	uploadsPerHour = 10

	uploadTimeout = 2 * time.Minute
)

// greece is the time zone the exam periods follow. The container has no
// zoneinfo, so a fixed offset stands in when it is missing; only the month
// matters here.
var greece = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Athens"); err == nil {
		return loc
	}
	return time.FixedZone("EET", 2*60*60)
}()

// examTitle is the title the form starts with during an exam period, or ""
// outside one.
func examTitle(now time.Time) string {
	t := now.In(greece)
	var period string
	switch t.Month() {
	case time.January, time.February:
		period = "Ιαν-Φεβ"
	case time.June, time.July:
		period = "Ιουν-Ιουλ"
	case time.August, time.September, time.October:
		period = "Σεπ"
	default:
		return ""
	}
	return fmt.Sprintf("Θέματα εξεταστικής %s %d", period, t.Year())
}

// rawComponent marshals a component discordgo has no type for.
type rawComponent struct {
	kind discordgo.ComponentType
	body map[string]any
}

func (c rawComponent) Type() discordgo.ComponentType { return c.kind }

func (c rawComponent) MarshalJSON() ([]byte, error) {
	body := map[string]any{"type": c.kind}
	for k, v := range c.body {
		body[k] = v
	}
	return json.Marshal(body)
}

const (
	labelComponent      discordgo.ComponentType = 18
	fileUploadComponent discordgo.ComponentType = 19
)

// label wraps a form field with its label, as the new modal layout wants.
func label(text, description string, child map[string]any) rawComponent {
	body := map[string]any{"label": text, "component": child}
	if description != "" {
		body["description"] = description
	}
	return rawComponent{kind: labelComponent, body: body}
}

// openUploadForm answers the upload button with the form.
func (h *Handler) openUploadForm(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h.mat == nil || !h.inHomeGuild(i.GuildID) || i.Member == nil {
		return
	}
	if !h.matLive.Load() {
		h.respondNow(s, i, "Το ανέβασμα υλικού δεν είναι διαθέσιμο αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.")
		return
	}
	if timedOut(i.Member) {
		h.respondNow(s, i, "⛔ Δεν μπορείς να ανεβάσεις υλικό όσο έχεις timeout.")
		return
	}
	ctx, cancel := h.ctx()
	ch, err := h.channel(ctx, s, i.ChannelID)
	cancel()
	if err != nil || !isCourseChannel(ch) {
		h.respondNow(s, i, "Το ανέβασμα υλικού γίνεται μόνο στα κανάλια των μαθημάτων.")
		return
	}

	title := map[string]any{
		"type":        discordgo.TextInputComponent,
		"custom_id":   uploadTitleID,
		"style":       discordgo.TextInputShort,
		"min_length":  1,
		"max_length":  100,
		"required":    true,
		"placeholder": "π.χ. Σημειώσεις 3ου κεφαλαίου",
	}
	if v := examTitle(time.Now()); v != "" {
		title["value"] = v
	}
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: uploadModalID,
			Title:    "Ανέβασμα υλικού",
			Components: []discordgo.MessageComponent{
				label("Τίτλος", "Θα εμφανίζεται πάνω από το αρχείο στο κανάλι.", title),
				label("Αρχεία", "PDF ή JPG, έως 10 αρχεία.", map[string]any{
					"type":       fileUploadComponent,
					"custom_id":  uploadFileID,
					"min_values": 1,
					"max_values": uploadMaxFiles,
					"required":   true,
					"file_types": []string{".pdf", ".jpg", ".jpeg"},
				}),
			},
		},
	})
	if err != nil {
		h.log.Error("upload: open form", "error", err)
	}
}

// uploadSubmit is the part of a raw modal submit the upload needs.
type uploadSubmit struct {
	ID        string                    `json:"id"`
	AppID     string                    `json:"application_id"`
	Type      discordgo.InteractionType `json:"type"`
	Token     string                    `json:"token"`
	GuildID   string                    `json:"guild_id"`
	ChannelID string                    `json:"channel_id"`
	Member    *discordgo.Member         `json:"member"`
	Data      struct {
		CustomID   string `json:"custom_id"`
		Components []struct {
			Component *struct {
				CustomID string   `json:"custom_id"`
				Value    string   `json:"value"`
				Values   []string `json:"values"`
			} `json:"component"`
		} `json:"components"`
		Resolved struct {
			Attachments map[string]*discordgo.MessageAttachment `json:"attachments"`
		} `json:"resolved"`
	} `json:"data"`
}

// field returns the submitted component with the given custom ID.
func (u *uploadSubmit) field(id string) (value string, values []string) {
	for _, c := range u.Data.Components {
		if c.Component != nil && c.Component.CustomID == id {
			return c.Component.Value, c.Component.Values
		}
	}
	return "", nil
}

// onRawEvent picks the upload form's submits out of the raw gateway stream.
func (h *Handler) onRawEvent(s *discordgo.Session, e *discordgo.Event) {
	if e.Type != "INTERACTION_CREATE" || !bytes.Contains(e.RawData, []byte(uploadModalID)) {
		return
	}
	var u uploadSubmit
	if err := json.Unmarshal(e.RawData, &u); err != nil {
		h.log.Error("upload: decode submit", "error", err)
		return
	}
	if u.Type != discordgo.InteractionModalSubmit || u.Data.CustomID != uploadModalID {
		return
	}
	h.onUploadSubmit(s, &u)
}

// onUploadSubmit posts the uploaded files to the channel.
func (h *Handler) onUploadSubmit(s *discordgo.Session, u *uploadSubmit) {
	i := &discordgo.Interaction{ID: u.ID, AppID: u.AppID, Token: u.Token, Type: u.Type}
	if h.mat == nil || !h.inHomeGuild(u.GuildID) || u.Member == nil || u.Member.User == nil {
		return
	}
	userID := u.Member.User.ID
	log := h.log.With("user_id", userID, "channel_id", u.ChannelID)

	if err := s.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		log.Error("upload: acknowledge", "error", err)
		return
	}
	answer := func(text string) {
		if _, err := s.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &text}); err != nil {
			log.Warn("upload: reply", "error", err)
		}
	}

	ctx, cancel := h.ctxTimeout(uploadTimeout)
	defer cancel()
	ch, err := h.channel(ctx, s, u.ChannelID)
	if err != nil || !isCourseChannel(ch) {
		answer("Το ανέβασμα υλικού γίνεται μόνο στα κανάλια των μαθημάτων.")
		return
	}
	if !h.matLive.Load() {
		answer("Το ανέβασμα υλικού δεν είναι διαθέσιμο αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.")
		return
	}
	// The bot posts on the member's behalf, so it must not carry the post
	// past a timeout the member could not have posted through.
	if timedOut(u.Member) {
		answer("⛔ Δεν μπορείς να ανεβάσεις υλικό όσο έχεις timeout.")
		return
	}
	if !h.gate.allow("upload:"+userID, uploadsPerHour, time.Hour) {
		answer("⏳ Έχεις ανεβάσει πολλά αρχεία την τελευταία ώρα. Δοκίμασε ξανά λίγο αργότερα.")
		return
	}

	title, _ := u.field(uploadTitleID)
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		answer("Γράψε έναν τίτλο για το υλικό.")
		return
	}
	_, ids := u.field(uploadFileID)
	if len(ids) == 0 || len(ids) > uploadMaxFiles {
		answer("Επίλεξε από 1 έως 10 αρχεία.")
		return
	}
	limit := h.uploadLimit(s, u.GuildID)
	var files []*discordgo.MessageAttachment
	var total int64
	for _, id := range ids {
		a := u.Data.Resolved.Attachments[id]
		if a == nil {
			answer("Κάτι πήγε στραβά με τα αρχεία. Δοκίμασε ξανά.")
			return
		}
		if materialType(a) == "" {
			answer(fmt.Sprintf("Το «%s» δεν είναι PDF ή JPG. Ανέβασε μόνο αρχεία PDF ή JPG.", truncateRunes(a.Filename, 60)))
			return
		}
		total += int64(a.Size)
		files = append(files, a)
	}
	if total > limit {
		answer(fmt.Sprintf("Τα αρχεία είναι πολύ μεγάλα (%s συνολικά, όριο %s). Ανέβασέ τα σε μικρότερες ομάδες.",
			formatBytes(total), formatBytes(limit)))
		return
	}

	send := &discordgo.MessageSend{
		Content: fmt.Sprintf("📚 **%s**\nΑνέβηκε από <@%s>", escapeMarkdown(title), userID),
		// The credit is shown, not pinged: the uploader knows.
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
	for _, a := range files {
		body, err := download(ctx, a.URL, int64(a.Size))
		if err != nil {
			log.Error("upload: fetch uploaded file", "filename", a.Filename, "error", err)
			answer("Δεν μπόρεσα να παραλάβω τα αρχεία. Δοκίμασε ξανά σε λίγο.")
			return
		}
		send.Files = append(send.Files, &discordgo.File{
			Name:        a.Filename,
			ContentType: materialType(a),
			Reader:      bytes.NewReader(body),
		})
	}

	msg, err := s.ChannelMessageSendComplex(ch.ID, send, discordgo.WithContext(ctx))
	send.Files = nil // let the buffers go before the remaining calls
	if err != nil {
		log.Error("upload: post files", "error", err)
		answer("Δεν μπόρεσα να δημοσιεύσω το υλικό στο κανάλι. Δοκίμασε ξανά σε λίγο.")
		return
	}

	added := h.recordAttachments(ctx, msg, materialHome{id: ch.ID, folders: []string{ch.Name}},
		u.GuildID, userID, false, truncateRunes(title, captionLimit))
	if added > 0 {
		h.mat.Wake()
	}
	h.audit(userID, "", "material_uploaded", map[string]any{
		"channel_id": ch.ID, "message_id": msg.ID, "files": len(files), "title": title,
	}, userID)
	log.Info("upload: posted", "message_id", msg.ID, "files", len(files), "bytes", total)

	pinned := true
	if err := h.retry(ctx, "upload.pin", func() error {
		return s.ChannelMessagePin(ch.ID, msg.ID, discordgo.WithContext(ctx))
	}); err != nil {
		pinned = false
		log.Warn("upload: pin", "error", err)
		h.alertModsOnce(s, "pin:"+ch.ID, "⚠️ Δεν μπόρεσα να καρφιτσώσω το υλικό που ανέβηκε στο <#%s> (%v). "+
			"Ίσως έχει γεμίσει το όριο καρφιτσωμένων μηνυμάτων.", ch.ID, err)
	}
	// The post and the "pinned a message" notice both came from the bot,
	// which the message handler ignores, so the sticky is moved here.
	h.bumpSticky(s, ch.ID)

	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", u.GuildID, ch.ID, msg.ID)
	if pinned {
		answer("✅ Το υλικό ανέβηκε και καρφιτσώθηκε στο κανάλι. Ευχαριστούμε! " + link)
	} else {
		answer("✅ Το υλικό ανέβηκε στο κανάλι. Ευχαριστούμε! " + link)
	}
}

// timedOut reports whether a member is currently timed out.
func timedOut(m *discordgo.Member) bool {
	return m.CommunicationDisabledUntil != nil && m.CommunicationDisabledUntil.After(time.Now())
}

// uploadLimit is the most one upload may carry in a guild: Discord's own
// attachment limit for its boost level, capped by the bot's memory budget.
func (h *Handler) uploadLimit(s *discordgo.Session, guildID string) int64 {
	limit := int64(10 << 20)
	if g, err := s.State.Guild(guildID); err == nil {
		switch g.PremiumTier {
		case discordgo.PremiumTier2:
			limit = 50 << 20
		case discordgo.PremiumTier3:
			limit = 100 << 20
		}
	}
	return min(limit, uploadMemoryCap)
}

// download reads a file of the expected size from Discord's CDN.
func download(ctx context.Context, url string, size int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > size {
		return nil, fmt.Errorf("file is larger than declared")
	}
	return body, nil
}

// formatBytes renders a size in MB for members.
func formatBytes(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// escapeMarkdown keeps a member's title from breaking out of the bold text
// it is shown in.
func escapeMarkdown(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`",
		"|", `\|`, ">", `\>`, "#", `\#`, "[", `\[`, "]", `\]`).Replace(s)
}
