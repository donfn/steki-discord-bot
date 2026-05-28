package registration

const (
	StepSendEmail   = "sendEmail"
	StepVerifyEmail = "verifyEmail"
)

type Registration struct {
	ID                        interface{} `bson:"_id,omitempty"`
	UserID                    string      `bson:"user"`
	Step                      string      `bson:"step"`
	Email                     string      `bson:"email,omitempty"`
	EncryptedVerificationCode string      `bson:"encryptedVerificationCode,omitempty"`
	FailedAttempts            int         `bson:"failedAttempts,omitempty"`
}
