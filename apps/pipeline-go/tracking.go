package main

import (
	"fmt"
	"sync/atomic"
)

// Tracking Manager: consumes delivery results and maintains each notification's
// lifecycle (the requirements' "Tracking Engine", spec section 6.1).
//
//	success -> DELIVERED, delivered_at set, trials + 1
//	failure -> trials + 1; FAILED once max_attempts is exhausted or the failure
//	           is permanent, otherwise status STAYS PENDING with next_attempt_at
//
// Updates go through subdocument mutations, never read-modify-write.

type Tracking struct {
	db       *DB
	catalog  *Catalog
	counters *Counters
	bus      *Bus
	metrics  *Metrics
	trace    *TraceStore
	workers  int
	failures atomic.Int64
}

func (t *Tracking) Start() {
	for _, ch := range AllChannels {
		t.bus.Subscribe(topicResults(ch), t.workers, func(msg any) {
			res, ok := msg.(DeliveryResult)
			if !ok {
				return
			}
			t.handle(res)
		})
	}
}

func (t *Tracking) Failures() int64 { return t.failures.Load() }

func (t *Tracking) handle(r DeliveryResult) {
	now := nowMs()
	o := applyResult(t.catalog.Policy(), r.Cmd.Trials, r.OK, r.HTTPStatus, r.Err, now)

	if err := t.metrics.Time("tracking_mutatein", func() error {
		return t.db.ApplyTracking(r.Cmd.NotificationID, o, now)
	}); err != nil {
		// A failed tracking update leaves the notification PENDING with a stale
		// next_attempt_at; the retry scheduler will pick it up.
		t.failures.Add(1)
		t.trace.Add(r.Cmd.EventID, "failed", r.Cmd.Channel, "tracking update failed: "+err.Error())
		return
	}

	t.metrics.Deliveries.Add(1)
	if o.Trials > 1 {
		t.counters.Bump("retried", 1)
	}

	switch o.Status {
	case StatusDelivered:
		t.counters.Bump("delivered", 1)
		t.counters.Drop("pending", 1)
		t.trace.Add(r.Cmd.EventID, "delivered", r.Cmd.Channel,
			fmt.Sprintf("after %d trial(s)", o.Trials))
	case StatusFailed:
		t.counters.Bump("failed", 1)
		t.counters.Drop("pending", 1)
		t.trace.Add(r.Cmd.EventID, "failed", r.Cmd.Channel,
			fmt.Sprintf("delivery was not successful after %d trial(s): %s", o.Trials, o.LastError))
	default:
		// Still PENDING. The durable next_attempt_at on the document IS the
		// queue, so nothing is held in memory and a restart loses nothing.
		t.trace.Add(r.Cmd.EventID, "scheduled_retry", r.Cmd.Channel,
			fmt.Sprintf("trial %d failed (%s); next attempt in %dms",
				o.Trials, o.LastError, o.NextAttemptAtMs-now))
	}
}
