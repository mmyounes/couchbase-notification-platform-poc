package main

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Sustained-rate load driver, in-process.
//
// In the Node implementation this had to be extracted into its own service: an
// in-process generator could only load the replica it lived in, which capped the
// first load test at ~520 events/sec. Here one process uses every core, so the
// generator can submit directly to the processor - no extra service, no HTTP
// hop, no nginx in the measurement path.

type LoadSettings struct {
	RatePerSec       int      `json:"ratePerSec"`
	DurationSec      int      `json:"durationSec"`
	UserPool         int      `json:"userPool"`
	Channels         []string `json:"channels"`
	FailureInjection float64  `json:"failureInjection"`
}

type LoadStatus struct {
	Running            bool          `json:"running"`
	StartedAt          *string       `json:"startedAt"`
	ElapsedSec         int64         `json:"elapsedSec"`
	Submitted          int64         `json:"submitted"`
	Rejected           int64         `json:"rejected"`
	InFlight           int64         `json:"inFlight"`
	AchievedRatePerSec int64         `json:"achievedRatePerSec"`
	Settings           *LoadSettings `json:"settings"`
	LastError          *string       `json:"lastError"`
}

type LoadGen struct {
	proc    *Processor
	catalog *Catalog

	mu        sync.Mutex
	running   bool
	settings  *LoadSettings
	startedMs int64
	stoppedMs int64
	cancel    chan struct{}

	submitted atomic.Int64
	rejected  atomic.Int64
	inFlight  atomic.Int64
	lastErr   atomic.Value
	rate      *rateWindow
	// Bounds concurrent in-flight submissions. Without it the ticker spawns
	// goroutines faster than they complete, gocb's operation queue overflows,
	// and the failure cascades into tracking errors. Backpressure here keeps
	// overload visible as a lower achieved rate rather than as corruption.
	sem chan struct{}
}

func NewLoadGen(p *Processor, c *Catalog) *LoadGen {
	lg := &LoadGen{proc: p, catalog: c, rate: newRateWindow(60),
		sem: make(chan struct{}, envInt("LOADGEN_MAX_INFLIGHT", 16384))}
	lg.lastErr.Store("")
	return lg
}

var loadApps = []string{"ncgrdemo", "portal", "hr_self_service", "procurement", "payroll", "licensing"}

func (l *LoadGen) Start(s LoadSettings) error {
	l.Stop()
	defs := l.catalog.AllEventDefs()
	if len(defs) == 0 {
		return errNoEventDefs
	}
	l.mu.Lock()
	l.running = true
	l.settings = &s
	l.startedMs = nowMs()
	l.stoppedMs = 0
	l.cancel = make(chan struct{})
	cancel := l.cancel
	l.mu.Unlock()

	l.submitted.Store(0)
	l.rejected.Store(0)
	l.lastErr.Store("")

	// 20ms ticks: finer than the pipeline's unit of work, so arrival is smooth
	// rather than bursty.
	const tickMs = 20
	perTick := float64(s.RatePerSec) * tickMs / 1000.0

	go func() {
		t := time.NewTicker(tickMs * time.Millisecond)
		defer t.Stop()
		carry := 0.0
		for {
			select {
			case <-cancel:
				return
			case <-t.C:
				if s.DurationSec > 0 && (nowMs()-l.startedMs)/1000 >= int64(s.DurationSec) {
					l.Stop()
					return
				}
				// Carry the fractional remainder so the average rate is exact
				// rather than silently rounded down every tick.
				carry += perTick
				n := int(carry)
				carry -= float64(n)
				for i := 0; i < n; i++ {
					select {
					case l.sem <- struct{}{}:
						go func() { defer func() { <-l.sem }(); l.fire(s, defs) }()
					default:
						// Saturated: count it rather than queue it, so the
						// achieved rate honestly reflects what the system took.
						l.rejected.Add(1)
						l.lastErr.Store("load generator saturated: max in-flight reached")
					}
				}
			}
		}
	}()
	return nil
}

func (l *LoadGen) fire(s LoadSettings, defs []EventDef) {
	def := defs[rand.Intn(len(defs))]
	req := SubmitRequest{
		// usr_ prefix - never user1 / user2, reserved for mobile sync tests.
		UserID:  "usr_" + pad6(1+rand.Intn(s.UserPool)),
		AppName: loadApps[rand.Intn(len(loadApps))],
		Type:    def.EventType,
		Params:  l.paramsFor(def),
		Source:  "load_test",
	}
	if rand.Float64() < s.FailureInjection {
		req.ForceFail = map[string]bool{}
		for _, ch := range s.Channels {
			req.ForceFail[ch] = true
		}
	}
	l.inFlight.Add(1)
	defer l.inFlight.Add(-1)
	// Not traced: tracing every event under load would consume the heap.
	if _, err := l.proc.Submit(req, false); err != nil {
		l.rejected.Add(1)
		l.lastErr.Store(err.Error())
		return
	}
	l.submitted.Add(1)
	l.rate.Add(1)
}

func (l *LoadGen) paramsFor(def EventDef) map[string]any {
	tpl, ok := l.catalog.Template(def.TemplateID)
	out := map[string]any{}
	if !ok {
		return out
	}
	for _, p := range tpl.Params {
		if v, found := sampleParams[p]; found {
			out[p] = v
		} else {
			out[p] = p + "-x"
		}
	}
	return out
}

func (l *LoadGen) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return
	}
	l.running = false
	l.stoppedMs = nowMs()
	close(l.cancel)
}

// Status reports the trailing window while running, and the completed run's
// true average once stopped. Reporting the trailing window after a run ends
// silently mixes in idle seconds and makes a sustained 996/sec read as 462.
func (l *LoadGen) Status() LoadStatus {
	l.mu.Lock()
	running, settings, started, stopped := l.running, l.settings, l.startedMs, l.stoppedMs
	l.mu.Unlock()

	st := LoadStatus{
		Running: running, Submitted: l.submitted.Load(), Rejected: l.rejected.Load(),
		InFlight: l.inFlight.Load(), Settings: settings,
	}
	if started > 0 {
		s := isoMs(fromMs(started))
		st.StartedAt = &s
		st.ElapsedSec = (nowMs() - started) / 1000
	}
	if e, _ := l.lastErr.Load().(string); e != "" {
		st.LastError = &e
	}
	if running {
		st.AchievedRatePerSec = l.rate.PerSec()
	} else if started > 0 && stopped > started && st.Submitted > 0 {
		st.AchievedRatePerSec = st.Submitted / max64((stopped-started)/1000, 1)
	}
	return st
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func pad6(n int) string {
	s := []byte("000000")
	i := 5
	for n > 0 && i >= 0 {
		s[i] = byte('0' + n%10)
		n /= 10
		i--
	}
	return string(s)
}

var errNoEventDefs = errStr("no event definitions loaded - seed the config collections first")

type errStr string

func (e errStr) Error() string { return string(e) }

var sampleParams = map[string]any{
	"app_name": "ncgrdemo", "temp_password": "Xk7-92Qa", "expiry_minutes": 15,
	"otp_code": "482915", "invoice_no": "INV-228543", "amount": "900.49",
	"due_date": "2026-12-25", "reference": "PAY-QLF6JX8ZNK", "method": "Mada card",
	"service": "passport renewal", "branch": "Riyadh Olaya", "appointment_time": "09:30",
	"ticket": "TKT-48213", "document": "commercial registration", "expiry_date": "2026-11-04",
	"days_left": 30, "request_no": "SR-2841903", "new_status": "under review",
	"agent": "Support Desk", "activity": "a sign-in from a new device",
	"city": "Riyadh", "occurred_at": "14:22",
}
