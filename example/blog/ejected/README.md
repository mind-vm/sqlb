# `ejected` — ejected from a sqlb schema

This package was written by `sqlb eject`. It imports
[pgx](https://github.com/jackc/pgx) and the standard library, and nothing else —
no sqlb, no huma, no router. Deleting sqlb from `go.mod` after taking
this is a supported end state, which is the entire point of the command.

Nothing here is precious. Edit it, delete the parts you do not serve, or keep
running `sqlb eject -check` in CI for as long as you want the exit kept
current — and drop that gate on the day you stop.

## What is here

| File | What it is |
|---|---|
| `schema.sql` | The whole schema as DDL. The same statements `sqlb migrate` would write for a first migration. |
| `models.go` | The row structs, with the `sqlb` tags removed. |
| `store.go` | One function per statement. The SQL is written out. |
| `support.go` | Query-string parsing, WHERE assembly, JSON writing. The only file that is the same in every project. |
| `handlers.go` | `net/http` handlers, one per exposed operation. |

## The endpoints

| Method | Path | Handler |
|---|---|---|
| GET | `/authors` | `ListAuthor` |
| GET | `/authors/{id}` | `GetAuthor` |
| POST | `/authors` | `InsertAuthor` |
| PATCH | `/authors/{id}` | `UpdateAuthor` |
| DELETE | `/authors/{id}` | `DeleteAuthor` |
| GET | `/orgs` | `ListOrg` |
| GET | `/orgs/{id}` | `GetOrg` |
| GET | `/posts` | `ListPost` |
| GET | `/posts/{id}` | `GetPost` |
| POST | `/posts` | `InsertPost` |
| PATCH | `/posts/{id}` | `UpdatePost` |

## What came out whole


- **CRUD and list**, at the same paths, with the same status codes and the same
  JSON envelope — `items`, `page`, `per_page`, `has_more`, and `total` when
  `?count=exact` was asked for.
- **The filter operators that are one SQL fragment each**: `eq`, `ne`, `lt`,
  `lte`, `gt`, `gte`, `in`, `nin`, `isnull`, `notnull`, `between`, `like`,
  `ilike`, `contains`, `startswith`, `endswith` — and the bare
  `?column=value` shorthand for equality.
- **`?sort`**, **`?search`** and **`?page`/`?per_page`**, with the ceilings the
  schema declared.
- **The wire spelling.** Whatever the schema's `WireCase` decided is what a
  request says here, in the query string as well as in the body. `store.go`'s
  column table carries both names — `Wire` is what a request may use, `Name`
  is what the SQL is built from — and it is the only place the two are related,
  because nothing on the request path computes a spelling.
- **Capabilities as refusals.** A column that never declared `Filterable` cannot
  be filtered here either, and the rejection lists the ones that can be. That is
  a security property, not a convenience: a column left out of the grammar
  cannot be probed through it. Hidden columns are absent from the column table
  entirely.
- **The error shape.** RFC 9457 problem documents, with the `allowed` list on
  each detail, so a client's error handling does not change.
- **The constraint mapping.** A duplicate unique value is still a `409`, and a
  foreign-key, check or not-null violation still a `422`, classified off
  SQLSTATE class 23 exactly as before — so a retry loop keyed on `409` keeps
  working. The `detail` text is generic where the API named the resource; the
  status, which is what clients branch on, is identical.
- **The request budgets.** `MaxFilters`, `MaxSortTerms` and `MaxOffset` come from
  the schema; the list cap (100 values in one `in`/`nin`) and the value-length cap
  (256 bytes) are constants at the top of `support.go`, edit them there. The
  offset budget matters more here than it did behind the API: `?cursor` did not
  come out, so a client reading deep has no cheaper spelling to be sent to.
  `?search` escapes `%` and `_` in the term, so a search for a
  literal percent sign still matches literally.
- **The default ordering.** `DefaultSort` comes out as a resolved
  `[]Order` on the resource's `Limits`, applied when a request names no
  `?sort`. It is not a budget and it is here for a different reason: a list
  is well-formed in any order, so an exit that dropped it would answer every
  unsorted request with a different collection and nothing would say so.
- **The obligation.** A table that declared `Scoped` or `SoftDelete` refuses to
  register without a `Confine` hook, and a scoped table with a create endpoint
  refuses without an `Assign` hook. Startup errors, exactly as before.

## What did not come out, by name

- **Keyset pagination (`?cursor`).** Offset paging is here; the cursor is
  not. `?cursor` is refused with a message saying so rather than ignored.
- **Sparse projections (`?select`).** Every read returns the full row.
- **Relation expansion (`?expand`).** One statement that joined a target and
  built a JSON object for it was the engine, not the surface. Fetch the related
  row from its own endpoint.
- **The JSON filter tree (`?filter=`).** Arbitrary and/or/not nesting is gone;
  the query-parameter operators are not. Negation survives only where an
  operator spells it — `ne`, `nin`, `notnull` — so a filter that leaned on a
  `not` group has to be restated as one of those or moved into the handler.
- **Array and document operators** (`has`, `hasany`, `hasall`, `hasdoc`, and
  their negations `nhas`, `nhasany`, `nhasall`, `nhasdoc`). The columns are
  still there and still returned; the containment operators are not.
- **The OpenAPI document**, and with it the generated TypeScript, Dart and CLI
  clients. They were emitted from the schema, and the schema is what you are
  leaving. The wire format they speak is unchanged, so a committed client keeps
  working — it just has no generator behind it any more.
- **Hooks other than the two seams above.** `BeforeCreate`, `AfterUpdate` and the
  rest were registrations on a runtime that is no longer here; the handler is a
  function, so the code that ran in a hook goes in it.
- **Transactions across handlers.** Each handler runs one statement. `DB` is an
  interface a `pgx.Tx` satisfies, so wrapping is yours to arrange.
- **Type overrides.** The models use the default type mapping; a column that had
  a `Types` override in the generator has its default Go type here. Enums are
  plain strings, and the CHECK constraint in `schema.sql` is what still
  enforces the value set.

## Notes for this schema

- `authors` expanded `org` through `?expand`. The foreign key is still returned; the joined row is not.
- `posts` expanded `author` through `?expand`. The foreign key is still returned; the joined row is not.

## Wiring it up

```go
mux := http.NewServeMux()
pool, err := pgxpool.New(ctx, dsn)
if err != nil {
	log.Fatal(err)
}
if err := ejected.Register(mux, pool, ejected.Options{
	Posts: ejected.PostsHooks{
		// Required: deleted_at declares a soft delete.
		Confine: func(r *http.Request) ([]ejected.Condition, error) {
			return []ejected.Condition{
				{Column: "deleted_at", Op: ejected.OpIsNull},
			}, nil
		},
	},
}); err != nil {
	// A resource whose schema declared Scoped or SoftDelete fails here rather
	// than serving unconfined rows.
	log.Fatal(err)
}
log.Fatal(http.ListenAndServe(":8080", mux))
```
