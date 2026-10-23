package store

import "time"

// Registration steps.
const (
	StepAwaitingEmail = "awaiting_email"
	StepAwaitingCode  = "awaiting_code"
	StepDone          = "done"
)

// Terminal outcomes for a registration row. A row with no outcome is "open".
const (
	OutcomeVerified        = "verified"
	OutcomeAbandonedKicked = "abandoned_kicked"
	OutcomeLeft            = "left"
	OutcomeRestarted       = "restarted"
	OutcomeModerator       = "moderator"
)

// Audit event kinds written to registration_events.
const (
	EventJoined              = "joined"
	EventChannelCreated      = "channel_created"
	EventChannelCreateFailed = "channel_create_failed"
	EventEmailSubmitted      = "email_submitted"
	EventEmailRejected       = "email_rejected"
	EventEmailInUse          = "email_in_use"
	EventCodeSent            = "code_sent"
	EventCodeSendFailed      = "code_send_failed"
	EventCodeWrong           = "code_wrong"
	EventCodeExpired         = "code_expired"
	EventVerified            = "verified"
	EventNudged              = "nudged"
	EventKicked              = "kicked"
	EventRestarted           = "restarted"
	EventLeft                = "left"
	EventModAction           = "mod_action"
)

// Provenance values for verified_emails.source.
const (
	SourceLive       = "live"
	SourceDiscordLog = "discord_log"
	SourceModerator  = "moderator"
)

// Member is a Discord user the bot has seen, tracked across leaves and rejoins.
type Member struct {
	UserID        string
	Username      string
	FirstSeenAt   time.Time
	LastJoinAt    time.Time
	JoinCount     int
	LeftAt        *time.Time
	VerifiedAt    *time.Time
	VerifiedEmail *string
	KickedCount   int
}

// Registration is one attempt by one member to verify.
type Registration struct {
	ID             string
	UserID         string
	GuildID        string
	ChannelID      *string
	CategoryID     *string
	Step           string
	Email          *string
	Code           *string
	CodeExpiresAt  *time.Time
	CodeSentCount  int
	FailedAttempts int
	LastActivityAt time.Time
	NudgedAt       *time.Time
	CreatedAt      time.Time
	CompletedAt    *time.Time
	Outcome        *string
}

// IdleFor reports how long the registration has been without user activity.
func (r *Registration) IdleFor(now time.Time) time.Duration {
	return now.Sub(r.LastActivityAt)
}

// CodeValid reports whether a code is present and not yet expired.
func (r *Registration) CodeValid(now time.Time) bool {
	return r.Code != nil && *r.Code != "" &&
		r.CodeExpiresAt != nil && now.Before(*r.CodeExpiresAt)
}

// VerifiedEmail is an address that has been used to verify an account.
type VerifiedEmail struct {
	Email      string
	UserID     *string
	Domain     string
	Source     string
	VerifiedAt time.Time
}

// Event is one entry in the audit trail.
type Event struct {
	ID        int64
	UserID    string
	RegID     *string
	Kind      string
	Detail    map[string]any
	ActorID   *string
	CreatedAt time.Time
}
