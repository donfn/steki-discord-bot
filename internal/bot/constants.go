package bot

import "time"

// Discord role IDs for the Steki guild.
const (
	StaffRoleID    = "886976948172124160"
	ModRoleID      = "887063786240999475"
	ModViewRoleID  = "887348241350402138" // can view register channels
	HMURoleID      = "886993717725102103"
	OutsiderRoleID = "993612908074381352"
	GoldenRoleID   = "943889393863774279"
	ExamRoleID     = "943886410782494771"  // shared photos of final exam papers
	GhostRoleID    = "1565626375941918850" // members silent for three months
)

// VerifiedRoleIDs are the roles that mean a member has completed registration.
var VerifiedRoleIDs = []string{HMURoleID, OutsiderRoleID}

// ProtectedRoleIDs are never kicked by the reaper regardless of idle time.
var ProtectedRoleIDs = []string{StaffRoleID, ModRoleID, ModViewRoleID, HMURoleID, OutsiderRoleID}

// Time budgets for one unit of work. Every database and Discord call made
// from a handler runs under one of these, so nothing can wedge indefinitely.
const (
	// DBTimeout covers a short exchange: a lookup, an audit write.
	DBTimeout = 10 * time.Second

	// FlowTimeout covers one registration step end to end, which may include
	// sending an email or creating a channel with retries.
	FlowTimeout = 45 * time.Second

	// ReapTimeout and ReconcileTimeout cover one background sweep.
	ReapTimeout      = 2 * time.Minute
	ReconcileTimeout = 5 * time.Minute
)

// RegisterChannelPrefix is how a member's registration channel is named.
const RegisterChannelPrefix = "register-"

// MsgGenericError is what a member is told when something failed on our side.
const MsgGenericError = "Κάτι πήγε στραβά. Δοκίμασε ξανά σε λίγο."
