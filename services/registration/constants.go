package registration

import "time"

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

// restartEmoji restarts the registration flow when reacted to. New channels
// use the restart button instead; this keeps the reaction under intros posted
// before the button existed working.
const restartEmoji = "🔄"

// Greek copy shared by the reaper and the cleanup paths.
const (
	// msgNudge is posted in the register channel after one hour of silence.
	// It replaces the old direct messages, which were sent every 20 minutes
	// for as long as the registration stayed open.
	msgNudge = "👋 <@%s>, δεν έχεις τελειώσει ακόμα την εγγραφή σου.\n" +
		"Αν δεν την τελειώσεις μέσα σε **5 ώρες**, θα αφαιρεθείς αυτόματα από τον server " +
		"(μπορείς να ξαναμπείς όποτε θέλεις).\n" +
		"-# Κόλλησες; Πάτα **Χρειάζομαι βοήθεια** στο μήνυμα της εγγραφής."

	msgKickReason = "Η εγγραφή δεν ολοκληρώθηκε μέσα σε 6 ώρες (αυτόματη εκκαθάριση)."
)

const (
	// maxMailsPerUserHour, maxMailsPerAddrHour and maxMailsPerHour bound the
	// verification emails one member, one address and the whole bot may
	// send, so leaving and rejoining cannot reset the per-registration cap
	// and the bot cannot be used to flood an address or burn the quota.
	maxMailsPerUserHour = 6
	maxMailsPerAddrHour = 3
	maxMailsPerHour     = 150
)
