# sqlb

[![Go Reference](https://pkg.go.dev/badge/github.com/jryannel/sqlb.svg)](https://pkg.go.dev/github.com/jryannel/sqlb)
[![CI](https://github.com/jryannel/sqlb/actions/workflows/ci.yml/badge.svg)](https://github.com/jryannel/sqlb/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/jryannel/sqlb)](go.mod)
[![License: Proprietary](https://img.shields.io/badge/license-proprietary-lightgrey.svg)](LICENSE)

Declarative Postgres for Go. A schema is ordinary Go, and everything else is
derived from it: migrations, typed models, composable queries, REST handlers,
an OpenAPI document, and clients for TypeScript, Dart and the command line.
Nothing downstream is written by hand, so nothing drifts out of step.

**[Documentation](https://jryannel.github.io/sqlb/)** ·
[Quickstart](https://jryannel.github.io/sqlb/start/quickstart/) ·
[Cheatsheet](docs/cheatsheet/README.md) ·
[API reference](https://pkg.go.dev/github.com/jryannel/sqlb) ·
[Decision records](https://jryannel.github.io/sqlb/project/architecture/#decisions)

## Why

Static query generators cannot express *"this WHERE clause exists only when the
user typed something in the search box."* The usual workaround is string
concatenation, which is why the HTTP layer of a filter/sort/search page is
mostly boilerplate.

PostgREST solves that by making the database the API, but there is then nowhere
to put Go domain logic, and the whole schema sits one policy mistake away from
being public.

sqlb takes the middle path. A query is a **value**, so predicates can be added
conditionally:

```go
q := sqlb.Query[Post]().Where(sqlb.F("status").Eq("published"))
if search != "" {
    q = q.Where(sqlb.F("title").Contains(search))
}
posts, err := q.OrderBy(sqlb.F("created_at").Desc()).Limit(50).All(ctx, db)
```

and the REST filter grammar compiles into that **same** predicate AST. One
compiler, one bind-parameter discipline, one set of hooks — two producers.

## What that buys

- **Capabilities are opt-in per column.** `Filterable`, `Sortable`,
  `Searchable`, `Hidden`. A column that does not declare a capability cannot be
  reached through it — ever, and the failure is a 400 naming what *would* have
  been accepted, not a leak. This is the difference between this and exposing
  the database.
- **Hooks are the domain seam.** `BeforeQuery` receives the query itself, so one
  registration constrains every read of a model — including the reads that
  generated REST handlers issue. Tenant scoping stops being something each call
  site has to remember.
- **Paging that survives a write.** `?cursor=` names the position of the last
  row rather than counting to it, so page 500 costs what page 1 costs and a
  concurrent insert cannot make a client read a row twice. Every list response
  carries the cursor for the next page, so adopting it needs no flag.
- **Nothing runs unasked.** `SQL()` renders text and args without executing.
  `Explain` plans against the live schema without running it, so it also fails
  on the migration that was written and never applied — which a compile-time
  column check cannot. `Diff` returns migration changes as values; your runner
  applies them.
- **The clients are generated from the schema too.** A TypeScript client,
  emitted into the repository that consumes it, where `where` admits only
  filterable columns with the operators their type accepts, `select` narrows the
  response type, and a hidden column has no spelling at all. The OpenAPI
  document cannot say any of that — `?status=eq.published` documents as
  `array<string>` — so it is generated from the model instead
  ([guide](https://jryannel.github.io/sqlb/typescript/)). The same vocabulary
  reaches a Flutter app as Dart — plus the cursor pager an infinite-scrolling
  list needs, which is the piece a mobile client otherwise rebuilds out of
  `has_more` and an offset counter
  ([guide](https://jryannel.github.io/sqlb/dart/)) — and **Go**, as a typed
  client that imports the standard library and nothing else, plus an optional
  [cobra](https://github.com/spf13/cobra) command tree over it: one flag per
  filterable column, its operators in the usage string, so `--help` states what
  a resource accepts without a request — which is the form the guarantee has to
  take for a caller with no compile step, such as an agent
  ([guide](https://jryannel.github.io/sqlb/cli/)).
- **A live view is a subscription, not a poll.** `rest.Events` mounts a
  Server-Sent Events stream that carries the *address* of a change —
  `{table, key, op}` — and never the row, so the refetch goes through the
  ordinary read path and every rule that path enforces still holds. Two sources
  behind one endpoint and one wire format: `rest.Broker` is in-memory and
  single-replica, `outbox.Dispatcher` writes the event in the transaction that
  made the change and is at-least-once across any number of replicas, and
  swapping them is a constructor call. The subscriber is generated too, in
  TypeScript and in Dart, so what an application writes is which cache to
  invalidate rather than which keys to invalidate in it
  ([guide](https://jryannel.github.io/sqlb/rest/events/)).
- **One dependency, and it is the one you already have.** The engine is written
  on [pgx](https://github.com/jackc/pgx) and takes nothing else; a CI gate fails
  on anything that is not pgx or something pgx itself pulls in. That is a
  deliberate reversal — sqlb used to depend on the standard library alone, and
  [ADR-0040](docs/architecture.md#the-driver-is-a-dependency) says what it bought:
  sqlb writes join a `pgx.Tx` your own code opened, arrays need no codec, and
  pgvector's binary format is reachable. Only the REST adapter pulls in
  [Huma](https://huma.rocks), and only if you use it. The generated TypeScript,
  the generated Dart and the generated CLI are separate toolchains and separate
  opt-ins; the emitters produce text, so `codegen` itself takes nothing.

## Install

```bash
go get github.com/jryannel/sqlb
```

Go 1.25 or newer, and Postgres.
[Quickstart](https://jryannel.github.io/sqlb/start/quickstart/) goes
from here to a running server.

The generator is a command, and the loop is one line each way:

```bash
go install github.com/jryannel/sqlb/cmd/sqlb@latest

sqlb generate ./schema                # models, typed columns, REST bodies, manifest, clients
sqlb check ./schema                   # the CI drift gate: writes nothing, fails if stale
sqlb migrate -name adds_slug ./schema # the migration that closes the gap
sqlb eject ./schema                   # the way out: the schema as SQL, the resources as
                                      # plain net/http handlers over pgx, importing no sqlb
```

The argument is the package that declares your schema, and the package says what
to emit and where by exporting one function. Because the schema is Go, `sqlb`
compiles a driver against your module to read it — see
[ADR-0032](docs/architecture.md#sqlb-command) for why that is forced and what it
costs.

`generate` and `check` need no database. `migrate` works out the current schema
by replaying your committed migrations into a scratch Postgres, because reading
a live one tells you what the database looks like rather than whether the
migrations produce it — so it needs an empty database, except for the very first
migration, which diffs against nothing.

The schema DSL and code generation are both optional: `sqlb.Describe[T]()`
layers the same capabilities over structs you already have, including stock
[sqlc](docs/with-sqlc.md) output, without editing them. Moving one endpoint
across is [worked in four stages](docs/refactoring-from-sqlc.md), each a place
to stop, with a test that requires all four to return the same rows.

And the way out is generated too. `sqlb eject` writes a package that depends on
pgx and the standard library — your schema as DDL, your statements as SQL you
can read, your endpoints as `net/http` handlers — with everything it does not
carry refused by name rather than quietly missing.
[`example/blog/ejected`](example/blog/ejected/) is a committed one, and a test
serves it beside the generated resources it came from and compares the answers
request by request. See [the way out](docs/eject.md).

## Status

**Pre-1.0, one author, no observed consumers.** That is the honest starting
position, and no amount of feature work substitutes for elapsed time under real
traffic. [Compatibility](https://jryannel.github.io/sqlb/project/compatibility/)
says what `v0.1.0` freezes and which surfaces are expected to move.

What *is* proven, and re-checked on every run rather than asserted: CI applies
the generated DDL to a real Postgres 18, reads it back with `introspect`, and
requires the round trip to be a fixpoint; the query path runs through a real
PgBouncer in transaction pooling, because that is the deployed topology; and the
blog example is generated from its schema, so every behaviour test in it is also
a test of the generator's output.

Postgres only. `LISTEN/NOTIFY`, jsonb aggregation and `RETURNING` are all
load-bearing; multi-dialect support would cost the best features.

Not built yet, in the order they matter: an MCP server over the manifest.
[Vision](https://jryannel.github.io/sqlb/project/vision/) has the detail.

## Documentation

| | |
|---|---|
| [Start here](https://jryannel.github.io/sqlb/start/) | Overview, quickstart, a worked first app, structs-first adoption |
| [Concepts](https://jryannel.github.io/sqlb/concepts/) | The five ideas the rest of it rests on |
| [Cheatsheet](docs/cheatsheet/README.md) | Every surface on one page — schema DSL, builder, mutations, hooks, filter grammar, migrations, codegen, CLI. A lookup table, and the file to hand a coding agent ([on the site](https://jryannel.github.io/sqlb/cheatsheet/)) |
| [Schema](https://jryannel.github.io/sqlb/schema/) · [Queries](https://jryannel.github.io/sqlb/queries/) · [REST](https://jryannel.github.io/sqlb/rest/) · [TypeScript](https://jryannel.github.io/sqlb/typescript/) · [Dart](https://jryannel.github.io/sqlb/dart/) · [CLI](https://jryannel.github.io/sqlb/cli/) · [Migrations](https://jryannel.github.io/sqlb/migrations/) | One section per surface |
| [Examples](https://jryannel.github.io/sqlb/examples/) | Six worked applications, and what each one proves |
| [Reference](https://jryannel.github.io/sqlb/reference/) | Filter operators, column types, capabilities, codegen options, CLI, rejections |
| [Architecture](https://jryannel.github.io/sqlb/project/architecture/) | How the pieces fit, the request path, where safety lives |
| [Decision records](https://jryannel.github.io/sqlb/project/architecture/#decisions) | What was decided, why, and what would change our mind |
| [`example/recipes`](example/recipes/) | Eighty-odd small examples, one file per aspect — the place to look when you know what you are building and need to know how one piece is spelled |
| [`example/blog`](example/blog/) | A worked schema and everything codegen emits from it |
| [`example/tasks`](example/tasks/) | A multi-tenant task manager: auth, migrations, a runnable server, and a generated TypeScript client, Dart client and CLI |
| [`example/fxapp`](example/fxapp/) | The same pieces assembled by uber-go/fx on the [`fxkit`](example/fxapp/fxkit/) glue — copyable, not importable: hooks arriving through a value group, a pluggable auth module behind the principal seam, and a resource that refuses to mount without its hooks |
| [`example/auth-workos`](example/auth-workos/) | A `sqlb.Verifier[T]` adapter verifying WorkOS AuthKit access tokens against WorkOS's JWKS endpoint — the principal seam's first real, non-hand-rolled adapter |
| [`example/computed`](example/computed/) | Four ways to get a derived value out of Postgres — generated columns, trigger counters, projected expressions, views — and where sqlb's ceiling is today ([ADR-0041](docs/architecture.md#computed-fields)) |
| [`example/evolve`](example/evolve/) | A schema that changed five times: what is free, what destroys data, and the rename that is a clean migration and a broken client at once ([the walkthrough](docs/migrations/refactoring-a-database.md)) |
| [`example/withsqlc`](example/withsqlc/) | sqlb and sqlc over one schema, plus one list endpoint in [four stages](docs/refactoring-from-sqlc.md) from static SQL to a generated resource |
| [`example/meter`](example/meter/) | An arithmetic upsert under real concurrency, the composite-key workaround it needs, and the `date_trunc`/empty-aggregate traps a metering chart hits first ([docs/special-cases.md](docs/special-cases.md#2-meter--the-write-is-an-increment)) |
| [`example/rooms`](example/rooms/) | An `EXCLUDE` constraint stopping double-booked rooms under contention, and the silent-zero-rows timestamptz day-filter trap beside it ([docs/special-cases.md](docs/special-cases.md#4-rooms--two-bookings-cannot-overlap)) |
| [`example/attachments`](example/attachments/) | Presigned direct-to-S3 uploads — RustFS, MinIO, anything S3-compatible — where the bytes never pass through the server: the row-before-bytes ordering, an object deleted only after the commit, a sweeper for both directions of what that leaves behind, and a stdlib SigV4 presigner cross-checked against `aws-sdk-go-v2` |
| [`example/vault`](example/vault/) | A table whose entire payload is `Hidden`: a generated read surface, no generated write surface at all, and the hand-written endpoint that fills the gap ([docs/special-cases.md](docs/special-cases.md#5-vault--the-row-whose-payload-only-go-may-write)) |
| [`example/catalog`](example/catalog/) | A self-referencing category tree with a real foreign key via `TableDef.AddField`, and where `ILIKE` search stops on purpose ([docs/special-cases.md](docs/special-cases.md#6-catalog--the-tree-and-where-search-stops)) |
| [`example/outbox`](example/outbox/) | A competing-consumers job queue — `FOR UPDATE SKIP LOCKED`, retry backoff, dead-letter — explicitly one worked answer rather than a format sqlb ships ([ADR-0012](docs/architecture.md#change-feed-outbox)) |
| [`example/tasks-evolved`](example/tasks-evolved/) | Six non-additive schema changes in a row against live data — a rename, a NOT NULL backfill, a destructive drop, an index build that fails and leaves an invalid index behind ([docs/special-cases.md](docs/special-cases.md#1-tasks-evolved--the-second-year)) |

## Development

```bash
mise run test    # the inner loop; no Docker or Postgres needed
mise run ci      # the full gate, same as .github/workflows/ci.yml
mise tasks       # everything else
```

Tool versions are pinned in `mise.toml`, so a green run locally and a green run
in CI use the same Go and the same linter. The engine's tests run against an
in-memory executor rather than a database, which keeps the inner loop fast;
`test-pg` answers what that cannot — whether the generated SQL is *valid* rather
than merely expected — and is part of `ci`.

[CONTRIBUTING.md](CONTRIBUTING.md) has what a change is expected to carry, and
where to argue with a decision record rather than around it.

## License

Proprietary. All rights reserved — see [LICENSE](LICENSE). Use requires a
separate written agreement with the copyright holder.
