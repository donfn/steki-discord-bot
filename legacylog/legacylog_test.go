package legacylog

import (
	"testing"
	"time"

	"steki-bot/store"
)

const (
	owner = "100000000000000001"
	other = "100000000000000002"
	email = "th123@edu.hmu.gr"
)

var t0 = time.Date(2023, 3, 1, 12, 0, 0, 0, time.UTC)

func submitted(userID, hash string) string {
	return "Ο χρήστης <@" + userID + "> έστειλε mail για εγγραφή: ```Hash: " + hash + "```"
}

func submittedOld(userID, hash string) string {
	return "Ο χρήστης <@" + userID + "> έστειλε το εξής mail για εγγραφή: ```Hash: " + hash + "```"
}

func inUse(email, hash string) string {
	return "Το " + email + " έχει ήδη χρησιμοποιηθεί για εγγραφή. Αναγνωριστικό για αφαίρεση απο db: ```" + hash + "```"
}

func parse(t *testing.T, id, content string, at time.Time) store.LogEvidence {
	t.Helper()
	e, ok := Parse(id, content, at)
	if !ok {
		t.Fatalf("no evidence in %q", content)
	}
	return e
}

func TestParseBothSubmissionShapes(t *testing.T) {
	hash := store.LegacyHash(email)
	for _, line := range []string{submitted(owner, hash), submittedOld(owner, hash)} {
		e := parse(t, "1", line, t0)
		if e.Kind != store.EvidenceSubmitted || e.UserID != owner || e.Hash != hash {
			t.Errorf("Parse(%q) = %+v", line, e)
		}
	}
}

func TestParseInUseKeepsMatchingPlaintextOnly(t *testing.T) {
	hash := store.LegacyHash(email)
	if e := parse(t, "1", inUse(email, hash), t0); e.Email != email || e.Kind != store.EvidenceInUse {
		t.Errorf("matching notice = %+v", e)
	}
	if e := parse(t, "2", inUse("th999@edu.hmu.gr", hash), t0); e.Email != "" {
		t.Errorf("kept plaintext %q whose hash does not match", e.Email)
	}
}

func TestParseIgnoresCodeHashAndPlainLines(t *testing.T) {
	const hash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	for _, line := range []string{
		"Ο χρήστης <@1> έλαβε τον κωδικό εγγραφής ```Hash: " + hash + "```.",
		"Ο χρήστης <@1> έλαβε στο inbox του τον κωδικό εγγραφής ```Hash: " + hash + "``` .",
		"Ο χρήστης <@1> έστειλε mail για εγγραφή: (no hash here)",
		"Το email `Hash: " + hash + "` αφαιρέθηκε από την db.",
	} {
		if e, ok := Parse("1", line, t0); ok {
			t.Errorf("Parse(%q) = %+v, want nothing", line, e)
		}
	}
}

func TestResolveSingleSubmitterOwns(t *testing.T) {
	hash := store.LegacyHash(email)
	got, _ := Resolve([]store.LogEvidence{parse(t, "1", submitted(owner, hash), t0)})
	if got != owner {
		t.Errorf("owner = %q, want %q", got, owner)
	}
}

// Someone else trying the address later is refused; the notice right after
// their submission marks them as the refused one, not the owner.
func TestResolveRefusedLaterAttemptIsNotOwner(t *testing.T) {
	hash := store.LegacyHash(email)
	ev := []store.LogEvidence{
		parse(t, "1", submitted(owner, hash), t0),
		parse(t, "2", submitted(other, hash), t0.Add(30*24*time.Hour)),
		parse(t, "3", inUse(email, hash), t0.Add(30*24*time.Hour+2*time.Second)),
	}
	got, plain := Resolve(ev)
	if got != owner {
		t.Errorf("owner = %q, want %q", got, owner)
	}
	if plain != email {
		t.Errorf("email = %q, want %q", plain, email)
	}
}

func TestResolveAmbiguousWithTwoUnrefusedSubmitters(t *testing.T) {
	hash := store.LegacyHash(email)
	ev := []store.LogEvidence{
		parse(t, "1", submitted(owner, hash), t0),
		parse(t, "2", submitted(other, hash), t0.Add(time.Hour)),
	}
	if got, _ := Resolve(ev); got != "" {
		t.Errorf("owner = %q, want none when two members submitted the hash", got)
	}
}

// An owner who left and came back is refused their own address; their
// original submission still identifies them.
func TestResolveOwnerRefusedOnRejoinStillOwns(t *testing.T) {
	hash := store.LegacyHash(email)
	ev := []store.LogEvidence{
		parse(t, "1", submitted(owner, hash), t0),
		parse(t, "2", submitted(owner, hash), t0.Add(90*24*time.Hour)),
		parse(t, "3", inUse(email, hash), t0.Add(90*24*time.Hour+time.Second)),
	}
	if got, _ := Resolve(ev); got != owner {
		t.Errorf("owner = %q, want %q", got, owner)
	}
}

// A notice with no submission just before it refuses nobody.
func TestResolveNoticeFarFromSubmissionRefusesNobody(t *testing.T) {
	hash := store.LegacyHash(email)
	ev := []store.LogEvidence{
		parse(t, "1", submitted(owner, hash), t0),
		parse(t, "2", inUse(email, hash), t0.Add(time.Hour)),
	}
	if got, _ := Resolve(ev); got != owner {
		t.Errorf("owner = %q, want %q", got, owner)
	}
}
