// Notification Platform - dataset seeder
//
// Generates notification instances matching spec section 5.2 exactly.
// Generation is a pure function of (--seed, event index): re-running with the
// same seed rewrites byte-identical documents, so seeding is idempotent.
//
//	ncgr-seed --count 500 --dry-run
//	ncgr-seed --count 500
//	ncgr-seed --count 500000000 --workers 64 --batch 512
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/gocb/v2"
)

type Delivery struct {
	Trials        int     `json:"trials"`
	LastAttemptAt *string `json:"last_attempt_at"`
	NextAttemptAt *string `json:"next_attempt_at"`
	LastError     *string `json:"last_error"`
	DeliveredAt   *string `json:"delivered_at"`
}

// Notification mirrors spec section 5.2 field-for-field.
type Notification struct {
	TenantID        string   `json:"tenant_id"`
	EventID         string   `json:"event_id"`
	UserID          string   `json:"user_id"`
	AppName         string   `json:"app_name"`
	Type            string   `json:"type"`
	Channel         string   `json:"channel"`
	Subject         string   `json:"subject"`
	Message         string   `json:"message"`
	TemplateID      string   `json:"template_id"`
	TemplateVersion int      `json:"template_version"`
	Timestamp       string   `json:"timestamp"`
	Status          string   `json:"status"`
	Seen            bool     `json:"seen"`
	Delivery        Delivery `json:"delivery"`
	SortKey         string   `json:"sort_key"`
	SyncChannels    []string `json:"sync_channels"`
}

type item struct {
	Key string
	Doc Notification
}

var cfg struct {
	host, user, pass   string
	bucket, scope      string
	tenant             string
	count              int
	seed               int
	days               int
	users              int
	workers, batch     int
	kvTimeoutMs        int
	maxRetries         int
	startEvent         int
	dryRun, skipConfig bool
	configOnly         bool
}

// Timestamps end slightly in the past so "latest" is stable during a demo.
var endMs = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC).UnixMilli()

func main() {
	flag.StringVar(&cfg.host, "host", env("CB_HOST", "127.0.0.1"), "Couchbase host")
	flag.StringVar(&cfg.user, "user", env("CB_USER", "Administrator"), "username")
	flag.StringVar(&cfg.pass, "pass", env("CB_PASS", "password"), "password")
	flag.StringVar(&cfg.bucket, "bucket", env("CB_BUCKET", "ncgr"), "bucket")
	flag.StringVar(&cfg.tenant, "tenant", env("TENANT_ID", "ncgr"),
		"tenant id written into every document and key")
	flag.StringVar(&cfg.scope, "scope", env("CB_SCOPE", "platform"), "scope")
	flag.IntVar(&cfg.count, "count", 500, "number of notification instances")
	flag.IntVar(&cfg.seed, "seed", 20260825, "PRNG seed (same seed => same documents)")
	flag.IntVar(&cfg.days, "days", 90, "spread timestamps over the last N days")
	flag.IntVar(&cfg.users, "users", 0, "user pool size (0 = derive from count)")
	flag.IntVar(&cfg.workers, "workers", 0, "upsert goroutines (0 = auto)")
	flag.IntVar(&cfg.batch, "batch", 256, "generator channel depth per worker")
	flag.IntVar(&cfg.kvTimeoutMs, "kv-timeout-ms", 10000, "KV operation timeout")
	flag.IntVar(&cfg.maxRetries, "max-retries", 5, "retries per document on transient failure")
	flag.IntVar(&cfg.startEvent, "start-event", 0, "resume: begin at this event index (see --count for how many to write)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print samples and distribution, write nothing")
	flag.BoolVar(&cfg.skipConfig, "skip-config", false, "do not write config collections")
	flag.BoolVar(&cfg.configOnly, "config-only", false, "write only config collections")
	flag.Parse()

	// catalog.go builds every document key and tenant_id from this. Assign it
	// before any generation runs.
	tenantID = cfg.tenant

	startMs := endMs - int64(cfg.days)*86400_000
	eventCount := int(float64(cfg.count)/avgChannels()) + 1
	userPool := cfg.users
	if userPool == 0 {
		userPool = max(50, min(100_000, cfg.count/4))
	}
	if cfg.workers == 0 {
		cfg.workers = min(64, max(8, runtime.NumCPU()*4))
	}

	z := newZipf(userPool, 1.07)
	gen := &generator{startMs: startMs, endMs: endMs, zipf: z, seed: uint32(cfg.seed)}

	if cfg.dryRun {
		dryRun(gen, eventCount, userPool, startMs)
		return
	}

	cluster, err := gocb.Connect("couchbase://"+cfg.host, gocb.ClusterOptions{
		Authenticator: gocb.PasswordAuthenticator{Username: cfg.user, Password: cfg.pass},
		TimeoutsConfig: gocb.TimeoutsConfig{
			KVTimeout:      time.Duration(cfg.kvTimeoutMs) * time.Millisecond,
			ConnectTimeout: 20 * time.Second,
		},
	})
	must(err)
	defer cluster.Close(nil)

	bucket := cluster.Bucket(cfg.bucket)
	must(bucket.WaitUntilReady(20*time.Second, nil))
	scope := bucket.Scope(cfg.scope)

	if !cfg.skipConfig {
		n := 0
		for _, d := range configDocs() {
			_, err := scope.Collection(d.Collection).Upsert(d.Key, d.Doc, nil)
			must(err)
			n++
		}
		fmt.Printf("config: %d documents written (tenant, policy, %d templates, %d event defs)\n",
			n, len(templates), len(templates))
	}
	if cfg.configOnly {
		return
	}

	coll := scope.Collection("notifications")
	fmt.Printf("seeding %d notifications (seed=%d, users=%d, window=%dd, workers=%d)\n",
		cfg.count, cfg.seed, userPool, cfg.days, cfg.workers)

	ch := make(chan item, cfg.workers*cfg.batch)
	var ok, failed, retried atomic.Int64
	var firstErr atomic.Value

	// One generator goroutine feeds many upsert workers. Generation is pure CPU
	// (~1us/doc) and far cheaper than the network round trip, so a single
	// producer keeps the workers saturated while preserving exact ordering and
	// an exact total count.
	go func() {
		defer close(ch)
		emitted := 0
		for e := cfg.startEvent; emitted < cfg.count; e++ {
			for _, it := range gen.forEvent(e) {
				if emitted >= cfg.count {
					return
				}
				ch <- it
				emitted++
			}
		}
	}()

	t0 := time.Now()
	done := make(chan struct{})
	go progress(&ok, &failed, t0, done, cfg.count)

	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				// Upserting identical content is idempotent, so a blind retry is
				// always safe - including after an ambiguous timeout, where the
				// first attempt may already have landed. Without this, a 1-in-500
				// transient failure becomes ~1M lost documents at 500M scale.
				var err error
				for attempt := 0; attempt <= cfg.maxRetries; attempt++ {
					if _, err = coll.Upsert(it.Key, it.Doc, nil); err == nil {
						break
					}
					retried.Add(1)
					time.Sleep(time.Duration(50<<attempt) * time.Millisecond)
				}
				if err != nil {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, err)
				} else {
					ok.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(done)

	secs := time.Since(t0).Seconds()
	fmt.Printf("\rdone: %d written, %d failed, %d retries in %.2fs (%.0f/sec)%s\n",
		ok.Load(), failed.Load(), retried.Load(), secs, float64(ok.Load())/secs, "          ")
	if e := firstErr.Load(); e != nil {
		fmt.Fprintln(os.Stderr, "first error:", e.(error).Error())
		os.Exit(1)
	}
}

func progress(ok, failed *atomic.Int64, t0 time.Time, done chan struct{}, total int) {
	tk := time.NewTicker(10 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			n := ok.Load()
			secs := time.Since(t0).Seconds()
			rate := float64(n) / secs
			pct := 100 * float64(n) / float64(total)
			eta := "-"
			if rate > 0 && n < int64(total) {
				eta = (time.Duration(float64(int64(total)-n)/rate) * time.Second).String()
			}
			fmt.Printf("[%s] %d/%d (%.2f%%) %.0f/sec failed=%d eta=%s\n",
				time.Now().UTC().Format("15:04:05"), n, total, pct, rate, failed.Load(), eta)
		}
	}
}

// --- generation -------------------------------------------------------------

type generator struct {
	startMs, endMs int64
	zipf           *zipfSampler
	seed           uint32
}

// forEvent returns all notifications for one event index: one per channel on
// the event definition, sharing an event_id and differing by channel
// (spec section 5.2). Deterministic in (seed, e).
func (g *generator) forEvent(e int) []item {
	r := newRng(seedFor(g.seed, e))

	tpl := weightedTemplate(r)
	app := r.pick(apps)
	rank := g.zipf.rank(r)
	// Deliberately not user1 / user2 - reserved for mobile sync tests.
	userID := fmt.Sprintf("usr_%06d", rank)

	ts := g.startMs + int64(r.next()*float64(g.endMs-g.startMs))
	eventID := ulidAt(ts, r)
	params := tpl.Make(r, app)

	out := make([]item, 0, len(tpl.Channels))
	for _, channel := range tpl.Channels {
		v, present := tpl.Variants[channel]
		if !present {
			panic(fmt.Sprintf("%s has no %s variant", tpl.TemplateID, channel))
		}

		// Delivery outcome: 85% delivered / 10% failed / 5% pending, with a
		// slice of delivered ones having needed retries so trials > 0 is visible.
		var status string
		var trials int
		var lastErr *string
		roll := r.next()
		switch {
		case roll < 0.85:
			status = "DELIVERED"
			rr := r.next()
			trials = 1
			if rr >= 0.85 {
				trials = 2
			}
			if rr >= 0.97 {
				trials = 3
			}
		case roll < 0.95:
			status = "FAILED"
			trials = policy.Retry.MaxAttempts
			e := r.pick([]string{"503 upstream unavailable", "504 gateway timeout",
				"connection reset by peer", "400 invalid recipient address", "502 bad gateway"})
			lastErr = &e
		default:
			status = "PENDING"
			trials = 0
			if r.next() >= 0.8 {
				trials = 1 + r.intn(2)
			}
		}

		// Attempt timings follow the backoff curve so trials and timestamps agree.
		b := policy.Retry.Backoff
		elapsed := int64(120 + r.intn(800))
		for i := 1; i < max(trials, 1); i++ {
			step := float64(b.InitialMs) * powInt(b.Multiplier, i-1)
			elapsed += int64(min(step, float64(b.MaxMs)))
		}

		var lastAttempt, deliveredAt, nextAttempt *string
		if trials > 0 {
			s := isoMilli(ts + elapsed)
			lastAttempt = &s
			if status == "DELIVERED" {
				deliveredAt = &s
			}
			if status == "PENDING" {
				step := min(float64(b.InitialMs)*powInt(b.Multiplier, trials-1), float64(b.MaxMs))
				n := isoMilli(ts + elapsed + int64(step))
				nextAttempt = &n
			}
		}

		subject := v.Subject
		if subject == "" {
			subject = v.Title
		}
		subjectOut := ""
		if subject != "" {
			s, err := render(subject, params)
			must(err)
			subjectOut = s
		}
		body, err := render(v.Body, params)
		must(err)

		seen := false
		if status == "DELIVERED" {
			seen = r.next() < 0.45
		}

		out = append(out, item{
			Key: fmt.Sprintf("ntf::%s::%s::%s", tenantID, eventID, channel),
			Doc: Notification{
				TenantID: tenantID, EventID: eventID, UserID: userID, AppName: app,
				Type: tpl.TemplateID, Channel: channel,
				Subject: subjectOut, Message: body,
				TemplateID: tpl.TemplateID, TemplateVersion: tpl.Version,
				// Millisecond precision, matching the ULID exactly. Second-level
				// truncation would put timestamp and event_id out of step and
				// make sort_key range scans drop boundary documents.
				Timestamp: isoMilli(ts), Status: status,
				// Written only by the mobile app via Sync Gateway; the pipeline
				// never touches it. Seeded so deferred sync work has real state.
				Seen: seen,
				Delivery: Delivery{Trials: trials, LastAttemptAt: lastAttempt,
					NextAttemptAt: nextAttempt, LastError: lastErr, DeliveredAt: deliveredAt},
				SortKey:      fmt.Sprintf("%s::%s", eventID, channel),
				SyncChannels: []string{fmt.Sprintf("tenant::%s::user::%s", tenantID, userID)},
			},
		})
	}
	return out
}

func dryRun(g *generator, eventCount, userPool int, startMs int64) {
	fmt.Printf("DRY RUN - no writes\n\n")
	fmt.Printf("events needed : %d (avg %.2f channels/event)\n", eventCount, avgChannels())
	fmt.Printf("user pool     : %d (usr_000001 .. usr_%06d)\n", userPool, userPool)
	fmt.Printf("time window   : %s .. %s\n\n", isoSec(startMs), isoSec(g.endMs))

	shown := 0
	for e := 0; shown < 2; e++ {
		for _, it := range g.forEvent(e) {
			if shown >= 2 {
				break
			}
			b, _ := json.MarshalIndent(it.Doc, "", "  ")
			fmt.Printf("--- %s\n%s\n", it.Key, b)
			shown++
		}
	}

	dist := map[string]int{}
	emitted := 0
	// Verify the ULID<->timestamp invariant across the whole sample: the
	// embedded ULID time must decode back to the document's timestamp.
	badUlid := 0
	for e := 0; emitted < cfg.count; e++ {
		for _, it := range g.forEvent(e) {
			if emitted >= cfg.count {
				break
			}
			dist[it.Doc.Status]++
			dist["type:"+it.Doc.Type]++
			dist["ch:"+it.Doc.Channel]++
			if decodeUlidTime(it.Doc.EventID) != parseIso(it.Doc.Timestamp) {
				badUlid++
			}
			emitted++
		}
	}
	fmt.Printf("\ndistribution over %d notifications:\n", cfg.count)
	for _, k := range sortedKeys(dist) {
		fmt.Printf("  %-28s %d\n", k, dist[k])
	}
	fmt.Printf("\nULID/timestamp invariant: %d mismatches out of %d\n", badUlid, emitted)
}
