package registration

import (
	"regexp"

	"steki-bot/internal/bot"
)

var (
	hmuEmailRegex = regexp.MustCompile(`^th[0-9]+@edu\.hmu\.gr$`)
	// tpEmailRegex is another HMU department: same university, but its
	// students join as Verified Outsiders.
	tpEmailRegex  = regexp.MustCompile(`^tp[0-9]+@edu\.hmu\.gr$`)
	csdEmailRegex = regexp.MustCompile(`^csd[0-9]+@csd\.uoc\.gr$`)
)

// classifyEmail maps a submitted address to the role it earns, reporting
// whether the address is acceptable at all.
func classifyEmail(email string) (roleID string, ok bool) {
	switch {
	case hmuEmailRegex.MatchString(email):
		return bot.HMURoleID, true
	case tpEmailRegex.MatchString(email), csdEmailRegex.MatchString(email):
		return bot.OutsiderRoleID, true
	default:
		return "", false
	}
}
