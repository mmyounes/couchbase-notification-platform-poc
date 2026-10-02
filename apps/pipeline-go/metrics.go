package main

import (
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
)

// Per-stage latency histograms. Splitting by stage is what separates Couchbase
// latency from the mock channel's simulated delay, which is what makes the
// reported numbers defensible (spec section 8.1).
//
// Unlike the Node implementation there is ONE registry for the whole service:
// goroutines share it, so no cross-replica aggregation problem exists.

var Stages = []string{
	"policy", "render", "event_insert", "ntf_insert", "channel_call",
	"tracking_mutatein", "retry_pickup", "search_gsi", "search_fts", "search_hydrate",
}

// There is deliberately no end-to-end stage. A single journey figure conflates
// work the pipeline does with time spent queued and with retry backoff, and it
// is dominated by the mock provider - so it invites exactly the wrong reading.
// The per-stage figures above say the same thing without the ambiguity: sum the
// Couchbase stages for the database's share, and read channel_call separately
// for the simulated provider's.

type StageSnapshot struct {
	Count int64 `json:"count"`
	P50   int64 `json:"p50"`
	P95   int64 `json:"p95"`
	P99   int64 `json:"p99"`
	Max   int64 `json:"max"`
	Mean  int64 `json:"mean"`
}

type rateWindow struct {
	mu      sync.Mutex
	buckets map[int64]int64
	window  int64
}

func newRateWindow(sec int64) *rateWindow {
	return &rateWindow{buckets: map[int64]int64{}, window: sec}
}

func (r *rateWindow) Add(n int64) {
	s := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets[s] += n
	for k := range r.buckets {
		if k < s-r.window {
			delete(r.buckets, k)
		}
	}
}

func (r *rateWindow) Series() []int64 {
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, 0, r.window)
	for s := now - r.window + 1; s <= now; s++ {
		out = append(out, r.buckets[s])
	}
	return out
}

// Average over seconds that actually carried traffic, not the whole window.
// Dividing by the full window counts leading zeros from before a run started,
// which makes a steady rate read as a ramp.
func (r *rateWindow) PerSec() int64 {
	s := r.Series()
	if len(s) > 0 {
		s = s[:len(s)-1] // drop the partial current second
	}
	first := -1
	for i, v := range s {
		if v > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return 0
	}
	active := s[first:]
	var total int64
	for _, v := range active {
		total += v
	}
	return total / int64(len(active))
}

type Metrics struct {
	mu    sync.Mutex
	hists map[string]*hdrhistogram.Histogram

	Events        *rateWindow
	Notifications *rateWindow
	Deliveries    *rateWindow
}

func NewMetrics() *Metrics {
	m := &Metrics{
		hists:         map[string]*hdrhistogram.Histogram{},
		Events:        newRateWindow(60),
		Notifications: newRateWindow(60),
		Deliveries:    newRateWindow(60),
	}
	for _, s := range Stages {
		m.hists[s] = hdrhistogram.New(1, 60_000_000, 3) // microseconds, up to 60s
	}
	return m
}

// hdrhistogram is not goroutine-safe; a mutex is sufficient at these rates and
// far simpler than per-goroutine histograms merged on read.
func (m *Metrics) Record(stage string, micros int64) {
	if micros < 1 {
		micros = 1
	}
	m.mu.Lock()
	if h, ok := m.hists[stage]; ok {
		_ = h.RecordValue(micros)
	}
	m.mu.Unlock()
}

// Time runs fn and records how long it took.
func (m *Metrics) Time(stage string, fn func() error) error {
	t0 := time.Now()
	err := fn()
	m.Record(stage, time.Since(t0).Microseconds())
	return err
}

func (m *Metrics) Snapshot() map[string]StageSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]StageSnapshot, len(m.hists))
	for k, h := range m.hists {
		out[k] = StageSnapshot{
			Count: h.TotalCount(),
			P50:   h.ValueAtQuantile(50),
			P95:   h.ValueAtQuantile(95),
			P99:   h.ValueAtQuantile(99),
			Max:   h.Max(),
			Mean:  int64(h.Mean()),
		}
	}
	return out
}

func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.hists {
		h.Reset()
	}
}
