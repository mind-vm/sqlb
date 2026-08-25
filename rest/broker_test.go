package rest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/rest"
)

func change(table, key string, op rest.Change) rest.Event {
	return rest.Event{Table: table, Key: key, Op: op}
}

// recv takes one delivery, failing the test rather than hanging the suite.
func recv(t *testing.T, ch <-chan rest.Delivery) (rest.Delivery, bool) {
	t.Helper()
	select {
	case d, open := <-ch:
		return d, open
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a delivery")
		return rest.Delivery{}, false
	}
}

// idle asserts that nothing more is queued. It is the other half of every
// "the subscriber received X" assertion: a test that only checks what arrived
// cannot tell a correct stream from one that also sent something else.
func idle(t *testing.T, ch <-chan rest.Delivery) {
	t.Helper()
	select {
	case d, open := <-ch:
		if !open {
			t.Fatal("the subscriber was disconnected, want it still connected")
		}
		t.Fatalf("an unexpected delivery arrived: %+v", d)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerFansOutToEverySubscriber(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	second, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	b.Publish(change("posts", "p1", rest.Created))

	for name, ch := range map[string]<-chan rest.Delivery{"first": first, "second": second} {
		d, open := recv(t, ch)
		if !open {
			t.Fatalf("%s: the channel closed", name)
		}
		if d.ID != 1 || d.Event != change("posts", "p1", rest.Created) {
			t.Errorf("%s: got %+v", name, d)
		}
	}
}

// A fresh connection is about to fetch the current state anyway, so it is told
// nothing rather than told to refetch.
func TestBrokerFreshSubscriberGetsNoBacklog(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	b.Publish(change("posts", "p1", rest.Created), change("posts", "p2", rest.Created))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	idle(t, ch)
}

func TestBrokerReplaysFromLastEventID(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{History: 8})
	b.Publish(
		change("posts", "p1", rest.Created),
		change("posts", "p2", rest.Created),
		change("posts", "p3", rest.Created),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 1) // saw event 1, missed 2 and 3
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	for _, want := range []uint64{2, 3} {
		d, open := recv(t, ch)
		if !open {
			t.Fatal("the channel closed during replay")
		}
		if d.Reset != nil {
			t.Fatalf("event %d was replaced by a reset: %+v", want, d.Reset)
		}
		if d.ID != want {
			t.Errorf("replayed id = %d, want %d", d.ID, want)
		}
	}

	// And the replay is followed by live events, in order.
	b.Publish(change("posts", "p4", rest.Created))
	if d, _ := recv(t, ch); d.ID != 4 {
		t.Errorf("live id = %d, want 4", d.ID)
	}
}

// The guard proven in both directions: one position inside the retained window
// replays, and the position one older than it resets.
func TestBrokerResetsOnlyWhenThePositionFellOutOfHistory(t *testing.T) {
	newBroker := func(t *testing.T) *rest.Broker {
		t.Helper()
		b := rest.NewBroker(rest.BrokerOptions{History: 2})
		// Five events, of which the broker still holds 4 and 5.
		for _, key := range []string{"p1", "p2", "p3", "p4", "p5"} {
			b.Publish(change("posts", key, rest.Created))
		}
		return b
	}

	t.Run("the oldest replayable position replays", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := newBroker(t).Subscribe(ctx, 3) // wants 4 onwards, and 4 is held
		if err != nil {
			t.Fatalf("subscribing: %v", err)
		}
		d, _ := recv(t, ch)
		if d.Reset != nil {
			t.Fatalf("a replayable position was reset: %+v", d.Reset)
		}
		if d.ID != 4 {
			t.Errorf("first replayed id = %d, want 4", d.ID)
		}
	})

	t.Run("one position older resets", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, err := newBroker(t).Subscribe(ctx, 2) // wants 3 onwards, and 3 is gone
		if err != nil {
			t.Fatalf("subscribing: %v", err)
		}
		d, _ := recv(t, ch)
		if d.Reset == nil {
			t.Fatalf("an unreplayable gap was delivered as an event: %+v", d)
		}
		// The reset carries the current position, so acting on it and
		// reconnecting asks to resume from where the stream is.
		if d.ID != 5 {
			t.Errorf("reset id = %d, want 5", d.ID)
		}
	})
}

// A Last-Event-ID from a previous process is ahead of a sequence that restarted
// at zero. Treating it as "already current" would drop everything since the
// restart without saying so.
func TestBrokerResetsAPositionAheadOfTheStream(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	b.Publish(change("posts", "p1", rest.Created))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 9000)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	d, _ := recv(t, ch)
	if d.Reset == nil {
		t.Fatalf("a position ahead of the stream was accepted: %+v", d)
	}
}

// A caught-up client reconnecting immediately should not be made to refetch.
func TestBrokerSaysNothingToACaughtUpSubscriber(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	b.Publish(change("posts", "p1", rest.Created))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 1)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	idle(t, ch)
}

func TestBrokerDropsASubscriberThatStopsKeepingUp(t *testing.T) {
	// History negative disables replay, which is what keeps Buffer at 1 rather
	// than being raised to hold a replay.
	b := rest.NewBroker(rest.BrokerOptions{Buffer: 1, History: -1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// Nothing is reading, so the second event finds the buffer full.
	b.Publish(change("posts", "p1", rest.Created))
	b.Publish(change("posts", "p2", rest.Created))

	if d, open := recv(t, ch); !open || d.ID != 1 {
		t.Fatalf("first delivery = %+v (open %v), want event 1", d, open)
	}
	if _, open := recv(t, ch); open {
		t.Error("the overflowing subscriber stayed connected; a dropped event is a client that never refetches")
	}
	if n := b.Subscribers(); n != 0 {
		t.Errorf("Subscribers() = %d after the drop, want 0", n)
	}
}

// The other direction: a subscriber inside its buffer is not disconnected.
func TestBrokerKeepsASubscriberInsideItsBuffer(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{Buffer: 4, History: -1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	b.Publish(change("posts", "p1", rest.Created))
	b.Publish(change("posts", "p2", rest.Created))

	for _, want := range []uint64{1, 2} {
		d, open := recv(t, ch)
		if !open {
			t.Fatalf("the subscriber was dropped at id %d while inside its buffer", want)
		}
		if d.ID != want {
			t.Errorf("id = %d, want %d", d.ID, want)
		}
	}
}

func TestBrokerUnsubscribesWhenTheClientGoesAway(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := b.Subscribe(ctx, 0); err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	if n := b.Subscribers(); n != 1 {
		t.Fatalf("Subscribers() = %d, want 1", n)
	}

	cancel()
	waitFor(t, "the subscriber to be released", func() bool { return b.Subscribers() == 0 })
}

func TestBrokerCloseDisconnectsAndRefuses(t *testing.T) {
	b := rest.NewBroker(rest.BrokerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	b.Close()

	if _, open := recv(t, ch); open {
		t.Error("Close left a subscriber connected")
	}
	if _, err := b.Subscribe(ctx, 0); !errors.Is(err, rest.ErrBrokerClosed) {
		t.Errorf("Subscribe after Close = %v, want ErrBrokerClosed", err)
	}
	// Publishing to a closed broker is a no-op rather than a panic: an
	// after-commit callback racing shutdown must not fail a durable write.
	b.Publish(change("posts", "p1", rest.Created))
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
