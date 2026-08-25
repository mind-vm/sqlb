# Rejections

A column that does not declare a capability cannot be reached through it, and
the rejection is data rather than prose
([ADR-0011](../architecture.md#actionable-errors)):

```json
{
  "title": "Bad Request", "status": 400,
  "detail": "one or more query parameters were rejected",
  "errors": [{
    "message": "column is not sortable",
    "location": "query.sort", "value": "body",
    "allowed": ["title", "status", "view_count", "published_at", "created_at"]
  }]
}
```

Every problem in a request is reported at once, not one per round trip — a
malformed request takes one round trip to fix rather than one per mistake. The
caller most likely to read this is a program assembling requests against a
schema it only partly knows, and "column is not sortable" is a dead end where
the same message plus the sortable columns is a fix.

The full catalogue of messages is in the
[rejection reference](../reference/rejections.md).

## Reading it in Go

Reach the structured form with `filter.AsErrors`, which unwraps as it goes.
Prefer it to a type assertion, which panics the moment a middleware wraps the
error:

```go
if errs, ok := filter.AsErrors(err); ok {
    errs.WriteHTTP(w)
    return
}
```

## A hidden column appears nowhere

Not as a parameter, not in the response schema, and **not in that `allowed`
list**. It cannot be recovered by probing.

That is the difference between "not permitted" and "not present", and it is why
`Hidden` plus `Filterable` is a schema validation error rather than a
combination you can write: a filterable secret can be recovered a character at a
time by an attacker who is patient about 200s and 404s.

## What each status means here

| Status | Cause |
|---|---|
| 400 | The query string could not be understood, or named something that has not opted in. Carries `errors[]` with allow-lists |
| 404 | No row matched, after hooks applied their predicates. A row confined away by a tenant scope is indistinguishable from one that does not exist, which is the intent |
| 409 | A unique or exclusion constraint refused the write — the request is well formed and would be valid against a different state of the database |
| 422 | The body parsed but is not acceptable: a foreign key, check or not-null constraint, cross-field validation from `Row()`, or a hook that refused |
| 500 | Anything the layer could not classify. The body says only that the request could not be completed, and the error is logged |

The line between those is not left to chance. A constraint violation is
classified from its SQLSTATE into a `sqlb.ConstraintError` and answered in the
terms of the request — so a duplicate is a 409 rather than the 500 an
unrecognised database error would otherwise become. The constraint's *name* is
deliberately absent from the body: it is available to Go callers on the error
value, which is where branching on it belongs, and putting it on the wire would
publish an internal identifier to whoever provoked it.

That is also why the unclassified case is so blunt. An unwrapped database error
names tables, columns and constraints and can carry the compiled SQL with it, so
it goes to the log and the caller gets a sentence. An error that already carries
a status is passed through unchanged, so a hook's deliberate refusal keeps the
status it chose.

The reverse also holds, and it is easy to miss from the hook's own type: a hook
that returns a plain `errors.New(...)` carries no status, so it lands in the
blunt case above and answers 500 — a refusal for a missing tenant header reads
identically to a bug. Return a `huma.StatusError` instead —
`huma.Error400BadRequest("X-Workspace-Id is required")` for that case — and
[`sqlb.Hooks`'s doc comment](https://pkg.go.dev/github.com/mind-vm/sqlb#Hooks)
says the same thing from the side a hook author is actually looking at
([#255](https://github.com/mind-vm/sqlb/issues/255)).

See [Mutations](../queries/mutations.md#when-the-database-refuses-a-write) for
the Go side, including the driver classifier that fills in the constraint name.

## It survives to the last consumer

The allow-list reaching a JSON body is only half the guarantee. The generated
[TypeScript client](../typescript/README.md) types that body rather than
flattening it to a message, so a UI can offer the alternatives:

```ts
import { allowedFor, isProblem } from './api/client.gen';

if (isProblem(body)) {
  const sortable = allowedFor(body, 'query.sort');  // ["title", "view_count", ...]
}
```

And the [CLI](../cli/README.md) prints it, keeping the list intact:

```
$ taskctl tasks list --sort -nonexistent
Error: the request could not be understood (HTTP 400)
  query.sort: column is not sortable
    allowed: title, status, priority, due_at, completed_at, position, comment_count
```

For the CLI there is a stronger version still: a column that never declared
`.Filterable()` has **no flag at all**, so the request the server would reject
has no spelling. The rejection is the fallback, not the mechanism.

## Next

- [Mounting resources](README.md)
- [Capabilities](../concepts/capabilities.md) — why the refusal is shaped this way
- [ADR-0011](../architecture.md#actionable-errors) — the decision record
