# example/meter — the write is an increment

`docs/special-cases.md` calls this the second most common construct in the
corpus it counted (53 lines, 37 of them `DO UPDATE`) and the one sqlb's census
said had no spelling. That finding is out of date. **The arithmetic-upsert gap
is closed** — `Insert.OnConflictSet(column string, value Expr)` landed in
[#90](https://github.com/mind-vm/sqlb/issues/90) — and this example is the
demonstration under real concurrency, not a bug report. If you came here
looking for the gap, it is not here; what is here is what is left once it
closes.

```go
sqlb.InsertRows(&Meter{Tenant: t, Kind: k, Count: n}).
    OnConflictUpdate([]string{"tenant", "kind"}).
    OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count"))).
    One(ctx, db)
```

One statement, atomic, inside the builder — the model, the hooks and
`RETURNING` all intact. `TestArithmeticUpsertUnderConcurrency` fires twenty
goroutines at the same `(tenant, kind)` key and asserts zero errors and a final
count of twenty, because a metering table's actual requirement is not "correct
run twice in sequence" — it is correct under concurrent writers, which is the
one thing a hand-rolled read-modify-write cannot promise and this can.

The default stays unchanged, and should: `OnConflictUpdate` without
`OnConflictSet` still copies `EXCLUDED.<col>`, so upserting 3 over an existing
5 leaves 3. That is exactly right for a profile, a cache entry, a settings row
— every upsert that is not arithmetic — which is why the increment form is
something a caller opts into by name rather than something the default
quietly started doing.

## What is still a trap here, for the first time in a worked example

Two more rows of the census measured as **half** or **needs Coalesce**, and
this table is where a reader hits both for the first time on something real
rather than in a unit test.

**`date_trunc` bucketing.** The obvious way to bucket `count` by day —
`Sel(Call{"date_trunc", …})` handed to both `Select` and `GroupByExpr` — reads
correctly and does not run. `TestDateTruncBucket` shows why: the compiler
numbers a bind parameter per occurrence, so a `Param{"day"}` unit becomes `$1`
in the projection and `$2` in the `GROUP BY`. Postgres matches `GROUP BY`
entries structurally, sees two different expressions, and refuses the query.
The fix is `Raw{SQL: "'day'"}` in place of the `Param` — safe, because the
bucket unit is a developer-chosen constant and never user input, and
undiscoverable, because nothing says so until now.

**The empty-range aggregate.** A bare `Sum` over a filter matching nothing —
which is exactly what a chart looks like the first day a tenant has no
activity — does not report zero. SQL says an aggregate over no rows is `NULL`,
and pgx says `NULL` does not scan into `int64`, so the query fails with an
error naming `NULL` rather than the empty range that produced it.
`sqlb.Coalesce(sqlb.Sum(...).Expr(), sqlb.Raw{SQL: "0"})` is the fix, and it
still reports the true total when rows exist. `Count` is the one aggregate
that needs no `Coalesce` — `COUNT(*)` over zero rows is `0`, not `NULL` — which
is exactly what makes this trap survive review and fail on the first quiet
day. `TestEmptyRangeAggregateNeedsCoalesce` proves all three: the failure, the
fix, and the exception.

## The composite key this table actually wants

`meters` wants a primary key of `(tenant, kind)`. sqlb refuses composite
primary keys outright
([ADR-0034](../../docs/architecture.md#one-column-addresses-a-row)) — the
declaration is a schema-time error naming its own workaround, proven directly
in `pgtest/census_test.go`'s
`TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround` and not re-proven
here. `meterschema.Meter` applies the workaround: a surrogate `id` nothing in
this example reads, plus `.UniqueIndex("tenant", "kind")` to carry the
invariant the primary key can't.

That workaround has a cost, and it is worth stating rather than absorbing
silently:

- an extra column (`id`) that exists only to give the row *a* single-column
  address, not the one the application means;
- an extra index — the unique index is what actually enforces "one row per
  tenant and kind", not the primary key;
- a client that has to know the surrogate is not the identity, or it will
  build a resource path on `id` and be unable to explain what the id names.

`TestUniqueIndexHoldsTheCompositeKey` checks that the workaround actually
holds for this table: a second, unconditional `InsertRows` at an existing
`(tenant, kind)` is rejected, `errors.As` unwraps it to a
`*sqlb.ConstraintError`, and `Kind == sqlb.ConstraintUnique` with
`Constraint == "meters_tenant_kind_uniq"` — the index name the schema chose,
not a generic "duplicate key".

## Running it

```bash
mise run pg-up   # or point SQLB_TEST_POSTGRES at any empty Postgres 18
cd example/meter
go mod tidy
SQLB_TEST_POSTGRES='postgres://sqlb:sqlb@localhost:15432/sqlb?sslmode=disable' go test ./... -v -race
```

Each test opens its own `CREATE DATABASE` and applies the DDL
`meterschema.Meter` declares through
`migrate.Diff(nil, schema.DefaultRegistry(), migrate.MinPostgres(18))` — so
what runs is the migration this schema actually produces, not a hand-written
paraphrase of it.

## Deliberately not

**Deliberately not: billing, invoicing, or price.** This is a counter, not a
ledger; turning a count into money is a different table with different
rounding rules and a different set of things that must never silently
overwrite each other.

**Deliberately not: an aggregate REST response.** That shape does not exist
yet in `rest` — every generated endpoint returns rows, and a bucketed sum is
not a row. Whether a chart endpoint over a metering table should be
generated rather than hand-written is a design question, not a bug this
example can fix; `docs/special-cases.md` still lists it as open.
