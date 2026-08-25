package pgtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/outbox"
	"github.com/mind-vm/sqlb/rest"
)

// The durable half of the change feed, against a real Postgres, which is the
// only place most of it can be tested at all: that the event and the row commit
// together, that a rolled-back write records nothing, that ids come out in
// commit order under concurrency, and that a subscriber is replayed by a
// *different* dispatcher from the one it was talking to.
//
// pgtest/events_test.go is the same contract over rest.Broker. The two suites
// asserting the same wire behaviour is the point of the seam: ADR-0045 says
// swapping the source changes no client, and this is where that is checked
// rather than claimed.

// outboxFixture is a notes server whose writes record into an outbox, with a
// dispatcher tailing it.
type outboxFixture struct {
	server *httptest.Server
	pool   *pgxpool.Pool
	disp   *outbox.Dispatcher
	errs   *errSink
}

// errSink collects what the outbox could not return to anyone. A test that
// leaves failures in here unexamined would be a test passing over a broken
// dispatcher, so the fixture checks it is empty at the end.
type errSink struct {
	mu   sync.Mutex
	seen []error
}

func (s *errSink) add(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, err)
}

func (s *errSink) all() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.seen...)
}

// outboxNotesDB creates the notes table and the outbox beside it.
func outboxNotesDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := freshStockDB(t)
	// The same DEFERRABLE constraint events_test.go uses, and for the same
	// reason: it is what produces a rollback *after* a successful INSERT and a
	// hook that already ran, which is the only ordering that can produce a
	// phantom event.
	mustExec(t, pool, `
		CREATE TABLE notes (
			id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			body text   NOT NULL,
			CONSTRAINT notes_body_key UNIQUE (body) DEFERRABLE INITIALLY DEFERRED
		)
	`)
	if err := outbox.Install(context.Background(), pool, outbox.Options{}); err != nil {
		t.Fatalf("installing the outbox: %v", err)
	}
	return pool
}

// newOutboxFixture stands up the whole path: real Postgres, generated handlers
// recording into the outbox, a dispatcher tailing it, and the SSE endpoint over
// that dispatcher.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	pool := outboxNotesDB(t)
	f := &outboxFixture{pool: pool, errs: &errSink{}}
	f.disp = f.runDispatcher(t, outbox.DispatcherOptions{})

	ob, err := outbox.New(pool, outbox.Options{OnError: f.errs.add})
	if err != nil {
		t.Fatalf("building the outbox: %v", err)
	}

	scoped := sqlb.NewRegistry()
	if err := rest.PublishChanges[Note](scoped, ob); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	db := sqlb.New(pool).WithHooks(scoped)

	srv := rest.NewServer(rest.Config{Title: "Notes", Version: "1.0.0"})
	if err := rest.Resource[Note, NoteCreate, rest.None[Note]](srv.API, db, rest.Options{
		Path: "/notes",
		Name: "note",
		Ops:  rest.OpCreate | rest.OpRead | rest.OpDelete | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	if err := rest.Events(srv.API, rest.EventsOptions{Source: f.disp}); err != nil {
		t.Fatalf("mounting the events endpoint: %v", err)
	}

	f.server = httptest.NewServer(srv.Handler)
	t.Cleanup(f.server.Close)
	t.Cleanup(func() {
		// A dispatcher that reported a failure and still passed its test is a
		// test that proved nothing about the dispatcher.
		for _, err := range f.errs.all() {
			t.Errorf("the outbox reported: %v", err)
		}
	})
	return f
}

// runDispatcher starts a dispatcher over the fixture's pool and stops it when
// the test ends. A short poll interval so a test never waits five seconds for
// the fallback — the notification path is what should deliver, and the tests
// below say so by not tolerating the poll's latency.
func (f *outboxFixture) runDispatcher(t *testing.T, opts outbox.DispatcherOptions) *outbox.Dispatcher {
	t.Helper()
	opts.OnError = f.errs.add
	if opts.Poll == 0 {
		opts.Poll = 250 * time.Millisecond
	}
	d, err := outbox.NewDispatcher(context.Background(), f.pool, opts)
	if err != nil {
		t.Fatalf("building the dispatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return d
}

func (f *outboxFixture) post(t *testing.T, body string) Note {
	t.Helper()
	status, raw := postJSON(t, f.server.URL+"/notes", map[string]any{"body": body})
	if status != http.StatusCreated {
		t.Fatalf("POST /notes %q: status %d: %s", body, status, raw)
	}
	var created Note
	decodeInto(t, raw, &created)
	return created
}

// outboxRows reads the table directly, which is what makes "recorded in the
// same transaction" checkable rather than inferred from what a subscriber saw.
func outboxRows(t *testing.T, pool *pgxpool.Pool) []rest.Event {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT table_name, row_key, op, scope FROM sqlb_outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	defer rows.Close()

	var out []rest.Event
	for rows.Next() {
		var e rest.Event
		var op string
		if err := rows.Scan(&e.Table, &e.Key, &op, &e.Scope); err != nil {
			t.Fatalf("scanning the outbox: %v", err)
		}
		e.Op = rest.Change(op)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	return out
}

// The claim ADR-0012 is about: the event is in the table, written by the
// transaction that wrote the row, before anything fanned anything out.
func TestOutboxRecordsTheWrite(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	created := f.post(t, "hello")

	got := outboxRows(t, f.pool)
	want := []rest.Event{{Table: "notes", Key: itoa(created.ID), Op: rest.Created}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("outbox = %+v, want %+v", got, want)
	}
}

// The guard that makes the feed worth trusting, and the one an at-most-once
// broker gets right for a different reason. Here the event is written *inside*
// the transaction, so what must be true is that the failing COMMIT takes the
// event with it — the same rollback that discards the note discards the row that
// would have announced it.
//
// Three writes, of which the middle one aborts on the deferred constraint. The
// assertion is on the keys: the aborted insert consumes an identity value, so a
// leaked event would carry a key no note has.
func TestOutboxRecordsNothingWhenTheCommitFails(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	first := f.post(t, "taken")

	if status, body := postJSON(t, f.server.URL+"/notes", map[string]any{"body": "taken"}); status < 400 {
		t.Fatalf("the duplicate was accepted: status %d: %s", status, body)
	}

	third := f.post(t, "third")

	got := outboxRows(t, f.pool)
	want := []rest.Event{
		{Table: "notes", Key: itoa(first.ID), Op: rest.Created},
		{Table: "notes", Key: itoa(third.ID), Op: rest.Created},
	}
	if len(got) != len(want) {
		t.Fatalf("outbox = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("outbox[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// End to end over the same endpoint and the same wire format the Broker serves,
// with the dispatcher as the source.
func TestOutboxDispatcherFeedsTheStream(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	events := openEventStream(t, f.server.URL+"/events")
	waitUntil(t, "the subscription to register", func() bool { return f.disp.Stats().Subscribers == 1 })

	created := f.post(t, "hello")

	got := events.nextChange(t)
	if want := (rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Created}); got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}

	// The whole contract: the key refetches through the ordinary endpoint.
	if fetched := getJSON(t, f.server.URL+"/notes/"+got.Key); fetched["body"] != "hello" {
		t.Errorf("refetching the key from the event gave %v", fetched)
	}
}

// The payoff, and the thing a Broker structurally cannot do: a client resumes
// against a dispatcher that never saw the events it missed, because the position
// is a row id rather than a per-process counter.
//
// The first dispatcher is stopped, three writes happen with nothing tailing, a
// second dispatcher starts, and a subscriber resuming from position 1 is handed
// 2, 3 and 4 out of the table.
func TestOutboxReplaysAcrossADispatcherRestart(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	f.post(t, "first")
	waitUntil(t, "the first event to be dispatched", func() bool { return f.disp.Stats().Cursor >= 1 })

	// Everything from here lands in the table with nothing fanning it out.
	f.disp.Close()
	for _, body := range []string{"second", "third", "fourth"} {
		f.post(t, body)
	}

	// A dispatcher that has never seen any of it, started from the head — so
	// anything this subscriber receives came out of the table rather than off
	// the live path.
	fresh := f.runDispatcher(t, outbox.DispatcherOptions{})
	waitUntil(t, "the replacement dispatcher to reach the head", func() bool { return fresh.Stats().Cursor >= 4 })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := fresh.Subscribe(ctx, 1)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	var keys []string
	for i := 0; i < 3; i++ {
		select {
		case d, open := <-ch:
			if !open {
				t.Fatalf("the stream closed after %d of 3 replayed events", i)
			}
			if d.Reset != nil {
				t.Fatalf("replay %d was a reset (%s); the events were still retained", i, d.Reset.Reason)
			}
			if d.ID != uint64(i+2) {
				t.Errorf("replay %d arrived at position %d, want %d", i, d.ID, i+2)
			}
			keys = append(keys, d.Event.Key)
		case <-ctx.Done():
			t.Fatalf("timed out after %d of 3 replayed events", i)
		}
	}
	if len(keys) != 3 {
		t.Fatalf("replayed %d events, want 3", len(keys))
	}
}

// A fresh connection is not replayed: it is about to fetch current state through
// the ordinary endpoints, so catching it up would only make it fetch twice.
// (ADR-0016: the replay above is only meaningful if this does not happen.)
func TestOutboxDoesNotReplayAFreshConnection(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	f.post(t, "before")
	waitUntil(t, "the event to be dispatched", func() bool { return f.disp.Stats().Cursor >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := f.disp.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	select {
	case d := <-ch:
		t.Fatalf("a fresh subscriber was sent %+v", d)
	case <-time.After(500 * time.Millisecond):
	}
}

// Retention is a delivery setting before it is a disk setting: a position that
// has been pruned away cannot be replayed, and the subscriber has to be told to
// refetch rather than handed a stream with a hole in it.
func TestOutboxResetsAPositionThatHasBeenPruned(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	for _, body := range []string{"one", "two", "three"} {
		f.post(t, body)
	}
	waitUntil(t, "the events to be dispatched", func() bool { return f.disp.Stats().Cursor >= 3 })

	// Retention zero prunes everything already written, which is what a client
	// disconnected for longer than the window comes back to.
	ob, err := outbox.New(f.pool, outbox.Options{Retention: time.Nanosecond})
	if err != nil {
		t.Fatalf("building the outbox: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if n, err := ob.Prune(context.Background()); err != nil || n != 3 {
		t.Fatalf("Prune = %d, %v; want 3 rows removed", n, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := f.disp.Subscribe(ctx, 1)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	select {
	case d, open := <-ch:
		if !open {
			t.Fatal("the stream closed instead of resetting")
		}
		if d.Reset == nil {
			t.Fatalf("delivery = %+v, want a reset: position 1 was pruned", d)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the reset")
	}
}

// The guard the advisory lock exists for, and the reason it is worth what it
// costs.
//
// Twenty concurrent writes. A bare sequence would let two transactions take ids
// 5 and 6 and commit in the other order, and a dispatcher tailing `id > cursor`
// would advance past 5 while 5 was still uncommitted — losing it, silently, with
// nothing failing. The assertion is that every id from 1 to 20 arrives, in
// order: no gap, no reordering.
//
// Proven the other way by removing the lock from Outbox.write, which fails this
// test and no other (ADR-0016).
func TestOutboxIdOrderIsCommitOrder(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	const writes = 20
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ch, err := f.disp.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	waitUntil(t, "the subscription to register", func() bool { return f.disp.Stats().Subscribers == 1 })

	var wg sync.WaitGroup
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, body := postJSON(t, f.server.URL+"/notes", map[string]any{"body": itoa(int64(i))})
			if status != http.StatusCreated {
				t.Errorf("POST %d: status %d: %s", i, status, body)
			}
		}(i)
	}
	wg.Wait()

	var positions []uint64
	for len(positions) < writes {
		select {
		case d, open := <-ch:
			if !open {
				t.Fatalf("the stream closed after %d of %d events", len(positions), writes)
			}
			if d.Reset != nil {
				t.Fatalf("a reset arrived mid-stream: %s", d.Reset.Reason)
			}
			positions = append(positions, d.ID)
		case <-ctx.Done():
			t.Fatalf("timed out after %d of %d events; the outbox holds %d",
				len(positions), writes, len(outboxRows(t, f.pool)))
		}
	}

	// Strictly increasing, and covering every id: a skipped id is an event a
	// client would never hear about, and an out-of-order one is an id a
	// reconnecting client would resume past.
	if !sort.SliceIsSorted(positions, func(i, j int) bool { return positions[i] < positions[j] }) {
		t.Errorf("positions arrived out of order: %v", positions)
	}
	for i, pos := range positions {
		if pos != uint64(i+1) {
			t.Fatalf("positions = %v; position %d is %d, want %d — the tail skipped an id", positions, i, pos, i+1)
		}
	}
}

// The doorbell is doing the work, not the fallback poll.
//
// This is the guard for the failure ADR-0012 warns can hide: a `LISTEN` that is
// accepted and never delivers leaves the feed *correct*, because the poll covers
// for it, so every other test in this file would still pass. The poll here is an
// hour, which means anything that arrives promptly arrived because the trigger
// rang and this connection heard it.
//
// It is also the positive half of the dispatcher's startup probe. The probe
// complains when the counter stays flat; this asserts it does not.
func TestOutboxDeliversByNotificationNotByPolling(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	d := f.runDispatcher(t, outbox.DispatcherOptions{Poll: time.Hour})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ch, err := d.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	// Both conditions, and the second is the one that matters here. A NOTIFY
	// reaches only the sessions listening when it fires, so a write that beats
	// the LISTEN is delivered by the poll — which this test has moved an hour
	// away. That is a real property of the dispatcher and not only of the test:
	// a process that writes the instant it starts waits a poll interval.
	waitUntil(t, "the subscription and the LISTEN to be established", func() bool {
		s := d.Stats()
		return s.Subscribers == 1 && s.Listening
	})

	created := f.post(t, "hello")

	select {
	case del, open := <-ch:
		if !open {
			t.Fatal("the stream closed")
		}
		if want := (rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Created}); del.Event != want {
			t.Errorf("event = %+v, want %+v", del.Event, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing arrived within 10s and the poll is an hour away: the notification never landed")
	}

	if n := d.Stats().Notifications; n == 0 {
		t.Errorf("Notifications = 0, so the event came from somewhere other than the doorbell")
	}
}

// A subscriber that attaches before Run has executed a line still receives what
// is written next.
//
// This is a regression test for a startup race that only appeared under -race:
// Run is started in a goroutine, so a subscriber can attach before it runs, and
// while Run was the thing that read the head of the table it would then set the
// cursor past events written in between — which were behind the cursor, so
// nothing ever fanned them out and the subscriber waited forever. Fixing the
// position in NewDispatcher closes the window rather than narrowing it, and this
// is what says so.
func TestOutboxDeliversToASubscriberThatBeatTheRunLoop(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)

	d, err := outbox.NewDispatcher(context.Background(), f.pool, outbox.DispatcherOptions{
		Options: outbox.Options{OnError: f.errs.add},
		Poll:    250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("building the dispatcher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Subscribed and written to before Run is ever called.
	ch, err := d.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	created := f.post(t, "written before the loop started")

	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })

	select {
	case del, open := <-ch:
		if !open {
			t.Fatal("the stream closed")
		}
		if want := (rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Created}); del.Event != want {
			t.Errorf("event = %+v, want %+v", del.Event, want)
		}
	case <-ctx.Done():
		t.Fatalf("nothing arrived; the cursor moved past an event nobody was sent (stats %+v)", d.Stats())
	}
}

// Two dispatchers over one table both deliver, which is the horizontal-scaling
// claim: a write served by one replica reaches subscribers connected to another.
// A Broker cannot do this at all — its subscribers are the ones in its own
// process.
func TestOutboxServesTwoDispatchersFromOneWrite(t *testing.T) {
	t.Parallel()
	f := newOutboxFixture(t)
	second := f.runDispatcher(t, outbox.DispatcherOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a, err := f.disp.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing to the first dispatcher: %v", err)
	}
	b, err := second.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("subscribing to the second dispatcher: %v", err)
	}
	waitUntil(t, "both subscriptions to register", func() bool {
		return f.disp.Stats().Subscribers == 1 && second.Stats().Subscribers == 1
	})

	created := f.post(t, "hello")
	want := rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Created}

	for name, ch := range map[string]<-chan rest.Delivery{"first": a, "second": b} {
		select {
		case d, open := <-ch:
			if !open {
				t.Fatalf("the %s dispatcher closed its stream", name)
			}
			if d.Event != want {
				t.Errorf("the %s dispatcher delivered %+v, want %+v", name, d.Event, want)
			}
		case <-ctx.Done():
			t.Fatalf("the %s dispatcher delivered nothing; first=%+v second=%+v, outbox holds %d",
				name, f.disp.Stats(), second.Stats(), len(outboxRows(t, f.pool)))
		}
	}
}
