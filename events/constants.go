package events

// Discord role IDs
const (
	staffRoleID    = "886976948172124160"
	modRoleID      = "887063786240999475"
	modViewRoleID  = "887348241350402138" // can view register channels
	hmuRoleID      = "886993717725102103"
	csdRoleID      = "993612908074381352"
)

// Reaction → role mapping for the interest selection step
var reactionRoles = map[string]string{
	"🖥️": "886993717725102103",
	"🤖": "887198468421083136",
	"⚙️": "887198536360419348",
	"👪": "887198604027101284",
	"🌱": "887198628899356683",
	"💉": "887198656837591041",
	"🎶": "887200015825641542",
	"💼": "887198689372815410",
	"✈️": "887198713628483664",
	"🍎": "887198734209925141",
	"💸": "887198782255661086",
}
