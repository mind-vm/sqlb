# Rejection reference

Checked against `rest/errors.go`, `rest/item.go` and `filter/filter.go`. The
guide page is [Rejections](../rest/errors.md).

## The document

RFC 9457 shaped, like Huma's own, so a generated client sees one error type
across the whole API. The one addition is `allowed` on each detail, which
carries what the caller could have asked for instead.

```json
{
  "type": "…",
  "title": "Bad Request",
  "status": 400,
  "detail": "one or more query parameters were rejected",
  "errors": [{
    "message": "column is not sortable",
    "location": "query.sort",
    "value": "body",
    "allowed": ["title", "status", "view_count", "published_at", "created_at"]
  }]
}
```

| Field | Meaning |
|---|---|
| `errors[]` | **Every** problem found, not just the first, so a malformed request takes one round trip to fix rather than one per mistake |
| `message` | What was wrong |
| `location` | A path-like pointer: `query.sort`, `body.title` |
| `value` | The rejected value, echoed back |
| `allowed` | What would have been accepted, where there is a finite set |

**Hidden columns never appear in `allowed`.** The diagnostic must not become an
oracle for what a resource is concealing.

## Query parameter messages

| Message | Cause |
|---|---|
| `unknown parameter` | Not a reserved parameter and not a column of this model |
| `unknown column` | Named in `select`, `sort` or a group, and not a column |
| `column is not filterable` | The column exists and did not declare `Filterable` |
| `column is not sortable` | The column exists and did not declare `Sortable` |
| `relation is not expandable` | The relation exists and is not expandable in the direction asked |
| `unknown operator "…"` | Carries the full operator list |
| `operator "…" needs a text column, but … is …` | A pattern operator on a non-string column |
| `operator "…" needs at least one value` | An empty `in` or `nin` list |
| `operator "between" needs exactly two values, got N` | |
| `expected column.operator.value` | A malformed condition inside a group |
| `expected a parenthesised group such as (status.eq.active,age.gt.18)` | A malformed `?or=` or `?and=` |
| `filter groups nested deeper than 3 levels` | |
| `unknown sort direction "…", expected asc or desc` | |
| `search is not enabled for this resource` | `?search=` where no column is searchable |
| `no column of this resource is searchable` | The same, from the other path |
| `N filters requested, the limit is M` | `MaxFilters`, counted per leaf condition |
| `N sort terms requested, the limit is M` | `MaxSortTerms` |
| `operator "in" was given N values, the limit is M` | `MaxListValues` |
| `value is N bytes, the limit is M` | `MaxValueLength` |
| `must be at least 1` | `page` below 1 |
| `must not be negative` | A negative `offset` or `limit` |
| `not a number` | A non-numeric `page`, `per_page`, `limit` or `offset` |
| `starts past the offset budget of N rows; use ?cursor= to read deeper` | `MaxOffset`, via `?page=` |
| `is past the offset budget of N rows; use ?cursor= to read deeper` | `MaxOffset`, via `?offset=` |
| `sent N times; … takes one value per request` | A repeated single-valued reserved parameter |

Coercion failures carry the type's own message — `expected an integer, got "x"`,
`expected an RFC 3339 timestamp or a date, got "x"`, or
`values of type … cannot be used in a filter` for a column no filter can name.

A page size **above** the maximum is capped rather than rejected, on the
grounds that a client asking for too much should get the maximum.

## Body messages

| Message | Cause |
|---|---|
| `unknown column` | A field of the request body that is not a column |
| `column is read-only` | A field the schema marked `ReadOnly` |

Immutable columns are absent from the update body entirely, so naming one is the
`unknown column` case rather than a distinct message.

An empty PATCH change set is a 400 rather than a no-op update, because it almost
always means the client sent the wrong shape.

## Statuses

| Status | Meaning |
|---|---|
| 400 | The query string or body could not be understood, or named something that has not opted in. Carries `errors[]` |
| 404 | No row matched, **after** hooks applied their predicates. A row confined away by a tenant scope is indistinguishable from one that does not exist, which is the intent |
| 409 | A unique or exclusion constraint refused the write — the request is well formed and would be valid against a different state of the database |
| 422 | The body parsed but is not acceptable: a foreign key, check or not-null constraint, cross-field validation from `Row()`, or a hook that refused |
| 500 | Anything the layer could not classify. The body says only that the request could not be completed; the error is logged |

## Refused writes

`rest` classifies a constraint violation rather than letting it become a 500,
using the same `sqlb.ConstraintError` a Go caller branches on:

| Constraint kind | SQLSTATE | Status |
|---|---|---|
| `ConstraintUnique` | 23505 | 409 |
| `ConstraintExclusion` | 23P01 | 409 |
| `ConstraintForeignKey` | 23503 | 422 |
| `ConstraintCheck` | 23514 | 422 |
| `ConstraintNotNull` | 23502 | 422 |

**The constraint's name is deliberately not in the body.** It is available to Go
callers on `sqlb.ConstraintError`, which is where branching on it belongs;
putting it on the wire would publish an internal identifier to whoever provoked
it.

The same reasoning shapes the unclassified case. An error `rest` cannot
classify answers a generic 500 and is logged, rather than reaching the client —
an unwrapped database error names tables, columns and constraints, and can carry
the compiled SQL with it.

An error that already carries a status is passed through, so a deliberate
refusal from a hook keeps the status it chose rather than being flattened to a
500. Filling in `ConstraintError.Constraint` needs a driver-specific classifier
registered once at startup; see [Mutations](../queries/mutations.md#when-the-database-refuses-a-write).

## Reading it

In Go, from a hand-written handler:

```go
if errs, ok := filter.AsErrors(err); ok {
    errs.WriteHTTP(w)
    return
}
```

`AsErrors` unwraps as it goes. Prefer it to a type assertion, which panics the
moment a middleware wraps the error.

In TypeScript:

```ts
import { allowedFor, isProblem } from './api/client.gen';

if (isProblem(body)) {
  const sortable = allowedFor(body, 'query.sort');
}
```

## Errors that are not rejections

| Sentinel | Meaning |
|---|---|
| `sqlb.ErrNotFound` | `One` matched nothing |
| `sqlb.ErrUnscoped` | An update or delete with no `Where`; call `Everything()` to confirm |
| `sqlb.ErrBadCursor` | A cursor used against a different ordering. The message names both |
| `sqlb.ErrAfterCommit` | The write committed; one or more after-commit callbacks failed. The two cases need opposite responses — see [Hooks](../queries/hooks.md#aftercommit-for-side-effects) |
| `sqlb.ErrConstraint` | The class of every write a database constraint refused. `errors.As` into `*sqlb.ConstraintError` for the kind and, with a classifier registered, the name |

See [ADR-0011](../architecture.md#actionable-errors) for why rejections carry data
rather than prose.
