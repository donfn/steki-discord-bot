package main

import (
	"testing"

	"steki-bot/store"
)

const (
	submitter = "100000000000000001"
	other     = "100000000000000002"
	email     = "th123@edu.hmu.gr"
)

func submittedLine(userID, hash string) string {
	return "Ο χρήστης <@" + userID + "> έστειλε mail για εγγραφή: ```Hash: " + hash + "```"
}

func alreadyUsedLine(email, hash string) string {
	return "Το " + email + " έχει ήδη χρησιμοποιηθεί. Hash: " + hash
}

func TestIngestLineJoinsSubmissionAndNotice(t *testing.T) {
	hash := store.LegacyHash(email)
	byHash := map[string]*recovered{}

	ingestLine(submittedLine(submitter, hash), byHash)
	ingestLine(alreadyUsedLine(email, hash), byHash)

	r := byHash[hash]
	if r == nil {
		t.Fatal("hash not recorded")
	}
	if !r.used {
		t.Error("an \"already used\" notice must confirm the hash as used")
	}
	if got := r.owner(); got != submitter {
		t.Errorf("owner = %q, want %q", got, submitter)
	}
	if r.email != email {
		t.Errorf("email = %q, want %q", r.email, email)
	}
}

func TestIngestLineNoticeMentionIsNotOwner(t *testing.T) {
	hash := store.LegacyHash(email)
	byHash := map[string]*recovered{}

	// Scanned newest first: the later attempt by someone else comes before
	// the original submission.
	ingestLine("Ο <@"+other+"> δοκίμασε: "+alreadyUsedLine(email, hash), byHash)
	ingestLine(submittedLine(submitter, hash), byHash)

	if got := byHash[hash].owner(); got != submitter {
		t.Errorf("owner = %q, want the submitter %q", got, submitter)
	}
}

func TestIngestLineSubmissionAloneIsUnconfirmed(t *testing.T) {
	hash := store.LegacyHash(email)
	byHash := map[string]*recovered{}
	ingestLine(submittedLine(submitter, hash), byHash)

	if byHash[hash].used {
		t.Error("a submission alone may have been abandoned and must not count as used")
	}
}

func TestOwnerAmbiguousWithSeveralSubmitters(t *testing.T) {
	hash := store.LegacyHash(email)
	byHash := map[string]*recovered{}
	ingestLine(submittedLine(submitter, hash), byHash)
	ingestLine(submittedLine(other, hash), byHash)

	if got := byHash[hash].owner(); got != "" {
		t.Errorf("owner = %q, want none when two members submitted the hash", got)
	}
}

func TestIngestLineRejectsMismatchedPlaintext(t *testing.T) {
	hash := store.LegacyHash(email)
	byHash := map[string]*recovered{}
	ingestLine(alreadyUsedLine("th999@edu.hmu.gr", hash), byHash)

	if got := byHash[hash].email; got != "" {
		t.Errorf("accepted plaintext %q whose hash does not match", got)
	}
}

func TestIngestLineIgnoresCodeHash(t *testing.T) {
	const hash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	byHash := map[string]*recovered{}
	// The "received code" line must never be treated as an email hash.
	ingestLine("Ο χρήστης <@1> έλαβε τον κωδικό εγγραφής ```Hash: "+hash+"```", byHash)
	if len(byHash) != 0 {
		t.Errorf("code hash was wrongly ingested: %v", byHash)
	}
}

func TestIngestLineRequiresHash(t *testing.T) {
	byHash := map[string]*recovered{}
	ingestLine("Ο χρήστης <@1> έστειλε mail για εγγραφή: (no hash here)", byHash)
	if len(byHash) != 0 {
		t.Errorf("line without hash produced an entry: %v", byHash)
	}
}
