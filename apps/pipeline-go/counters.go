package main

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/gocb/v2"
)

// Throttle and statistics counters.
//
// Atomic KV increments with a TTL, never queries: sub-millisecond,
// self-expiring, no index, and indifferent to whether the bucket holds 500
// documents or 500 million (spec D8).

type Counters struct {
	coll   *gocb.Collection
	tenant string

	// Statistics are aggregated IN MEMORY and flushed periodically, not written
	// per bump.
	//
	// The first design did one KV round trip per increment. At 6,000 events/sec
	// that is ~20,000 extra operations per second purely for statistics; the
	// bounded queue in front of it overflowed and silently dropped 2.8M bumps,
	// so the dashboard under-reported by 3x while the pipeline was in fact
	// writing every document. Aggregating locally turns 20,000 ops/sec into
	// six, and makes drops impossible.
	pending sync.Map // stat name -> *atomic.Int64
	stop    chan struct{}
}

func NewCounters(coll *gocb.Collection, tenant string, flushMs int) *Counters {
	c := &Counters{coll: coll, tenant: tenant, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(time.Duration(flushMs) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				c.Flush()
				return
			case <-t.C:
				c.Flush()
			}
		}
	}()
	return c
}

func (c *Counters) slot(name string) *atomic.Int64 {
	v, _ := c.pending.LoadOrStore(name, &atomic.Int64{})
	return v.(*atomic.Int64)
}

// Flush applies accumulated deltas. Swap-to-zero first so concurrent bumps
// during the flush are counted in the next round rather than lost.
func (c *Counters) Flush() {
	c.pending.Range(func(k, v any) bool {
		name := k.(string)
		delta := v.(*atomic.Int64).Swap(0)
		if delta == 0 {
			return true
		}
		key := keyStat(c.tenant, name)
		var err error
		if delta > 0 {
			_, err = c.coll.Binary().Increment(key,
				&gocb.IncrementOptions{Initial: delta, Delta: uint64(delta)})
		} else {
			_, err = c.coll.Binary().Decrement(key,
				&gocb.DecrementOptions{Initial: 0, Delta: uint64(-delta)})
		}
		if err != nil {
			// Put it back rather than lose it: statistics are advisory but they
			// should not silently drift.
			v.(*atomic.Int64).Add(delta)
		}
		return true
	})
}

func (c *Counters) Close() { close(c.stop) }

// incr is synchronous: throttle decisions depend on the returned value.
func (c *Counters) incr(key string, ttl time.Duration) (int64, error) {
	res, err := c.coll.Binary().Increment(key, &gocb.IncrementOptions{
		Initial: 1, Delta: 1, Expiry: ttl,
	})
	if err != nil {
		return 0, err
	}
	return int64(res.Content()), nil
}

type ThrottleVerdict struct {
	Blocked string // "" when allowed
	Counts  map[string]int64
}

// CheckThrottle evaluates every rule for one (user, event type, channel).
//
// Every rule increments even when an earlier one already blocked: the counters
// describe attempted volume, and short-circuiting would make a user who is
// being flood-blocked appear to consume no quota.
func (c *Counters) CheckThrottle(p Policy, userID, evType, ch string, now time.Time) ThrottleVerdict {
	minute, hour := minuteBucket(now), hourBucket(now)
	identical, _ := c.incr(keyDedup(c.tenant, userID, evType, ch, minute), 120*time.Second)
	perUserHr, _ := c.incr(keyRateUserHour(c.tenant, userID, hour), 7200*time.Second)
	// One random shard, compared against its proportional share of the cap.
	shard := rand.Intn(ChannelRateShards)
	perChanShard, _ := c.incr(keyRateChannelMin(c.tenant, ch, minute, shard), 120*time.Second)

	counts := map[string]int64{
		"identical": identical, "perUserHour": perUserHr,
		"perChannelShard": perChanShard,
	}
	t := p.Throttle
	shardCap := int64(t.MaxPerChannelPerMinute / ChannelRateShards)
	if shardCap < 1 {
		shardCap = 1
	}
	switch {
	case identical > int64(t.MaxIdenticalPerUserPerMinute):
		return ThrottleVerdict{"identical_cap", counts}
	case perUserHr > int64(t.MaxPerUserPerHour):
		return ThrottleVerdict{"receiver_hourly_cap", counts}
	case perChanShard > shardCap:
		return ThrottleVerdict{"channel_minute_cap", counts}
	}
	return ThrottleVerdict{"", counts}
}

// --- dashboard statistics --------------------------------------------------
// The dashboard reads six keys per second regardless of dataset size; a
// COUNT(*) over 500M is slow with any index (spec section 7.1).

// Cumulative totals - safe to zero, because they only ever count upward.
var cumulativeStats = []string{"submitted", "delivered", "failed", "retried", "suppressed"}

// `pending` is deliberately NOT in that list. It is a GAUGE - the number of
// notifications currently in flight - maintained by +1 on create and -1 on
// terminal state. Zeroing it mid-flight guarantees it goes negative as the
// in-flight work completes and decrements a counter that no longer counts
// those notifications. A gauge is only meaningful as a live level, so Reset
// leaves it alone.
var statNames = append(append([]string{}, cumulativeStats...), "pending")

// Bump is a single atomic add - no I/O on the hot path, and nothing to drop.
func (c *Counters) Bump(name string, by int64) { c.slot(name).Add(by) }

func (c *Counters) Drop(name string, by int64) { c.slot(name).Add(-by) }

// PendingDeltas reports what has not yet been flushed, so /health can show
// whether statistics are keeping up instead of hiding a backlog.
func (c *Counters) PendingDeltas() int64 {
	var n int64
	c.pending.Range(func(_, v any) bool { n += abs64(v.(*atomic.Int64).Load()); return true })
	return n
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (c *Counters) Read() StatCounters {
	get := func(n string) int64 {
		res, err := c.coll.Get(keyStat(c.tenant, n), nil)
		if err != nil {
			return 0
		}
		var v int64
		if err := res.Content(&v); err != nil {
			return 0
		}
		return v
	}
	// Include not-yet-flushed deltas so the dashboard is current rather than
	// trailing the flush interval.
	local := func(n string) int64 {
		if v, ok := c.pending.Load(n); ok {
			return v.(*atomic.Int64).Load()
		}
		return 0
	}
	// Clamp at zero. Couchbase's binary decrement already floors at zero, but
	// the unflushed local delta does not - adding a negative delta to a floored
	// stored value is what surfaced negative counts in the UI. None of these
	// quantities can legitimately be negative.
	sum := func(n string) int64 {
		v := get(n) + local(n)
		if v < 0 {
			return 0
		}
		return v
	}
	s := StatCounters{
		Submitted:  sum("submitted"),
		Delivered:  sum("delivered"),
		Failed:     sum("failed"),
		Retried:    sum("retried"),
		Suppressed: sum("suppressed"),
		Pending:    sum("pending"),
	}
	if res, err := c.coll.Get(keyStat(c.tenant, "reset_at"), nil); err == nil {
		var t string
		if res.Content(&t) == nil {
			s.Since = &t
		}
	}
	return s
}

// Reset zeroes the cumulative totals only. `pending` is a live gauge and is
// left untouched - see the comment on statNames.
func (c *Counters) Reset() error {
	// Discard unflushed deltas for the cumulative stats, or they land
	// immediately after the reset. The pending delta is preserved.
	for _, n := range cumulativeStats {
		if v, ok := c.pending.Load(n); ok {
			v.(*atomic.Int64).Store(0)
		}
	}
	for _, n := range cumulativeStats {
		if _, err := c.coll.Upsert(keyStat(c.tenant, n), 0, nil); err != nil {
			return err
		}
	}
	_, err := c.coll.Upsert(keyStat(c.tenant, "reset_at"), isoMs(time.Now()), nil)
	return err
}
