package main

import "fmt"

// Configuration catalogue: tenant, policy, templates and event definitions.
// This is both what gets written to the config collections and what the
// notification generator draws from, so seeded notifications always reference
// templates that actually exist.

// Set once from --tenant before any generation runs; see main(). A var rather
// than a const so the tenant is configurable, and package-level rather than
// threaded through every function so the many key-building call sites below
// stay readable.
var tenantID = "ncgr"

var apps = []string{
	"ncgrdemo", "portal", "hr_self_service",
	"procurement", "payroll", "licensing",
}

var allChannels = []string{"email", "sms", "push"}

// --- policy (spec section 5.5) ----------------------------------------------

type Backoff struct {
	InitialMs  int  `json:"initial_ms"`
	Multiplier int  `json:"multiplier"`
	MaxMs      int  `json:"max_ms"`
	Jitter     bool `json:"jitter"`
}

type RetryPolicy struct {
	MaxAttempts int     `json:"max_attempts"`
	Backoff     Backoff `json:"backoff"`
}

type ThrottlePolicy struct {
	MaxIdenticalPerUserPerMinute int `json:"max_identical_per_user_per_minute"`
	MaxPerUserPerHour            int `json:"max_per_user_per_hour"`
	MaxPerChannelPerMinute       int `json:"max_per_channel_per_minute"`
}

type Policy struct {
	Retry    RetryPolicy    `json:"retry"`
	Throttle ThrottlePolicy `json:"throttle"`
}

var policy = Policy{
	Retry: RetryPolicy{
		MaxAttempts: 3,
		Backoff:     Backoff{InitialMs: 1000, Multiplier: 2, MaxMs: 30000, Jitter: true},
	},
	Throttle: ThrottlePolicy{
		MaxIdenticalPerUserPerMinute: 10,
		MaxPerUserPerHour:            100,
		MaxPerChannelPerMinute:       500000,
	},
}

// --- templates ---------------------------------------------------------------

type Variant struct {
	Subject string `json:"subject,omitempty"`
	Title   string `json:"title,omitempty"`
	Body    string `json:"body"`
}

type Template struct {
	TemplateID string             `json:"template_id"`
	Version    int                `json:"version"`
	Weight     int                `json:"-"`
	Channels   []string           `json:"-"`
	Priority   string             `json:"-"`
	Params     []string           `json:"params"`
	Variants   map[string]Variant `json:"variants"`
	Make       func(r *rng, app string) map[string]string `json:"-"`
}

func dateIn2026(r *rng) string {
	return fmt.Sprintf("2026-%02d-%02d", 1+r.intn(12), 1+r.intn(28))
}

func money(r *rng, maxMinor, minMajor int) string {
	return fmt.Sprintf("%.2f", float64(r.intn(maxMinor))/100.0+float64(minMajor))
}

// Eight distinct types with genuinely different vocabulary. If every document
// carried near-identical text, a full-text search would match all 500M and the
// search demo would prove nothing (spec section 9.2).
var templates = []Template{
	{
		TemplateID: "reset_password", Version: 3, Weight: 22, Priority: "critical",
		Channels: []string{"email", "sms", "push"},
		Params:   []string{"app_name", "temp_password", "expiry_minutes"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"app_name":       app,
				"temp_password":  randToken(r, 8),
				"expiry_minutes": fmt.Sprint(r.pickInt([]int{10, 15, 20, 30})),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Password reset for {{app_name}}",
				Body: "Your temporary password for {{app_name}} is {{temp_password}}. It expires in {{expiry_minutes}} minutes. If you did not request this reset, contact your administrator immediately."},
			"sms":  {Body: "{{app_name}} temporary password: {{temp_password}} (valid {{expiry_minutes}}m)"},
			"push": {Title: "Password reset", Body: "Your temporary password for {{app_name}} is ready. Open the app to view it."},
		},
	},
	{
		TemplateID: "otp_login", Version: 2, Weight: 20, Priority: "critical",
		Channels: []string{"sms", "push"},
		Params:   []string{"app_name", "otp_code", "expiry_minutes"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"app_name":       app,
				"otp_code":       randDigits(r, 6),
				"expiry_minutes": fmt.Sprint(r.pickInt([]int{3, 5, 10})),
			}
		},
		Variants: map[string]Variant{
			"sms":  {Body: "Your {{app_name}} verification code is {{otp_code}}. Do not share it with anyone. Valid for {{expiry_minutes}} minutes."},
			"push": {Title: "Verification code", Body: "A sign-in verification code was requested for {{app_name}}."},
		},
	},
	{
		TemplateID: "invoice_ready", Version: 1, Weight: 14, Priority: "normal",
		Channels: []string{"email", "push"},
		Params:   []string{"app_name", "invoice_no", "amount", "due_date"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"app_name":   app,
				"invoice_no": "INV-" + randDigits(r, 6),
				"amount":     money(r, 900000, 50),
				"due_date":   dateIn2026(r),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Invoice {{invoice_no}} is ready",
				Body: "Invoice {{invoice_no}} for {{amount}} SAR is now available in {{app_name}}. Payment is due by {{due_date}}. You can download the invoice from the billing section of your account."},
			"push": {Title: "Invoice available", Body: "Invoice {{invoice_no}} for {{amount}} SAR is ready to view."},
		},
	},
	{
		TemplateID: "payment_received", Version: 1, Weight: 12, Priority: "normal",
		Channels: []string{"email", "sms"},
		Params:   []string{"app_name", "amount", "reference", "method"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"app_name":  app,
				"amount":    money(r, 500000, 25),
				"reference": "PAY-" + randToken(r, 10),
				"method":    r.pick([]string{"SADAD", "Mada card", "bank transfer", "Apple Pay"}),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Payment received - {{reference}}",
				Body: "We have received your payment of {{amount}} SAR by {{method}}. Your reference number is {{reference}}. This receipt confirms settlement against your {{app_name}} account."},
			"sms": {Body: "Payment of {{amount}} SAR received via {{method}}. Ref {{reference}}."},
		},
	},
	{
		TemplateID: "appointment_reminder", Version: 4, Weight: 10, Priority: "normal",
		Channels: []string{"email", "sms", "push"},
		Params:   []string{"service", "branch", "appointment_time", "ticket"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"service": r.pick([]string{"passport renewal", "commercial registration",
					"vehicle inspection", "residency permit", "municipal licensing"}),
				"branch": r.pick([]string{"Riyadh Olaya", "Jeddah Corniche",
					"Dammam Central", "Makkah Aziziyah", "Abha Central"}),
				"appointment_time": fmt.Sprintf("%02d:%s", 8+r.intn(9), r.pick([]string{"00", "15", "30", "45"})),
				"ticket":           "TKT-" + randDigits(r, 5),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Reminder: your {{service}} appointment",
				Body: "This is a reminder of your {{service}} appointment at {{branch}} at {{appointment_time}}. Please arrive fifteen minutes early and bring your national ID. Your ticket number is {{ticket}}."},
			"sms":  {Body: "Reminder: {{service}} at {{branch}}, {{appointment_time}}. Ticket {{ticket}}."},
			"push": {Title: "Appointment reminder", Body: "Your {{service}} appointment at {{branch}} is at {{appointment_time}}."},
		},
	},
	{
		TemplateID: "document_expiry", Version: 2, Weight: 9, Priority: "high",
		Channels: []string{"email", "push"},
		Params:   []string{"document", "expiry_date", "days_left"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"document": r.pick([]string{"commercial registration", "municipal licence",
					"vehicle registration", "professional accreditation", "import permit"}),
				"expiry_date": dateIn2026(r),
				"days_left":   fmt.Sprint(r.pickInt([]int{7, 14, 30, 60, 90})),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Your {{document}} expires in {{days_left}} days",
				Body: "Your {{document}} is due to expire on {{expiry_date}}, which is {{days_left}} days from now. Renew before the expiry date to avoid interruption of service or administrative penalties."},
			"push": {Title: "Renewal required", Body: "Your {{document}} expires on {{expiry_date}}."},
		},
	},
	{
		TemplateID: "service_request_update", Version: 1, Weight: 8, Priority: "normal",
		Channels: []string{"email", "push"},
		Params:   []string{"request_no", "new_status", "agent", "app_name"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"request_no": "SR-" + randDigits(r, 7),
				"new_status": r.pick([]string{"under review", "approved",
					"awaiting documents", "escalated", "closed"}),
				"agent": r.pick([]string{"Support Desk", "Licensing Unit",
					"Finance Office", "Technical Services"}),
				"app_name": app,
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Service request {{request_no}} is now {{new_status}}",
				Body: "The status of your service request {{request_no}} has changed to {{new_status}}. It is currently being handled by the {{agent}}. You can review the full history in {{app_name}}."},
			"push": {Title: "Request updated", Body: "Service request {{request_no}} is now {{new_status}}."},
		},
	},
	{
		TemplateID: "account_alert", Version: 5, Weight: 5, Priority: "critical",
		Channels: []string{"email", "sms", "push"},
		Params:   []string{"app_name", "activity", "city", "occurred_at"},
		Make: func(r *rng, app string) map[string]string {
			return map[string]string{
				"app_name": app,
				"activity": r.pick([]string{"a sign-in from a new device",
					"a change to your registered email", "three failed sign-in attempts",
					"a new API key issued"}),
				"city":        r.pick([]string{"Riyadh", "Jeddah", "Dammam", "Tabuk", "Abha", "Madinah"}),
				"occurred_at": fmt.Sprintf("%02d:%02d", r.intn(24), r.intn(60)),
			}
		},
		Variants: map[string]Variant{
			"email": {Subject: "Security alert on your {{app_name}} account",
				Body: "We detected {{activity}} on your {{app_name}} account at {{occurred_at}} from {{city}}. If this was you, no action is needed. If you do not recognise this activity, reset your password immediately."},
			"sms":  {Body: "Security alert: {{activity}} on {{app_name}} from {{city}} at {{occurred_at}}."},
			"push": {Title: "Security alert", Body: "Unusual activity was detected on your {{app_name}} account."},
		},
	},
}

// weightedTemplate picks a template by weight.
func weightedTemplate(r *rng) *Template {
	total := 0
	for i := range templates {
		total += templates[i].Weight
	}
	v := r.next() * float64(total)
	for i := range templates {
		v -= float64(templates[i].Weight)
		if v <= 0 {
			return &templates[i]
		}
	}
	return &templates[len(templates)-1]
}

// avgChannels is used to estimate how many events yield N notifications.
func avgChannels() float64 {
	tw, cw := 0, 0
	for _, t := range templates {
		tw += t.Weight
		cw += t.Weight * len(t.Channels)
	}
	return float64(cw) / float64(tw)
}

// --- documents written to the config collections -----------------------------

type kv struct {
	Collection string
	Key        string
	Doc        any
}

func configDocs() []kv {
	out := []kv{
		{"tenants", "tnt::" + tenantID, map[string]any{
			"tenant_id":        tenantID,
			"name":             "National Center for Government Resources",
			"channels_enabled": allChannels,
			"created_at":       "2026-08-25T00:00:00Z",
		}},
		{"policies", "policy::global", policy},
	}
	for i := range templates {
		t := templates[i]
		out = append(out, kv{"templates", fmt.Sprintf("tpl::%s::%s", tenantID, t.TemplateID),
			map[string]any{
				"template_id": t.TemplateID,
				"tenant_id":   tenantID,
				"version":     t.Version,
				"variants":    t.Variants,
				"params":      t.Params,
			}})
		out = append(out, kv{"event_defs", fmt.Sprintf("evd::%s::%s", tenantID, t.TemplateID),
			map[string]any{
				"tenant_id":   tenantID,
				"event_type":  t.TemplateID,
				"channels":    t.Channels,
				"template_id": t.TemplateID,
				"priority":    t.Priority,
			}})
	}
	return out
}
