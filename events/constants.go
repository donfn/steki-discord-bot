package events

import "time"

// Discord role IDs for the Steki guild.
const (
	staffRoleID    = "886976948172124160"
	modRoleID      = "887063786240999475"
	modViewRoleID  = "887348241350402138" // can view register channels
	hmuRoleID      = "886993717725102103"
	outsiderRoleID = "993612908074381352"
	goldenRoleID   = "943889393863774279"
	ghostRoleID    = "1565626375941918850" // members silent for three months
)

// verifiedRoleIDs are the roles that mean a member has completed registration.
var verifiedRoleIDs = []string{hmuRoleID, outsiderRoleID}

// protectedRoleIDs are never kicked by the reaper regardless of idle time.
var protectedRoleIDs = []string{staffRoleID, modRoleID, modViewRoleID, hmuRoleID, outsiderRoleID}

// Registration lifecycle timings.
const (
	// linkTTL is how long a registration email link stays valid. Only the
	// newest link sent for a registration works.
	linkTTL = 30 * time.Minute

	// maxCodeSends caps how many emails one registration may request, so the
	// Resend quota cannot be burned by a single member.
	maxCodeSends = 5

	// nudgeAfter / kickAfter implement the abandonment policy: a reminder in
	// the register channel at one hour, removal from the server at six.
	nudgeAfter = 1 * time.Hour
	kickAfter  = 6 * time.Hour

	// sweepEvery is how often the reaper runs.
	sweepEvery = 5 * time.Minute

	// maxReapPerTick bounds how many members may be nudged or kicked in a
	// single sweep, so a backlog can never turn into a mass-kick.
	maxReapPerTick = 15

	// missedJoinWindow limits /reg reconcile's "missed join" recovery to
	// members who joined recently. A gateway outage drops joins for minutes,
	// not days; without this bound a pass would open a registration — and so
	// start the kick clock — for every long-standing member who happens to
	// hold no role.
	missedJoinWindow = 24 * time.Hour

	// maxOnboardPerSweep caps how many channels one /reg reconcile pass may
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

// restartEmoji restarts the registration flow when reacted to. New channels
// use the restart button instead; this keeps the reaction under intros posted
// before the button existed working.
const restartEmoji = "🔄"

// Greek copy shared by the welcome flow and the restart flow.
const (
	msgWelcome = "**Γεια σου <@%s>, καλώς ήρθες στο Steki!**\n" +
		"Πριν μπεις, πρέπει να επιβεβαιώσουμε ότι είσαι φοιτητής.\n\n" +
		"**Θα σου στείλω έναν σύνδεσμο στο ακαδημαϊκό σου email. Άνοιξέ τον, πάτα αποδοχή και μπαίνεις αμέσως.**"

	msgRestartHint = "Αν κολλήσεις κάπου, πάτα το κουμπί παρακάτω για να ξεκινήσεις από την αρχή " +
		"ή γράψε στο <#%s>."

	msgAskEmail = "**Πάτα το κουμπί και γράψε το ακαδημαϊκό σου email, " +
		"αυτό της μορφής *thXXXXXX@edu.hmu.gr* που σου έδωσε η γραμματεία**.\n\n" +
		":new: Φοιτητές άλλων τμημάτων (tpXXXXX@edu.hmu.gr, csd.uoc.gr) μπορούν πλέον να γραφτούν ως Verified Outsiders."

	// msgNudge is posted in the register channel after one hour of silence.
	// It replaces the old direct messages, which were sent every 20 minutes
	// for as long as the registration stayed open.
	msgNudge = "👋 <@%s>, δεν έχεις τελειώσει ακόμα την εγγραφή σου.\n" +
		"Αν δεν την τελειώσεις μέσα σε **5 ώρες**, θα αφαιρεθείς αυτόματα από τον server " +
		"(μπορείς να ξαναμπείς όποτε θέλεις).\n" +
		"-# Κόλλησες; Πάτα **Χρειάζομαι βοήθεια** στο μήνυμα της εγγραφής."

	msgKickReason = "Η εγγραφή δεν ολοκληρώθηκε μέσα σε 6 ώρες (αυτόματη εκκαθάριση)."
)

// phishRefreshEvery is how often the phishing domain feeds are re-downloaded.
const phishRefreshEvery = 6 * time.Hour
