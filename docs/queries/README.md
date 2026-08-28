# Queries

Nothing runs when you build a query. That is the whole design: predicates can be
added on a branch, which is what static query generators structurally cannot
express.

```go
q := sqlb.Query[Post]().Where(sqlb.F("status").Eq("published"))
if search != "" {
    q = q.Where(sqlb.F("title").Contains(search))
}
posts, err := q.OrderBy(sqlb.F("created_at").Desc()).Limit(50).All(ctx, db)
```

Methods mutate the builder and return it, so a query can be assembled across
branches without reassignment gymnastics — and so a hook can amend a query it is
handed. Use `Clone()` before sharing a partially built query between goroutines
or request scopes.

`Where` skips the zero `Pred`, so `If` removes the surrounding statement
entirely when a filter is optional:

```go
q.Where(
    sqlb.F("status").Eq("published"),
    sqlb.If(minViews > 0, sqlb.F("view_count").Gte(minViews)),
)
```

## Terminal methods

| Method | Returns |
|---|---|
| `All(ctx, db)` | Every matching row |
| `One(ctx, db)` | The single match; `ErrNotFound` if none, an error if more than one |
| `First(ctx, db)` | The first match; pair it with `OrderBy` to be deterministic |
| `Count(ctx, db)` | Row count, ignoring pagination; group count for a grouped query |
| `Exists(ctx, db)` | Whether anything matched |
| `SQL()` | The statement and its bind parameters, executing nothing |

`One` fetches two rows so it can tell you the result was ambiguous rather than
silently returning the first — a caller asking for one row is asserting only one
exists.

The builder is cloned before hooks run, so running the same builder twice does
not accumulate their predicates.

## Predicates

`sqlb.F("column")` is the untyped reference; the generated `PostCols.Title` is
the typed one, and worth preferring — see [Typed columns](typed-columns.md).

Comparison: `Eq`, `Neq`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `NotBetween`,
`OneOf`, `NotOneOf`, `IsNull`, `NotNull`, `EqField`.

Text: `Contains`, `StartsWith`, `EndsWith`, `Like`, `ILike`.

`Contains`, `StartsWith` and `EndsWith` escape LIKE metacharacters, so a user
typing `50%` searches for that literal string. `Like` and `ILike` do not — use
them only for patterns your own code wrote.

`Eq(nil)` becomes `IS NULL` rather than `= NULL`, which is never true and is
never what the caller meant.

Combine with `And`, `Or` and `Not`. All three skip zero predicates, and `Not` of
a zero predicate stays zero, so an absent filter stays absent rather than
becoming always-false.

## Matching against another query

A query is a value, so it nests. `InQuery` and `NotInQuery` match a column
against the single column a subquery selects; `Exists` and `NotExists` ask
whether it returns any row:

```go
tagged := sqlb.Query[PostTag]().Select(sqlb.F("post_id")).Where(sqlb.F("tag_id").Eq(id))
posts, err := sqlb.Query[Post]().Where(sqlb.F("id").InQuery(tagged)).All(ctx, db)
```

This is `OneOf` over a set the database computes rather than one your code
enumerates, and the difference is not only convenience: a list of values costs a
bind parameter each, and a statement carries at most 65,535 of them.

**A nested query has to be resolved first if its model's reads are confined by a
hook.** Nesting compiles a query rather than running it, and hooks apply when a
query runs — so the scope would be silently absent from inside someone else's
`WHERE` clause. That is refused rather than dropped:

```go
sub, err := sqlb.Query[Post]().Select(sqlb.F("author_id")).Resolved(ctx, db)
```

A model with no hook needs none of this. [ADR-0055](../architecture.md#a-nested-query-runs-nobodys-hooks)
has the reasoning, including why the inner query is not resolved for you.

## Aggregates and other shapes

`Collect[R]` scans into a type other than the model, which is how grouped
queries are read:

```go
type Revenue struct {
    Status string  `db:"status"`
    Total  float64 `db:"revenue"`
}

rows, err := sqlb.Collect[Revenue](ctx, db,
    sqlb.Query[Order]().
        GroupBy(sqlb.F("status")).
        Select(sqlb.F("status"), sqlb.Sum(sqlb.F("total")).As("revenue")))
```

**Reading a grouped query with `All` is refused**, rather than answered with
rows whose numbers are missing. The model has no field for the aggregate, so
scanning into it discarded exactly the column the query was written to get and
returned the right number of rows with a zero where the answer should be — and a
nil error, so there was nothing to search for. The refusal names the dropped
column and points here ([#306](https://github.com/mind-vm/sqlb/issues/306)). A
grouped query whose projection the model *can* hold — grouping by the primary
key and selecting its own columns — still scans as before.

Query hooks still run, so tenant scoping applies to aggregates too. That is the
reason to reach for this rather than dropping to `DB.Query` and hand-writing the
SQL: a raw statement leaves the confinement hooks behind, and a dashboard count
is exactly where a missing tenant predicate is invisible — the number is merely
wrong, not obviously unauthorised.

Unlike
`All`, `Collect` requires every field of `R` to be filled by some result column:
`R` was written to match this projection, so an unfilled field is a mistyped
alias rather than a deliberate partial select — and a mistyped alias on a `Sum`
would otherwise report zero revenue silently.

`Raw` and `RawPred` are the escape hatch for expressions the builder cannot
model. Their contents are not validated; their `?` placeholders are renumbered
by the compiler.

## Next

- [Typed columns](typed-columns.md) — putting column names under the compiler
- [Paging](paging.md) — walking a whole result set without `OFFSET`
- [Mutations and transactions](mutations.md) — inserts, updates, `WithTx`
- [Hooks](hooks.md) — where domain logic lives
- [Inspecting and tracing](inspecting.md) — `SQL()`, `Explain`, wrapping the
  executor
- [Testing an application on sqlb](testing.md) — the database-free double, and
  what it can and cannot answer
