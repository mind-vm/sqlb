# Overview

sqlb is declarative Postgres for Go. You declare your tables once, as ordinary
Go values, and everything else is derived from that one declaration: migrations,
typed models, composable queries, REST handlers, an OpenAPI document, and
clients for TypeScript, Dart and the command line. Domain logic hangs off the
same declaration as hooks, so nothing downstream is written by hand and nothing
drifts out of step.

It exists because the layer between an HTTP handler and the database is
high-volume, low-novelty code that gets rewritten per resource — parse the query
string, decide what a client may filter on, assemble SQL, paginate, count,
serialise, thread authorisation through all of it — and it is a good place for
security bugs to hide.

## Five surfaces, one declaration

Each of these is a section of this documentation, and each is independently
optional. A project can take the query builder and nothing else.

| Surface | What it gives you | |
|---|---|---|
| **Schema** | Tables as Go values: columns, references, indexes, constraints, and the capabilities that decide what the outside world can reach | [Declaring tables](../schema/README.md) |
| **Queries & domain logic** | A query is a value, so predicates compose on a branch. Hooks are where the rules live | [Queries](../queries/README.md) |
| **REST API** | List, read, create, patch and delete per exposed table, with filtering, sorting, search, pagination and an OpenAPI document | [Mounting resources](../rest/README.md) |
| **TypeScript SDK** | A generated client where `where` admits only filterable columns, `select` narrows the response type, and a hidden column has no spelling | [TypeScript SDK](../typescript/README.md) |
| **Dart SDK** | The same vocabulary for a Flutter app, plus the cursor pager an infinite-scrolling list needs | [Dart SDK](../dart/README.md) |
| **Go CLI** | A cobra command tree over the same vocabulary, so `--help` states what a resource accepts without sending a request | [Go CLI](../cli/README.md) |

Alongside them, [Migrations](../migrations/README.md) turns a schema edit into
files your runner applies — and turns an existing database into a schema file,
which is the same machinery pointed the other way.

## The one idea

A query is a **value**, not a statement that runs when you build it:

```go
q := sqlb.Query[Post]().Where(sqlb.F("status").Eq("published"))
if search != "" {
    q = q.Where(sqlb.F("title").Contains(search))
}
posts, err := q.OrderBy(sqlb.F("created_at").Desc()).Limit(50).All(ctx, db)
```

Static query generators cannot express *"this WHERE clause exists only when the
user typed something in the search box"*, which is why the dynamic parts end up
being built by string concatenation. PostgREST solves that by making the
database the API, but then there is nowhere to put Go domain logic and the whole
schema sits one policy mistake away from being public.

sqlb takes the middle path: the REST filter grammar compiles into the *same*
predicate AST your Go code produces. One compiler, one bind-parameter
discipline, one set of hooks — two producers.

## How this documentation is organized

Four kinds of page, and knowing which you want saves reading the wrong one:

- **[Start here](README.md)** — this section. Takes you through it step by step,
  from `go get` to a running server.
- **[Concepts](../concepts/README.md)** — the reasoning everything else assumes.
  Five pages, one idea each, no API detail.
- **[How-to](../howto/README.md)** — recipes, indexed by the task you arrived
  with rather than the surface that owns it.
- **[Reference](../reference/README.md)** — exactly what each
  thing accepts, as tables rather than prose, plus a
  [glossary](../reference/glossary.md) of the
  vocabulary these pages use.

Between the concepts and the reference sit the seven surface sections in the
table above, one per thing derived from the declaration. The API reference is
none of these: it is the Go doc comments on
[pkg.go.dev](https://pkg.go.dev/github.com/mind-vm/sqlb), where the compiled
`Example` functions are attached to the symbols they document.

## What to read

**New here.** [Quickstart](quickstart.md) goes from `go get` to a running
server in five steps. [Your first app](first-app.md) then walks a complete
worked one.

**Deciding whether to adopt it.** [Concepts](../concepts/README.md) is the
short version of the reasoning — five pages, one idea each.
[How sqlb compares](../comparisons.md) is honest about sqlc, ent, PostgREST,
Atlas and Bun, including when not to use this.
[Compatibility](../compatibility.md) says what is frozen and what is expected
to move.

**Already have model structs.** The schema DSL and code generation are both
optional. [Using your own structs](structs-first.md) layers the same
capabilities over structs you already have, including stock
[sqlc](../with-sqlc.md) output, without editing them.
[Refactoring a sqlc endpoint](../refactoring-from-sqlc.md) walks one all the way
from static SQL to a generated resource, in four stages you can stop after.

**Here with a specific task.** [How-to](../howto/README.md) indexes the recipes
by the question rather than by the surface — adopting a database, moving one
sqlc endpoint, renaming a column without breaking clients, getting out again.

**Already have a database.** [Adopting a database](../migrations/adopting.md)
reads it back into a schema file, and
[Refactoring a database](../migrations/refactoring-a-database.md) is what
happens to it afterwards — adding, renaming and dropping, and which of those
break a client rather than a table.

## Requirements

Go 1.25 or newer, and Postgres. Nothing else: the engine depends on the standard
library alone, and a check in CI enforces it. Only the `rest` package pulls in
[Huma](https://huma.rocks), and only if you use it; the generated TypeScript
client and the generated CLI are separate toolchains and separate opt-ins.

Postgres only, deliberately. `LISTEN/NOTIFY`, jsonb aggregation and `RETURNING`
are all load-bearing, and multi-dialect support would cost the best features
([ADR-0001](../architecture.md#postgres-only)).

## Status

sqlb is pre-1.0, has one author and no observed consumers. That is the honest
starting position, and no amount of feature work substitutes for elapsed time
under real traffic. The
[adoption review](../review-adoption-readiness.md) is an outside read on what
that means in practice.
