package mail

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"
)

func testLink() Link {
	return Link{
		URL:        "https://steki.example/link?t=a.b.c&x=1",
		Username:   `Μαρία <script>alert(1)</script>`,
		AvatarURL:  "https://cdn.discordapp.com/embed/avatars/3.png",
		Email:      "th12345@edu.hmu.gr",
		UniName:    "Ελληνικό Μεσογειακό Πανεπιστήμιο",
		UniLogoURL: "https://steki.example/verify/uni/hmu.png",
		LogoURL:    "https://steki.example/verify/logo.png",
		ValidFor:   30 * time.Minute,
	}
}

func TestHTMLBodyEscapesAndFillsEverything(t *testing.T) {
	body := htmlBody(testLink(), false)
	if strings.Contains(body, "<script>") {
		t.Fatal("member-controlled name was not escaped")
	}
	if strings.Contains(body, "{{") {
		t.Fatal("a placeholder was left unfilled")
	}
	for _, want := range []string{
		"https://steki.example/link?t=a.b.c&amp;x=1", "th12345<wbr>@edu.hmu.gr",
		"/verify/uni/hmu.png", "embed/avatars/3.png", "30 λεπτά", "Σύνδεση λογαριασμού",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("email lacks %q", want)
		}
	}
	// STEKI_EMAIL_PREVIEW=path writes the rendered email for a visual check.
	if p := os.Getenv("STEKI_EMAIL_PREVIEW"); p != "" {
		l := testLink()
		l.Username = "Μαρία Παπαδάκη"
		l.URL, l.UniLogoURL, l.LogoURL = "http://localhost:8099/link", "http://localhost:8099/verify/uni/hmu.png",
			"http://localhost:8099/verify/logo.png"
		if err := os.WriteFile(p, []byte(htmlBody(l, false)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlainBodyHasLinkAndExpiry(t *testing.T) {
	body := plainBody(testLink())
	if !strings.Contains(body, "https://steki.example/link?t=a.b.c&x=1") || !strings.Contains(body, "30 λεπτά") {
		t.Fatalf("plain body = %q", body)
	}
}

func TestHTMLBodyWithPictureUsesTheInlineHeader(t *testing.T) {
	body := htmlBody(testLink(), true)
	if !strings.Contains(body, `src="cid:`+headerCID+`"`) {
		t.Fatal("email does not show the inline header")
	}
	if strings.Contains(body, "embed/avatars/3.png") || strings.Contains(body, "{{") || strings.Contains(body, "<script>") {
		t.Fatal("picture email still carries the HTML header, a placeholder or an unescaped name")
	}
}

func TestRenderHeaderDrawsAPNG(t *testing.T) {
	l := testLink()
	l.AvatarURL = "" // no network: the fallback disc is drawn
	var err error
	if l.Logo, err = os.ReadFile("../brand/logo.png"); err != nil {
		t.Fatal(err)
	}
	if l.UniLogo, err = os.ReadFile("../brand/uni/hmu.png"); err != nil {
		t.Fatal(err)
	}
	b, err := renderHeader(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if w := img.Bounds().Dx(); w != headerWidth*scale {
		t.Errorf("width = %d", w)
	}
	// STEKI_HEADER_PREVIEW=path writes the PNG for a visual check.
	if p := os.Getenv("STEKI_HEADER_PREVIEW"); p != "" {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRenderHeaderNeedsTheLogos(t *testing.T) {
	if _, err := renderHeader(t.Context(), testLink()); err == nil {
		t.Fatal("drew a header without logos")
	}
}

func TestWrapSplitsAnAddressAtTheAt(t *testing.T) {
	f := newFace(boldFont, 13)
	got := wrap(f, "th12345678@edu.hmu.gr", 110, 2)
	if len(got) != 2 || !strings.HasPrefix(got[1], "@") {
		t.Errorf("wrap = %q", got)
	}
	if got := wrap(f, strings.Repeat("Μεσογειακό ", 10), 120, 3); len(got) != 3 || !strings.HasSuffix(got[2], "…") {
		t.Errorf("long text = %q", got)
	}
}
