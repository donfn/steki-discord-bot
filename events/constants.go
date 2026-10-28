package events

import "time"

// Discord role IDs for the Steki guild.
const (
	staffRoleID   = "886976948172124160"
	modRoleID     = "887063786240999475"
	modViewRoleID = "887348241350402138" // can view register channels
	hmuRoleID     = "886993717725102103"
	csdRoleID     = "993612908074381352"
)

// verifiedRoleIDs are the roles that mean a member has completed registration.
var verifiedRoleIDs = []string{hmuRoleID, csdRoleID}

// protectedRoleIDs are never kicked by the reaper regardless of idle time.
var protectedRoleIDs = []string{staffRoleID, modRoleID, modViewRoleID, hmuRoleID, csdRoleID}

// Registration lifecycle timings.
const (
	// codeTTL is how long a verification code stays valid. Previously codes
	// never expired.
	codeTTL = 15 * time.Minute

	// maxFailedAttempts is the number of wrong codes a member may enter; the
	// last of them destroys the code and sends the member back to step one.
	maxFailedAttempts = 3

	// maxCodeSends caps how many codes one registration may request, so the
	// Resend quota cannot be burned by a single member.
	maxCodeSends = 5

	// nudgeAfter / kickAfter implement the abandonment policy: a reminder in
	// the register channel at one hour, removal from the server at six.
	nudgeAfter = 1 * time.Hour
	kickAfter  = 6 * time.Hour

	// sweepEvery is how often the reaper and reconciler run.
	sweepEvery = 5 * time.Minute

	// maxReapPerTick bounds how many members may be nudged or kicked in a
	// single sweep, so a backlog can never turn into a mass-kick.
	maxReapPerTick = 15

	// reconcileEvery is how often the reconciler runs between gateway
	// reconnects, each of which also triggers a pass.
	reconcileEvery = time.Hour

	// missedJoinWindow limits the reconciler's "missed join" recovery to
	// members who joined recently. A gateway outage drops joins for minutes,
	// not days; without this bound the first pass after a deploy would open
	// a registration — and so start the kick clock — for every long-standing
	// member who happens to hold no role.
	missedJoinWindow = 24 * time.Hour

	// maxOnboardPerSweep caps how many channels one reconciler pass may
	// create, so recovering from a long outage cannot flood the guild.
	maxOnboardPerSweep = 25
)

// Time budgets for one unit of work. Every database and Discord call made
// from a handler runs under one of these, so nothing can wedge indefinitely.
const (
	// dbTimeout covers a short exchange: a lookup, an audit write.
	dbTimeout = 10 * time.Second

	// flowTimeout covers one registration step end to end, which may include
	// sending an email or creating a channel with retries.
	flowTimeout = 45 * time.Second

	// reapTimeout and reconcileTimeout cover one background sweep.
	reapTimeout      = 2 * time.Minute
	reconcileTimeout = 5 * time.Minute
)

// registerChannelPrefix is how a member's registration channel is named.
const registerChannelPrefix = "register-"

// Reaction → role mapping for the interest selection step.
var reactionRoles = map[string]string{
	"🖥️": "886993717725102103",
	"🤖":  "887198468421083136",
	"⚙️": "887198536360419348",
	"👪":  "887198604027101284",
	"🌱":  "887198628899356683",
	"💉":  "887198656837591041",
	"🎶":  "887200015825641542",
	"💼":  "887198689372815410",
	"✈️": "887198713628483664",
	"🍎":  "887198734209925141",
	"💸":  "887198782255661086",
}

// restartEmoji restarts the registration flow when reacted to.
const restartEmoji = "🔄"

// Greek copy shared by the welcome flow and the restart flow.
const (
	msgWelcome = "**Γειά σου <@%s>, καλωσόρισες στο Steki!** \n" +
		"Θα χρειαστεί να επιβεβαιώσουμε την φοιτητική σου ιδιότητα.\n\n" +
		"Είναι πολύ απλό: \n" +
		"**Θα σου στείλω έναν κωδικό στο ακαδημαϊκό σου email και εσύ θα πρέπει να μου τον γράψεις εδώ.**"

	msgRestartHint = "Αν κολλήσεις σε κάποιο από τα βήματα, μπορείς να κάνεις react με :arrows_counterclockwise: " +
		"στο μήνυμα αυτό και θα αρχίσει η διαδικασία από την αρχή. Εναλλακτικά, στείλε στο <#%s>."

	msgAskEmail = "**Για να συνεχίσουμε, στείλε μου εδώ το ακαδημαϊκό σου email. " +
		"Θα πρέπει να είναι της μορφής *thXXXXXX@edu.hmu.gr* (αυτό που σου έδωσε η γραμματεία κατά την εγγραφή)**.\n\n" +
		":new: Τώρα φοιτητές από εξωτερικά τμήματα (csd.uoc.gr) μπορούν να γραφτούν ως Verified Outsiders."

	// msgNudge is posted in the register channel after one hour of silence.
	// It replaces the old direct messages, which were sent every 20 minutes
	// for as long as the registration stayed open.
	msgNudge = "👋 <@%s>, η εγγραφή σου είναι ακόμα σε εκκρεμότητα.\n\n" +
		"⏳ **Αν δεν ολοκληρώσεις την εγγραφή σου μέσα στις επόμενες 5 ώρες, " +
		"θα αποχωρήσεις αυτόματα από τον σέρβερ** και το κανάλι αυτό θα διαγραφεί. " +
		"Μπορείς να ξαναμπείς όποτε θέλεις και να δοκιμάσεις ξανά.\n\n" +
		"💁🏻 Αν αντιμετωπίζεις κάποιο πρόβλημα, στείλε στο <#%s>."

	msgKickReason = "Η εγγραφή δεν ολοκληρώθηκε μέσα σε 6 ώρες (αυτόματη εκκαθάριση)."
)

// phishRefreshEvery is how often the phishing domain feeds are re-downloaded.
const phishRefreshEvery = 6 * time.Hour
