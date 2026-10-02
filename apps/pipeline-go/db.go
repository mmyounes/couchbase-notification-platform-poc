package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/couchbase/gocb/v2"
)

type DB struct {
	Cluster *gocb.Cluster
	Bucket  *gocb.Bucket
	Scope   *gocb.Scope
	cfg     Config
}

func OpenDB(cfg Config) (*DB, error) {
	// kv_pool_size raises the number of TCP connections per data node. The
	// gocb default is 1, so ALL key-value traffic to each node is serialised
	// over a single socket - at ~57,000 ops/sec across three nodes that becomes
	// the limit long before CPU does (observed: the process waiting at 378% of
	// 1600% available while throughput plateaued).
	connStr := fmt.Sprintf("couchbase://%s?kv_pool_size=%d", cfg.CBHost, cfg.KvPoolSize)
	cluster, err := gocb.Connect(connStr, gocb.ClusterOptions{
		Authenticator: gocb.PasswordAuthenticator{Username: cfg.CBUser, Password: cfg.CBPass},
		TimeoutsConfig: gocb.TimeoutsConfig{
			// The SDK default of 2.5s produced ambiguous timeouts during
			// connection warm-up under load.
			KVTimeout:      10 * time.Second,
			QueryTimeout:   120 * time.Second,
			SearchTimeout:  60 * time.Second,
			ConnectTimeout: 20 * time.Second,
		},
	})
	if err != nil {
		return nil, err
	}
	bucket := cluster.Bucket(cfg.Bucket)
	if err := bucket.WaitUntilReady(20*time.Second, nil); err != nil {
		return nil, err
	}
	return &DB{Cluster: cluster, Bucket: bucket, Scope: bucket.Scope(cfg.Scope), cfg: cfg}, nil
}

func (d *DB) Coll(name string) *gocb.Collection { return d.Scope.Collection(name) }

func (d *DB) Close() { _ = d.Cluster.Close(nil) }

func (d *DB) keyspace(coll string) string {
	return fmt.Sprintf("`%s`.`%s`.`%s`", d.cfg.Bucket, d.cfg.Scope, coll)
}

func isDocExists(err error) bool   { return errors.Is(err, gocb.ErrDocumentExists) }
func isDocNotFound(err error) bool { return errors.Is(err, gocb.ErrDocumentNotFound) }

// --- notifications -------------------------------------------------------------

type DueRetry struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Trials  int    `json:"trials"`
}

// DueRetries returns notifications whose backoff has elapsed, WITHIN a recency
// horizon.
//
// The horizon is load-bearing. A 500M historical dataset holds ~25M PENDING
// notifications whose next_attempt_at is months past; an unbounded
// `next_attempt_at <= now` matches all of them and the scheduler spends forever
// re-delivering old messages (observed: ~28,000 seeded documents mutated before
// the bound was added). Bounding BOTH ends keeps this a pure range scan on the
// partial index idx_retry.
//
// It also subsumes reconciliation: the processor stamps
// next_attempt_at = now + grace on insert, so a notification whose delivery
// command was lost simply becomes due when its grace expires.
func (d *DB) DueRetries(limit int, now int64, horizonMs int64) ([]DueRetry, error) {
	stmt := fmt.Sprintf(`
		SELECT META().id AS id, n.channel, n.delivery.trials
		FROM %s AS n
		WHERE n.status = 'PENDING'
		  AND n.delivery.next_attempt_at BETWEEN $from AND $now
		LIMIT $limit`, d.keyspace("notifications"))
	rows, err := d.Scope.Query(stmt, &gocb.QueryOptions{
		NamedParameters: map[string]interface{}{
			"from":  isoMs(fromMs(now - horizonMs)),
			"now":   isoMs(fromMs(now)),
			"limit": limit,
		},
		Adhoc: false,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueRetry
	for rows.Next() {
		var r DueRetry
		if err := rows.Row(&r); err == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// FeedFilter is every non-text predicate the search page can apply.
type FeedFilter struct {
	After        string // keyset cursor (exclusive upper bound on sort_key)
	UserID       string
	AppName      string
	Channel      string
	Status       string
	Type         string
	Seen         *bool
	FromSK, ToSK string
	Limit        int
}

// Feed serves the latest-notifications view AND every keyword-filtered search,
// keyset paginated on sort_key.
//
// The composite index idx_multi (tenant_id, sort_key DESC, status, channel,
// app_name, type, seen, user_id) is what makes the filtered cases fast. Because
// its leading key is also the sort order, the planner seeks to the newest entry,
// scans backwards evaluating the trailing predicates INSIDE the index, and stops
// at LIMIT. FTS cannot do that - it must collect and rank every match before
// returning a page, which is why status=FAILED cost 1.4s there and 5ms here.
//
// Page 1 and page 500 cost the same: no OFFSET anywhere.
// buildFeedQuery returns the parameterised statement, its parameters, and a
// display form with literals inlined so it can be pasted straight into the
// Couchbase query workbench.
// Three values are written as LITERALS rather than bound parameters:
// status="PENDING", channel="cblite" and seen=true. Each is the condition of a
// partial index (idx_pending_feed, idx_cblite_feed, idx_seen_feed), and a
// partial index can only be chosen when the planner can PROVE its condition
// holds. Queries here run with Adhoc:false, so the plan is built by PREPARE
// with the parameters still unbound - the planner cannot prove anything about
// $status and silently falls back to idx_multi.
//
// Measured on 522M documents: status=PENDING takes 10.4s on the prepared plan
// via idx_multi, and 296ms via idx_pending_feed. Nothing errors, so the only
// symptom is a slow query and an index that looks like it did not help.
//
// These literals are constants in this file, never user input, so there is
// nothing to escape. Every other value stays parameterised.
func (d *DB) buildFeedQuery(tenant string, f FeedFilter) (stmt string, params map[string]interface{}, display string) {
	where := []string{"n.tenant_id = $tenant"}
	params = map[string]interface{}{"tenant": tenant, "limit": f.Limit}
	lit := map[string]string{"$tenant": quoteLit(tenant), "$limit": fmt.Sprint(f.Limit)}
	after, fromSK, toSK := f.After, f.FromSK, f.ToSK
	if f.UserID != "" {
		where = append(where, "n.user_id = $user")
		params["user"] = f.UserID
		lit["$user"] = quoteLit(f.UserID)
	}
	if f.AppName != "" {
		where = append(where, "n.app_name = $app")
		params["app"] = f.AppName
		lit["$app"] = quoteLit(f.AppName)
	}
	if f.Channel != "" {
		if f.Channel == ChannelCBLite {
			where = append(where, `n.channel = "cblite"`)
		} else {
			where = append(where, "n.channel = $chan")
			params["chan"] = f.Channel
		}
		lit["$chan"] = quoteLit(f.Channel)
	}
	if f.Status != "" {
		if f.Status == StatusPending {
			where = append(where, `n.status = "PENDING"`)
		} else {
			where = append(where, "n.status = $status")
			params["status"] = f.Status
		}
		lit["$status"] = quoteLit(f.Status)
	}
	if f.Type != "" {
		where = append(where, "n.type = $ntype")
		params["ntype"] = f.Type
		lit["$ntype"] = quoteLit(f.Type)
	}
	if f.Seen != nil {
		if *f.Seen {
			where = append(where, "n.seen = true")
		} else {
			where = append(where, "n.seen = $seen")
			params["seen"] = *f.Seen
		}
		lit["$seen"] = fmt.Sprint(*f.Seen)
	}
	if after != "" {
		where = append(where, "n.sort_key < $after")
		params["after"] = after
		lit["$after"] = quoteLit(after)
	}
	if fromSK != "" {
		where = append(where, "n.sort_key >= $from")
		params["from"] = fromSK
		lit["$from"] = quoteLit(fromSK)
	}
	if toSK != "" {
		where = append(where, "n.sort_key <= $to")
		params["to"] = toSK
		lit["$to"] = quoteLit(toSK)
	}

	stmt = fmt.Sprintf("SELECT n.*\nFROM %s AS n USE INDEX (%s USING GSI)\nWHERE %s\nORDER BY n.sort_key DESC\nLIMIT $limit",
		d.keyspace("notifications"), feedIndexFor(f), strings.Join(where, "\n  AND "))

	return stmt, params, inlineParams(stmt, lit)
}

// feedIndexFor names the index a feed query must use.
//
// The choice is pinned rather than left to the optimizer because these queries
// run as PREPARE + EXECUTE, so the plan is built while the parameters are still
// unbound. Unable to estimate how selective $app or $chan will turn out to be,
// the optimizer assumed the worst for idx_multi's post-filter and instead chose
// idx_user_multi for 9 of the 20 reachable multi-filter shapes. idx_user_multi
// is (tenant_id, user_id, sort_key DESC, ...): with no user_id predicate it can
// neither seek nor supply the ORDER BY, so the plan gained an Order operator and
// had to materialise and sort every match before LIMIT saw a row. Measured on
// 522M documents: 2.04M index entries scanned in 17s and still running, against
// 6.8ms for the same query planned with its values known.
//
// UPDATE STATISTICS does not help - and was tried. The problem is not
// estimating the distribution of a value, it is that the value is unknown at
// plan time, which no histogram can fix.
//
// Order here is by SELECTIVITY, most selective first, because every filter the
// chosen index does not lead on degrades to a post-filter within its scan:
//
//	channel=cblite            5 documents
//	user_id             ~5,200 documents (521M / 100k users)
//	status=PENDING        24.9M documents
//	seen=true            191.3M documents
//	otherwise            idx_multi, already in sort_key order
//
// The three partial indexes additionally require their conditioned value to be
// written as a LITERAL - see the comment on buildFeedQuery. Hinting a partial
// index against a bound parameter is reported as hints_not_followed and silently
// falls back to idx_multi.
func feedIndexFor(f FeedFilter) string {
	switch {
	case f.Channel == ChannelCBLite:
		return "idx_cblite_feed"
	case f.UserID != "":
		return "idx_user_multi"
	case f.Status == StatusPending:
		return "idx_pending_feed"
	case f.Seen != nil && *f.Seen:
		return "idx_seen_feed"
	default:
		return "idx_multi"
	}
}

// inlineParams substitutes $placeholders with their literals in a single scan.
// Sequential ReplaceAll would be wrong: a literal containing "$to" would be
// corrupted by a later pass, and short names would match inside longer ones.
func inlineParams(stmt string, lit map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(stmt); i++ {
		if stmt[i] != '$' {
			b.WriteByte(stmt[i])
			continue
		}
		j := i + 1
		for j < len(stmt) && (stmt[j] == '_' ||
			(stmt[j] >= 'a' && stmt[j] <= 'z') || (stmt[j] >= 'A' && stmt[j] <= 'Z') ||
			(stmt[j] >= '0' && stmt[j] <= '9')) {
			j++
		}
		if v, ok := lit[stmt[i:j]]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(stmt[i:j])
		}
		i = j - 1
	}
	return b.String()
}

func quoteLit(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// Feed also returns the statement it ran, in a form that can be pasted into the
// query workbench - the search UI exposes it so the query behind a result set
// is inspectable rather than hidden.
func (d *DB) Feed(tenant string, f FeedFilter) ([]Notification, string, error) {
	stmt, params, display := d.buildFeedQuery(tenant, f)
	rows, err := d.Scope.Query(stmt, &gocb.QueryOptions{NamedParameters: params, Adhoc: false})
	if err != nil {
		return nil, display, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Row(&n); err == nil {
			out = append(out, n)
		}
	}
	return out, display, rows.Err()
}

// ApplyTracking updates a notification using SUBDOCUMENT mutations.
//
// This is a hard requirement, not an optimisation (spec section 6.3): the
// document body never crosses the network, and the increment on delivery.trials
// is atomic server-side so there is no lost-update race and no CAS retry loop.
// Any implementation doing get -> mutate -> replace is wrong.
//
// It deliberately never touches `seen`: that field belongs to the mobile app
// via Sync Gateway.
func (d *DB) ApplyTracking(id string, o Outcome, now int64) error {
	ts := isoMs(fromMs(now))
	specs := []gocb.MutateInSpec{
		gocb.IncrementSpec("delivery.trials", 1, nil),
		gocb.UpsertSpec("status", o.Status, nil),
		gocb.UpsertSpec("delivery.last_attempt_at", ts, nil),
	}
	if o.LastError != "" {
		specs = append(specs, gocb.UpsertSpec("delivery.last_error", o.LastError, nil))
	} else {
		specs = append(specs, gocb.UpsertSpec("delivery.last_error", nil, nil))
	}
	if o.NextAttemptAtMs > 0 {
		specs = append(specs, gocb.UpsertSpec("delivery.next_attempt_at", isoMs(fromMs(o.NextAttemptAtMs)), nil))
	} else {
		specs = append(specs, gocb.UpsertSpec("delivery.next_attempt_at", nil, nil))
	}
	if o.Status == StatusDelivered {
		specs = append(specs, gocb.UpsertSpec("delivery.delivered_at", ts, nil))
	}
	_, err := d.Coll("notifications").MutateIn(id, specs, nil)
	return err
}

func (d *DB) GetNotification(id string) (*Notification, error) {
	res, err := d.Coll("notifications").Get(id, nil)
	if err != nil {
		return nil, err
	}
	var n Notification
	if err := res.Content(&n); err != nil {
		return nil, err
	}
	return &n, nil
}

// BulkGet hydrates a page of ids. FTS and GSI return ids only; a 100-key
// multi-get is the cheapest thing Couchbase does (spec section 7.3).
func (d *DB) BulkGet(ids []string) []Notification {
	out := make([]Notification, 0, len(ids))
	type res struct {
		i int
		n *Notification
	}
	ch := make(chan res, len(ids))
	for i, id := range ids {
		go func(i int, id string) {
			n, err := d.GetNotification(id)
			if err != nil {
				ch <- res{i, nil}
				return
			}
			ch <- res{i, n}
		}(i, id)
	}
	byIdx := make([]*Notification, len(ids))
	for range ids {
		r := <-ch
		byIdx[r.i] = r.n
	}
	// Preserve the ranking order the index returned.
	for _, n := range byIdx {
		if n != nil {
			out = append(out, *n)
		}
	}
	return out
}

// kvGetDisplay renders the hydration step as something a reader can recognise:
// the ids are fetched individually over the KV protocol, not via a query.
func kvGetDisplay(ids []string) string {
	if len(ids) == 0 {
		return "// no matches - nothing to hydrate"
	}
	shown := ids
	suffix := ""
	if len(shown) > 5 {
		shown = shown[:5]
		suffix = fmt.Sprintf("\n  ... %d more", len(ids)-5)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("collection.Get() x%d, issued in parallel:\n", len(ids)))
	for _, id := range shown {
		b.WriteString("  " + id + "\n")
	}
	return strings.TrimRight(b.String(), "\n") + suffix
}

// Tenants lists the tenancy records. Small collection with a primary index, so
// a full scan is correct here - unlike anything touching `notifications`.
func (d *DB) Tenants() ([]Tenant, error) {
	stmt := fmt.Sprintf("SELECT t.* FROM %s AS t ORDER BY t.tenant_id", d.keyspace("tenants"))
	rows, err := d.Scope.Query(stmt, &gocb.QueryOptions{Adhoc: false})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Row(&t); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}
