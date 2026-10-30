package events

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestMaterialType(t *testing.T) {
	cases := map[string]struct {
		name, ctype, want string
	}{
		"pdf":           {"Θέματα.PDF", "", "application/pdf"},
		"pdf by type":   {"blob", "application/pdf", "application/pdf"},
		"jpg":           {"photo.jpg", "image/jpeg", "image/jpeg"},
		"jpeg":          {"photo.JPEG", "", "image/jpeg"},
		"png is not":    {"shot.png", "image/png", ""},
		"zip is not":    {"notes.zip", "application/zip", ""},
		"docx is not":   {"notes.docx", "", ""},
		"pdf lookalike": {"notes.pdf.exe", "", ""},
	}
	for name, c := range cases {
		if got := materialType(&discordgo.MessageAttachment{Filename: c.name, ContentType: c.ctype}); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestExamTitle(t *testing.T) {
	at := func(m time.Month) time.Time { return time.Date(2026, m, 15, 12, 0, 0, 0, greece) }
	want := map[time.Month]string{
		time.January: "Θέματα εξεταστικής Ιαν-Φεβ 2026", time.February: "Θέματα εξεταστικής Ιαν-Φεβ 2026",
		time.June: "Θέματα εξεταστικής Ιουν-Ιουλ 2026", time.July: "Θέματα εξεταστικής Ιουν-Ιουλ 2026",
		time.August: "Θέματα εξεταστικής Σεπ 2026", time.September: "Θέματα εξεταστικής Σεπ 2026",
		time.October: "Θέματα εξεταστικής Σεπ 2026",
		time.March:   "", time.May: "", time.November: "", time.December: "",
	}
	for m, w := range want {
		if got := examTitle(at(m)); got != w {
			t.Errorf("%s: %q, want %q", m, got, w)
		}
	}
}

func TestStickyCard(t *testing.T) {
	embeds, comps := stickyCard(12, "https://x/materials?t=abc")
	if len(embeds) != 1 || embeds[0].Title == "" || embeds[0].Color != stickyColor {
		t.Errorf("card: %+v", embeds)
	}
	row := comps[0].(discordgo.ActionsRow)
	view, upload := row.Components[0].(discordgo.Button), row.Components[1].(discordgo.Button)
	if len(row.Components) != 2 || view.Label != "Προβολή υλικού μαθήματος (12)" ||
		view.Style != discordgo.LinkButton || view.URL != "https://x/materials?t=abc" || view.CustomID != "" ||
		upload.Label != "Ανέβασμα υλικού" || upload.CustomID != uploadButtonID {
		t.Errorf("sticky buttons: %+v", row.Components)
	}
}

func TestHasSticky(t *testing.T) {
	ch := func(name string) *discordgo.Channel {
		return &discordgo.Channel{Name: name, Type: discordgo.ChannelTypeGuildText, ParentID: "938731151705649152"}
	}
	if !hasSticky(ch("αλγόριθμοι")) {
		t.Error("course channel has no sticky")
	}
	if hasSticky(ch("στατιστικά-εξάμηνο-1")) || hasSticky(ch("Στατιστικά-εξάμηνο-2")) {
		t.Error("statistics channel has a sticky")
	}
	if hasSticky(&discordgo.Channel{Name: "general", Type: discordgo.ChannelTypeGuildText, ParentID: "1"}) {
		t.Error("non-course channel has a sticky")
	}
}

func TestConvertibleImage(t *testing.T) {
	for name, want := range map[string]bool{"a.PNG": true, "a.webp": true, "IMG_1.HEIC": true, "a.heif": true,
		"a.jpg": false, "a.gif": false, "a.pdf": false} {
		if got := convertibleImage(&discordgo.MessageAttachment{Filename: name}); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if !convertibleImage(&discordgo.MessageAttachment{Filename: "blob", ContentType: "image/webp"}) {
		t.Error("declared type ignored")
	}
}

func TestIsSilent(t *testing.T) {
	if isSilent(&discordgo.Message{Flags: discordgo.MessageFlagsSuppressEmbeds}) {
		t.Error("message without SUPPRESS_NOTIFICATIONS counted as silent")
	}
	if !isSilent(&discordgo.Message{Flags: discordgo.MessageFlagsSuppressNotifications}) {
		t.Error("silent message not recognised")
	}
}

func TestNewerID(t *testing.T) {
	if !newerID("1000000000000000000", "999999999999999999") {
		t.Error("numeric comparison failed where string comparison would")
	}
	if newerID("5", "5") || newerID("", "5") {
		t.Error("equal or empty IDs compared as newer")
	}
}

func TestIsCourseChannel(t *testing.T) {
	if !isCourseChannel(&discordgo.Channel{Type: discordgo.ChannelTypeGuildText, ParentID: "938731151705649152"}) {
		t.Error("course text channel not recognised")
	}
	if isCourseChannel(&discordgo.Channel{Type: discordgo.ChannelTypeGuildVoice, ParentID: "938731151705649152"}) {
		t.Error("voice channel counted as a course channel")
	}
	if isCourseChannel(&discordgo.Channel{Type: discordgo.ChannelTypeGuildText, ParentID: "1"}) {
		t.Error("channel outside the course categories counted")
	}
}

func TestUploadSubmitDecoding(t *testing.T) {
	// The shape Discord sends for a modal with Label-wrapped components.
	raw := []byte(`{"id":"1","application_id":"2","type":5,"token":"tok","guild_id":"g","channel_id":"c",
		"member":{"user":{"id":"u"}},
		"data":{"custom_id":"materials:upload:form","components":[
			{"type":18,"id":1,"component":{"type":4,"id":2,"custom_id":"title","value":"Θέματα εξεταστικής Ιαν-Φεβ 2026"}},
			{"type":18,"id":3,"component":{"type":19,"id":4,"custom_id":"files","values":["900"]}}],
		"resolved":{"attachments":{"900":{"id":"900","filename":"a.pdf","size":10,"url":"https://cdn/a.pdf","content_type":"application/pdf"}}}}}`)
	var u uploadSubmit
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	title, _ := u.field(uploadTitleID)
	_, ids := u.field(uploadFileID)
	if title != "Θέματα εξεταστικής Ιαν-Φεβ 2026" || len(ids) != 1 || u.Data.Resolved.Attachments[ids[0]].Filename != "a.pdf" {
		t.Errorf("decoded title %q ids %v", title, ids)
	}
	if u.Member.User.ID != "u" || u.Token != "tok" || u.Type != discordgo.InteractionModalSubmit {
		t.Errorf("decoded interaction %+v", u)
	}
}

func TestUploadFormJSON(t *testing.T) {
	b, err := json.Marshal(label("Αρχεία", "PDF", map[string]any{"type": fileUploadComponent, "custom_id": "files"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"component":{"custom_id":"files","type":19},"description":"PDF","label":"Αρχεία","type":18}` {
		t.Errorf("label JSON %s", b)
	}
}

func TestEscapeMarkdown(t *testing.T) {
	if got := escapeMarkdown("**hi** @everyone"); got != `\*\*hi\*\* @everyone` {
		t.Errorf("%q", got)
	}
}
