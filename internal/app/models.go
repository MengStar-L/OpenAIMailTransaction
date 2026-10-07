package app

import (
	"openai-mail-transaction/internal/provider"
	"time"
)

type Settings struct {
	Brand           string `json:"brand"`
	PhoneEnabled    bool   `json:"phone_enabled"`
	EmailEnabled    bool   `json:"email_enabled"`
	PhoneService    string `json:"phone_service"`
	EmailService    string `json:"email_service"`
	PhoneCountry    string `json:"phone_country"`
	EmailDomain     string `json:"email_domain"`
	PhoneMaxPrice   string `json:"phone_max_price"`
	EmailMaxPrice   string `json:"email_max_price"`
	PhoneTTLMinutes int    `json:"phone_ttl_minutes"`
	EmailTTLMinutes int    `json:"email_ttl_minutes"`
	BackgroundType  string `json:"background_type"`
	BackgroundURL   string `json:"background_url"`
}

func defaultSettings() Settings {
	return Settings{Brand: "拾光", PhoneEnabled: true, EmailEnabled: true, PhoneService: "dr", EmailService: "dr", EmailDomain: "gmail.com", PhoneTTLMinutes: 20, EmailTTLMinutes: 25, BackgroundType: "none"}
}

type CDK struct {
	ID          string    `json:"id"`
	BatchID     string    `json:"batch_id"`
	MaskedCode  string    `json:"masked_code"`
	Code        string    `json:"code,omitempty"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status"`
	Note        string    `json:"note"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	UsageLimit  int       `json:"usage_limit"`
	UsedCount   int       `json:"used_count"`
	CodeCipher  string    `json:"-"`
	Hash        string    `json:"-"`
	Snapshot    Settings  `json:"-"`
}

type ReceivedCode struct {
	Round      int       `json:"round"`
	Code       string    `json:"code"`
	ReceivedAt time.Time `json:"received_at"`
}

type Order struct {
	PhoneChannel       *provider.PhoneChannel `json:"phone_channel,omitempty"`
	ID                 string                 `json:"id"`
	Source             string                 `json:"source"`
	RequestID          string                 `json:"request_id,omitempty"`
	Kind               string                 `json:"kind"`
	Status             string                 `json:"status"`
	QueueExpiresAt     *time.Time             `json:"queue_expires_at,omitempty"`
	QueueNextAttemptAt *time.Time             `json:"queue_next_attempt_at,omitempty"`
	QueueAttempts      int                    `json:"queue_attempts,omitempty"`
	QueueMinutes       int                    `json:"queue_minutes,omitempty"`
	Resource           string                 `json:"resource"`
	Code               string                 `json:"code"`
	Codes              []ReceivedCode         `json:"codes"`
	MailRound          int                    `json:"mail_round"`
	CreatedAt          time.Time              `json:"created_at"`
	ExpiresAt          time.Time              `json:"expires_at"`
	CancelAfter        time.Time              `json:"cancel_after"`
	CanRetry           bool                   `json:"can_retry"`
	CanNextCode        bool                   `json:"can_next_code"`
	AutoNextState      string                 `json:"auto_next_state,omitempty"`
	AutoNextAt         *time.Time             `json:"auto_next_at,omitempty"`
	UsageLimit         int                    `json:"usage_limit"`
	UsedCount          int                    `json:"used_count"`
	Attempt            int                    `json:"attempt"`
	MaxAttempts        int                    `json:"max_attempts"`
	Message            string                 `json:"message"`
	CDKID              string                 `json:"-"`
	ProviderID         string                 `json:"-"`
	CancelReason       string                 `json:"-"`
	LastPoll           time.Time              `json:"-"`
	UsageCounted       bool                   `json:"-"`
	RoundWaitSeen      bool                   `json:"-"`
}

type AdminOrder struct {
	Order
	MaskedCode string `json:"masked_code"`
	CDKCode    string `json:"cdk_code,omitempty"`
	ProviderID string `json:"provider_id"`
	Note       string `json:"note"`
}
