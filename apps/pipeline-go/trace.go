package main

import (
	"container/list"
	"sync"
	"time"
)

// Per-event trace for the single-message console.
//
// Bounded on purpose: at 1000 events/sec an unbounded map would consume the
// heap in minutes. Load-generated events are not traced at all - only events
// submitted from the Send console, which is the only place a trace is read.

const maxTracedEvents = 200

type TraceStore struct {
	mu    sync.Mutex
	data  map[string][]TraceEntry
	order *list.List // eviction order, oldest at the front
	elems map[string]*list.Element
}

func NewTraceStore() *TraceStore {
	return &TraceStore{data: map[string][]TraceEntry{}, order: list.New(),
		elems: map[string]*list.Element{}}
}

func (t *TraceStore) Open(eventID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.data[eventID]; exists {
		return
	}
	for t.order.Len() >= maxTracedEvents {
		front := t.order.Front()
		if front == nil {
			break
		}
		old := front.Value.(string)
		t.order.Remove(front)
		delete(t.data, old)
		delete(t.elems, old)
	}
	t.data[eventID] = []TraceEntry{}
	t.elems[eventID] = t.order.PushBack(eventID)
}

func (t *TraceStore) Add(eventID, kind, channel, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entries, ok := t.data[eventID]
	if !ok {
		return // not traced - the common case under load
	}
	t.data[eventID] = append(entries, TraceEntry{
		At: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind,
		Channel: channel, Detail: detail,
	})
}

func (t *TraceStore) Get(eventID string) ([]TraceEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.data[eventID]
	if !ok {
		return nil, false
	}
	out := make([]TraceEntry, len(e))
	copy(out, e)
	return out, true
}
