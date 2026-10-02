package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// On a cluster restart the pipeline container comes up before Couchbase's query
// service is ready, so the catalog's first load fails. The periodic refresh is
// what heals that - it must run regardless of whether the initial load worked.
//
// Regression: Start() used to return early on initial failure and never launch
// the loop, so the catalog stayed empty for the life of the process. main.go
// treats the error as non-fatal, so the pipeline served traffic with no event
// definitions and bulk load failed with "no event definitions loaded" until
// someone restarted it by hand.
func TestStartRefreshesAfterInitialFailure(t *testing.T) {
	c := NewCatalog(nil, "ncgr", 20)
	defer c.Stop()

	var calls atomic.Int32
	c.refreshFn = func() error {
		if calls.Add(1) == 1 {
			return errors.New("query service not ready")
		}
		return nil
	}

	_ = c.Start()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			return // loop is running despite the initial failure
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refresh loop never ran after a failed initial load: refresh called %d time(s), want >= 2", calls.Load())
}

// The initial error must still reach the caller so main.go can log it.
func TestStartReturnsInitialError(t *testing.T) {
	c := NewCatalog(nil, "ncgr", 20)
	defer c.Stop()

	want := errors.New("query service not ready")
	c.refreshFn = func() error { return want }

	if err := c.Start(); !errors.Is(err, want) {
		t.Fatalf("Start() error = %v, want %v", err, want)
	}
}

// feedIndexFor pins the index per query shape. The ordering is by selectivity,
// and getting it wrong is not a style question: the optimizer's own choice for
// these shapes scanned 2.04M index entries in 17s where the pinned one takes
// under 10ms (see the comment on feedIndexFor).
func TestFeedIndexForPicksBySelectivity(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name string
		f    FeedFilter
		want string
	}{
		{"bare feed", FeedFilter{}, "idx_multi"},
		{"app_name only", FeedFilter{AppName: "ncgrdemo"}, "idx_multi"},
		{"channel email", FeedFilter{Channel: "email"}, "idx_multi"},
		{"status DELIVERED", FeedFilter{Status: "DELIVERED"}, "idx_multi"},
		{"seen false", FeedFilter{Seen: &fa}, "idx_multi"},
		// the shapes the optimizer got wrong
		{"app+channel", FeedFilter{AppName: "ncgrdemo", Channel: "email"}, "idx_multi"},
		{"app+channel+status+seen", FeedFilter{AppName: "p", Channel: "email", Status: "DELIVERED", Seen: &fa}, "idx_multi"},
		// partial indexes, each gated on a literal predicate
		{"status PENDING", FeedFilter{Status: StatusPending}, "idx_pending_feed"},
		{"channel cblite", FeedFilter{Channel: ChannelCBLite}, "idx_cblite_feed"},
		{"seen true", FeedFilter{Seen: &tr}, "idx_seen_feed"},
		// user_id beats PENDING and seen=true: ~5,200 docs per user against 24.9M and 191M
		{"user_id", FeedFilter{UserID: "usr_1"}, "idx_user_multi"},
		{"user_id + PENDING", FeedFilter{UserID: "usr_1", Status: StatusPending}, "idx_user_multi"},
		{"user_id + seen true", FeedFilter{UserID: "usr_1", Seen: &tr}, "idx_user_multi"},
		// cblite is 5 documents in 522M - more selective than any single user
		{"user_id + cblite", FeedFilter{UserID: "usr_1", Channel: ChannelCBLite}, "idx_cblite_feed"},
	}
	for _, c := range cases {
		if got := feedIndexFor(c.f); got != c.want {
			t.Errorf("%s: feedIndexFor = %q, want %q", c.name, got, c.want)
		}
	}
}

// seen=true implies DELIVERED (see impossibleFilter). The pairing is not merely
// empty today - it is unreachable by construction - and asking GSI for it costs
// 20.3s to return nothing.
func TestImpossibleFilter(t *testing.T) {
	cases := []struct {
		seen, status string
		impossible   bool
	}{
		{"true", StatusPending, true},
		{"true", StatusFailed, true},
		{"true", StatusDelivered, false}, // 191M real rows
		{"true", "", false},              // seen=true alone is fine
		{"false", StatusPending, false},  // the normal pending view
		{"false", StatusDelivered, false},
		{"", StatusPending, false}, // no seen filter at all
		{"", "", false},
	}
	for _, c := range cases {
		got := impossibleFilter(c.seen, c.status)
		if got != c.impossible {
			t.Errorf("impossibleFilter(seen=%q, status=%q) impossible=%v, want %v",
				c.seen, c.status, got, c.impossible)
		}
	}
}
