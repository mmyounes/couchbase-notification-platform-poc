package main

import (
	"fmt"
	"sync"
	"time"
)

// Event Processor: the first runtime component (spec section 6.1).
//   1. resolve tenant + event definition   (in-memory cache, no I/O)
//   2. throttle checks                     (KV atomic counters)
//   3. INSERT event document               (insert -> replays rejected)
//   4. per surviving channel: render, INSERT notification, publish command

type Processor struct {
	db       *DB
	tenant   string
	catalog  *Catalog
	counters *Counters
	bus      *Bus
	metrics  *Metrics
	trace    *TraceStore
	graceMs  int64
}

type ErrUnknownEventType struct{ Type string }

func (e ErrUnknownEventType) Error() string {
	return fmt.Sprintf("no event definition for type '%s'", e.Type)
}

func (p *Processor) Submit(req SubmitRequest, traced bool) (SubmitResult, error) {
	now := nowMs()
	def, ok := p.catalog.EventDef(req.Type)
	if !ok {
		return SubmitResult{}, ErrUnknownEventType{req.Type}
	}
	channels := p.catalog.ResolveChannels(req.Type, req.Channels)

	// The ULID carries the submit time in its prefix; `timestamp` below is
	// written from the same millisecond so the two agree exactly.
	eventID := ULID(now)
	if traced {
		p.trace.Open(eventID)
	}
	p.trace.Add(eventID, "accepted", "", fmt.Sprintf("type=%s channels=%v", req.Type, channels))

	// --- 2. throttle, evaluated concurrently per channel ---------------------
	suppressed := map[Channel]string{}
	var accepted []Channel
	policy := p.catalog.Policy()

	t0 := time.Now()
	type verdict struct {
		ch string
		v  ThrottleVerdict
	}
	vch := make(chan verdict, len(channels))
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(ch string) {
			defer wg.Done()
			vch <- verdict{ch, p.counters.CheckThrottle(policy, req.UserID, req.Type, ch, time.Now())}
		}(ch)
	}
	wg.Wait()
	close(vch)
	byChan := map[string]ThrottleVerdict{}
	for v := range vch {
		byChan[v.ch] = v.v
	}
	// Iterate `channels` rather than the map so ordering is deterministic.
	for _, ch := range channels {
		v := byChan[ch]
		if v.Blocked != "" {
			suppressed[ch] = v.Blocked
			p.counters.Bump("suppressed", 1)
			p.trace.Add(eventID, "suppressed", ch, fmt.Sprintf("%s (identical %d/%d)",
				v.Blocked, v.Counts["identical"], policy.Throttle.MaxIdenticalPerUserPerMinute))
		} else {
			accepted = append(accepted, ch)
			p.trace.Add(eventID, "dedup_ok", ch, fmt.Sprintf("identical %d, user/hr %d",
				v.Counts["identical"], v.Counts["perUserHour"]))
		}
	}
	p.metrics.Record("policy", time.Since(t0).Microseconds())

	// --- 3. event document ---------------------------------------------------
	// Written even when every channel was suppressed: it is the audit record
	// for deliveries that deliberately did not happen (spec D5).
	ev := Event{
		EventID: eventID, TenantID: p.tenant, UserID: req.UserID, AppName: req.AppName,
		Type: req.Type, TemplateID: def.TemplateID, Params: req.Params,
		SubmittedAt: isoMs(fromMs(now)), Source: orDefault(req.Source, "api"),
		ChannelsRequested: channels, ChannelsAccepted: accepted,
	}
	if len(suppressed) > 0 {
		ev.Suppressed = suppressed
	}
	if ev.Params == nil {
		ev.Params = map[string]any{}
	}
	if ev.ChannelsAccepted == nil {
		ev.ChannelsAccepted = []Channel{}
	}

	err := p.metrics.Time("event_insert", func() error {
		_, e := p.db.Coll("events").Insert(keyEvent(p.tenant, eventID), ev, nil)
		return e
	})
	if err != nil {
		return SubmitResult{}, err
	}
	p.counters.Bump("submitted", 1)
	p.trace.Add(eventID, "event_persisted", "",
		fmt.Sprintf("accepted=%d suppressed=%d", len(accepted), len(suppressed)))

	tpl, hasTpl := p.catalog.Template(def.TemplateID)
	if !hasTpl && len(accepted) > 0 {
		return SubmitResult{}, fmt.Errorf("template %q referenced by event definition is missing", def.TemplateID)
	}

	// --- 4. notifications ----------------------------------------------------
	for _, ch := range accepted {
		variant, ok := variantFor(tpl, ch)
		if !ok {
			p.trace.Add(eventID, "failed", ch, "template has no "+ch+" variant")
			continue
		}
		tr := time.Now()
		subjectSrc := variant.Subject
		if subjectSrc == "" {
			subjectSrc = variant.Title
		}
		subject := ""
		if subjectSrc != "" {
			if subject, err = render(subjectSrc, req.Params); err != nil {
				return SubmitResult{}, err
			}
		}
		body, err := render(variant.Body, req.Params)
		if err != nil {
			return SubmitResult{}, err
		}
		p.metrics.Record("render", time.Since(tr).Microseconds())

		// Grace period, not a scheduled retry: normal delivery overwrites this
		// long before it comes due. A notification whose delivery command was
		// lost is still PENDING when it expires, and the retry scheduler
		// recovers it - reconciliation for free, with no second query.
		// cblite has no external provider: persisting the document IS the
		// delivery, and Sync Gateway replicates it whenever the device next
		// connects. So it is born DELIVERED, carries no grace stamp (nothing
		// will ever retry it) and ignores force_fail, which has no meaning
		// without a provider to fail.
		isCBLite := ch == ChannelCBLite

		status := StatusPending
		delivery := DeliveryState{Trials: 0}
		if isCBLite {
			nowISO := isoMs(fromMs(now))
			status = StatusDelivered
			delivery.DeliveredAt = &nowISO
		} else {
			// Grace period, not a scheduled retry: normal delivery overwrites
			// this long before it comes due.
			grace := isoMs(fromMs(now + p.graceMs))
			delivery.NextAttemptAt = &grace
		}

		n := Notification{
			TenantID: p.tenant, EventID: eventID, UserID: req.UserID, AppName: req.AppName,
			Type: req.Type, Channel: ch, Subject: subject, Message: body,
			TemplateID: def.TemplateID, TemplateVersion: tpl.Version,
			Timestamp: isoMs(fromMs(now)), Status: status,
			Seen:          false, // never written again by any server-side component
			Delivery:      delivery,
			SortKey:       sortKeyOf(eventID, ch),
			SyncChannels:  []string{syncChannel(p.tenant, req.UserID)},
			TestForceFail: !isCBLite && req.ForceFail[ch],
		}
		if err := p.metrics.Time("ntf_insert", func() error {
			_, e := p.db.Coll("notifications").Insert(keyNotification(p.tenant, eventID, ch), n, nil)
			return e
		}); err != nil {
			return SubmitResult{}, err
		}
		p.metrics.Notifications.Add(1)

		if isCBLite {
			// No delivery command: there is nothing to call. Counted as
			// delivered on the spot rather than passing through PENDING, so the
			// dashboard gauge is never transiently wrong.
			p.counters.Bump("delivered", 1)
			p.trace.Add(eventID, "notification_created", ch, "status=DELIVERED (stored for sync)")
			p.trace.Add(eventID, "delivered", ch, "persisted; Sync Gateway replicates on next device connection")
			continue
		}

		p.counters.Bump("pending", 1)
		p.trace.Add(eventID, "notification_created", ch, "status=PENDING trials=0")

		p.bus.Publish(topicDelivery(ch), DeliveryCommand{
			NotificationID: keyNotification(p.tenant, eventID, ch),
			EventID:        eventID, TenantID: p.tenant, UserID: req.UserID, Channel: ch,
			Subject: subject, Message: body, Trials: 0,
			ForceFail: req.ForceFail[ch], SubmittedAtMs: now, Traced: traced,
		})
	}

	p.metrics.Events.Add(1)
	if accepted == nil {
		accepted = []Channel{}
	}
	return SubmitResult{EventID: eventID, ChannelsAccepted: accepted, Suppressed: suppressed}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
