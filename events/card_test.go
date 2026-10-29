package events

import (
	"encoding/json"
	"strings"
	"testing"
)

func cardText(t *testing.T, c card) string {
	t.Helper()
	b, err := json.Marshal(c.components())
	if err != nil {
		t.Fatalf("marshal card %d: %v", c.state, err)
	}
	return string(b)
}

func TestCardStatesRender(t *testing.T) {
	for st := cardAsk; st <= cardExpired; st++ {
		c := card{userID: "42", state: st, email: "th12345@edu.hmu.gr"}
		js := cardText(t, c)
		if !strings.Contains(js, `"type":17`) || !strings.Contains(js, `\u003c@42\u003e`) {
			t.Errorf("state %d: not a container card for the member: %s", st, js)
		}
		if !strings.Contains(js, helpButtonID) || !strings.Contains(js, restartButtonID) {
			t.Errorf("state %d: help or restart button missing", st)
		}
		if len(js) > 4000 {
			t.Errorf("state %d: card is %d bytes", st, len(js))
		}
	}
}

func TestCardAskOffersCompleteRegistration(t *testing.T) {
	js := cardText(t, card{userID: "42", state: cardAsk})
	if !strings.Contains(js, "Ολοκλήρωση Εγγραφής") || !strings.Contains(js, emailButtonID) {
		t.Fatalf("ask card lacks the main button: %s", js)
	}
}

func TestCardSentEchoesAddressAndTicksStepOne(t *testing.T) {
	c := card{userID: "42", state: cardSent, email: "th12345@edu.hmu.gr"}
	if !strings.Contains(c.status(), "Έστειλες το `th12345@edu.hmu.gr` για email.") {
		t.Errorf("status = %q", c.status())
	}
	if !strings.HasPrefix(c.steps(), "✅") || !strings.Contains(c.steps(), "👉") {
		t.Errorf("steps = %q", c.steps())
	}
	if !strings.Contains(cardText(t, c), "webmail.edu.hmu.gr") {
		t.Error("HMU card has no webmail button")
	}
}

func TestCardSentMarksOutsiders(t *testing.T) {
	cases := []struct {
		email             string
		outsider, webmail bool
	}{
		{"th12345@edu.hmu.gr", false, true},
		{"tp12345@edu.hmu.gr", true, true},
		{"csd1234@csd.uoc.gr", true, false},
	}
	for _, tc := range cases {
		c := card{userID: "42", state: cardSent, email: tc.email}
		if got := strings.Contains(c.status(), "Verified Outsider"); got != tc.outsider {
			t.Errorf("%s: outsider note = %v, want %v", tc.email, got, tc.outsider)
		}
		if got := strings.Contains(cardText(t, c), "webmail.edu.hmu.gr"); got != tc.webmail {
			t.Errorf("%s: webmail button = %v, want %v", tc.email, got, tc.webmail)
		}
	}
}

func TestCardTooManyHasNoEmailButton(t *testing.T) {
	if strings.Contains(cardText(t, card{userID: "42", state: cardTooMany}), emailButtonID) {
		t.Error("too-many card still offers another email")
	}
}
