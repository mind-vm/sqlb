package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/rest"
)

// ErrDispatcherClosed reports a subscription to a Dispatcher that has been
// closed.
var ErrDispatcherClosed = errors.New("outbox: the dispatcher is closed")

const (
	defaultPoll      = 5 * time.Second
	defaultBuffer    = 256
	defaultBatch     = 512
	defaultMaxReplay = 1000
	defaultProbeWait = 10 * time.Second
	pruneEvery       = time.Hour
	listenBackoff    = time.Second
	maxListenBackoff = 30 * time.Second

	// probePayload distinguishes the dispatcher's own startup ring from the
	// trigger's. The trigger sends an empty payload; anything else on this
	// channel is not a change.
	probePayload = "sqlb-probe"
)

// DispatcherOptions configures a [Dispatcher].
type DispatcherOptions struct {
	// Options describes the table being tailed, and must match the Options the
	// [Outbox] writing it was built with.
	Options

	// Poll is how often the table is read in the absence of a notification.
	// Defaults to 5 seconds; it is a fallback, not the delivery mechanism.
	//
	// It exists because a lost notification must degrade to latency rather than
	// to lost data — a connection pooler in transaction mode swallows LISTEN
	// entirely ([ADR-0019]), and this is what keeps the feed correct there. See
	// [DispatcherOptions.OnError] for why that is reported rather than silently
	// absorbed.
	//
	// [ADR-0019]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#pgbouncer-in-the-path
	Poll time.Duration

	// Buffer is how many events may queue for one subscriber before the
	// dispatcher gives up on it and closes its channel. Defaults to 256.
	//
	// Dropping the subscriber rather than the event is [ADR-0045]'s policy and
	// this implementation keeps it: a dropped event is a client that stays wrong
	// forever, and a dropped connection is a client that reconnects, is replayed
	// from the table, and converges.
	//
	// [ADR-0045]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#the-stream-is-a-seam
	Buffer int

	// Batch is how many rows one tail query reads. Defaults to 512.
	Batch int

	// MaxReplay is the largest catch-up a reconnecting subscriber is given
	// before it is told to refetch instead. Defaults to 1000.
	//
	// Past some gap a replay stops being cheaper than the thing it saves. A
	// client resuming across forty thousand invalidations would receive forty
	// thousand messages and then refetch most of its views anyway, so beyond
	// this it gets one reset and does that directly.
	MaxReplay int

	// Replay, when false, refuses to catch a reconnecting subscriber up from
	// the table and answers every resumption with a reset.
	//
	// The default — replay enabled — is the reason to run an outbox at all: a
	// position means the same thing in every process, so a rolling restart costs
	// connected clients nothing instead of costing each of them a full refetch.
	// Turning it off trades that for never running the catch-up query.
	DisableReplay bool

	// StartAtBeginning dispatches the whole retained table on the first run
	// rather than starting at its head.
	//
	// The default starts at the head, because the rows already in the table were
	// delivered by whoever was running before and re-sending them invalidates
	// every client's world for no reason. Set this when the dispatcher is the
	// first one to run against a table that was already being written.
	StartAtBeginning bool
}

func (o *DispatcherOptions) applyDefaults() error {
	if err := o.Options.applyDefaults(); err != nil {
		return err
	}
	if o.Poll <= 0 {
		o.Poll = defaultPoll
	}
	if o.Buffer <= 0 {
		o.Buffer = defaultBuffer
	}
	if o.Batch <= 0 {
		o.Batch = defaultBatch
	}
	if o.MaxReplay <= 0 {
		o.MaxReplay = defaultMaxReplay
	}
	return nil
}

// Dispatcher tails the outbox table and fans it out to subscribers. It is the
// [rest.Source] that replaces [rest.Broker].
//
// It takes a *pgxpool.Pool rather than an [sqlb.Executor] because it needs one
// thing an Executor cannot express: a connection of its own to hold a `LISTEN`
// on. That connection is hijacked out of the pool rather than borrowed, so a
// session carrying a `LISTEN` is never returned for someone else's query.
//
// **The pool must reach Postgres directly.** [ADR-0019] measured PgBouncer in
// transaction pooling accepting a `LISTEN` and then silently never delivering
// on it, which leaves this working — at the poll interval — while looking
// broken to nobody. [Dispatcher.Run] probes for exactly that at startup and
// reports it through [Options.OnError].
//
// [ADR-0019]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#pgbouncer-in-the-path
type Dispatcher struct {
	pool *pgxpool.Pool
	opts DispatcherOptions

	tailSQL   string
	oldestSQL string
	headSQL   string
	listenSQL string

	// wake carries "something was inserted" from the LISTEN goroutine to the
	// tail loop. Capacity one and a non-blocking send: the doorbell says only
	// that there is work, so two rings before the door is answered are one.
	wake chan struct{}

	mu        sync.Mutex
	cursor    uint64
	subs      map[*dispatchSub]struct{}
	done      bool
	listening bool
	delivered uint64
	notified  uint64
	probed    uint64
}

// dispatchSub is one connected stream's live queue. dead is under the
// Dispatcher's mutex for the same reason [rest.Broker] holds it there: the
// fan-out and the subscriber's own goroutine both close this channel, and
// whichever arrives second must not close it again.
type dispatchSub struct {
	ch   chan rest.Delivery
	dead bool
}

// Stats is what a metric or a health check reads off a Dispatcher.
type Stats struct {
	// Cursor is the highest outbox id dispatched.
	Cursor uint64
	// Subscribers is how many streams are connected.
	Subscribers int
	// Listening reports whether the LISTEN connection is currently established.
	// It says nothing about whether notifications actually arrive on it, which
	// is the failure a pooled connection produces; Notifications is what answers
	// that.
	Listening bool
	// Delivered counts events fanned out since the process started.
	Delivered uint64
	// Notifications counts doorbells heard on the LISTEN connection.
	//
	// It is the metric to alert on. Listening true with this flat is a feed
	// running entirely on its fallback poll — correct, slow, and identical from
	// the outside to one that is working.
	Notifications uint64
}

// NewDispatcher returns a Dispatcher over pool, with its starting position
// already fixed.
//
// The position is read here rather than in [Run], and that is a correctness
// requirement rather than tidiness. Run is started in a goroutine, so a
// subscriber can attach before it has executed a line — and if Run then set the
// cursor to the head of the table, every event written in between would be
// behind the cursor and delivered to nobody. Fixing the position before the
// caller has a Dispatcher at all removes the window instead of narrowing it.
//
// It also means a missing outbox table fails here, where a caller is looking at
// an error, rather than arriving later through [Options.OnError] from a
// goroutine nobody is watching.
func NewDispatcher(ctx context.Context, pool *pgxpool.Pool, opts DispatcherOptions) (*Dispatcher, error) {
	if pool == nil {
		return nil, errors.New("outbox: NewDispatcher needs a *pgxpool.Pool")
	}
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	table := quoteIdent(opts.Table)
	d := &Dispatcher{
		pool: pool,
		opts: opts,
		tailSQL: fmt.Sprintf(
			`SELECT id, table_name, row_key, op, scope FROM %s WHERE id > $1 ORDER BY id LIMIT $2`, table),
		oldestSQL: fmt.Sprintf(`SELECT coalesce(min(id), 0) FROM %s`, table),
		headSQL:   fmt.Sprintf(`SELECT coalesce(max(id), 0) FROM %s`, table),
		// LISTEN takes an identifier, not a parameter, which is the other reason
		// Options.Channel is validated as one rather than trusted.
		listenSQL: "LISTEN " + quoteIdent(opts.Channel),
		wake:      make(chan struct{}, 1),
		subs:      make(map[*dispatchSub]struct{}),
	}

	if !opts.StartAtBeginning {
		if err := pool.QueryRow(ctx, d.headSQL).Scan(&d.cursor); err != nil {
			return nil, fmt.Errorf("outbox: reading the head of %s: %w", opts.Table, err)
		}
	}
	return d, nil
}

// MustDispatcher is NewDispatcher for a startup path with nowhere to put an
// error.
func MustDispatcher(d *Dispatcher, err error) *Dispatcher {
	if err != nil {
		panic(err)
	}
	return d
}

// Run tails the table until ctx is cancelled. It blocks, and is meant to be the
// body of a goroutine started once at startup.
//
// It returns ctx.Err() and nothing else: the one failure it could not proceed
// through — establishing the starting position — happened in [NewDispatcher],
// and everything from here is transient by nature. A poll that failed or a
// LISTEN that dropped goes to [Options.OnError] while the loop keeps going,
// because a dispatcher that gave up on a failed query would be a feed that
// stops on the first blip and says so only to whoever reads a returned error.
func (d *Dispatcher) Run(ctx context.Context) error {
	defer d.Close()

	go d.listen(ctx)

	poll := time.NewTicker(d.opts.Poll)
	defer poll.Stop()

	var pruning <-chan time.Time
	if d.opts.Retention >= 0 {
		p := time.NewTicker(pruneEvery)
		defer p.Stop()
		pruning = p.C
	}

	// A first pass before waiting on anything: a dispatcher told to start at the
	// beginning has a table to deliver, and one that restarted has whatever
	// landed while it was gone.
	d.drain(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.wake:
			d.drain(ctx)
		case <-poll.C:
			d.drain(ctx)
		case <-pruning:
			if _, err := prune(ctx, d.pool, d.opts.Table, d.opts.Retention); err != nil && ctx.Err() == nil {
				d.opts.report(err)
			}
		}
	}
}

// Subscribe implements [rest.Source].
func (d *Dispatcher) Subscribe(ctx context.Context, since uint64) (<-chan rest.Delivery, error) {
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return nil, ErrDispatcherClosed
	}
	s := &dispatchSub{ch: make(chan rest.Delivery, d.opts.Buffer)}
	// Captured under the same lock that registers the subscriber, so that every
	// event is either in the replay or in the live queue and none is in both
	// gaps. Live deliveries at or below it are dropped by serve, because the
	// replay already carried them.
	start := d.cursor
	d.subs[s] = struct{}{}
	d.mu.Unlock()

	out := make(chan rest.Delivery)
	go d.serve(ctx, s, out, since, start)
	return out, nil
}

// serve is one subscriber: the catch-up first, then the live queue.
//
// out is unbuffered. A subscriber that stops reading therefore backs up into
// s.ch, and when that fills the fan-out drops it — which closes s.ch, ends the
// range below, closes out and ends the stream. That is the intended path for a
// slow client, not an edge case.
func (d *Dispatcher) serve(ctx context.Context, s *dispatchSub, out chan rest.Delivery, since, start uint64) {
	defer close(out)
	defer d.unsubscribe(s)

	for _, del := range d.replay(ctx, since, start) {
		select {
		case out <- del:
		case <-ctx.Done():
			return
		}
	}

	for del := range s.ch {
		// Already replayed. A Reset is never suppressed: it carries the position
		// it was issued at rather than describing one event, and swallowing one
		// leaves a client believing in a stream it is not on.
		if del.Reset == nil && del.ID <= start {
			continue
		}
		select {
		case out <- del:
		case <-ctx.Done():
			return
		}
	}
}

// replay is what a reconnecting subscriber receives before it sees anything
// live: nothing, the events it missed read back out of the table, or a reset.
//
// Reading them from the table rather than from a ring in memory is the whole
// point of the outbox. A client whose Last-Event-ID predates a deployment is
// caught up by the process that replaced the one it was talking to, because the
// position is a row id and not a per-process counter.
func (d *Dispatcher) replay(ctx context.Context, since, start uint64) []rest.Delivery {
	switch {
	case since == 0:
		// A fresh connection is about to fetch current state through the
		// ordinary endpoints. There is nothing to catch up on, and a reset would
		// only make it fetch twice.
		return nil
	case since > start:
		// A position this stream has never issued: a client holding an id from a
		// Broker's per-process counter, or from before the database was
		// restored. Replaying from here would look like a working stream while
		// skipping whatever the discrepancy hides.
		return []rest.Delivery{resetAt(start, "the stream restarted and the requested position is not in it")}
	case since == start:
		return nil
	case d.opts.DisableReplay:
		return []rest.Delivery{resetAt(start, "the stream does not replay")}
	case start-since > uint64(d.opts.MaxReplay):
		return []rest.Delivery{resetAt(start, "the requested position is too far behind to replay")}
	}

	var oldest uint64
	if err := d.pool.QueryRow(ctx, d.oldestSQL).Scan(&oldest); err != nil {
		// A cancelled context here is the subscriber going away mid-catch-up,
		// which is a disconnection and not a finding.
		if ctx.Err() == nil {
			d.opts.report(fmt.Errorf("outbox: reading the oldest retained event: %w", err))
		}
		return []rest.Delivery{resetAt(start, "the retained events could not be read")}
	}
	// oldest == 0 is an empty table, which with start > since means every event
	// the client is asking about has been pruned.
	if oldest == 0 || since+1 < oldest {
		return []rest.Delivery{resetAt(start, "the requested position is older than the events still held")}
	}

	// The subtraction is bounded by the MaxReplay check above, so the conversion
	// cannot truncate.
	rows, err := d.read(ctx, since, int32(start-since))
	if err != nil {
		if ctx.Err() == nil {
			d.opts.report(fmt.Errorf("outbox: replaying from %d: %w", since, err))
		}
		return []rest.Delivery{resetAt(start, "the retained events could not be read")}
	}
	// Bound the replay by start as well as by count: anything above it is
	// arriving on the live queue.
	out := make([]rest.Delivery, 0, len(rows))
	for _, del := range rows {
		if del.ID > start {
			break
		}
		out = append(out, del)
	}
	return out
}

// drain reads everything past the cursor and fans it out, in batches, until the
// table has nothing further.
func (d *Dispatcher) drain(ctx context.Context) {
	for ctx.Err() == nil {
		d.mu.Lock()
		cursor := d.cursor
		closed := d.done
		d.mu.Unlock()
		if closed {
			return
		}

		batch, err := d.read(ctx, cursor, int32(d.opts.Batch))
		if err != nil {
			if ctx.Err() == nil {
				d.opts.report(fmt.Errorf("outbox: tailing %s: %w", d.opts.Table, err))
			}
			return
		}
		if len(batch) == 0 {
			return
		}
		d.fanout(batch)
		if len(batch) < d.opts.Batch {
			return
		}
	}
}

// read returns up to limit deliveries after since, in id order.
func (d *Dispatcher) read(ctx context.Context, since uint64, limit int32) ([]rest.Delivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, d.tailSQL, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rest.Delivery
	for rows.Next() {
		var (
			id                       uint64
			table, key, op, scopeVal string
		)
		if err := rows.Scan(&id, &table, &key, &op, &scopeVal); err != nil {
			return nil, err
		}
		out = append(out, rest.Delivery{
			ID: id,
			Event: rest.Event{
				Table: table,
				Key:   key,
				Op:    rest.Change(op),
				Scope: scopeVal,
			},
		})
	}
	return out, rows.Err()
}

// fanout advances the cursor and queues the batch for every subscriber.
//
// The cursor moves under the same lock that reads the subscriber set, so a
// Subscribe interleaving with a fan-out either captures the position before the
// batch — and receives it live — or after it, and receives it in the replay.
func (d *Dispatcher) fanout(batch []rest.Delivery) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	for _, del := range batch {
		if del.ID > d.cursor {
			d.cursor = del.ID
		}
		d.delivered++
		for s := range d.subs {
			select {
			case s.ch <- del:
			default:
				// Full. Dropping the subscriber rather than the event: the
				// subscriber reconnects with its Last-Event-ID and is replayed
				// out of the table, where a dropped event would be a client that
				// never learns the row changed.
				d.drop(s)
			}
		}
	}
}

// listen holds a connection with a LISTEN on it and rings the doorbell,
// reconnecting with backoff when it drops.
func (d *Dispatcher) listen(ctx context.Context) {
	backoff := listenBackoff
	for ctx.Err() == nil {
		err := d.listenOnce(ctx)
		d.setListening(false)
		if ctx.Err() != nil {
			return
		}
		d.opts.report(fmt.Errorf("outbox: the LISTEN connection dropped, falling back to polling every %s: %w",
			d.opts.Poll, err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxListenBackoff {
			backoff *= 2
		}
	}
}

// listenOnce establishes one LISTEN and pumps notifications until it fails.
func (d *Dispatcher) listenOnce(ctx context.Context) error {
	pooled, err := d.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection: %w", err)
	}
	// Hijacked rather than released: a session carrying a LISTEN must never go
	// back into the pool for someone else's query, and a pool that handed one
	// out would deliver notifications to whatever ran next.
	conn := pooled.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, d.listenSQL); err != nil {
		return fmt.Errorf("issuing %s: %w", d.listenSQL, err)
	}
	d.setListening(true)
	go d.probe(ctx)

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		d.mu.Lock()
		if n.Payload == probePayload {
			// The startup probe's own ring. Counted apart from the trigger's,
			// and deliberately not a wake: it says nothing about the table, and
			// a Notifications count that included it would report a working
			// doorbell on a database whose trigger had been dropped.
			d.probed++
		} else {
			d.notified++
		}
		payload := n.Payload
		d.mu.Unlock()

		if payload == probePayload {
			continue
		}
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
}

// probe answers the question a working-looking dispatcher cannot otherwise
// answer: are notifications actually arriving, or is this feed running entirely
// on its fallback poll?
//
// [ADR-0019] measured PgBouncer in transaction pooling accepting a LISTEN and
// silently never delivering on it. ADR-0012 named the consequence — "delivery
// latency sits at the poll interval rather than spiking to it" — and asked for a
// startup assertion. This is it: ring the doorbell from a different connection,
// and see whether this one hears it.
//
// It reports rather than fails. A dispatcher on a pooled connection is correct
// and slow, which is worth a loud complaint and is not worth refusing to serve.
//
// [ADR-0019]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#pgbouncer-in-the-path
// What it observes is the notification counter, which the LISTEN loop
// increments — a direct reading rather than an inference from whether the tail
// loop happened to wake. An earlier version guessed, from the depth of the wake
// channel and the delivered count, and guessed wrong under load in both
// directions: it is the counter that makes this a check rather than a hint.
func (d *Dispatcher) probe(ctx context.Context) {
	before := d.probes()

	// pg_notify rather than NOTIFY, because the channel is a value here and
	// NOTIFY takes an identifier.
	if _, err := d.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, d.opts.Channel, probePayload); err != nil {
		// A cancelled context is a shutdown, not a finding. Reporting it would
		// mean every clean stop logged a failure, which is how an OnError an
		// application actually reads becomes one it filters.
		if ctx.Err() == nil {
			d.opts.report(fmt.Errorf("outbox: probing the notification channel: %w", err))
		}
		return
	}

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(defaultProbeWait)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if d.probes() != before {
				return
			}
		case <-deadline:
			if d.probes() != before || ctx.Err() != nil {
				return
			}
			d.opts.report(fmt.Errorf(
				"outbox: LISTEN on %q was accepted but no notification arrived within %s — "+
					"this dispatcher is running on its %s fallback poll. A connection pooler in "+
					"transaction mode swallows LISTEN (ADR-0019); give the dispatcher a pool that "+
					"reaches Postgres directly",
				d.opts.Channel, defaultProbeWait, d.opts.Poll))
			return
		}
	}
}

// Close disconnects every subscriber and refuses further subscriptions.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	d.done = true
	for s := range d.subs {
		d.drop(s)
	}
}

// Stats reports what the dispatcher is doing. It exists for a metric and a
// health check; nothing on the request path reads it.
func (d *Dispatcher) Stats() Stats { return d.stats() }

func (d *Dispatcher) stats() Stats {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Stats{
		Cursor:        d.cursor,
		Subscribers:   len(d.subs),
		Listening:     d.listening,
		Delivered:     d.delivered,
		Notifications: d.notified,
	}
}

// probes is how many of this dispatcher's own startup rings have come back.
func (d *Dispatcher) probes() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.probed
}

func (d *Dispatcher) setListening(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.listening = v
}

// drop disconnects a subscriber. Called with the lock held.
func (d *Dispatcher) drop(s *dispatchSub) {
	if s.dead {
		return
	}
	s.dead = true
	close(s.ch)
	delete(d.subs, s)
}

func (d *Dispatcher) unsubscribe(s *dispatchSub) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drop(s)
}

// resetAt is the delivery that stands in for events that cannot be replayed. It
// carries the position the stream is actually at, so a client that acts on it
// and reconnects resumes from there rather than from the gap.
func resetAt(pos uint64, reason string) rest.Delivery {
	return rest.Delivery{ID: pos, Reset: &rest.Reset{Reason: reason}}
}
