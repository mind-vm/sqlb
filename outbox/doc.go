// Package outbox is the durable half of the change feed: a table written in the
// same transaction as the change, and a dispatcher that tails it.
//
// It is the [rest.Source] [ADR-0012] describes and [ADR-0045] left a seam for.
// Swapping [rest.Broker] for a [Dispatcher] is a constructor call — the
// endpoint, the wire format, the `Last-Event-ID` contract and every generated
// client are unchanged — and what it buys is the two properties the Broker
// documents itself as lacking:
//
//   - **At-least-once instead of at-most-once.** The event and the data commit
//     together. A process that dies between the commit and the fan-out has
//     already written the event, and the dispatcher delivers it when it comes
//     back.
//   - **Correct across replicas instead of correct on one.** Every replica
//     writes to one table and every dispatcher reads it, so a write served by
//     one replica reaches subscribers connected to another.
//
// # The shape
//
//	outbox.Install(ctx, pool, outbox.Options{})          // the table and its trigger
//	ob := outbox.New(pool, outbox.Options{})
//	rest.Must(rest.PublishChanges[blog.Post](reg, ob))    // unchanged from the Broker
//
//	d := outbox.NewDispatcher(ctx, pool, outbox.DispatcherOptions{})
//	go d.Run(ctx)
//	rest.Must(rest.Events(api, rest.EventsOptions{Source: d}))
//
// [Outbox] implements [rest.TxPublisher], so `rest.PublishChanges` records into
// the writing transaction rather than after it. That assertion is the whole of
// the swap: nothing else in an application changes.
//
// # Why a subscriber can be replayed after a restart
//
// The stream position is the outbox row's `id`, so it means the same thing in
// every process and across every restart. A client reconnecting with
// `Last-Event-ID: 4210` is answered from the table rather than from a ring
// buffer that died with the last deployment — which is the difference between a
// rolling restart costing every connected client a full refetch and costing them
// nothing.
//
// A position older than the oldest row still retained gets a [rest.Reset], the
// same as it would from a Broker whose history had rolled over. Retention is
// therefore a delivery guarantee and not only a disk-space setting; see
// [Options.Retention].
//
// # Ordering, and what it costs
//
// A tail of `id > cursor ORDER BY id` is only correct if rows become visible in
// id order, and a bare sequence does not promise that: two transactions can take
// ids 5 and 6 and commit in the other order, and a dispatcher that read 6 first
// would advance its cursor past 5 and lose it silently. Losing an invalidation
// silently is the one failure this whole design exists to avoid.
//
// So [Outbox.Record] takes a transaction-scoped advisory lock before it
// inserts. The lock is held until commit, so id order is commit order by
// construction and the dispatcher's tail needs no reasoning about visibility at
// all.
//
// **The cost is real and should be read before adopting this.** Writes to
// published models serialise against each other from the outbox insert to the
// commit — roughly the commit itself, since the outbox write is the last thing
// the mutation does. That bounds write throughput on published models at about
// one transaction per commit latency. For the applications this feature is for
// it is not the binding constraint; for a write-heavy ingest path it may be, and
// the answer there is not to publish that model. [ADR-0012] carries the revisit
// trigger.
//
// It is `pg_advisory_xact_lock` rather than the session form deliberately:
// [ADR-0019] forbids anything session-scoped on the query path, because the
// target deployment runs PgBouncer in transaction pooling. A transaction-scoped
// lock is released by the commit that returns the connection, so it is safe
// through a pooler.
//
// # What still needs a direct connection
//
// The dispatcher's `LISTEN`, and only that. [ADR-0019] measured it: PgBouncer in
// transaction pooling *accepts* a `LISTEN` and then silently never delivers, so
// [Dispatcher.Run] must be given a pool that reaches Postgres directly. `NOTIFY`
// is transactional and survives the pooler, so the write path — which is
// everything else — needs no exception.
//
// A dispatcher whose `LISTEN` is silently useless still works, at the poll
// interval, which is exactly the failure ADR-0012 warns can hide. [Dispatcher]
// therefore reports it rather than absorbing it: see [DispatcherOptions.OnError]
// and [Dispatcher.Stats].
//
// [ADR-0012]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#change-feed-outbox
// [ADR-0019]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#pgbouncer-in-the-path
// [ADR-0045]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#the-stream-is-a-seam
package outbox
