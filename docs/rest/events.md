# Change events

A view that shows a row should find out when the row changes. `rest.Events`
mounts a Server-Sent Events stream that says so — and says nothing else. A
subscriber receives the *address* of a change, never the row:

```
event: change
id: 41
data: {"table":"posts","key":"p1","op":"update"}
```

The client refetches through the ordinary `GET` endpoints — with a generated
subscriber that resolves the address into what to refetch, in
[TypeScript and Dart alike](#subscribing). That is the whole protocol, and the
reason for it is in
[ADR-0045](../architecture.md#the-stream-is-a-seam): a payload would have to be built
per subscriber under that subscriber's context, or the `BeforeQuery` hook that
scopes every other read of that table would not run on it — and a change feed
that skips the scope hands one tenant's rows to another. Sending the address
keeps the read path the only thing that ever reads.

## Pick a source first

There are two, they have different guarantees, and the difference matters more
than anything else on this page.

| | `rest.Broker` | `outbox.Dispatcher` |
|---|---|---|
| Where the event lives | memory | a table, written by the transaction that made the change |
| Delivery | at-most-once | at-least-once |
| Replicas | **one** | any number |
| Resume after a restart | reset — refetch everything | replayed from the table |
| Costs | nothing | a table to prune, and writes to published models serialise |
| Needs | nothing | one migration, a goroutine, a direct connection |

**`rest.Broker` is a real feature for a single-replica deployment and a trap for
a horizontally scaled one.** A crash between the commit and the fan-out loses the
event and no client learns the row changed; a write served by one replica is
invisible to subscribers connected to another. Both are consequences of the event
being in memory, and both are stated at the top of its doc comment rather than in
a changelog, because the failure they produce — a client showing stale data
forever — is invisible from the outside.

**`outbox.Dispatcher` is the durable version**
([ADR-0012](../architecture.md#change-feed-outbox)), and swapping to it is a
constructor call: the endpoint, the wire format, the `Last-Event-ID` contract and
both generated clients are unchanged. Skip to [the outbox](#the-outbox) for what
it costs.

## Wiring it

Three calls: a source, the models whose writes feed it, and the endpoint.

```go
srv := rest.NewServer(rest.Config{Title: "Blog", Version: "1.0.0"})
rest.Must(blog.Register(srv.API, db))          // generated, unchanged

broker := rest.NewBroker(rest.BrokerOptions{})
defer broker.Close()

rest.Must(rest.PublishChanges[blog.Post](broker))
rest.Must(rest.PublishChanges[blog.Comment](broker))
rest.Must(rest.Events(srv.API, rest.EventsOptions{Source: broker}))

http.ListenAndServe(":8080", srv.Handler)
```

`PublishChanges[T]` registers hooks, not handler wrappers. One registration
therefore covers the generated CRUD handlers, the generated actions, and your own
`sqlb` writes alike — including the background job and the admin script, which is
exactly where a handler-level feed would go quiet. It is the same reasoning that
makes `BeforeQuery` the place tenant scoping lives.

Publication happens through `sqlb.AfterCommit`, so a write that rolls back
announces nothing. A resource that set `DisableTransactions` still publishes:
under autocommit the statement is already durable when the hook runs.

The one exception is a publisher that can do better, which is the next section.

The stream is in the OpenAPI document like every other operation, with a
`text/event-stream` response and a schema per event type, because it registers
through huma's `sse` package rather than as a hand-rolled handler on the mux.

What the document cannot say is which *event names* carry which schema, or that
a subscriber has to handle both — OpenAPI has no vocabulary for it, so a
generic generator reads the operation as a streaming response of an anonymous
union and emits nothing useful. That is not what limits sqlb's clients: they
are generated from the schema rather than from the document, the same as every
other operation ([ADR-0028](../architecture.md#typescript-client)), so the
subscriber below is typed on both ends. See
[Subscribing](#subscribing).

## The outbox

`outbox.Outbox` records each change into a table **inside the transaction that
made it**, and `outbox.Dispatcher` tails that table and fans it out. The event
and the row commit together or neither does, which is the whole of what this buys
over a `Broker`.

```go
// Once, in a migration — or Install() at startup for a single binary.
outbox.Install(ctx, pool, outbox.Options{})

ob := outbox.Must(outbox.New(pool, outbox.Options{OnError: log.Error}))
rest.Must(rest.PublishChanges[blog.Post](reg, ob))   // the same call as before

d := outbox.MustDispatcher(outbox.NewDispatcher(ctx, pool, outbox.DispatcherOptions{}))
go d.Run(ctx)
rest.Must(rest.Events(srv.API, rest.EventsOptions{Source: d}))
```

`PublishChanges` is unchanged, and that is deliberate. `Outbox` implements
`rest.TxPublisher` — an optional interface that `PublishChanges` asserts for, the
way `sqlb.Beginner` extends `sqlb.Executor` — so the same registration records
into the transaction instead of announcing after it.

**The visible difference is the failure.** A `Broker` cannot fail a write; it is
told after the commit, when there is nothing left to refuse. An `Outbox` that
cannot record the event rolls the mutation back, because a row that exists while
every subscriber believes it does not is the failure this feed exists to prevent,
reached from the other side.

### What it costs

Three things, and the first is the one to weigh before adopting it.

**Writes to published models serialise.** A tail of `id > cursor ORDER BY id` is
only correct if rows become visible in id order, and a bigserial does not promise
that — two transactions can take ids 5 and 6 and commit in the other order, and
the tail would advance past 5 and lose it silently. So recording takes a
transaction-scoped advisory lock, held to the commit. That bounds write
throughput on published models at roughly one transaction per commit latency. For
a filterable-list application it is not the binding constraint; for a write-heavy
ingest path it may be, and the remedy is to not publish that model.
[ADR-0012](../architecture.md#change-feed-outbox) carries the argument and the
revisit trigger.

**The table needs pruning, and retention is a delivery setting.** A client whose
`Last-Event-ID` is older than the oldest retained row gets a `reset`, so
`Options.Retention` (24 hours by default) is the longest disconnection a client
survives cheaply. `Dispatcher.Run` prunes on a timer; `Outbox.Prune` is there for
a worker fleet that publishes and serves no stream.

**The dispatcher needs a direct connection.** PgBouncer in transaction pooling
*accepts* a `LISTEN` and then silently never delivers on it
([ADR-0019](../architecture.md#pgbouncer-in-the-path)), which leaves the feed correct
and running entirely on its fallback poll. Everything else — including every
write — is fine through a pooler, because `NOTIFY` is transactional.

That failure looks like nothing being wrong, so the dispatcher checks for it:
after `LISTEN` succeeds it rings the doorbell from another connection and reports
through `OnError` if it does not hear it. **Set `OnError`.** It is where a failure
that has nowhere else to go ends up.

`Dispatcher.Stats()` is the continuous version of the same question.
`Notifications` counts doorbells actually heard, and `Listening: true` with
`Notifications` flat is a feed running entirely on its poll — which is the metric
to alert on, because nothing else about it looks wrong.

### What it buys back

Two things, both downstream of the stream position being a row id rather than a
per-process counter.

**A deploy stops costing every client a refetch.** A subscriber reconnecting with
`Last-Event-ID: 4210` is caught up out of the table by the process that replaced
the one it was talking to. A `Broker`'s history dies with its process, so the
same reconnection is a `reset`.

**Two replicas both deliver.** A write served by one reaches subscribers
connected to the other, because they read one table rather than each other's
memory.

## What a client sees

Two event types.

**`change`** carries `{table, key, op}`, where `op` is `create`, `update` or
`delete`. `key` is the row's primary key rendered the way the URL renders it, so
it concatenates onto the resource path.

A delete carries its `key` like any other change, one event per removed row.
That is `AfterDeleteRows` doing the work: `PublishChanges` registers it, so the
delete runs `DELETE … RETURNING` and the publisher sees what went. The cost is a
scan of everything the statement matched, paid on every delete of a published
model — the alternative was an event naming no row, which a subscriber keyed on
one cannot use and a tenant filter cannot attribute.

`key` may still be **empty**, and a client has to handle it: a publisher written
by hand against `AfterDelete` gets a count and can only name the table. Read a
keyless event as "invalidate this collection", which is what a delete asks of a
client anyway — the row is gone and the list it was in changed.

**`reset`** carries `{reason}` and means the stream could not be resumed —
refetch everything you display. It arrives when a reconnection's `Last-Event-ID`
is older than the retained history, when that header cannot be read, or when it
is *ahead* of the stream, which is what a client from before a restart looks
like.

## Subscribing

The subscriber is generated, in both clients. What a change event invalidates is
a lookup from the schema — which is exactly what a hand-written listener gets
subtly wrong, and what the [TypeScript](../typescript/README.md) and
[Dart](../dart/README.md) clients each emit half of.

**TypeScript.** `subscribeChanges` owns the `EventSource`, narrows the table to
one this client serves, and resolves the event into the cache keys that read it:

```ts
const stop = subscribeChanges(`${baseUrl}/events`, {
  onChange: ({ keys }) => {
    for (const queryKey of keys) void queryClient.invalidateQueries({ queryKey });
  },
  onReset: () => void queryClient.invalidateQueries(),
});
```

A keyed event resolves to that row's detail queries plus the lists and infinite
walks it may have moved in or out of; a keyless one resolves to the table. What
is left to the caller is the one thing a schema cannot say: which cache, and
what a reset should show while it refills.

`EventSource` cannot carry an `Authorization` header, so a bearer-token
deployment passes its own opener — `{ open: (url) => new Polyfill(url) }` — the
same seam `Transport` is. `changeKeys(event)` is the derivation without the
stream, and `subscribeEvents` is the stream without the narrowing.

**Dart.** There is no `EventSource` to own, so the application opens the
request and `ChangeFeed` reads the body:

```dart
final feed = ChangeFeed();
await for (final event in feed.read(body, parseJson: jsonDecode)) {
  switch (event) {
    case ChangeEvent():
      final change = TableChange.from(event);
      if (change != null) refetch(change.table, change.key);
    case ResetEvent():
      refetchEverything();
  }
}
```

`FeedEvent` is sealed, so that `switch` is exhaustive — forgetting the reset
case does not compile. `TableChange.from` is the narrowing, and answers null for
a table this client does not serve. There are no cache keys, because Dart's
state managers have no keyed cache to hand them to.

Reconnecting differs by language and that is the whole of the difference.
`EventSource` does it on its own, resending `Last-Event-ID`, so nothing in the
TypeScript client has to remember the position; in Dart, `feed.lastEventId` is
that position and the caller sends it on the next request.

Neither client is required. The wire is three fields and two event types, and a
hand-written subscriber against `EventSource` and `JSON.parse` is a dozen lines
— which is what the generated one replaces, along with the `keysByTable[table]`
lookup that has to be spelled right every time.

## Failure is a reconnection, on purpose

Every failure mode here converts into a refetch rather than into silence:

| What happened | What the subscriber gets |
|---|---|
| It stopped reading and filled its buffer | **Disconnected.** It reconnects with its `Last-Event-ID` and is replayed or reset |
| It was away longer than the retained history | A `reset` |
| Its `Last-Event-ID` is unreadable | A `reset` |
| The stream sat idle | A comment line every `Heartbeat`, so an intermediary does not reclaim the connection |

The rule underneath all of it: a dropped *event* is a client that shows stale
data forever and never finds out, while a dropped *connection* is a client that
reconnects and converges. When in doubt, drop the connection.

`BrokerOptions` tunes the two numbers that decide this — `History`, how many
events are kept for replay (256), and `Buffer`, how many may queue for one
subscriber before it is dropped (256, raised to `History+1` if set lower).

`DispatcherOptions` has the same `Buffer` and the same policy. What it has
instead of `History` is `Options.Retention`, because the replay comes out of the
table rather than a ring — plus `MaxReplay` (1000), past which a returning client
is reset rather than sent a catch-up longer than the refetch it would replace.

## Who sees what

By default, **every subscriber receives every event**. The events carry no row
data, but a primary key is still a fact about what exists, and nothing else on
this path is scoped: an `Event` is published by a write rather than read through
a query, so the `BeforeQuery` hook that confines every other read of that table
does not run here.

`Filter` is where that decision goes. It runs per event per subscriber with the
request's context, so it can reach whatever your authentication middleware put
there — and the event tells it which tenant the change belonged to, because the
schema already declared which column confines the rows
([ADR-0030](../architecture.md#declared-scope-is-required)):

```go
rest.Must(rest.Events(srv.API, rest.EventsOptions{
    Source: broker,
    Filter: func(ctx context.Context, e rest.Event) bool {
        org, err := orgOf(ctx)          // fails closed: no claims, no events
        return err == nil && e.Scope != "" && e.Scope == org
    },
}))
```

`Event.Scope` is the value of the `.Scoped()` column on the row that changed, and
it is **not on the wire**. It exists for this decision. A subscriber gains
nothing from being told its own tenant id, and the wire is the half
[ADR-0045](../architecture.md#the-stream-is-a-seam) records as expensive to change.

It is empty when the model declares no scope. It used to be empty on a hard
delete too — which meant a tenant filter had to let every delete through to
everyone — and is not any more: `PublishChanges` reads the removed rows, so a
hard delete carries its scope like any other change. A publisher written by hand
against `AfterDelete` still produces the empty form, so decide what an empty
scope means for you. The example refuses it, on the grounds that an event it
cannot attribute is one it should not deliver.

A filtered event's id is never written, so a subscriber filtered out of
everything keeps an old `Last-Event-ID` and is eventually told to reset when it
reconnects. That costs it a refetch, which is the safe direction: the alternative
is advancing its position past events it was never shown, and then a genuine gap
would be indistinguishable from a filtered one.

## Bringing your own source

`Source` is one method:

```go
type Source interface {
    Subscribe(ctx context.Context, since uint64) (<-chan Delivery, error)
}
```

`since` is the client's `Last-Event-ID`, or zero for a fresh connection. A source
that can replay from that position should; one that cannot must open the stream
with a `Delivery` carrying a `Reset`, so the gap is announced rather than
skipped. Close the channel to disconnect a subscriber — the client reconnects on
its own.

That is the seam `outbox.Dispatcher` goes behind, and River or NATS would too.
The endpoint, the wire format and every client stay as they are — which is not a
prediction any more: the outbox landed through this interface without changing
one of them.

One thing it asks for that is easy to miss. `since` is a position a client will
send back, so a source has to be able to *honour* it. The outbox pays a real
price to make its ids dense and monotonic; a source over a Kafka offset or a NATS
sequence, which are per-partition, would have to either fake a position or reset
every reconnection. Resetting is the honest option, and the endpoint handles it.
