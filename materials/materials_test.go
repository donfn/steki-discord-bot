package materials

import (
	"context"
	"io"
	"strings"
	"testing"

	"steki-bot/store"
)

func TestObjectKey(t *testing.T) {
	cases := []struct {
		folders []string
		file    string
		want    string
	}{
		{[]string{"μαθηματικά-ι"}, "Θέματα 2024.pdf", "μαθηματικά-ι/42-Θέματα 2024.pdf"},
		{[]string{"φυσική", "λύσεις/ασκήσεις"}, "a.jpg", "φυσική/λύσεις-ασκήσεις/42-a.jpg"},
		{[]string{".."}, "../../etc", "channel/42--..-etc"},
		{[]string{"c"}, "", "c/42-file"},
	}
	for _, c := range cases {
		if got := ObjectKey(c.folders, "42", c.file); got != c.want {
			t.Errorf("ObjectKey(%q, %q) = %q, want %q", c.folders, c.file, got, c.want)
		}
	}
}

func TestBasicScreener(t *testing.T) {
	cases := []struct {
		ctype, body, want string
	}{
		{TypePDF, "%PDF-1.7\n...", store.MaterialApproved},
		{TypePDF, "\n\n%PDF-1.4", store.MaterialApproved},
		{TypePDF, "<html>not a pdf", store.MaterialRejected},
		{TypeJPEG, "\xff\xd8\xff\xe0JFIF", store.MaterialApproved},
		{TypeJPEG, "%PDF-1.7", store.MaterialRejected},
		{"application/zip", "PK", store.MaterialRejected},
	}
	for _, c := range cases {
		open := func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(c.body)), nil }
		v, err := BasicScreener{}.Screen(context.Background(), &store.Material{ContentType: c.ctype}, open)
		if err != nil || v.Status != c.want {
			t.Errorf("%s %q: %+v %v, want %s", c.ctype, c.body, v, err, c.want)
		}
	}
}
