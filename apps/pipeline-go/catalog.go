package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/couchbase/gocb/v2"
)

// In-memory cache of configuration. Refreshed on an interval so the hot path
// performs ZERO configuration reads: at 1000 events/sec, reading the policy per
// event would add 1000 KV round trips per second to no purpose. The refresh is
// also what makes the policy live-editable from the UI (spec section 5.5).

type Catalog struct {
	db      *DB
	tenant  string
	refresh time.Duration

	mu        sync.RWMutex
	policy    Policy
	templates map[string]Template
	eventDefs map[string]EventDef
	lastOK    atomic.Int64
	lastErr   atomic.Value // string

	// What the refresh loop calls. Always Refresh in production; a seam so the
	// loop's retry behaviour can be tested without a live cluster.
	refreshFn func() error

	stop chan struct{}
}

func fallbackPolicy() Policy {
	var p Policy
	p.Retry.MaxAttempts = 3
	p.Retry.Backoff = Backoff{InitialMs: 1000, Multiplier: 2, MaxMs: 30000, Jitter: true}
	p.Throttle.MaxIdenticalPerUserPerMinute = 10
	p.Throttle.MaxPerUserPerHour = 100
	p.Throttle.MaxPerChannelPerMinute = 500000
	return p
}

func NewCatalog(db *DB, tenant string, refreshMs int) *Catalog {
	c := &Catalog{
		db: db, tenant: tenant, refresh: time.Duration(refreshMs) * time.Millisecond,
		policy: fallbackPolicy(), templates: map[string]Template{}, eventDefs: map[string]EventDef{},
		stop: make(chan struct{}),
	}
	c.lastErr.Store("")
	c.refreshFn = c.Refresh
	return c
}

// Start performs the initial load and then refreshes on an interval. The
// initial error is returned but is NOT a reason to skip the loop: on a cluster
// restart this process comes up before Couchbase's query service is ready, and
// the loop is the only thing that ever heals that. Returning early here left
// the catalog empty for the life of the process - templates and event
// definitions included - which surfaced much later as bulk load failing with
// "no event definitions loaded" long after the cluster was healthy.
func (c *Catalog) Start() error {
	err := c.refreshFn()
	go func() {
		t := time.NewTicker(c.refresh)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				_ = c.refreshFn()
			}
		}
	}()
	return err
}

func (c *Catalog) Stop() { close(c.stop) }

func (c *Catalog) Policy() Policy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.policy
}

func (c *Catalog) Template(id string) (Template, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.templates[id]
	return t, ok
}

func (c *Catalog) EventDef(t string) (EventDef, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.eventDefs[t]
	return d, ok
}

func (c *Catalog) AllTemplates() []Template {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Template, 0, len(c.templates))
	for _, t := range c.templates {
		out = append(out, t)
	}
	return out
}

func (c *Catalog) AllEventDefs() []EventDef {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]EventDef, 0, len(c.eventDefs))
	for _, d := range c.eventDefs {
		out = append(out, d)
	}
	return out
}

func (c *Catalog) Stats() (int, int, string, string) {
	c.mu.RLock()
	nt, nd := len(c.templates), len(c.eventDefs)
	c.mu.RUnlock()
	e, _ := c.lastErr.Load().(string)
	return nt, nd, isoMs(fromMs(c.lastOK.Load())), e
}

func (c *Catalog) Refresh() error {
	pol, perr := c.loadPolicy()
	tpls, terr := c.loadTemplates()
	defs, derr := c.loadEventDefs()
	if terr != nil || derr != nil {
		// Keep serving the last known-good configuration rather than failing
		// requests because a refresh blipped.
		err := terr
		if err == nil {
			err = derr
		}
		c.lastErr.Store(err.Error())
		return err
	}
	c.mu.Lock()
	if perr == nil {
		c.policy = pol
	}
	c.templates = map[string]Template{}
	for _, t := range tpls {
		c.templates[t.TemplateID] = t
	}
	c.eventDefs = map[string]EventDef{}
	for _, d := range defs {
		c.eventDefs[d.EventType] = d
	}
	c.mu.Unlock()
	c.lastOK.Store(nowMs())
	c.lastErr.Store("")
	return nil
}

func (c *Catalog) loadPolicy() (Policy, error) {
	var p Policy
	res, err := c.db.Coll("policies").Get(keyPolicy(), nil)
	if err != nil {
		return p, err
	}
	return p, res.Content(&p)
}

// Config collections are tiny and have primary indexes, so a full scan here is
// correct and cheap - unlike anything touching `notifications`.
func (c *Catalog) loadTemplates() ([]Template, error) {
	stmt := fmt.Sprintf("SELECT c.* FROM %s AS c WHERE c.tenant_id = $t", c.db.keyspace("templates"))
	rows, err := c.db.Scope.Query(stmt, &gocb.QueryOptions{
		NamedParameters: map[string]interface{}{"t": c.tenant}, Adhoc: false})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Row(&t); err == nil {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (c *Catalog) loadEventDefs() ([]EventDef, error) {
	stmt := fmt.Sprintf("SELECT c.* FROM %s AS c WHERE c.tenant_id = $t", c.db.keyspace("event_defs"))
	rows, err := c.db.Scope.Query(stmt, &gocb.QueryOptions{
		NamedParameters: map[string]interface{}{"t": c.tenant}, Adhoc: false})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventDef
	for rows.Next() {
		var d EventDef
		if err := rows.Row(&d); err == nil {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

func (c *Catalog) SavePolicy(p Policy) error {
	if _, err := c.db.Coll("policies").Upsert(keyPolicy(), p, nil); err != nil {
		return err
	}
	return c.Refresh()
}

// ResolveChannels intersects the event definition's channels with any explicit
// request.
func (c *Catalog) ResolveChannels(evType string, requested []Channel) []Channel {
	def, ok := c.EventDef(evType)
	if !ok {
		return nil
	}
	if len(requested) == 0 {
		return def.Channels
	}
	allowed := map[string]bool{}
	for _, ch := range def.Channels {
		allowed[ch] = true
	}
	// cblite is available on every event type without being declared on any of
	// them. Adding it to the definitions instead would make the load generator
	// fan out over it too, since loadgen takes its channels from the definition.
	allowed[ChannelCBLite] = true
	var out []Channel
	for _, ch := range requested {
		if allowed[ch] {
			out = append(out, ch)
		}
	}
	return out
}
