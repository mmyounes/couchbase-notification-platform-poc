package main

import (
	"crypto/rand"
	"fmt"
	"math"
	mrand "math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// --- ULID ---------------------------------------------------------------------
// Unique, lexicographically sortable, time-ordered, with the timestamp
// recoverable from the prefix. The seeder uses the identical encoding - both
// must agree or seeded and live documents will not sort together.

const b32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func encodeTime(ms int64, n int) string {
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = b32[ms%32]
		ms /= 32
	}
	return string(out)
}

func ULID(ms int64) string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	out := make([]byte, 16)
	for i, b := range buf {
		out[i] = b32[int(b)%32]
	}
	return encodeTime(ms, 10) + string(out)
}

func UlidFloor(ms int64) string { return encodeTime(ms, 10) + strings.Repeat("0", 16) }
func UlidCeil(ms int64) string  { return encodeTime(ms, 10) + strings.Repeat("Z", 16) }

// --- time ---------------------------------------------------------------------

func isoMs(t time.Time) string  { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func nowMs() int64              { return time.Now().UnixMilli() }
func fromMs(ms int64) time.Time { return time.UnixMilli(ms) }

func minuteBucket(t time.Time) string { return t.UTC().Format("200601021504") }
func hourBucket(t time.Time) string   { return t.UTC().Format("2006010215") }

// --- keys ---------------------------------------------------------------------
// Defined once so no component can invent a different shape (spec section 5).

func keyEvent(tenant, eventID string) string { return "evt::" + tenant + "::" + eventID }
func keyNotification(tenant, eventID, ch string) string {
	return "ntf::" + tenant + "::" + eventID + "::" + ch
}
func keyTemplate(tenant, id string) string { return "tpl::" + tenant + "::" + id }
func keyPolicy() string                    { return "policy::global" }
func keyStat(tenant, name string) string   { return "stat::" + tenant + "::" + name }

// Dedup keys on tenant + user + event type + channel, NEVER on message content
// (spec D6): every password reset carries a different temp_password, so a
// content hash differs every time and flood control would never fire.
func keyDedup(tenant, user, evType, ch, minute string) string {
	return "dd::" + tenant + "::" + user + "::" + evType + "::" + ch + "::" + minute
}
func keyRateUserHour(tenant, user, hour string) string {
	return "rc::" + tenant + "::" + user + "::" + hour
}

// The per-channel rate counter is SHARDED.
//
// Unsharded it is a single document per channel per minute, and every
// notification on that channel increments it: ~14,000 writes/sec onto three
// documents at 10k events/sec. Couchbase serialises writes per document, so it
// becomes a hot key - measured as policy-stage latency rising from 1.02ms at
// 1,000 events/sec to 276ms at 10,000, which is superlinear and therefore
// contention rather than capacity.
//
// Sharding spreads those writes over ChannelRateShards documents. The cap is
// compared per shard against cap/shards: with uniform shard selection that is
// statistically equivalent and costs one increment, whereas summing every shard
// on each event would need N reads per check and be far worse than the problem.
const ChannelRateShards = 32

func keyRateChannelMin(tenant, ch, minute string, shard int) string {
	return "rc::" + tenant + "::ch::" + ch + "::" + minute + "::" + strconv.Itoa(shard)
}

func sortKeyOf(eventID, ch string) string { return eventID + "::" + ch }
func syncChannel(tenant, user string) string {
	return "tenant::" + tenant + "::user::" + user
}

// --- template rendering --------------------------------------------------------

var paramRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// A missing parameter is an error, not an empty string: shipping "Your
// temporary password is ." is worse than failing.
func render(tpl string, params map[string]any) (string, error) {
	var missing string
	out := paramRe.ReplaceAllStringFunc(tpl, func(m string) string {
		k := m[2 : len(m)-2]
		v, ok := params[k]
		if !ok {
			missing = k
			return m
		}
		return fmt.Sprint(v)
	})
	if missing != "" {
		return "", fmt.Errorf("template parameter %q not supplied", missing)
	}
	return out, nil
}

// --- retry ---------------------------------------------------------------------

type FailureClass int

const (
	Retryable FailureClass = iota
	Permanent
)

// Retrying a malformed request three times is a bug, not resilience.
func classify(status int) FailureClass {
	switch {
	case status == 0: // transport error / timeout
		return Retryable
	case status >= 500, status == 429:
		return Retryable
	case status >= 400:
		return Permanent
	}
	return Retryable
}

func backoffMs(p Policy, trials int) int64 {
	b := p.Retry.Backoff
	raw := math.Min(float64(b.InitialMs)*math.Pow(float64(b.Multiplier), math.Max(float64(trials-1), 0)),
		float64(b.MaxMs))
	if !b.Jitter {
		return int64(raw)
	}
	// Full jitter over [raw/2, raw]: spreads a synchronised burst of failures
	// instead of retrying them all in the same millisecond.
	return int64(raw/2 + mrand.Float64()*(raw/2))
}

type Outcome struct {
	Status          string
	Trials          int
	NextAttemptAtMs int64 // 0 = none
	LastError       string
}

// The Tracking Manager's decision, as a pure function of the delivery result.
// A failed attempt leaves status PENDING (spec D4).
func applyResult(p Policy, priorTrials int, ok bool, httpStatus int, errMsg string, now int64) Outcome {
	trials := priorTrials + 1
	if ok {
		return Outcome{Status: StatusDelivered, Trials: trials}
	}
	if errMsg == "" {
		if httpStatus == 0 {
			errMsg = "transport error"
		} else {
			errMsg = fmt.Sprintf("HTTP %d", httpStatus)
		}
	}
	if classify(httpStatus) == Permanent || trials >= p.Retry.MaxAttempts {
		return Outcome{Status: StatusFailed, Trials: trials, LastError: errMsg}
	}
	return Outcome{
		Status: StatusPending, Trials: trials,
		NextAttemptAtMs: now + backoffMs(p, trials), LastError: errMsg,
	}
}

func strp(s string) *string { return &s }

// variantFor picks the template variant to render for a channel.
//
// cblite is a delivery mechanism, not a new authoring surface, so templates are
// not required to define a variant for it - falling back keeps all eight
// existing templates working unchanged. Preference order is richest first:
// email carries a subject and full body, push a title and a short body, sms a
// body alone. A template that does define an explicit "cblite" variant wins.
func variantFor(tpl Template, ch string) (Variant, bool) {
	if v, ok := tpl.Variants[ch]; ok {
		return v, true
	}
	if ch != ChannelCBLite {
		return Variant{}, false
	}
	for _, fallback := range []string{"email", "push", "sms"} {
		if v, ok := tpl.Variants[fallback]; ok {
			return v, true
		}
	}
	return Variant{}, false
}
