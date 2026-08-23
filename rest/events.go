package rest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

// Change names what happened to a row.
//
// It is deliberately not [Op]. Op is a bitmask of the operations a resource
// *exposes*, and a change is one thing that happened to one row — a mask whose
// String is "create|update" would be meaningful as exposure and meaningless
// here. Only three of Op's five members can ever reach a change feed anyway:
// reading a row does not change it.
type Change string

const (
	Created Change = "create"
	Updated Change = "update"
	Deleted Change = "delete"
)

// Event is one invalidation: a row changed, and anything displaying it is now
// stale.
//
// It carries no row data, which is [ADR-0012]'s decision rather than an
// omission. A payload would have to be produced per subscriber, under that
// subscriber's context, or the resource's BeforeQuery scope would not apply to
// it — and a change feed that skips the scope hands one tenant's rows to
// another. Sending the address of the change instead keeps the read path the
// only thing that ever reads, so every rule the read path enforces still holds.
//
// The generated clients emit the other half of this. A TypeScript client
// receiving `{table: "posts", key: "42"}` resolves it through `keysByTable`
// into the cached queries that read it and refetches them through the ordinary
// GET endpoints — `subscribeChanges` does the whole of that — and a Dart one
// narrows the table to a `TableName` member with `TableChange.from`. Neither is
// required: the wire is three fields, and a hand-written subscriber against
// `EventSource` is a dozen lines.
//
// [ADR-0012]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#change-feed-outbox
type Event struct {
	// Table is the SQL table name, matching what the generated client's
	// keysByTable is keyed by.
	Table string `json:"table" doc:"SQL table the change happened in"`

	// Key is the primary key of the row, rendered as a string the way the URL
	// renders it, so that it concatenates onto the resource path.
	//
	// It is empty when the change was not attributable to one row, which an
	// empty Key means "something in this table changed" — invalidate the
	// collection.
	//
	// A delete used to be exactly that case, because sqlb's AfterDelete hook
	// receives a count rather than the rows it removed. It no longer is:
	// [PublishChanges] registers [sqlb.Hooks.AfterDeleteRows] and publishes one
	// event per removed row, key and all (#144). A hand-written publisher on the
	// count hook still produces the keyless form, and a subscriber has to keep
	// handling it.
	Key string `json:"key,omitempty" doc:"Primary key of the row, or empty when the whole table is invalidated"`

	// Op is what happened.
	Op Change `json:"op" enum:"create,update,delete" doc:"What happened to the row"`

	// Scope is the value of the column the model declared `scope`, when it
	// declared one: the tenant the changed row belonged to.
	//
	// It is **not on the wire**. It exists so that [EventsOptions.Filter] can
	// answer the one question a multi-tenant deployment has to answer about
	// this stream — is this event mine — without the endpoint hard-coding what
	// a tenant is. A subscriber gains nothing from being told its own tenant
	// id, and putting it on the wire would enlarge a contract ADR-0045 records
	// as the expensive half to change.
	//
	// It is empty when the model declares no scope. It used to be empty on every
	// delete as well, which meant a tenant filter had to let deletes through to
	// everyone; [PublishChanges] now reads the removed rows and a delete carries
	// its scope like any other change (#144).
	//
	// A Filter comparing scopes still has to decide what an empty one means,
	// because a hand-written publisher may produce one. The safe reading is that
	// such an event identifies nothing and may go to everyone, since it names
	// neither a row nor a tenant.
	Scope string `json:"-"`
}

// Reset tells a subscriber that its position in the stream could not be
// honoured and it should refetch everything it displays.
//
// It is what makes a dropped event safe. Every other failure mode in this file
// — a slow client, a restart, a gap longer than the replay history — is
// converted into one of these rather than into a silently missing invalidation,
// because a client that never learns a row changed shows stale data forever,
// while a client told to refetch is merely doing extra work.
type Reset struct {
	Reason string `json:"reason" doc:"Why the stream could not resume"`
}

// Delivery is one event as a [Source] hands it to a subscriber, carrying the
// stream position the client echoes back in Last-Event-ID.
type Delivery struct {
	// ID is the position. It increases by one per event within a Source and is
	// what a reconnecting client asks to resume after.
	ID uint64

	// Event is the change. It is the zero Event when Reset is set.
	Event Event

	// Reset, when non-nil, replaces the events the subscriber missed with an
	// instruction to refetch.
	Reset *Reset
}

// Source is where an event stream gets its events.
//
// This is the seam. [Broker] implements it in-process, which is what ships
// today and is honest about its limits (see its documentation). The outbox
// dispatcher [ADR-0012] describes — durable, at-least-once, correct across
// replicas — implements the same two-method contract and replaces the Broker
// without the endpoint, the wire format or any client changing.
//
// [ADR-0012]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#change-feed-outbox
type Source interface {
	// Subscribe returns the channel this subscriber's deliveries arrive on.
	//
	// since is the position the client last saw, from Last-Event-ID, or zero
	// for a fresh connection. A Source that can replay from that position
	// should; one that cannot must open the stream with a Delivery carrying a
	// Reset, so the gap is announced rather than skipped.
	//
	// The channel is closed when ctx is done, and may also be closed by the
	// Source to disconnect a subscriber it can no longer keep up with. A
	// closed channel ends the stream; the client reconnects on its own.
	Subscribe(ctx context.Context, since uint64) (<-chan Delivery, error)
}

// Publisher is what a write announces itself to. [Broker] is one; a test
// double or an adapter onto an existing message bus is another.
//
// It takes no context and returns no error because [Broker] is in-process and
// at-most-once: the write is already durable when it is called, and a change
// feed that could fail a committed request would be worse than one that drops an
// event and tells the client that missed it. A publisher that can do better than
// at-most-once implements [TxPublisher] as well.
type Publisher interface {
	Publish(events ...Event)
}

// TxPublisher is a [Publisher] that can record an event inside the transaction
// that made the change, rather than after it commits.
//
// It is an optional interface, asserted for rather than required, the way
// [sqlb.Beginner] extends [sqlb.Executor] ([ADR-0040]). That is what makes the
// durable feed a swap rather than a migration: [PublishChanges] finds Record and
// uses it, so replacing a [Broker] with an outbox is one constructor call and no
// change to any registration, the endpoint, the wire format or a client.
//
// The contract is the part worth reading before implementing one:
//
//   - Record runs **inside** the mutation, not after it. The transaction is on
//     the context, reachable with [sqlb.TxFrom].
//   - Its error rolls the write back. That is the point: a change nobody can be
//     told about should not be a change. An implementation that would rather
//     lose the event than the write must return nil and report the failure some
//     other way.
//   - [Publisher.Publish] remains the path for a write that ran outside a
//     transaction, where there is no transaction to record into and the change
//     is already durable. Both methods therefore have to work.
//
// [ADR-0040]: https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#the-driver-is-a-dependency
type TxPublisher interface {
	Publisher

	// Record writes the events into the transaction carried by ctx.
	Record(ctx context.Context, events ...Event) error
}

// EventsOptions describes the change-feed endpoint.
type EventsOptions struct {
	// Source supplies the events. Required.
	Source Source

	// Path is where the stream is served. Defaults to "/events".
	Path string

	// Heartbeat is how often a comment line is written to an idle stream, to
	// keep an intermediary from reclaiming a connection it believes is dead.
	// Defaults to 25 seconds, which is under the 30 seconds that is the
	// shortest idle timeout in common proxy defaults. Negative disables it.
	Heartbeat time.Duration

	// Retry is the reconnection delay the endpoint suggests to the client,
	// sent once when the stream opens. Defaults to 3 seconds.
	Retry time.Duration

	// Filter, if set, decides whether one event reaches this subscriber. It
	// runs per event per subscriber, with the request's context, so it can
	// reach whatever the authentication middleware put there.
	//
	// Read the default — every subscriber receives every event — as the thing
	// to think about before mounting this on a multi-tenant deployment. The
	// events carry no row data, but a primary key is still a fact about what
	// exists, and nothing else on this path is scoped: an Event is published
	// by a write, not read through a query, so the BeforeQuery hook that
	// confines every other read of that table does not run here.
	//
	// One consequence to know about: a filtered event's id is never written,
	// so a subscriber that is filtered out of everything keeps an old
	// Last-Event-ID and will eventually be told to reset when it reconnects.
	// That costs it a refetch, which is the safe direction — the alternative
	// is advancing its position past events it was not shown, and then a
	// genuine gap would be indistinguishable from a filtered one.
	Filter func(ctx context.Context, e Event) bool

	// Summary, Description and Tag document the operation. Each has a default.
	Summary     string
	Description string
	Tag         string

	// Security is the OpenAPI security requirement the operation carries, in
	// the same shape and with the same meaning as Options.Security: it
	// documents, and middleware enforces.
	Security []map[string][]string
}

const (
	defaultEventsPath = "/events"
	defaultHeartbeat  = 25 * time.Second
	defaultRetry      = 3 * time.Second
)

func (o *EventsOptions) applyDefaults() error {
	if o.Source == nil {
		return errors.New("rest: EventsOptions.Source is required")
	}
	if o.Path == "" {
		o.Path = defaultEventsPath
	}
	if !strings.HasPrefix(o.Path, "/") {
		return fmt.Errorf("rest: EventsOptions.Path %q must start with a slash", o.Path)
	}
	if o.Heartbeat == 0 {
		o.Heartbeat = defaultHeartbeat
	}
	if o.Retry == 0 {
		o.Retry = defaultRetry
	}
	if o.Summary == "" {
		o.Summary = "Subscribe to change events"
	}
	if o.Tag == "" {
		o.Tag = "events"
	}
	if o.Description == "" {
		o.Description = eventsDescription
	}
	return nil
}

const eventsDescription = "A Server-Sent Events stream of invalidations. Each `change` event names a " +
	"table and, where the change was attributable to one row, its primary key; the client refetches " +
	"through the ordinary GET endpoints rather than receiving row data here.\n\n" +
	"A client that reconnects with `Last-Event-ID` resumes after that position when the events are " +
	"still held. When they are not, the stream opens with a `reset` event instead, and the client " +
	"should refetch everything it displays."

// eventsInput is what the operation reads off the request.
//
// Last-Event-ID is declared as a field rather than only in the document because
// huma builds its parameter set from the input struct: a header that is not a
// field is a header the handler cannot see. The browser's EventSource sends it
// automatically on reconnection, so nothing client-side has to remember to.
type eventsInput struct {
	LastEventID string `header:"Last-Event-ID" doc:"Position to resume after, echoed from the last event received"`
}

// Events mounts the change-feed endpoint on api.
//
// The stream is documented in the OpenAPI document like every other operation —
// one `oneOf` per event type, with the payload schema of each — because it is
// registered through huma rather than as a hand-rolled handler on the mux. A
// consumer generating a client from the document therefore learns the event
// shapes rather than being told the response is text.
//
//	broker := rest.NewBroker(rest.BrokerOptions{})
//	rest.Must(rest.PublishChanges[blog.Post](broker))
//	rest.Must(rest.Events(srv.API, rest.EventsOptions{Source: broker}))
//
// Registration is the startup path, so failures are returned rather than
// panicked, as with Resource.
func Events(api huma.API, opts EventsOptions) error {
	if api == nil {
		return errors.New("rest: Events needs a huma.API")
	}
	if err := opts.applyDefaults(); err != nil {
		return err
	}

	op := huma.Operation{
		OperationID: "subscribe-events",
		Method:      http.MethodGet,
		Path:        opts.Path,
		Summary:     opts.Summary,
		Description: opts.Description,
		Tags:        []string{opts.Tag},
		Security:    opts.Security,
		// As on every other operation: a parameter the operation does not
		// declare is a mistake, and a stream is the worst place to discover
		// one, because a typo that is merely ignored looks exactly like a
		// filter that is working.
		RejectUnknownQueryParameters: true,
	}

	// The map is what turns a Go type into the SSE `event:` name and into the
	// document's schema for it. Two types, two names: a change and a reset are
	// different instructions to the client and a client that treated them
	// alike would refetch either everything or nothing.
	events := map[string]any{
		"change": Event{},
		"reset":  Reset{},
	}

	sse.Register(api, op, events, func(ctx context.Context, in *eventsInput, send sse.Sender) {
		stream(ctx, opts, in, send)
	})
	return nil
}

// stream is the send loop for one subscriber. It returns when the client goes
// away, when the Source drops the subscriber, or on the first failed write.
func stream(ctx context.Context, opts EventsOptions, in *eventsInput, send sse.Sender) {
	since, ok := parsePosition(in.LastEventID)

	ch, err := opts.Source.Subscribe(ctx, since)
	if err != nil {
		// The subscription itself failed, which for the in-process Broker means
		// it is closed and the process is going away. Say so and end the
		// stream; the client's own reconnection is the retry.
		_ = send(sse.Message{Data: Reset{Reason: "the event source is unavailable"}})
		return
	}

	// The retry hint goes out first and on its own, so that a client which
	// disconnects before any change still learns the interval. The comment is
	// what commits the response: an EventSource's onopen does not fire until
	// some bytes arrive, and a feed with nothing to say can otherwise leave a
	// client waiting on a connection that is already established.
	if err := send(sse.Message{Retry: int(opts.Retry / time.Millisecond), Comment: "ok"}); err != nil {
		return
	}

	// A Last-Event-ID that does not parse is not a fresh connection: the client
	// had a position, and we cannot tell which. Resuming from zero would look
	// identical to a working stream while silently skipping everything that
	// happened in between, so it refetches instead.
	if !ok {
		if err := send(sse.Message{Data: Reset{Reason: "the Last-Event-ID header could not be read"}}); err != nil {
			return
		}
	}

	var beats <-chan time.Time
	if opts.Heartbeat > 0 {
		ticker := time.NewTicker(opts.Heartbeat)
		defer ticker.Stop()
		beats = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-beats:
			if err := send.Comment("keepalive"); err != nil {
				return
			}

		case d, open := <-ch:
			if !open {
				// The Source dropped us — this subscriber fell too far behind
				// to be caught up. Ending the stream is the loud version of
				// that: the client reconnects with its Last-Event-ID and is
				// either replayed or told to reset, and either way it converges.
				return
			}
			if err := deliver(ctx, opts, send, d); err != nil {
				return
			}
		}
	}
}

// deliver writes one delivery, applying the filter. A filtered-out event is
// skipped without advancing what the client believes it has seen — its id is
// never written — so a later reconnection replays from the last id the client
// actually received rather than from one it was not allowed to know about.
func deliver(ctx context.Context, opts EventsOptions, send sse.Sender, d Delivery) error {
	if d.Reset != nil {
		return send(sse.Message{ID: int(d.ID), Data: *d.Reset})
	}
	if opts.Filter != nil && !opts.Filter(ctx, d.Event) {
		return nil
	}
	return send(sse.Message{ID: int(d.ID), Data: d.Event})
}

// parsePosition reads a Last-Event-ID. The bool distinguishes "no header",
// which is a fresh connection and reported as (0, true), from "a header that
// makes no sense", which is a client with an unknown position and reported as
// (0, false).
func parsePosition(raw string) (uint64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
