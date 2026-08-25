# Inspecting and tracing

Nothing runs unasked. Four separate things can be asked of a query without
executing it, and they answer different questions.

## SQL(), which executes nothing

```go
sql, args, err := q.SQL()
// SELECT "posts"."id", ... FROM "posts" WHERE ("status" = $1) AND ("title" ILIKE $2)
// [published %postgres%]
```

This is the inspection point — log it, diff it in a test, paste it into
`EXPLAIN`. It is also how a reader confirms the claims made elsewhere in this
documentation: values are always bind parameters, and only identifiers validated
against the model are interpolated.

**What it renders is what the builder holds.** A [`BeforeQuery` hook](hooks.md)
amends a *clone* on the exec path, so on a model whose reads are confined by one
— which in a multi-tenant application is most of them — the `WHERE` clause above
is missing the predicate that confines it. Printing `SQL()` to check that the
tenant scope is really on every read shows a statement without it, which reads
as the hook not working ([#153]).

## Resolved, which renders the statement that runs

```go
resolved, err := q.Resolved(ctx, db)
sql, args, err := resolved.SQL()
// SELECT ... FROM "posts" WHERE ("status" = $1) AND ("org_id" = $2)
//                                                    ^ the hook's
```

`Resolved` applies the hooks registered for the model against `db` — and the
expansion scopes, for a query that expands a relation — and hands back a copy.
The receiver is untouched, as on every exec path, so resolving twice does not
accumulate anything.

Reach for it whenever the rendered text is going somewhere other than a log: a
`GROUP BY` across join tables that has to be raw SQL and must count the same rows
the listing returns, a test asserting that a scope is in force, a statement
pasted into `psql`. The alternative is writing the predicate a second time, in
another language, and a security predicate kept in two places is the worst kind
to let drift.

[`Update`](mutations.md) and `Delete` have the same method, for
`BeforeUpdate` and `BeforeDelete`. `Insert` deliberately does not: `BeforeCreate`
rewrites the rows rather than the statement, so resolving one would mutate the
caller's data as a side effect of inspecting it.

`Explain` and `Resolved` are one case of a general rule —
[ADR-0051](../architecture.md#a-gap-in-the-declaration-is-reported): a tool that
reports *no difference* is making a claim, and one that cannot see a property
makes that claim about it whether or not a difference exists.

[#153]: https://github.com/mind-vm/sqlb/issues/153

## Explain, which plans without running

```go
plan, err := sqlb.Explain(ctx, db, q)
if err != nil {
    t.Fatal(err)   // the query is not valid against this database
}
if d := plan.Diagnostics(); len(d) > 0 {
    t.Errorf("plan regressed:\n%s", sqlb.Diagnostics(d))
}
```

`Explain` plans the statement against the live schema without executing it,
which answers both halves of "did I break something": whether the query is valid
against *this* database, and whether the plan regressed.

The first half is worth dwelling on. It fails on the migration that was written
and never applied — which a compile-time column check structurally cannot,
because the column exists in the schema file either way.

The second half needs the statement to be the real one, so `Explain` resolves
hooks for you: it takes a `ctx` and a `db`, which is everything `Resolved` needs.
Without that it would plan a query nobody issues — `WHERE status = $1` and
`WHERE status = $1 AND org_id = $2` are different queries with different plans,
and the second is the one with the composite index behind it — so a
plan-regression test would have stayed green through exactly the change that
makes the real query seq-scan.

`ExplainAnalyze` gives real timings but **executes** the statement — on a
mutation that means it writes. Use it inside a transaction you roll back. It
resolves hooks too, which on a write is a correctness property rather than a
reporting one: what it runs is the confined statement.

## Tracing needs no API

`Executor` is two methods, so a wrapper observes every statement and reaches
OpenTelemetry, slog or a test double without sqlb depending on any of them:

```go
type tracer struct{ inner sqlb.Executor }

func (t tracer) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
    start := time.Now()
    rows, err := t.inner.Query(ctx, q, args...)
    slog.InfoContext(ctx, "sqlb", "sql", q, "dur", time.Since(start), "err", err)
    return rows, err
}
// Exec likewise, then pass the wrapper wherever you passed the pool.
```

If your wrapper should also support `WithTx`, implement `Beginner` on it —
`BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)` — alongside
`Executor`. It is asserted for rather than required, which is what keeps
`Executor` two methods.

pgx has a tracing seam of its own — `pgx.QueryTracer` on the pool config — and it
sees things a wrapper here cannot, such as which connection ran the statement.
Use that when you want the driver's view and this when you want sqlb's.

That two-method surface is also why a connection pooler in the path needs nothing
from sqlb: the query path is tested through a real PgBouncer in transaction
pooling, because that is the deployed topology
([ADR-0019](../architecture.md#pgbouncer-in-the-path)).

## Checking the schema, not the query

Two more inspections belong to design time rather than to a request, and they
are on their own pages:

- **`schema.Lint()`** reports what will behave badly in production — an
  unindexed filterable column, a list endpoint with nothing sortable. See
  [Declaring tables](../schema/README.md#checking-your-work).
- **`migrate.Diff` against a replayed history** answers whether the database and
  the migration files agree. See [Adopting a database](../migrations/adopting.md).

## Next

- [Hooks](hooks.md) — what amends a query between `SQL()` and the wire, and
  what `Resolved` applies for you
- [Queries](README.md)
