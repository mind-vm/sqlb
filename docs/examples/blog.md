# blog — the smallest whole thing

**In this repository:** [`example/blog/`](https://github.com/jryannel/sqlb/tree/main/example/blog)

Three tables, everything codegen emits from them, the pieces that had to be
written by hand, and an assembled server. It is a real test suite rather than a
listing, so it cannot drift from the code.

[Your first app](../start/first-app.md) walks it line by line. This page is the
summary.

## What it proves

**That the generated half and the hand-written half compose.** The soft delete
is the case: `schema.SoftDelete()` adds a `deleted_at` column and stops, so
making it mean something is a `BeforeQuery` hook that adds the predicate *and* a
hand-written `DELETE /posts/{id}` that stamps the column. Both are a few lines,
both are visible, and they mount onto the same Huma API in two calls rather than
through a wrapper.

**That capabilities are decisions.** `body` is searchable but not sortable.
`view_count` is read-only but still filterable and sortable, so "most read this
week" is a URL and no request can set the number. `password_hash` is hidden and
deliberately not filterable, because a filterable secret can be recovered by
probing.

**That a resource can refuse to mount.** `posts` declares `SoftDelete`, so
registration fails until something filters the column. The test suite has to
register the hook before it can build a server, which is the check working.

## The shape

```
blogschema/schema.go   three tables — the source of truth an author edits
        │
        ├─▶ models_gen.go      row structs
        ├─▶ columns_gen.go     typed column facade
        ├─▶ rest_gen.go        request bodies + Register
        └─▶ sqlb.json          the manifest

hooks.go        the soft-delete predicate
post_ext.go     AddViewCount — the generator cannot produce it
deletes.go      DELETE /posts/{id} as an UPDATE
server_test.go  the assembled server, and every claim asserted
```

## Read it for

| | |
|---|---|
| [`blogschema/schema.go`](https://github.com/jryannel/sqlb/blob/main/example/blog/blogschema/schema.go) | Four kinds of capability decision, with a comment on each that is not obvious |
| [`hooks.go`](https://github.com/jryannel/sqlb/blob/main/example/blog/hooks.go) | Eleven lines that constrain every read of a model |
| [`deletes.go`](https://github.com/jryannel/sqlb/blob/main/example/blog/deletes.go) | Why a hook cannot turn a `DELETE` into an `UPDATE`, and what to do instead |
| [`post_ext.go`](https://github.com/jryannel/sqlb/blob/main/example/blog/post_ext.go) | Incrementing a counter in the database rather than read-modify-write |
| [`server_test.go`](https://github.com/jryannel/sqlb/blob/main/example/blog/server_test.go) | The assembled chi + Huma + generated `Register` server |

## What it is not

There is no authentication, so it does not scope by tenant — though its tests
show the shape, and it is one more predicate on the same hook. It runs against
an in-memory `database/sql` driver rather than a container, which keeps it in
the fast inner loop: it is proving what codegen emits and how the pieces
assemble, not what Postgres does under concurrency. For that, read
[library](library.md) or [exchange](exchange.md).

## Next

- [Your first app](../start/first-app.md) — the walkthrough
- [tasks](tasks.md) — the same machinery once an application has a
  real shape
