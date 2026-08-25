package rest

import (
	"context"
	"errors"
	"sync"
)

// ErrBrokerClosed reports a subscription to a Broker that has been closed.
var ErrBrokerClosed = errors.New("rest: the broker is closed")

// BrokerOptions configures the in-process [Broker].
type BrokerOptions struct {
	// History is how many recent events are kept so that a client reconnecting
	// with Last-Event-ID can be caught up rather than told to refetch.
	// Defaults to 256. Zero after defaulting is not reachable; use a negative
	// value to disable replay, which makes every reconnection a reset.
	History int

	// Buffer is how many events may queue for one subscriber before the Broker
	// gives up on it and closes its channel.
	//
	// Defaults to 256, and is raised to History+1 if it is set lower, because
	// a subscription hands its replay to the channel before returning and a
	// buffer that could not hold the replay would deadlock the publisher.
	Buffer int
}

// Broker is an in-process [Source]: writes publish to it, and the subscribers
// connected to *this process* receive them.
//
// # What it is not
//
// It is not the change feed [ADR-0012] describes, and the difference is worth
// stating before anything is built on it.
//
//   - **At-most-once.** Publication happens after the transaction commits, in
//     the same process. A crash between the commit and the fan-out loses the
//     event, and no client learns the row changed.
//   - **One replica.** A Broker serves the subscribers holding a connection to
//     the process it lives in. Behind two replicas, a write served by one is
//     invisible to everyone connected to the other.
//
// Both are consequences of the event being held in memory rather than written
// to the database in the same transaction as the change, which is exactly what
// the outbox in ADR-0012 fixes. Until it exists, this is a real feature for a
// single-replica deployment and a trap for a horizontally scaled one, so it
// says so here rather than in a changelog.
//
// What it does do carefully is fail loudly. A subscriber that falls behind is
// disconnected rather than quietly skipped, a gap that cannot be replayed
// arrives as a [Reset] rather than as silence, and both converge on a client
// that refetches. The failure mode this avoids — a client that never learns a
// row changed and displays it forever — is the one that looks like everything
// is working.
//
// [ADR-0012]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#change-feed-outbox
type Broker struct {
	history int
	buffer  int

	mu   sync.Mutex
	seq  uint64
	ring []Delivery
	subs map[*subscriber]struct{}
	done bool
}

// subscriber is one connected stream.
//
// dead is under the Broker's mutex and exists because two things close this
// channel: the publisher, dropping a subscriber that stopped keeping up, and
// the goroutine watching the request context, when the client disconnects.
// Whichever arrives second must not close it again.
type subscriber struct {
	ch   chan Delivery
	dead bool
}

const defaultHistory = 256

// NewBroker returns an in-process event source. The zero BrokerOptions is
// usable.
func NewBroker(opts BrokerOptions) *Broker {
	history := opts.History
	switch {
	case history == 0:
		history = defaultHistory
	case history < 0:
		history = 0
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = defaultHistory
	}
	if buffer < history+1 {
		buffer = history + 1
	}
	return &Broker{
		history: history,
		buffer:  buffer,
		subs:    make(map[*subscriber]struct{}),
	}
}

// Publish numbers each event and fans it out to every current subscriber.
//
// It does not block and does not report failure, which is what lets it be
// called from an after-commit callback: the write is already durable by then,
// and a change feed that could fail a committed request would be worse than
// one that drops an event and says so to the client that missed it.
func (b *Broker) Publish(events ...Event) {
	if len(events) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	for _, ev := range events {
		b.seq++
		d := Delivery{ID: b.seq, Event: ev}
		b.remember(d)
		for s := range b.subs {
			select {
			case s.ch <- d:
			default:
				// Full. Dropping the subscriber rather than the event is the
				// whole policy: an event dropped here is a client that never
				// refetches, while a subscriber dropped here reconnects with
				// its Last-Event-ID and is replayed or reset.
				b.drop(s)
			}
		}
	}
}

// Subscribe implements [Source].
func (b *Broker) Subscribe(ctx context.Context, since uint64) (<-chan Delivery, error) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return nil, ErrBrokerClosed
	}

	s := &subscriber{ch: make(chan Delivery, b.buffer)}
	// Queued before the subscriber joins the fan-out, so that a replayed event
	// and a live one cannot arrive out of order. The buffer is sized to hold
	// the whole replay, so none of these can block while holding the lock.
	for _, d := range b.resume(since) {
		s.ch <- d
	}
	b.subs[s] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.unsubscribe(s)
	}()
	return s.ch, nil
}

// Close disconnects every subscriber and refuses further subscriptions.
// Publishing to a closed Broker is a no-op rather than an error, so a shutdown
// racing an in-flight after-commit callback does not turn into a logged
// failure on a write that succeeded.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.done = true
	for s := range b.subs {
		b.drop(s)
	}
}

// Subscribers reports how many streams are connected. It exists for tests and
// for a metric; nothing on the request path reads it.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// resume builds what a subscriber joining at position since should receive
// before it sees anything live: nothing, the events it missed, or a Reset.
//
// Called with the lock held.
func (b *Broker) resume(since uint64) []Delivery {
	switch {
	case since == 0:
		// A fresh connection. It is about to fetch the current state through
		// the ordinary endpoints, so there is nothing to catch up on and a
		// Reset would only make it fetch twice.
		return nil
	case since >= b.seq:
		// Already current. Also the case for a client whose Last-Event-ID is
		// from a previous process — the sequence restarted at zero, so it is
		// ahead of us, and every event we have is one it should see. Replaying
		// them is wrong (it has not seen them, but it is also about to be sent
		// everything from here on), so this reports a gap instead.
		if since > b.seq {
			return []Delivery{b.reset("the stream restarted and the requested position is not in it")}
		}
		return nil
	}

	oldest := b.seq - uint64(len(b.ring)) + 1
	if len(b.ring) == 0 || since+1 < oldest {
		return []Delivery{b.reset("the requested position is older than the events still held")}
	}

	missed := b.ring[since+1-oldest:]
	out := make([]Delivery, len(missed))
	copy(out, missed)
	return out
}

// reset is the delivery that stands in for events that cannot be replayed. It
// carries the current position, so a client that acts on it and reconnects asks
// to resume from where the stream actually is rather than from the gap.
//
// Called with the lock held.
func (b *Broker) reset(reason string) Delivery {
	return Delivery{ID: b.seq, Reset: &Reset{Reason: reason}}
}

// remember appends to the replay ring, trimming the front. Called with the
// lock held.
func (b *Broker) remember(d Delivery) {
	if b.history == 0 {
		return
	}
	b.ring = append(b.ring, d)
	if len(b.ring) > b.history {
		// Re-slice onto a fresh backing array rather than sliding the window,
		// so the ring cannot pin an ever-growing allocation.
		trimmed := make([]Delivery, b.history)
		copy(trimmed, b.ring[len(b.ring)-b.history:])
		b.ring = trimmed
	}
}

// drop disconnects a subscriber. Called with the lock held.
func (b *Broker) drop(s *subscriber) {
	if s.dead {
		return
	}
	s.dead = true
	close(s.ch)
	delete(b.subs, s)
}

func (b *Broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drop(s)
}
