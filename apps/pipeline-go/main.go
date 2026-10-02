package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const pageSize = 100

var (
	cfg      Config
	db       *DB
	metrics  *Metrics
	trace    *TraceStore
	bus      *Bus
	counters *Counters
	catalog  *Catalog
	srch     *Search
	proc     *Processor
	sched    *Scheduler
	loadgen  *LoadGen
)

func main() {
	cfg = LoadConfig()
	log.Printf("starting: %d vCPU visible, delivery workers=%d, tenant=%s",
		runtime.NumCPU(), cfg.DeliveryWorkers, cfg.TenantID)

	var err error
	if db, err = OpenDB(cfg); err != nil {
		log.Fatalf("couchbase: %v", err)
	}
	defer db.Close()

	metrics = NewMetrics()
	trace = NewTraceStore()
	bus = NewBus(50_000)
	counters = NewCounters(db.Coll("counters"), cfg.TenantID, envInt("STATS_FLUSH_MS", 1000))
	srch = NewSearch(cfg.FTSURL, cfg.FTSIndex, cfg.CBUser, cfg.CBPass, cfg.TenantID)

	catalog = NewCatalog(db, cfg.TenantID, cfg.PolicyRefreshMs)
	if err := catalog.Start(); err != nil {
		// Not fatal: the refresh loop is running and will fill the catalog once
		// the query service answers. Say what is actually empty, though - the
		// old wording mentioned only the policy, which hid the real symptom
		// (bulk load rejecting every request for want of event definitions).
		log.Printf("catalog: initial load failed (%v) - policy, templates and event "+
			"definitions are all EMPTY until a refresh succeeds; retrying every %dms. "+
			"Bulk load will report \"no event definitions loaded\" until then",
			err, cfg.PolicyRefreshMs)
	}

	proc = &Processor{db: db, tenant: cfg.TenantID, catalog: catalog, counters: counters,
		bus: bus, metrics: metrics, trace: trace, graceMs: int64(cfg.StaleAfterMs)}
	tracking := &Tracking{db: db, catalog: catalog, counters: counters, bus: bus,
		metrics: metrics, trace: trace, workers: cfg.DeliveryWorkers}
	adapter := NewAdapter(cfg.MockChannelsURL, bus, metrics, trace, cfg.DeliveryWorkers)
	sched = NewScheduler(db, bus, metrics, cfg.RetryTickMs, cfg.RetryBatch, cfg.RetryHorizonMs)
	loadgen = NewLoadGen(proc, catalog)

	// Results subscribed before deliveries, so no result is produced before
	// something is consuming them.
	tracking.Start()
	adapter.Start()
	// One process means exactly one scheduler by construction - no singleton
	// gating, no env flag, no risk of N replicas delivering the same rows.
	sched.Start()

	mux := http.NewServeMux()
	routes(mux, tracking)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      cors(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 150 * time.Second,
	}

	go func() {
		log.Printf("pipeline listening on :%d, fts=%s", cfg.Port, cfg.FTSURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	loadgen.Stop()
	sched.Stop()
	counters.Close() // flush any remaining deltas
	catalog.Stop()
	_ = srv.Close()
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// --- cursor ------------------------------------------------------------------
// Both query paths sort on the same value, so one opaque cursor serves both and
// the UI has a single pagination component.

func encodeCursor(path, after string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(path + ":" + after))
}

func decodeCursor(s string) (path, after string, ok bool) {
	if s == "" {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", "", false
	}
	i := strings.Index(string(raw), ":")
	if i < 0 {
		return "", "", false
	}
	p, a := string(raw[:i]), string(raw[i+1:])
	if (p != "gsi" && p != "fts") || a == "" {
		return "", "", false
	}
	return p, a, true
}

func routes(mux *http.ServeMux, tracking *Tracking) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		chOK, chDetail := checkChannels(cfg.MockChannelsURL)
		nt, nd, refreshed, cerr := catalog.Stats()
		writeJSON(w, 200, map[string]any{
			"ok": true, "tenant": cfg.TenantID, "runtime": "go", "cpus": runtime.NumCPU(),
			"goroutines": runtime.NumGoroutine(),
			"catalog": map[string]any{"templates": nt, "eventDefs": nd,
				"refreshedAt": refreshed, "error": cerr},
			"channels":          map[string]any{"ok": chOK, "detail": chDetail},
			"busDepth":          bus.Depth(),
			"scheduler":         sched.Stats(),
			"trackingFailures":  tracking.Failures(),
			"pendingStatDeltas": counters.PendingDeltas(),
		})
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"stages": metrics.Snapshot(),
			"throughput": map[string]any{
				"eventsPerSec":        metrics.Events.PerSec(),
				"notificationsPerSec": metrics.Notifications.PerSec(),
				"deliveriesPerSec":    metrics.Deliveries.PerSec(),
				"eventsSeries":        metrics.Events.Series(),
				"notificationsSeries": metrics.Notifications.Series(),
			},
			"busDepth":  bus.Depth(),
			"scheduler": sched.Stats(),
		})
	})

	// One process, one histogram registry: a reset is immediate and complete.
	// No marker document, no per-replica polling, no first-reset-swallowed bug.
	mux.HandleFunc("POST /metrics/reset", func(w http.ResponseWriter, r *http.Request) {
		metrics.Reset()
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, counters.Read())
	})
	mux.HandleFunc("POST /stats/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := counters.Reset(); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// Whole-collection totals, distinct from the session counters. Each status
	// is an FTS count over 500M (~700ms), so the result is cached.
	var dsMu sync.Mutex
	var dsCache *DatasetTotals
	var dsAt int64
	mux.HandleFunc("GET /stats/dataset", func(w http.ResponseWriter, r *http.Request) {
		dsMu.Lock()
		if dsCache != nil && nowMs()-dsAt < 30_000 {
			v := *dsCache
			dsMu.Unlock()
			writeJSON(w, 200, v)
			return
		}
		dsMu.Unlock()

		count := func(status string) int64 {
			p, _, err := srch.Query(SearchFilters{Status: status}, "", 0)
			if err != nil {
				return -1
			}
			return p.TotalHits
		}
		var d, f, pd int64
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); d = count(StatusDelivered) }()
		go func() { defer wg.Done(); f = count(StatusFailed) }()
		go func() { defer wg.Done(); pd = count(StatusPending) }()
		wg.Wait()

		total := int64(-1)
		if d >= 0 && f >= 0 && pd >= 0 {
			total = d + f + pd
		}
		v := DatasetTotals{Delivered: d, Failed: f, Pending: pd, Total: total,
			AsOf: isoMs(time.Now())}
		dsMu.Lock()
		dsCache, dsAt = &v, nowMs()
		dsMu.Unlock()
		writeJSON(w, 200, v)
	})

	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		var req SubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "invalid JSON: "+err.Error())
			return
		}
		traced := true
		if req.Traced != nil {
			traced = *req.Traced
		}
		res, err := proc.Submit(req, traced)
		if err != nil {
			if _, ok := err.(ErrUnknownEventType); ok {
				writeErr(w, 400, err.Error())
				return
			}
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, res)
	})

	mux.HandleFunc("GET /trace/{eventId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("eventId")
		entries, ok := trace.Get(id)
		if !ok {
			writeErr(w, 404, "no trace for that event (only Send-console events are traced)")
			return
		}
		writeJSON(w, 200, map[string]any{"event_id": id, "entries": entries})
	})

	// --- load generator, in-process --------------------------------------------
	mux.HandleFunc("GET /load", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, loadgen.Status())
	})
	mux.HandleFunc("POST /load/start", func(w http.ResponseWriter, r *http.Request) {
		var s LoadSettings
		_ = json.NewDecoder(r.Body).Decode(&s)
		if s.RatePerSec <= 0 {
			s.RatePerSec = 1000
		}
		if s.UserPool <= 0 {
			s.UserPool = 100_000
		}
		if len(s.Channels) == 0 {
			s.Channels = AllChannels
		}
		if s.FailureInjection < 0 {
			s.FailureInjection = 0
		}
		if s.FailureInjection > 1 {
			s.FailureInjection = 1
		}
		if err := loadgen.Start(s); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, loadgen.Status())
	})
	mux.HandleFunc("POST /load/stop", func(w http.ResponseWriter, r *http.Request) {
		loadgen.Stop()
		writeJSON(w, 200, loadgen.Status())
	})

	// --- configuration ---------------------------------------------------------
	mux.HandleFunc("GET /policy", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, catalog.Policy())
	})
	mux.HandleFunc("PUT /policy", func(w http.ResponseWriter, r *http.Request) {
		var p Policy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if err := catalog.SavePolicy(p); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, catalog.Policy())
	})
	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, catalog.AllTemplates())
	})
	mux.HandleFunc("GET /event-defs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, catalog.AllEventDefs())
	})

	// `active` is the tenant this process is bound to. It is a startup config
	// value, not a per-request one, so the UI can display the choice but cannot
	// change it - see the note on the selector in Nav.tsx.
	mux.HandleFunc("GET /tenants", func(w http.ResponseWriter, r *http.Request) {
		ts, err := db.Tenants()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"active": cfg.TenantID, "tenants": ts})
	})

	mux.HandleFunc("GET /search/lag", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"indexed": srch.IndexedCount()})
	})

	mux.HandleFunc("GET /notifications/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, err := db.GetNotification(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, "not found")
			return
		}
		writeJSON(w, 200, n)
	})

	mux.HandleFunc("GET /notifications", handleSearch)
}


// impossibleFilter reports whether a filter combination can never match, in
// which case it is answered without touching either engine.
//
// seen=true implies status=DELIVERED, guaranteed from both directions:
//
//   - Live documents: the pipeline never writes `seen` (spec 1.3) - only the
//     mobile app does, via Sync Gateway, and only cblite documents sync. cblite
//     is born DELIVERED and carries no grace stamp, so nothing ever retries it
//     into PENDING or FAILED (see processor.go).
//   - Seeded documents: the seeder sets `seen` only when status is DELIVERED
//     (seeder-go/main.go).
//
// Verified against all 522M documents: seen=true totals 191,258,613, of which
// 191,258,613 are DELIVERED and 0 are PENDING or FAILED.
//
// Short-circuiting is correctness-neutral with a large payoff. GSI cannot prove
// an empty result cheaply: seen=true and status=PENDING each have their own
// partial index, so whichever is used the other becomes a post-filter matching
// nothing and the scan runs to the end of the index - measured at 20.3s to
// return no rows. The answer is knowable without asking.
//
// The response is an ordinary empty result. It carries no explanation: an empty
// set is an empty set, and the UI says what it already says for any other one.
func impossibleFilter(seen, status string) bool {
	return seen == "true" && status != "" && status != StatusDelivered
}

// One endpoint serves both the latest feed and filtered search. With no filters
// it is the feed, from GSI keyset on idx_multi. Any keyword or text filter routes
// to FTS. Both paginate on the same value (spec sections 7.1-7.2).
func handleSearch(w http.ResponseWriter, r *http.Request) {
	// Server-side latency, excluding network and browser time, so the figure
	// the UI shows is what Couchbase plus this handler actually cost.
	tStart := time.Now()
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("text"))
	seenParam := q.Get("seen")

	// Answered before choosing an engine: the combination is impossible, so
	// which engine would have been faster is beside the point.
	if impossibleFilter(seenParam, q.Get("status")) {
		writeJSON(w, 200, map[string]any{
			"path": "none", "total": 0, "rows": []Notification{},
			"incomplete": false, "next": nil,
			// No queries to disclose - nothing ran. The disclosure panel renders
			// nothing for an empty list.
			"queries": []map[string]string{},
			"tookMs":  float64(time.Since(tStart).Microseconds()) / 1000,
		})
		return
	}
	// FTS is now used ONLY for message text. Every other filter is served by the
	// composite GSI idx_multi, measured at 5ms against FTS's 600-1,400ms for the
	// same shapes - FTS has to rank every match before returning a page, while a
	// pre-sorted index stops at 100 rows.
	// `engine=fts` forces the FTS path for a query the GSI would otherwise
	// serve, so both engines can be measured on identical filters from
	// /notifications-fts. The main dashboard never sends it, so its routing is
	// exactly as before: FTS only when there is message text to match.
	useFTS := text != "" || q.Get("engine") == "fts"

	cPath, cAfter, cOK := decodeCursor(q.Get("cursor"))
	wantPath := "gsi"
	if useFTS {
		wantPath = "fts"
	}
	// A cursor is only valid for the path that produced it: FTS cursors are
	// document ids, GSI cursors are bare sort_keys. Feeding one to the other
	// compares the wrong strings and silently returns a wrong page.
	after := ""
	if cOK && cPath == wantPath {
		after = cAfter
	}

	if useFTS {
		// Measured on 500M: cost tracks the number of MATCHING documents.
		// "temporary password" matches 139M (28% of the corpus) and takes
		// seconds; the same text scoped to a day matches 380k and takes 409ms.
		// So an unscoped text search gets a default window, reported back so
		// the UI can show it and let the operator widen it deliberately.
		from := q.Get("from")
		defaulted := text != "" && from == ""
		if defaulted {
			from = isoMs(fromMs(nowMs() - int64(cfg.DefaultTextWindowMs)))
		}
		var seen *bool
		if seenParam == "true" || seenParam == "false" {
			b := seenParam == "true"
			seen = &b
		}
		f := SearchFilters{
			Text: text, UserID: q.Get("user_id"), AppName: q.Get("app_name"),
			Channel: q.Get("channel"), Status: q.Get("status"), Type: q.Get("type"),
			Seen: seen, FromISO: from, ToISO: q.Get("to"),
		}
		var page SearchPage
		var err error
		var shownQuery string
		tq := time.Now()
		_ = metrics.Time("search_fts", func() error {
			page, shownQuery, err = srch.Query(f, after, pageSize)
			return err
		})
		tSearch := time.Since(tq)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		var rows []Notification
		th := time.Now()
		_ = metrics.Time("search_hydrate", func() error {
			rows = db.BulkGet(page.IDs)
			return nil
		})
		tHydrate := time.Since(th)
		var next *string
		if page.LastSort != "" && len(rows) == pageSize {
			s := encodeCursor("fts", page.LastSort)
			next = &s
		}
		var appliedWindow *string
		if defaulted {
			appliedWindow = &from
		}
		writeJSON(w, 200, map[string]any{
			"path": "fts", "total": page.TotalHits, "rows": rows,
			// The text path is two round trips: FTS returns ranked ids, then KV
			// hydrates them. Showing only the first would misrepresent the work.
			"queries": []map[string]string{
				{"label": "1. Full-text match (Search service)", "lang": "JSON", "text": shownQuery},
				{"label": fmt.Sprintf("2. Hydrate %d documents (KV multi-get)", len(page.IDs)),
					"lang": "Key-value", "text": kvGetDisplay(page.IDs)},
			},
			"tookMs":    float64(time.Since(tStart).Microseconds()) / 1000,
			"searchMs":  float64(tSearch.Microseconds()) / 1000,
			"hydrateMs": float64(tHydrate.Microseconds()) / 1000,
			// Non-zero failures mean the page is incomplete - surfaced rather
			// than silently under-reporting.
			"incomplete":        page.FailedPartitions > 0,
			"appliedWindowFrom": appliedWindow,
			"next":              next,
		})
		return
	}

	// GSI path: a time bound becomes a sort_key bound, because the ULID prefix
	// encodes the timestamp. Measured at 1.9ms server-side over 500M.
	//
	// Each bound is applied INDEPENDENTLY. Requiring both meant an open-ended
	// range ("everything since Monday") was silently dropped and the query
	// returned unfiltered latest results - wrong answers, no error.
	fromSK, toSK := "", ""
	if f := q.Get("from"); f != "" {
		if ft, err := time.Parse(time.RFC3339, f); err == nil {
			fromSK = UlidFloor(ft.UnixMilli())
		}
	}
	if t := q.Get("to"); t != "" {
		if tt, err := time.Parse(time.RFC3339, t); err == nil {
			toSK = UlidCeil(tt.UnixMilli())
		}
	}
	var seenFilter *bool
	if seenParam == "true" || seenParam == "false" {
		b := seenParam == "true"
		seenFilter = &b
	}
	var rows []Notification
	var shownQuery string
	var err error
	_ = metrics.Time("search_gsi", func() error {
		rows, shownQuery, err = db.Feed(cfg.TenantID, FeedFilter{
			After: after, UserID: q.Get("user_id"), AppName: q.Get("app_name"),
			Channel: q.Get("channel"), Status: q.Get("status"), Type: q.Get("type"),
			Seen: seenFilter, FromSK: fromSK, ToSK: toSK, Limit: pageSize,
		})
		return err
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var next *string
	if len(rows) == pageSize {
		s := encodeCursor("gsi", rows[len(rows)-1].SortKey)
		next = &s
	}
	writeJSON(w, 200, map[string]any{
		// `total` stays nil: a pre-sorted index scan stops at LIMIT and never
		// counts the rest. COUNT(*) over 500M on this index times out at 30s,
		// so the UI shows measured latency instead of a hit count.
		"path": "gsi", "total": nil, "rows": rows, "incomplete": false, "next": next,
		"queries": []map[string]string{
			{"label": "Keyset page (Query service)", "lang": "N1QL", "text": shownQuery},
		},
		"tookMs": float64(time.Since(tStart).Microseconds()) / 1000,
	})
}

var _ = strconv.Itoa
