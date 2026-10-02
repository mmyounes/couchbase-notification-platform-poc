package main

// Document shapes. Mirrors spec section 5 and the TypeScript definitions in
// packages/shared, which the UI still imports - the two must agree.

type Channel = string

// ChannelCBLite is delivered by storing the notification and letting Sync
// Gateway replicate it to the device. There is no external provider to call, so
// it has no retries, no backoff and no forced-failure mode.
const ChannelCBLite = "cblite"

// AllChannels is the set of PROVIDER channels - the ones that make an outbound
// call to mock-channels. Deliberately excludes cblite: this is what the load
// generator fans out over, and bulk load must stay exactly as it was.
var AllChannels = []Channel{"email", "sms", "push"}

// Status vocabulary is exactly the requirements' (spec D4): a failed attempt leaves
// PENDING and increments trials; only exhausting max_attempts sets FAILED.
// There is deliberately no RETRYING - "retried" is delivery.trials > 0.
const (
	StatusPending   = "PENDING"
	StatusDelivered = "DELIVERED"
	StatusFailed    = "FAILED"
)

type DeliveryState struct {
	Trials        int     `json:"trials"`
	LastAttemptAt *string `json:"last_attempt_at"`
	NextAttemptAt *string `json:"next_attempt_at"`
	LastError     *string `json:"last_error"`
	DeliveredAt   *string `json:"delivered_at"`
}

type Notification struct {
	TenantID        string  `json:"tenant_id"`
	EventID         string  `json:"event_id"`
	UserID          string  `json:"user_id"`
	AppName         string  `json:"app_name"`
	Type            string  `json:"type"`
	Channel         Channel `json:"channel"`
	Subject         string  `json:"subject"`
	Message         string  `json:"message"`
	TemplateID      string  `json:"template_id"`
	TemplateVersion int     `json:"template_version"`
	// Millisecond precision, exactly equal to the ULID's embedded time. Second
	// truncation would make sort_key range scans drop boundary documents.
	Timestamp string `json:"timestamp"`
	Status    string `json:"status"`
	// Written ONLY by the mobile app via Sync Gateway. No server-side component
	// may ever write this field.
	Seen         bool          `json:"seen"`
	Delivery     DeliveryState `json:"delivery"`
	SortKey      string        `json:"sort_key"`
	SyncChannels []string      `json:"sync_channels"`
	// Test affordances, persisted so a forced failure survives into retries.
	TestForceFail      bool `json:"test_force_fail,omitempty"`
	TestForcePermanent bool `json:"test_force_permanent,omitempty"`
}

type Event struct {
	EventID           string             `json:"event_id"`
	TenantID          string             `json:"tenant_id"`
	UserID            string             `json:"user_id"`
	AppName           string             `json:"app_name"`
	Type              string             `json:"type"`
	TemplateID        string             `json:"template_id"`
	Params            map[string]any     `json:"params"`
	SubmittedAt       string             `json:"submitted_at"`
	Source            string             `json:"source"`
	ChannelsRequested []Channel          `json:"channels_requested"`
	ChannelsAccepted  []Channel          `json:"channels_accepted"`
	Suppressed        map[Channel]string `json:"suppressed,omitempty"`
}

type Variant struct {
	Subject string `json:"subject,omitempty"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body"`
}

type Template struct {
	TemplateID string             `json:"template_id"`
	TenantID   string             `json:"tenant_id"`
	Version    int                `json:"version"`
	Variants   map[string]Variant `json:"variants"`
	Params     []string           `json:"params"`
}

// Tenant is the demo's tenancy record. The pipeline binds to exactly one
// tenant at startup (cfg.TenantID); this type exists so the UI can show what
// that tenant actually is rather than a hardcoded string.
type Tenant struct {
	TenantID        string    `json:"tenant_id"`
	Name            string    `json:"name"`
	ChannelsEnabled []Channel `json:"channels_enabled"`
	CreatedAt       string    `json:"created_at"`
}

type EventDef struct {
	TenantID   string    `json:"tenant_id"`
	EventType  string    `json:"event_type"`
	Channels   []Channel `json:"channels"`
	TemplateID string    `json:"template_id"`
	Priority   string    `json:"priority"`
}

type Backoff struct {
	InitialMs  int  `json:"initial_ms"`
	Multiplier int  `json:"multiplier"`
	MaxMs      int  `json:"max_ms"`
	Jitter     bool `json:"jitter"`
}

type Policy struct {
	Retry struct {
		MaxAttempts int     `json:"max_attempts"`
		Backoff     Backoff `json:"backoff"`
	} `json:"retry"`
	Throttle struct {
		MaxIdenticalPerUserPerMinute int `json:"max_identical_per_user_per_minute"`
		MaxPerUserPerHour            int `json:"max_per_user_per_hour"`
		MaxPerChannelPerMinute       int `json:"max_per_channel_per_minute"`
	} `json:"throttle"`
}

// --- API contracts ------------------------------------------------------------

type SubmitRequest struct {
	UserID    string           `json:"user_id"`
	AppName   string           `json:"app_name"`
	Type      string           `json:"type"`
	Params    map[string]any   `json:"params"`
	Channels  []Channel        `json:"channels"`
	ForceFail map[Channel]bool `json:"force_fail"`
	Source    string           `json:"source"`
	Traced    *bool            `json:"traced"`
}

type SubmitResult struct {
	EventID          string             `json:"event_id"`
	ChannelsAccepted []Channel          `json:"channels_accepted"`
	Suppressed       map[Channel]string `json:"suppressed"`
}

type StatCounters struct {
	Submitted  int64   `json:"submitted"`
	Delivered  int64   `json:"delivered"`
	Failed     int64   `json:"failed"`
	Retried    int64   `json:"retried"`
	Suppressed int64   `json:"suppressed"`
	Pending    int64   `json:"pending"`
	Since      *string `json:"since"`
}

type DatasetTotals struct {
	Delivered int64  `json:"delivered"`
	Failed    int64  `json:"failed"`
	Pending   int64  `json:"pending"`
	Total     int64  `json:"total"`
	AsOf      string `json:"asOf"`
}

type TraceEntry struct {
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Channel string `json:"channel,omitempty"`
	Detail  string `json:"detail"`
}
