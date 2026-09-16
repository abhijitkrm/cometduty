// Package broker is a tiny non-blocking publish/subscribe fan-out. It replaces
// the abandoned github.com/textileio/go-threads broadcast dependency.
//
// Publishers never block: if a subscriber's buffer is full the message is
// dropped for that subscriber. This is deliberate — a slow dashboard client must
// never stall the monitoring pipeline.
package broker

import "sync"

// Broker distributes published values to all subscribers.
type Broker[T any] struct {
	mu   sync.RWMutex
	subs map[int]chan T
	next int
}

// New returns a broker whose subscribers each get a buffer of size depth.
func New[T any]() *Broker[T] {
	return &Broker[T]{subs: map[int]chan T{}}
}

// Subscribe registers a listener. The returned channel is closed on Unsubscribe.
func (b *Broker[T]) Subscribe(depth int) (int, <-chan T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	ch := make(chan T, depth)
	b.subs[id] = ch
	return id, ch
}

// Unsubscribe removes a listener and closes its channel.
func (b *Broker[T]) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish sends v to every subscriber, dropping for any whose buffer is full.
func (b *Broker[T]) Publish(v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

// Len returns the subscriber count.
func (b *Broker[T]) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
