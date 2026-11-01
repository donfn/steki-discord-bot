package tokens

import (
	"strings"
	"testing"
	"time"
)

var (
	internal = strings.Repeat("i", 32)
	jwtKey   = strings.Repeat("j", 32)
)

func TestWebAcceptsWhatOtherServicesIssue(t *testing.T) {
	issuer, web := NewIssuer(internal), New(jwtKey, internal)
	exp := time.Now().Add(time.Hour)
	link, err := issuer.IssueLink("u", "g", "id", exp)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := issuer.Issue("u", "g", ReasonMedia, "id", exp)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{link, guard} {
		if _, err := web.Parse(raw); err != nil {
			t.Errorf("web refused it: %v", err)
		}
	}
}

// A taken-over registration or moderation service holds INTERNAL_SECRET. It must
// not be able to forge the links that open members' study rooms.
func TestThePageKeyCannotForgeWebTokens(t *testing.T) {
	web := New(jwtKey, internal)
	// The attacker's Tokens: JWT_SECRET's slot filled with the key they do have.
	attacker := New(string(pageKey(internal)), internal)
	exp := time.Now().Add(time.Hour)
	forge := map[string]func() (string, error){
		ReasonMaterials: func() (string, error) { return attacker.IssueMaterials("u", "g", "c", "id", "", exp) },
		ReasonSocket:    func() (string, error) { return attacker.IssueSocket("u", "g", "c", "id", exp) },
		ReasonMCPGuide:  func() (string, error) { return attacker.IssueGuide("u", "g", "id", exp) },
	}
	for reason, f := range forge {
		raw, err := f()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := web.Parse(raw); err == nil {
			t.Errorf("%s token signed with the page key was accepted", reason)
		}
	}
}

// Links already out when the keys were split were signed with JWT_SECRET.
func TestLinksFromBeforeTheSplitStillWork(t *testing.T) {
	before := New(jwtKey, internal)
	before.page = []byte(jwtKey)
	raw, err := before.IssueLink("u", "g", "id", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(jwtKey, internal).Parse(raw); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

func TestIssuerCannotCheckWebTokens(t *testing.T) {
	raw, err := New(jwtKey, internal).IssueGuide("u", "g", "id", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIssuer(internal).Parse(raw); err == nil {
		t.Fatal("an issuer accepted a token it holds no key for")
	}
}
