package main

import "sync"

// In-process bus standing in for Solace (spec D12). Topic names match the
// requirements': notification.delivery.{channel} and notification.results.{channel}.
//
// Go channels replace the JavaScript queue: a buffered channel gives natural
// backpressure, and N consumer goroutines per topic give real parallelism
// across cores - which is the whole reason this service is not replicated.

type Bus struct {
	mu     sync.RWMutex
	topics map[string]chan any
	wg     sync.WaitGroup
	closed bool
}

func NewBus(depth int) *Bus {
	return &Bus{topics: map[string]chan any{}}
}

func (b *Bus) channel(topic string, depth int) chan any {
	b.mu.RLock()
	ch, ok := b.topics[topic]
	b.mu.RUnlock()
	if ok {
		return ch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok = b.topics[topic]; ok {
		return ch
	}
	ch = make(chan any, depth)
	b.topics[topic] = ch
	return ch
}

// Publish blocks when the topic is full. That is deliberate backpressure: an
// unbounded queue would consume the heap until the process dies.
func (b *Bus) Publish(topic string, msg any) {
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return
	}
	b.channel(topic, 50_000) <- msg
}

// Subscribe starts `workers` goroutines consuming the topic.
func (b *Bus) Subscribe(topic string, workers int, handler func(any)) {
	ch := b.channel(topic, 50_000)
	for i := 0; i < workers; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for msg := range ch {
				handler(msg)
			}
		}()
	}
}

func (b *Bus) Depth() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, ch := range b.topics {
		n += len(ch)
	}
	return n
}

func (b *Bus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for _, ch := range b.topics {
		close(ch)
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func topicDelivery(ch string) string { return "notification.delivery." + ch }
func topicResults(ch string) string  { return "notification.results." + ch }

type DeliveryCommand struct {
	NotificationID string
	EventID        string
	TenantID       string
	UserID         string
	Channel        string
	Subject        string
	Message        string
	Trials         int
	ForceFail      bool
	ForcePermanent bool
	SubmittedAtMs  int64
	Traced         bool
}

type DeliveryResult struct {
	Cmd        DeliveryCommand
	OK         bool
	HTTPStatus int
	Err        string
	LatencyMs  int64
}
