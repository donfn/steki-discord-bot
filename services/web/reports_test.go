package web

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/materials"
	"steki-bot/internal/store"
)

func TestReportCard(t *testing.T) {
	m := &store.Material{AttachmentID: "555", UploaderID: "u9", ChannelID: "c1", GuildID: "g1", MessageID: "m1", Filename: "θέματα.pdf"}
	r := &store.Report{ID: 1, AttachmentID: "555", ReporterID: "u2", Reason: strings.Repeat("λόγος ", 200), Note: "δεν είναι υλικό"}
	e, rows := reportCard(m, r, "")

	var all strings.Builder
	for _, f := range e.Fields {
		if n := utf8.RuneCountInString(f.Value); n > 1024 {
			t.Errorf("field %q is %d characters, Discord takes 1024", f.Name, n)
		}
		all.WriteString(f.Value)
	}
	for _, want := range []string{"<@u2>", "<@u9>", "<#c1>", "δεν είναι υλικό"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("card lacks %q", want)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("%d component rows", len(rows))
	}
	btns := rows[0].(discordgo.ActionsRow).Components
	if len(btns) != 2 {
		t.Fatalf("%d buttons", len(btns))
	}
	if id := btns[0].(discordgo.Button).CustomID; id != reportRemovePrefix+"555" {
		t.Errorf("remove button id %q", id)
	}
	if id := btns[1].(discordgo.Button).CustomID; id != reportRestorePrefix+"555" {
		t.Errorf("restore button id %q", id)
	}
	// Report buttons must not be taken for the image review buttons.
	for _, b := range btns {
		id := b.(discordgo.Button).CustomID
		if strings.HasPrefix(id, reviewApprovePrefix) || strings.HasPrefix(id, reviewRejectPrefix) {
			t.Errorf("button %q collides with the image review prefixes", id)
		}
	}
}

func TestExamCard(t *testing.T) {
	m := &store.Material{AttachmentID: "555", UploaderID: "u9", ChannelID: "c1", GuildID: "g1", MessageID: "m1", Filename: "a.jpg"}
	for _, exam := range []string{materials.ExamSure, materials.ExamUnsure} {
		e, rows := examCard(m, materials.Verdict{Exam: exam, Note: "θέματα"}, "")
		btns := rows[0].(discordgo.ActionsRow).Components
		if len(btns) != 1 || btns[0].(discordgo.Button).CustomID != examUndoPrefix+"u9" {
			t.Errorf("%s: want one revert button for u9, got %+v", exam, btns)
		}
		if strings.Contains(e.Description, "δεν είναι σίγουρο") != (exam == materials.ExamUnsure) {
			t.Errorf("%s: description %q", exam, e.Description)
		}
	}
}
