package main

import (
	"sync/atomic"
	"time"
)

// Retry Scheduler and reconciliation.
//
// The durable retry queue is the notification document itself: status PENDING
// plus delivery.next_attempt_at. It survives restarts, is visible in the UI,
// and is read through the PARTIAL index idx_retry (WHERE status='PENDING').
//
// In a single-process deployment there is exactly one scheduler by construction
// - no singleton gating, no env flag, no risk of six replicas delivering the
// same rows six times. That whole class of bug does not exist here.

type Scheduler struct {
	db      *DB
	bus     *Bus
	metrics *Metrics
	tickMs  int
	batch   int
	horizon int64

	picked  atomic.Int64
	skipped atomic.Int64
	lastErr atomic.Value
	stop    chan struct{}
}

func NewScheduler(db *DB, bus *Bus, m *Metrics, tickMs, batch, horizonMs int) *Scheduler {
	s := &Scheduler{db: db, bus: bus, metrics: m, tickMs: tickMs, batch: batch,
		horizon: int64(horizonMs), stop: make(chan struct{})}
	s.lastErr.Store("")
	return s
}

func (s *Scheduler) Start() {
	go func() {
		t := time.NewTicker(time.Duration(s.tickMs) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.tick()
			}
		}
	}()
}

func (s *Scheduler) Stop() { close(s.stop) }

func (s *Scheduler) Stats() map[string]any {
	e, _ := s.lastErr.Load().(string)
	return map[string]any{
		"picked": s.picked.Load(), "skipped": s.skipped.Load(),
		"horizonMs": s.horizon, "lastError": e,
	}
}

func (s *Scheduler) tick() {
	var due []DueRetry
	err := s.metrics.Time("retry_pickup", func() error {
		var e error
		due, e = s.db.DueRetries(s.batch, nowMs(), s.horizon)
		return e
	})
	if err != nil {
		s.lastErr.Store(err.Error())
		return
	}
	s.lastErr.Store("")
	for _, d := range due {
		s.republish(d)
	}
	s.picked.Add(int64(len(due)))
}

func (s *Scheduler) republish(d DueRetry) {
	doc, err := s.db.GetNotification(d.ID)
	if err != nil || doc.Status != StatusPending {
		s.skipped.Add(1) // already resolved by another path
		return
	}
	submitted, _ := time.Parse(time.RFC3339, doc.Timestamp)
	s.bus.Publish(topicDelivery(doc.Channel), DeliveryCommand{
		NotificationID: d.ID, EventID: doc.EventID, TenantID: doc.TenantID,
		UserID: doc.UserID, Channel: doc.Channel, Subject: doc.Subject,
		Message: doc.Message, Trials: doc.Delivery.Trials,
		SubmittedAtMs: submitted.UnixMilli(), Traced: false,
		// Carried so a forced failure survives into retries; otherwise the
		// exhaustion path to FAILED can never be demonstrated.
		ForceFail: doc.TestForceFail, ForcePermanent: doc.TestForcePermanent,
	})
}
