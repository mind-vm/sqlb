# Typed columns

The engine is reflective, so `sqlb.F("titel")` is a runtime error. Since codegen
is already emitting models, it also emits a typed column set:

```go
q := sqlb.Query[Post]().
    Where(blog.PostCols.Status.Eq(blog.PostStatusPublished)).
    Where(blog.PostCols.Title.Contains(search)).
    OrderBy(blog.PostCols.ViewCount.Desc())
```

| | |
|---|---|
| `PostCols.Titel.Eq(…)` | does not compile — misspelled column |
| `PostCols.ViewCount.Eq("x")` | does not compile — wrong comparand type |
| `PostCols.ViewCount.Contains("x")` | does not compile — text operator on an integer |
| `AuthorCols.PasswordHash` | does not exist — hidden columns are omitted |
| `PostCols.Labels.Contains("x")` | does not compile — an array is not text |

The last three are why `Col[T]` does not embed `Field`: embedding would promote
every operator onto every column, so `Contains` on an integer would compile,
reach the database, and fail there. Pattern operators live on `TextCol[T
~string]` instead.

An array column gets `ArrayCol[E]`, carrying the containment operators and
nothing else — `Has` takes one element, `HasAny` and `HasAll` take a list:

```go
q.Where(tasks.TaskCols.Labels.Has("urgent"))
q.Where(tasks.TaskCols.Labels.HasAll("backend", "infra"))
```

`Has(42)` does not compile, and neither does an ordering operator: Postgres
would order arrays, but that is not an ordering the API offers.

Nullable columns are typed as their base type — `published_at` is `*time.Time`
on the model but `Col[time.Time]` here — so the comparand is a `time.Time` and
NULL is expressed with `IsNull` rather than by comparing against a pointer.

An `Enum` column emits a named Go string type with one constant per value, so
`blog.PostStatusPublished` exists and a typo does not compile. That is also what
carries the enum's values into the generated
[TypeScript client](../typescript/README.md) and the
[CLI's `--help`](../cli/README.md).

## Typed update statements

The same generation covers writes. `UpdatePost()` returns a statement with a
setter per writable column, typed by that column:

```go
_, err := blog.UpdatePost().
    SetStatus(blog.PostStatusPublished).
    Where(blog.PostCols.ID.Eq(id)).
    Stmt().
    Exec(ctx, db)
```

Worth using, since the untyped `Set(string, any)` checks the column name against
the model but not the value's type.

The wrapper carries the setters and `Where`, and **`Stmt()` is where the
statement itself is** — `Exec`, `One`, `Everything`, `SetExpr`, `Resolved`.
It is one call rather than a re-export of `Update[T]`'s surface because the
wrapper's job is the typed half; the rest is already spelled once, on the
statement.

`SetExpr` is the case worth naming, since it is why the escape hatch is one
method away rather than absent. Incrementing a counter in the database rather
than read-modify-write has no typed spelling — the value is an expression, not
a `int64` — so it goes through `Stmt()`:

```go
_, err := blog.UpdatePost().
    Where(blog.PostCols.ID.Eq(id)).
    Stmt().
    SetExpr("view_count", sqlb.Raw{SQL: "view_count + ?", Args: []any{1}}).
    Exec(ctx, db)
```

**Every writable column gets a setter, including `ReadOnly` and `Immutable`
ones.** Those two are REST-boundary rules and the boundary is defended where it
exists — they are absent from the generated request bodies and the handler
clears them — so excluding them here protected nothing. It only meant that the
code which is the *sole* writer of a `ReadOnly` column was the one code that
could not write it typed, and had to reach for `Set(string, any)` on exactly
the columns where a typo costs most.

Two columns stay out. The primary key, because it addresses the row rather than
being part of what an update writes; and a computed column, because `ReadOnly`
is a rule about who may write and a computed column has nothing to write to — a
setter for one would compile and then fail every statement it was used in.

## When it is not available

The facade is generated, so it exists only on the schema-first path. With
[structs you described yourself](../start/structs-first.md), `sqlb.F("column")`
is the vocabulary, and column names are checked against the model at build time
rather than at compile time — a real difference, and the main thing codegen buys
on the query side.

## Testing that the refusals hold

A facade that stopped refusing would be invisible to an ordinary test, because
the cases that matter are the ones that must *not* compile. The repository
checks them by attempting to compile each and confirming it fails: a test that
passes vacuously when the facade breaks is worse than no test.

The generated TypeScript client is checked the same way, with `@ts-expect-error`
over the requests that must not type — see
[`example/tasks/web/src/refusals.ts`](../../example/tasks/web/src/refusals.ts).

## Next

- [Queries](README.md) — the builder these columns feed
- [Mutations and transactions](mutations.md) — the typed update statement in
  context
- [ADR-0009](../architecture.md#typed-column-facade) — why the facade is generated
  rather than reflective
