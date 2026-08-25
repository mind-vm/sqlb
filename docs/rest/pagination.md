# Pagination

A list response pages without counting. `has_more` comes from reading one row
beyond the page, so a request for `per_page=5` reaches the database as `LIMIT 6`
— the probe is added by the handler, on top of the `LIMIT` that `filter.Apply`
produced:

```json
{"items": [...], "page": 1, "per_page": 20, "has_more": true,
 "next_cursor": "eyJrIjpbeyJjIjoiY3JlYXRlZF9hdCIsImQiOnRydWUsInYiOiIyMDI2LTA3LTI4VDA5OjAwOjAwWiJ9XX0"}
```

`total` costs a second query and so is opt-in, with `?count=exact`. It is the
size of the whole result set, so it does not shrink as a cursor advances.

## Two ways, and the response offers both

`?page=2&per_page=50` is offset paging. It is the right choice for a numbered
page control, where a client needs to jump to page 7 and the set is small enough
that the jump is cheap.

`?cursor=` is keyset paging, and it is what anything walking a whole result set
should use — infinite scroll, an export, a sync job. `OFFSET k` makes the
database produce `k + n` rows and discard `k`, so page 500 costs five hundred
times page 1; and because the page is addressed by its distance from the start,
a row inserted while a client pages shifts every later page by one, so the client
sees a row twice or never. A cursor names the position instead:

```
GET /posts?sort=-view_count&per_page=20
  → {"items": [...], "has_more": true, "next_cursor": "eyJrIjpb…"}

GET /posts?sort=-view_count&per_page=20&cursor=eyJrIjpb…
```

Three things are worth knowing.

**`next_cursor` is on every response that has a next page**, including one that
paged by offset, so adopting cursors needs no flag and there is no first cursor
to obtain some other way. It is absent on the last page, which is how a walk
knows to stop.

**A cursor belongs to its sort.** Changing `?sort=` and keeping the cursor is a
400 naming both orderings, because the cursor names a position in an ordering
that no longer exists. Drop the cursor when the sort changes.

**`?cursor=` cannot be combined with `?page=` or `?offset=`** — they are two
answers to where the page starts — and the rejection says which to drop.

## How deep offset paging may reach

`MaxOffset` bounds it, and a request past the bound is a 400 pointing at
`?cursor=`. Offset is the one dimension of a request whose cost grows with the
number the client sent — `?page=50000000` asks Postgres to produce and discard
ten billion rows before returning a page of twenty-five — and unlike a filter or
a sort term, it costs that whether or not anything matches.

Declare it beside the table, with the other four ceilings:

```go
Expose(schema.REST{
    Path:            "/products",
    Ops:             schema.Reads,
    DefaultPageSize: 25,
    MaxPageSize:     100,
    MaxFilters:      12,
    MaxSortTerms:    4,
    MaxOffset:       10_000,
})
```

The package default is 100,000, and it is deliberately loose: it has to be safe
for a table nobody described, which puts it two to four orders of magnitude above
what any particular resource wants. A catalog of ten thousand products has no
legitimate offset past ten thousand — every one above it is a guaranteed empty
page that still costs a scan to the end — and the row count that decides the
right number is known where the table is declared and nowhere else.

Leave it at zero to take the default. All five ceilings read a zero that way, so
none of them can be turned *off*; a negative one is refused at validation rather
than silently resolving to the loosest available bound ([#151]).

[#151]: https://github.com/mind-vm/sqlb/issues/151

## Every list is ordered deterministically

Whichever paging is used, `filter.Apply` appends the primary key unless the sort
already contains it. Without that, page two can repeat a row from page one or
skip one — `schema.Lint` used to warn about exactly this, and no longer needs
to.

The cost is that the tiebreaker can force a sort where an index on the sort
column alone would have streamed; the fix is a composite index on
`(sort_column, id)`, which is what `unindexed-sort` now suggests and the index
cursor paging wants anyway.

[ADR-0027](../architecture.md#keyset-pagination) has the boundary predicate, the
NULL handling and why the cursor is opaque by convention rather than signed.

## The same thing from Go

`After` and `CursorFor` are the builder's form of the same mechanism, for a
batch job that never goes through HTTP. See [Paging](../queries/paging.md).

The generated clients both have it too:
`postQueries(request).infinite({...})` in
[TypeScript](../typescript/README.md#paging), and `--all` in the
[CLI](../cli/README.md#reads), which follows `next_cursor` until the collection
is exhausted — so a walk cannot read a row twice, which is the failure a
hand-written loop over `?page=` has and does not report.

## Next

- [Expanding relations](expand.md) — where a *capped* collection appears
- [Rejections](errors.md) — the 400 a mismatched cursor produces
