# Quickstart

By the end of this page you have a schema, generated models, a query running
against Postgres, and a REST API in front of it.

sqlb needs Go 1.25 or newer and Postgres.

```bash
go get github.com/mind-vm/sqlb
```

## See one running first

Before building one, run one. [`example/tasks`](../../example/tasks/) is
everything on this page assembled into a multi-tenant task manager:

```bash
cd example/tasks
docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18
TASKS_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
  TASKS_JWT_SECRET="$(head -c 32 /dev/urandom | base64)" go run ./cmd/server
```

Migrations apply at startup, so an empty database is enough.
<http://localhost:8080/docs> is then the API generated from its schema — with
per-column filter operators, enumerated sort values and pagination, none of it
hand-written. That document is the thing this page is teaching you to produce.

## Two ways in

sqlb works in either direction, and you can start with one and move to the
other:

- **Schema-first** — declare tables as Go values, generate models, migrations
  and REST handlers from them. This is the path below, and the one the rest of
  the documentation assumes.
- **Structs-first** — you already have model structs, from another generator or
  written by hand. See [Using your own structs](structs-first.md); nothing here
  requires the DSL.

## Or scaffold it

The five steps below, run for you. `sqlb init` writes a project with one
table, everything `generate` produces from it, and a server built on
`rest.Serve` — most of the way through step 4 in about five commands:

```bash
go install github.com/mind-vm/sqlb/cmd/sqlb@latest
sqlb init -module github.com/you/blog
cd blog
go mod tidy
go generate ./...
go run ./cmd/server
```

`init` writes the project and prints those last three commands rather than
running them itself — each needs something it cannot promise: `go mod tidy`
needs the network to resolve a module it just started depending on, and
`go generate` needs that resolution to have already happened. What lands on
disk is `blogschema/schema.go` (one `Task` table, exposed as CRUD), the four
generated files [step 2](#2-generate) below produces from it, and a
`cmd/server/main.go` built on [`rest.Serve`](../rest/README.md#serve-the-whole-server)
with migrations applied from disk at startup, plus a `predicate_test.go` that
passes without a database ([step 5](#5-scope-every-read)) — `<dir>` must not
already contain a `go.mod`.

The rest of this page builds the same shape by hand, one decision at a time —
worth reading once a single table stops being enough.

## 1. Declare a schema

The schema is ordinary Go, in a package of its own. It lives apart from the
generated models because the two share names — `blogschema.Post` is the table
declaration, `blog.Post` is the row struct — and keeping them separate is what
lets both be called `Post`.

```go
// blogschema/schema.go
package blogschema

import "github.com/mind-vm/sqlb/schema"

var Author = schema.Table("authors",
    schema.UUIDv7("id").PrimaryKey(),
    schema.Text("email").Unique().Searchable(),
    schema.Text("name").Searchable().Sortable(),
    schema.Text("password_hash").Hidden(),
    schema.Timestamps(),
)

var Post = schema.Table("posts",
    schema.UUIDv7("id").PrimaryKey(),
    schema.Ref("author", Author).OnDelete(schema.Restrict),
    schema.Text("title").Searchable().Sortable(),
    schema.Enum("status", "draft", "review", "published").
        Default(schema.Value("draft")).
        Filterable().
        Sortable(),
    schema.Timestamps(),
).
    Index("author_id").
    Expose(schema.REST{Ops: schema.CRUD | schema.OpList, MaxPageSize: 100})
```

**Before you run this against your own database:** `UUIDv7` defaults to
`uuid_generate_v7()`, which is the [`pg_uuidv7`](https://github.com/fboulnois/pg_uuidv7)
extension's spelling, so the generated DDL will *not* apply to a stock install.
Three ways out, and you have to pick one:

| Your Postgres | Do this |
|---|---|
| 18 or newer | Pass `migrate.MinPostgres(18)` to `Diff` — it emits the built-in `uuidv7()` |
| 13–17 | `schema.UUID("id").PrimaryKey().Default(schema.GenUUIDv4())` — `gen_random_uuid()`, built in since 13 |
| Any, with the extension installed | Nothing; the default is already correct |

This is the one place the documentation will hand you DDL your server rejects,
which is why it is called out here rather than in
[Migrations](../migrations/README.md).

Two things are doing the work here.

**Capabilities are opt-in.** `Filterable`, `Sortable`, `Searchable`, `Hidden`. A
column that does not declare a capability cannot be reached through it — ever.
`password_hash` is readable by your Go code and absent from every REST response,
filter vocabulary and rejection message. This is the difference between sqlb and
exposing your database.

**`Expose` is what publishes a table.** Without that call, `authors` above is
reachable from Go and has no HTTP surface at all.

See [Declaring tables](../schema/README.md) for the full column vocabulary.

## 2. Generate

Codegen is a normal Go program that imports your schema package for its side
effects — declaring a table registers it — and writes the artefacts.

```go
// blogschema/gen/main.go
package main

import (
    "github.com/mind-vm/sqlb/codegen"
    "github.com/mind-vm/sqlb/schema"

    _ "yourmodule/blogschema"
)

func main() {
    codegen.Must(codegen.Generate(codegen.Options{
        Registry: schema.DefaultRegistry(),
        Dir:      "blog",
        Package:  "blog",
    }))
}
```

```bash
go run ./blogschema/gen
```

That writes four files into `blog/`:

| File | Contents |
|---|---|
| `models_gen.go` | The row structs, with `db` and `sqlb` tags |
| `columns_gen.go` | The typed column facade, and typed update statements |
| `rest_gen.go` | Request bodies and a `Register` function, one call per exposed table |
| `sqlb.json` | The manifest: every column, its capabilities, the operator vocabulary |

Wire it to `go generate` with a directive in the schema file, and add
`codegen.Check` to CI — generated code is committed, so it drifts the first time
someone edits a schema and forgets to regenerate. The
[TypeScript client](../typescript/README.md), the
[Dart client](../dart/README.md) and the [Go CLI](../cli/README.md) are three
more options on this same call.

**Run `go mod tidy` again after generating, not just before.** A schema
feature can pull in a package only the code generate just wrote imports — the
outbox/events feature reaching for huma's SSE adapter is the case that has
bitten a real port. `go mod tidy` before generate cannot see a dependency that
does not exist yet, and nothing runs it again for you, so a plain `go build`
right after `go generate` can fail on an import nobody wrote by hand. The
`sqlb generate` command (what `sqlb init`'s scaffold wires a `//go:generate`
directive to) prints a line saying so whenever the files it just wrote differ
from what was there before; a hand-rolled `gen/main.go` like the one above,
calling `codegen.Generate` directly, does not, so treat "run `go mod tidy`
again" as the default after any generate that touched files, not only when
told to.

## 3. Query

```go
// The Executor is pgx-native (ADR-0040), so a *pgxpool.Pool is what you pass.
// A *pgx.Conn and a pgx.Tx satisfy it as they stand, which is what lets a
// hook run inside a caller's transaction.
db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
defer db.Close()

posts, err := sqlb.Query[blog.Post]().
    Where(sqlb.F("status").Eq("published")).
    OrderBy(sqlb.F("created_at").Desc()).
    Limit(50).
    All(ctx, db)
```

A query is a **value**, not a statement that runs when you build it. That is the
point: a predicate can be added on a branch, which is exactly what static query
generators cannot express.

```go
q := sqlb.Query[blog.Post]().Where(sqlb.F("status").Eq("published"))
if search != "" {
    q = q.Where(sqlb.F("title").Contains(search))
}
posts, err := q.All(ctx, db)
```

`SQL()` renders the statement and its bind parameters without running anything,
which is the inspection point — log it, diff it in a test, paste it into
`EXPLAIN`:

```go
sql, args, err := q.SQL()
// SELECT "posts"."id", ... FROM "posts" WHERE ("status" = $1) AND ("title" ILIKE $2)
// [published %postgres%]
```

Values never reach the SQL text. Every user-supplied value becomes a bind
parameter, and only identifiers validated against the model are interpolated.

Prefer the generated typed columns to `sqlb.F` where you can — `blog.PostCols`
puts column names and comparand types back under the compiler:

```go
q := sqlb.Query[blog.Post]().
    Where(blog.PostCols.Status.Eq(blog.PostStatusPublished)).
    OrderBy(blog.PostCols.CreatedAt.Desc())
```

`PostCols.Titel` does not compile. Neither does `PostCols.ViewCount.Eq("x")`,
nor `PostCols.ViewCount.Contains("x")`. Hidden columns are not in the struct at
all. See [Typed columns](../queries/typed-columns.md).

## 4. Serve

`rest` takes a `huma.API` rather than building a router, so your router and its
middleware stay yours:

```go
router := chi.NewRouter()
router.Use(middleware.RequestID, middleware.Recoverer, yourAuth)

api := humachi.New(router, huma.DefaultConfig("Blog", "1.0.0"))
if err := blog.Register(api, db); err != nil {   // generated
    return err
}
http.ListenAndServe(":8080", router)
```

You now have list, read, create, patch and delete for every exposed table, with
filtering, sorting, search, pagination and an OpenAPI document built from each
column's capabilities. See [Mounting resources](../rest/README.md).

That snippet is the HTTP layer alone. The pool-open, migrate, listen,
graceful-shutdown code around it is identical in every sqlb server, and
`rest.Serve` writes it once for you — see [Serve: the whole
server](../rest/README.md#serve-the-whole-server).

## 5. Scope every read

Before this is safe to deploy multi-tenant, register the constraint once:

```go
sqlb.On[blog.Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[blog.Post]) error {
    org, ok := auth.OrgFrom(ctx)
    if !ok {
        return auth.ErrNoTenant
    }
    q.Where(sqlb.F("org_id").Eq(org))
    return nil
})
```

`BeforeQuery` receives the query itself, so this one registration applies to
every read of the model — including the reads the generated REST handlers
issue. Tenant scoping stops being something each call site has to remember, and
a hook returning an error aborts the operation.

A table can also *declare* that it expects to be scoped, so the missing
registration is caught at startup rather than discovered in production. See
[Hooks](../queries/hooks.md).

And then prove it reached the statement. A test against a real database sees
the right rows whether the hook narrowed the query or the fixture happened to
hold only matching ones, and it sees zero rows whether a read was refused or
merely matched nothing — which is the difference between a boundary and an
empty filter. `sqlbtest` records the SQL your code produced and the values it
bound, so both are answerable in a test that needs no Postgres:

```go
exec := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id", "title"}})
db := sqlb.New(exec).WithHooks(reg)

if _, err := sqlb.Query[blog.Post]().All(orgCtx("acme"), db); err != nil {
    t.Fatal(err)
}
if !strings.Contains(exec.LastStatement(), `"org_id" = $1`) {
    t.Errorf("the scoping hook did not reach the statement:\n%s", exec.LastStatement())
}
```

`sqlb init` scaffolds this test, and [Testing](../queries/testing.md) has the
rest — including the pattern that keeps these and the round-trip ones in one
`go test ./...`.

## Next

- [Your first app](first-app.md) — a complete worked one, small enough to read
- [Concepts](../concepts/README.md) — the five ideas the rest of this rests on
- [Declaring tables](../schema/README.md) — the full column vocabulary
- [Queries](../queries/README.md) — predicates, aggregates, transactions
- [Testing](../queries/testing.md) — predicates with no database, rows with one
