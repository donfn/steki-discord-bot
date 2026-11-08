// Package brand holds the images and university names the emails and pages share.
package brand

import "embed"

var (
	//go:embed logo.png
	logoPNG []byte
	//go:embed uni
	uniFS embed.FS
)

// Consent is what the link page shows the member before they accept: who they
// are on Discord and which address they are about to connect.
type Consent struct {
	Username   string
	AvatarURL  string
	Email      string
	University string // a key of Universities
}

// University is a school whose students can register.
type University struct {
	Name string
	Logo string // path under /verify/uni/
}

// Logo is the Steki logo PNG.
func Logo() []byte { return logoPNG }

// UniLogo is the PNG of a University's Logo, or nil.
func UniLogo(name string) []byte {
	b, _ := uniFS.ReadFile("uni/" + name)
	return b
}

// Universities are keyed by the value of Consent.University.
var Universities = map[string]University{
	"hmu": {Name: "Ελληνικό Μεσογειακό Πανεπιστήμιο", Logo: "hmu.png"},
	"uoc": {Name: "Πανεπιστήμιο Κρήτης", Logo: "uoc.png"},
}
