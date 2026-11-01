package moderation

import "time"

// phishRefreshEvery is how often the phishing domain feeds are re-downloaded.
const phishRefreshEvery = 6 * time.Hour

// A join rate above joinsPerWindow per joinWindow alerts the moderators; it
// does not block, so a genuine rush of students is never turned away.
const (
	joinsPerWindow = 40
	joinWindow     = 10 * time.Minute
)
