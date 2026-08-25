# Architecture

How sqlb fits together, and why the seams are where they are. For the reasoning
behind individual choices, see [Decisions](#decisions) below. For where this is
going, see the [vision](vision.md).

*Last reviewed: 2026-07-28.*

## The shape of it

```
  blogschema/schema.go          ← you edit this
         │
         │  go generate ./...           (a generator main, not a CLI)
         ├──────────────▶ migrations/*.sql  DDL, diffed against the last state
         ├──────────────▶ models.go         db + sqlb struct tags
         ├──────────────▶ columns.go        typed column facade
         ├──────────────▶ rest_gen.go       request bodies + registration
         ├──────────────▶ sqlb.json         the manifest
         ├──────────────▶ client.gen.ts     TypeScript client   (ADR-0028)
         ├──────────────▶ client.gen.dart   Dart client         (ADR-0031)
         ├──────────────▶ cli/client/       Go client, stdlib only (ADR-0029)
         └──────────────▶ cli/              cobra tree over it     (ADR-0029)

  The last four are generated from the *schema*, not from openapi.json. The
  OpenAPI document cannot say what they need to say — `?status=eq.published`
  documents as `array<string>`, which is exactly the guarantee being sold —
  so the emitters read the same declaration everything else does.

                    ┌─────────────────────────────┐
   Go code ────────▶│                             │
                    │      predicate AST          │──▶ compiler ──▶ SQL + args
   HTTP query ─────▶│   (sqlb.Pred, sqlb.Expr)    │
     (filter)       └─────────────────────────────┘
                                  ▲
                                  │
                            BeforeQuery hooks
```

Two things carry most of the design. The first is that a **query is a value**,
so it can be built conditionally, handed to a hook to amend, and inspected
without being run ([ADR-0002](#queries-are-values)). The second is
that there is **one predicate AST with two producers** — hand-written Go and the
URL filter grammar — so escaping, authorisation and hook application each happen
exactly once ([ADR-0003](#one-ast-two-producers)).

Almost everything else follows from those two.

## Packages

| Package | Responsibility | Depends on |
|---|---|---|
| `schema` | The declarative DSL and its validation. Design-time only; nothing at runtime imports it. | nothing |
| `.` (`sqlb`) | AST, Postgres compiler, generic builder, model reflection, mutations, hooks, `Describe`, and HTTP authentication middleware (`auth.go`). | stdlib only |
| `filter` | URL grammar → predicates, validated against model capabilities. | `sqlb` |
| `migrate` | Diffs two schemas into changes, renders them as Postgres DDL, and writes migration files for goose, golang-migrate or plain SQL. Does not apply them. | `schema` |
| `introspect` | Reads `pg_catalog` back into a `*schema.Registry`, and reports every construct the DSL cannot express. Design-time; connects through a `sqlb.Executor`, so the handle is the caller's. | `schema`, `sqlb` |
| `codegen` | Generates models, the typed column facade, the REST request bodies, the manifest, the TypeScript and Dart clients, and the cobra CLI. `Check` is the dry-run mode wired into CI. | `schema` |
| `rest` | Mounts a model on a Huma API: handlers, and an OpenAPI operation built from the model's capabilities. `Serve` wraps a whole server around it — pool, migrations, listen, graceful shutdown. | `sqlb`, `filter`, huma |
| `shadow` | Replays a checked-in migration history into an empty database, so the current side of a diff can come from the history rather than from a live schema. Design-time. | `schema`, `migrate` |
| `example/recipes` | One file per aspect, one point per example, each ending in output the test compares. The narrow-question counterpart to the worked applications, and the surface an agent greps. | all of the above |
| `example/blog` | A worked schema plus the artefacts codegen must produce. | all of the above |
| `example/tasks` | A multi-tenant task manager: hooks as the authorisation seam, JWT middleware feeding the context hooks read, and a migration history applied by goose. A separate module, like `pgtest`. | all of the above, `migrate` |
| `example/withsqlc` | The same schema rendered as DDL for sqlc, and a test that sqlb reads sqlc's structs. Proves [docs/with-sqlc.md](with-sqlc.md) rather than leaving it asserted. | `sqlb`, `filter`, stdlib |

The dependency direction matters: `schema` is a leaf that nothing imports at
runtime, and `sqlb` has no dependency on `schema`. That is deliberate. It is
what makes [ADR-0010](#codegen-is-optional) possible — the engine
cannot quietly grow a dependency on the schema DSL, because it cannot see it.
Capabilities reach the runtime as struct tags or `Describe` calls, never as a
schema import.

`migrate`, `introspect` and `codegen` sit on the other side of that line: all
three are design-time tools that read or write `schema`, and none is reachable
from the request path. `migrate` is the only package that renders DDL, which is
why the Postgres type mapping lives there rather than beside the query compiler
— a `Format` decides what a *runner* wants a file to look like, and the DDL
layer decides what the *database* wants a statement to look like.

`introspect` is the same mapping pointed backwards, and it is a separate package
because it connects to a database and `migrate` deliberately does not. That
separation is what keeps `migrate` a pure function over two data structures, and
it is why the two can be checked against each other: render a schema, apply it,
read it back, and the diff between what went in and what came out must be empty.

`sqlb` depends on pgx and nothing else, and neither does anything else on the
request path. `rest` is the single exception: it depends on huma, and nothing
depends on `rest`. `mise run deps-check` proves this per package rather than per
module — the allowed set is computed from what pgx itself pulls in, so it cannot
go stale — and it ends by checking that it can still see huma in `rest` and that
it still *refuses* huma everywhere else. A guard that cannot fail is worse than
no guard ([ADR-0016](#guards-proven-both-ways)).

`Executor` is the two-method subset of pgx that the engine needs — `Query` and
`Exec` — so a `*pgxpool.Pool`, a `*pgx.Conn` and any instrumenting wrapper all
work unchanged. So does a `pgx.Tx`, which is the point of taking pgx at all:
sqlb writes join a transaction the application opened
([ADR-0040](#the-driver-is-a-dependency)). `sqlb.DB` is a handle over
an `Executor`, adding `WithTx` and a scoped hook registry; it satisfies
`Executor` itself, which is what lets it be adopted without touching call sites
([ADR-0020](#transaction-scoped-handle)). `DB.Tx` reaches the
underlying `pgx.Tx`, which is how a unit of work is shared with code wanting
more than two methods — `CopyFrom`, `SendBatch`, or sqlc's generated `DBTX`.
`rest` takes a `huma.API`, not a router, so the choice of chi, gin, echo or
`net/http` — and all of that router's middleware — stays the application's. It
wraps each generated write in a transaction, which is what gives a hook a commit
to be after; reads are left alone, since one `SELECT` is atomic already
([ADR-0021](#hooks-receive-an-event)).

## Request path

A list request through `rest.Resource`:

1. **Parse.** `filter.Parse` reads the query string against the model. Unknown
   parameters, undeclared capabilities and uncoercible values are collected into
   a `filter.Errors` — all of them, not the first
   ([ADR-0011](#actionable-errors)). Values become typed Go values
   here; nothing downstream sees strings.
2. **Apply.** `filter.Apply` writes predicates, ordering, projection and limits
   onto a `*sqlb.Builder[T]`. It owns the projection and defaults to non-hidden
   columns, so a handler cannot leak a `Hidden` column by forgetting to project.
3. **Hook.** The terminal method clones the builder, then runs `BeforeQuery`.
   Cloning is what stops a hook's predicates accumulating when the same query
   value runs twice. A hook that returns an error aborts before any SQL is
   issued, so a missing tenant fails closed
   ([ADR-0008](#hooks-as-domain-seam)). Which registry the hooks come
   from is read off the executor: a `*sqlb.DB` carries one, and anything else
   carries none, so a statement issued against a bare pool runs unconfined
   ([ADR-0047](#no-default-hook-registry)).
4. **Compile.** The AST renders to SQL with `$N` placeholders. Values are always
   bind parameters. Identifiers are validated against the model and quoted.
   `LIMIT`/`OFFSET` are literals so the planner can see them — safe because both
   are range-checked ints.
5. **Scan.** Result columns are matched to struct fields by name. Unmatched
   columns are read and discarded, so a query selecting extra expressions still
   scans into the model.

A write takes the same path with a transaction around it: `BEGIN`, the hooks and
the statement, `COMMIT`, then the `AfterCommit` callbacks — outside the
transaction, since there is nothing left to join. A callback that fails does not
fail the request, because the row is already durable and a retry would write it
twice; `rest` logs it and returns the success it achieved.

## Where safety lives

Four independent mechanisms inside the query path, each covering what the
others cannot — plus authentication, below, sitting in front of all four:

**Bind parameters.** Values never reach SQL text. There is one `bind` method on
the compiler and no way to interpolate a value around it.

**Identifier validation.** Column names are checked against the reflected model
before compilation. `Raw` is the documented escape hatch and is the one place
this does not apply — which is why raw fragments are parenthesised as operands,
since their contents are opaque and could otherwise re-associate a surrounding
predicate.

**Opt-in capabilities.** A column that does not declare `Filterable` cannot be
filtered, ever ([ADR-0006](#capabilities-are-opt-in)). `Hidden` goes
further: the column is reported as unknown rather than as forbidden, so its
existence cannot be probed, and `Hidden` plus `Filterable` is a schema
validation error because a filterable secret can be recovered a character at a
time.

**Query hooks.** Tenant scoping applies to every read of a model, including
reads issued by generated handlers, because both go through the same builder.

**Authentication is a separate mechanism from the four above, composing
with rather than replacing them**
(["A Verifier composes with the principal
seam"](#a-verifier-composes-with-the-principal-seam)). `Middleware[T]`
(`auth.go`) verifies who is calling and writes the result to the context via
`WithPrincipal`; query hooks then read it back with `PrincipalFrom[T]` to
decide what that caller may see. An app can swap `Middleware[T]`'s
`Verifier[T]` — WorkOS, Clerk, Zitadel, a self-hosted JWT, more than one
realm at once — without touching a hook, and can change a hook's
row-scoping without touching how identity was established. Neither seam
substitutes for the other: `Middleware[T]` answering 200 says only that the
credential was valid, not what rows the resulting principal may reach.

Two smaller rails worth knowing: `Update` and `Delete` without a `WHERE` return
`ErrUnscoped` until `Everything()` is called explicitly, and LIKE
metacharacters in user input are escaped so a search for `50%` searches for the
literal string.

## Model metadata

The engine needs to know four things a Go struct does not say: which column is
the key, which columns the database defaults, which capabilities each column
exposes, and which columns are hidden.

That metadata arrives by one of two routes, which merge:

- **Struct tags** — `db:"email"` for the column name, `sqlb:"filter,sort"` for
  capabilities. This is what codegen emits.
- **`sqlb.Describe[T]()`** — the same information supplied at startup, for
  structs you did not generate and would rather not edit.

Without either, the builder still works — column names derive from field names —
but no column is filterable, so the REST layer exposes nothing. That default is
the point.

## API surface

There are no `internal/` packages, and the layout is flat
([ADR-0013](#no-internal-split)). The genuinely internal machinery —
the compiler, scanning, model building, escaping — is already unexported within
package `sqlb`, which is a finer-grained boundary than `internal/` can express.

What is exported falls into three tiers. They are a convention, not a compiler
check, and they exist because the module is `v0`:

| Tier | What | Promise |
|---|---|---|
| **Stable** | `Query`/`Builder`, `F`/`Pred`/`And`/`Or`/`Not`/`If`, `Field` and its operators, `Col`/`TextCol`/`Typed`/`TextColumn`, `Order`, the aggregates, `InsertRows`/`UpdateRows`/`DeleteRows`, `On`/`Hooks`, `Describe`, `Collect`, `Executor`, `DB`/`New`/`WithTx`, `ErrNotFound`/`ErrUnscoped`, all of `filter` and `schema` | Changes are breaking changes and are treated as such |
| **Provisional** | `Model`, `ColumnInfo`, `ModelOf`, `Selectable`, `Selection`, `Dialect`, `Postgres`, `Registry`/`On`/`WithHooks`, `Beginner`, `TxFrom` | Public because `filter` and generated code need them across a package boundary, or — for the registry surface — because they are new enough that no one has used them in anger yet |
| **Escape hatch** | `Expr` and the node types: `Raw`, `Binary`, `Unary`, `Call`, `Cast`, `BetweenExpr`, `List`, `Param`, `Column` | Use `Raw`, `RawPred`, `RawSel`. The rest is the compiler's vocabulary and will change without ceremony |

The tiers exist because the obvious extraction does not work: `Expr` and `Raw`
are the documented escape hatch and appear in `SetExpr`, `GroupByExpr`,
`Coalesce` and `OrderByDesc`, so they cannot be hidden. Hiding the rest of the
node set alone would buy little and would leave `Pred.Expr()` returning a type
callers cannot name.

The dialect is not among them. It is package-level but unexported and
unsettable: a mutable global read on the compile path of every query is a data
race with no legitimate trigger, since sqlb targets Postgres only. `UseDialect`
overrides it per statement, which is scoped and race-free.

## Failing loudly

Where sqlb cannot do the right thing, it says so rather than guessing. The rule
is that a wrong answer must never be quieter than no answer.

| Situation | Behaviour |
|---|---|
| `Collect[R]` has a field no result column fills | Error naming the field and both names — a mistyped `As("revenu")` would otherwise scan as a real-looking `0` |
| `Describe` called after a statement was built | Panic; mutating the cached model then would race and half-apply |
| `Describe` names a column that does not exist | Panic at startup, listing the columns that do |
| `Update`/`Delete` with no `WHERE` | `ErrUnscoped` until `Everything()` is called |
| Destructive migration | Rendered commented out with the reason stated |
| A change over a column a commented-out change adds | Commented out with it, naming what it waits for. Emitting it live makes the file fail partway through instead of being the no-op the guard intends |
| A column or table that was renamed | A drop and an add, unless `RenamedFrom` says otherwise — inferring a rename from a similar name would destroy data whenever the guess was wrong |
| A migration that rewrites or scans a table | Emitted live with the lock it takes and the sequence to use instead named above it. Not commented out: whether a scan matters depends on a row count the schema does not have. `migrate.Unblock` writes the sequence when the remedy is mechanical |
| A change with no `Down` | Renders an explanation, not an empty section that looks like a working rollback |
| Filter names an unknown or uncapable column | 400 listing what would have been accepted |
| Schema authoring mistake | Every problem reported at once, each with the fix |
| A resource over a `Scoped` or soft-deleting model with no hook confining it | Refused at mount, listing every missing registration and the declaration that asked for it. Serving it would answer 200 with another tenant's rows, which is the quietest wrong answer in the system |

Two deliberate exceptions, both documented where they happen: a page size above
the maximum is capped rather than rejected, since a client asking for too much
should get the maximum rather than an error; and `Builder.All` tolerates
unfilled fields, because a partial projection is exactly what `?select=id,name`
is. `Collect` is strict precisely because its destination type was written to
match the projection.

## Testing

The engine's tests run against an in-memory `Executor` that records statements
and replays canned rows, so hooks, scanning and the mutation paths are covered
end to end without a live Postgres. The pgx shapes that stands on —
`pgx.Rows` and `pgx.Tx` — are in `internal/pgfake`, written once and used by
every test package that needs them. SQL-string assertions cover the compiler.

What a fake cannot cover, `pgtest` does, and the driver flip made that split
sharper rather than softer: both bugs ADR-0040's port introduced were cases
where pgx hands back exactly what Postgres sent, and neither was reachable from
a canned result set.

The typed facade is checked by attempting to compile the cases that should fail
and confirming they do — a test that passes vacuously if the facade stops
working is worse than no test, so those are exercised as real build attempts.

## Known gaps

- `introspect` produces a registry from a live database and
  `codegen.RenderSchema` renders it back as `schema.go`, so adoption is a closed
  loop, and `shadow.Build` replays a migration history into an empty database so
  the current side of a diff can come from what the history builds. What it
  cannot reproduce is a destructive change: those render commented out, so the
  checked-in file is not the SQL that ran, and the shadow will differ from
  production wherever one was uncommented by hand.
- No change feed, and no MCP server over the manifest. See the
  [vision](vision.md). The TypeScript client, the Dart client and the CLI have
  since landed ([ADR-0028](#typescript-client),
  [ADR-0031](#dart-client), [ADR-0029](#go-cli)); all
  three read the schema rather than the OpenAPI document, for the reason the
  diagram above gives.
- `?expand` resolves one level. A relation expands to its row; that row's own
  relations do not expand in turn, and there is no `?expand=list.workspace`.
  One level is a join per relation and a bounded statement; nesting is where a
  depth limit and a cost model have to be argued for, and neither has been.

## Decisions

Decisions that shaped this codebase, folded in one at a time from a former
`docs/adr/` directory of individually numbered records. Each subsection below
used to be its own file; the reasoning now lives here, and its history lives
in this file's git history rather than in a separate directory —
`git log -G'### <heading>' -- docs/architecture.md` finds the commit that made
or last revised a given decision. A change to a decision below gets its own
commit, and the commit message carries the *why*, the way an ADR's body used
to.

### Postgres only

sqlb targets Postgres, and only Postgres. The `Dialect` interface exists for
placeholder style and identifier quoting, not for pretending to be portable.
The features the design leans on are not evenly available elsewhere:
`LISTEN/NOTIFY` for the change feed, jsonb aggregation for relation expansion,
`RETURNING` on every mutation, `ON CONFLICT`, `SKIP LOCKED`, partial and GIN
indexes, `ILIKE`. Supporting a second dialect would mean either dropping to
the intersection of what both support or carrying per-dialect branches
through the compiler, the schema DSL and the migration generator — and either
way, no feature could assume the best primitive available anymore. Targeting
one database is what keeps the compiler one small set of rules and lets
mutations return their rows in a single round trip.

The cost is asymmetric by design: narrowing further is free, but widening to
a second dialect later is close to a restart, since every compiler assumption
would need re-auditing and `LISTEN/NOTIFY` and jsonb expansion would need
replacements. Revisit only if a concrete project on MySQL or SQLite needs
supporting and cannot move, or if the `Dialect` seam turns out to leak beyond
placeholder rendering and quoting.

### Queries are values

A query is a value built up incrementally, not a statement assembled in one
shot. `Where` appends rather than replacing, the zero `Pred` is a no-op that
gets skipped, construction is separate from execution, and `SQL()` renders
without running anything:

```go
q := sqlb.Query[Post]().Where(sqlb.If(search != "", sqlb.F("title").Contains(search)))
```

Static query generators cannot express "this clause exists only when the user
typed something" without a combinatorial explosion of SQL strings, and teams
that hit that wall tend to fall back to concatenating SQL by hand — which
reintroduces every problem the typed builder was adopted to avoid. Making the
query a value sidesteps the explosion: conditional filters need no branching
at the call site, and the same value can be amended by a hook, produced by
the REST filter parser, inspected in a test, or printed for `EXPLAIN`. Hooks,
the filter grammar and query introspection all rest on this one property.

The cost is that builder methods mutate in place and return themselves, so a
shared base query can be aliased by accident — `Clone` exists but has to be
remembered — and errors are sticky, surfacing at the terminal method rather
than at the call that caused them. Revisit if aliasing bugs show up in
practice, which would justify the extra allocation of switching to
copy-on-write.

### One ast two producers

A query gets built two ways — a developer writes it in Go, or a client sends
filter parameters over HTTP — and giving each its own path would mean two
escaping strategies, two places to enforce authorisation, and two things to
keep in sync whenever a column changes. That is exactly where an injection
risk would live. So there is one predicate AST. Go code produces it through
`sqlb.F(...)`; the `filter` package produces it by parsing a request; both
feed the same builder, compiler and hooks, and the filter parser never emits
SQL text, only `sqlb.Pred` values. `filter` reads two wire formats — the URL
grammar and a JSON expression tree — but they are two frontends over one
compiler: both hand typed operands to a single internal `applyOp`, and a test
asserts that equivalent filters compile to byte-identical statements.

Bind-parameter discipline is enforced in exactly one place this way, and a
`BeforeQuery` hook constrains HTTP-driven and hand-written queries
identically, so tenant scoping cannot be bypassed by going through REST
instead of Go. A new builder feature reaches the filter grammar for free, and
adding a second wire format cost a parser rather than a second compiler. The
price is that the AST has to serve both producers — it carries nodes the
filter grammar will never emit and cannot be tuned narrowly for either side —
and the filter parser must coerce types up front, since the AST holds typed
Go values rather than strings.

This is cheap to keep and expensive to reverse: splitting the producers is
easy to do and hard to undo, since it reintroduces two escaping paths and two
authorisation points, and the resulting bugs are security bugs that surface
late. Revisit if the filter grammar needs an expression the builder cannot
represent — it may need its own compilation step, but it must still
terminate in `Pred` values.

### Schema as go dsl

Something has to be the single source of truth for table structure, since
every derived artefact — migrations, models, REST handlers, OpenAPI, three
clients — has to agree with it. The candidates were a Go DSL that generates
migrations (the ent/Convex approach) or introspection of an existing
database (the sqlc/PostgREST approach). DDL has nowhere to record
*capabilities*: there is no way to say "this column may be filtered on" in
`CREATE TABLE`, so an introspecting tool needs a side-car config file, and
now there are two sources of truth that can drift apart.

The schema is a Go DSL in its own package, and `sqlb generate` reads it to
emit migrations, models, typed column sets, REST handlers and OpenAPI. One
file is edited; everything else is derived. Capabilities, REST exposure,
comments and relations live next to the column they describe, which is a
large part of why this is pleasant to drive with an agent, and authoring
mistakes are caught before any SQL is generated. The cost lands on adoption:
bringing sqlb into an existing project means importing the current schema
and handing DDL control over to it, which is a real migration rather than an
afternoon's work.

Cost of change rises steadily once it's in use — after a production database
has been migrated by generated DDL, reversing course means reconciling a
generated history against a hand-managed one. Revisit if migration diffing
against a live database proves substantially harder than expected (invert to
introspection-first, keeping the DSL for capabilities), or if a generated
migration ever produces a destructive diff that wasn't intended — that would
be a stop-the-line signal for the whole approach.

### Runtime query engine

Since sqlb generates code anyway, the query builder could have been
generated per table — `Users.Age.Gte(18)` as real Go, the way ent does it.
That trades compile-time safety against generated-code volume: a generated
builder catches a hallucinated column name at compile time, which matters
when an agent is writing the queries, but adds hundreds of lines of API
surface per table.

Instead the query engine is generic and reflective: `sqlb.Query[T]()` builds
a model from `db` and `sqlb` struct tags, cached per type, and column
references are strings at the core (`sqlb.F("age")`). Compile-time safety is
recovered separately by a thin generated facade over the same model rather
than by generating the builder itself. This keeps the engine one small
package instead of a template that has to produce a correct API per table,
and it means the engine works on any tagged struct, including ones sqlb
never generated — which is what makes optional codegen possible. A new
builder feature benefits every table at once, with no regeneration needed.

The trade shows up as `sqlb.F("titel")` compiling fine and failing at
runtime, and as the engine having to validate column names itself rather
than getting that from the type system; reflective scanning is also slower
than generated field access, though that has not been measured against real
query latency. Revisit if the typed facade proves insufficient and
column-name typos keep reaching production — then generate the builder after
all — or if profiling shows reflective scanning is a meaningful share of
request time on realistic result sets, in which case generate scan
functions while keeping the builder itself generic.

### Capabilities are opt in

Exposing a table over a dynamic filter API means deciding what a client may
ask about. PostgREST's model — everything in the exposed schema is fair
game, with row-level security as the guard — puts the whole schema one
policy mistake away from public, and turns "which columns can a client
filter on" into a question answered by reading policies rather than the
table. sqlb instead makes every capability opt-in per column: `Filterable`,
`Sortable`, `Searchable`, `Expandable`, `Hidden`. A column that does not
declare a capability cannot be reached through it, and the request is
rejected with a 400 rather than silently ignored.

`Hidden` goes further than the others: a hidden column is reported as
*unknown* rather than *not filterable*, so its existence cannot be probed
from the rejection, and declaring a column both `Hidden` and `Filterable` is
a schema validation error, because a filterable secret can be recovered a
character at a time. `filter.Apply` owns the projection and defaults to
non-hidden columns, so a handler that forgets to specify one cannot leak a
hidden column by omission.

The payoff is that the blast radius of exposing a table is legible from the
schema file alone — adding a column never silently widens the API — and an
index can be guaranteed for every filterable column, because the set is
finite and declared. The cost is friction by design: every new filter needs
a schema edit and a regeneration. That cost is deliberately asymmetric in
the other direction too — loosening the default later is nearly
irreversible, since clients would come to depend on filters that opt-in
never granted, and tightening after that breaks them in ways that are hard
to see coming. Tightening from here stays cheap, because nothing is exposed
that was not declared. Revisit if the declare-and-regenerate loop becomes
the dominant complaint from people building views, which would argue for a
per-resource permissive mode that still excludes `Hidden`.

### Generated rest handlers

The REST surface could be one generic handler dispatching on the path, or a
generated handler per resource with a typed filter struct and a precise
OpenAPI operation. The apparent trade was boilerplate against client-side
typing — a generic handler supposedly cannot describe itself precisely,
because the filter grammar is compositional. But the grammar is
compositional and the *columns are not*: they are finite, known at
registration, and each admits a documented operator vocabulary. One query
parameter per filterable column describes the surface exactly without
describing the grammar, and [Huma](https://huma.rocks) makes this
buildable — it keeps explicitly-set operation parameters and hands an input
struct's `Resolve` the raw query values, so `filter.Parse` still owns
validation.

So sqlb uses one generic handler, instantiated per resource through
generics: `rest.Resource[T, C, U]` registers the exposed operations for a
model on a `huma.API`, and the OpenAPI operation is built per resource from
`sqlb.Model`. Generics rather than reflection, specifically, because hooks
are keyed by type — a reflective dispatcher holding a `reflect.Type` cannot
call `On[T]()`, which is how tenant scoping stops being something each
handler has to remember. Codegen emits only what generics cannot express:
the request bodies. Create, patch and row are three different JSON schemas
over one table, and no single Go type serves all three honestly, so
`rest_gen.go` holds two body types per writable resource plus one
`rest.Resource` call per exposed table. `rest` takes a `huma.API` rather
than building a router, so the application keeps its own router and
middleware.

The result is end-to-end typing into the frontend — a filter that does not
exist fails at the client's compile step, not as a runtime 400 — and adding
a table costs one generated registration, with response schema, parameter
list and rejection allow-list all deriving from the same capability flags so
they cannot disagree. The cost is a dependency on Huma's shape, and on huma
itself: it sets the module's Go floor at 1.25 for every consumer, which was
weighed against the module graph cost and accepted, since sqlb had already
given up "importing it costs nothing" the moment the driver became pgx
rather than `database/sql`. Moving off Huma later would cost only the `rest`
package — the engine, filter grammar, generated bodies and generated
clients all read `sqlb.Model`, never the OpenAPI document — but the response
and error shape (`{items, page, per_page, has_more, total}` and an RFC 9457
problem document) is the genuinely expensive surface to change, since a
generated client or an agent's retry logic depends on its exact structure.
Revisit if the per-column parameter list gets unwieldy at realistic column
counts — a fifty-column table documenting fifty parameters — which would
argue for collapsing the rare ones behind a single `filter` parameter with
looser typing.

### Hooks as domain seam

A generated data layer has to leave somewhere for domain logic to live, or
teams route around it. The common failure is that generated CRUD is
all-or-nothing: as soon as one endpoint needs to normalise an email or stamp
an owner, it gets written by hand and the generated version is abandoned.
Multi-tenant scoping is the sharpest case of this — `WHERE org_id = $1` has
to be on every read, and forgetting it once is a cross-tenant data leak.

sqlb registers hooks per model — `BeforeQuery`, `Before`/`AfterCreate`,
`Before`/`AfterUpdate`, `Before`/`AfterDelete`, and `AfterDeleteRows` —
and `BeforeQuery` is the load-bearing one. It receives the `*Builder` and
may amend it, so one registration constrains every read of that model,
including reads issued by generated REST handlers. Terminal methods clone
the builder before running hooks, so a hook's predicates cannot accumulate
across repeated executions of the same query value, and a hook that returns
an error aborts the operation before any SQL runs. This turns tenant scoping
and soft-delete filtering into one registration each, instead of a rule
every call site has to remember.

The cost is that hooks are action-at-a-distance — reading a query does not
tell you what will execute, and hook order is registration order. Two limits
worth naming: registration is default-*open*, where row-level security is
default-*deny*, so an unregistered model serves every tenant's rows with no
failure signal — this is closed only where handlers are generated, by a
schema declaration the mount checks, and not for queries written by hand in
Go. And write hooks were originally a thinner seam than intended:
`BeforeCreate` receives a bare row and `BeforeUpdate` cannot read its own
assignments; wrapping generated writes in a transaction closed most of that
gap by giving a hook something to query against, but a hook on an ordinary
read still has no executor. `AfterDeleteRows` exists alongside `AfterDelete`
rather than changing its signature, because the rows it carries are not
free — they arrive via `DELETE ... RETURNING`, so the clause is only added
when a rows-kind hook is actually registered, keeping the cost visible at
registration instead of charged to every delete in the process.

Removing hooks entirely would be the expensive direction: tenant scoping
would move back to individual call sites, losing the guarantee that it
cannot be forgotten. Revisit if people need to bypass a hook for a
legitimate admin path — the likely answer is an explicit unscoped builder,
not a way to disable hooks globally — or if hook ordering starts to matter
enough that registration order needs to become explicit priorities.

### Typed column facade

Making the query engine reflective means `sqlb.F("titel")` is a runtime
error, and that's the design's largest cost — it bites hardest in the
workflow sqlb targets, where an agent is writing the queries and a compile
error is a fast correction signal while a runtime error is a slow one.
Since codegen already emits models, a typed facade over them is nearly
free: sqlb generates a typed column set per table, so `PostCols.Status` is a
`sqlb.Col[PostStatus]` and `PostCols.Title` is a `sqlb.TextCol[string]`.
Predicate construction is type-checked; the builder underneath stays
generic.

A few choices sharpen the facade. `Col[T]` does not embed `Field`, because
embedding promoted every operator onto every column and made `Contains`
callable on an integer column — pattern operators live only on
`TextCol[T ~string]`. Nullable columns are typed as their base type, so the
comparand is a plain value and NULL is expressed with `IsNull` rather than
threading through the type parameter. Hidden columns are omitted from the
generated set entirely, so a predicate against one cannot even be written.
Update statements are wrapped too, since `Set(string, any)` checks neither
name nor type, but the select builder itself is not — its twenty-odd
chainable methods would each need a re-wrapped return type for safety the
column set already provides elsewhere.

Misspelled columns, wrong comparand types, and text operators on non-text
columns all fail at compile time now, for one small generated file per
table. What's given up is that predicates stay untyped — `sqlb.Pred`, not
`Pred[T]` — so a column from the wrong table still compiles and only fails
at the database, and the facade is a second artefact the generator has to
keep in step with the model. Removing it later is cheap, since it's purely
additive; `Pred[T]` would be the expensive direction, touching the AST,
every combinator, and the filter package's intermediate representation.
Revisit if cross-table column mixing turns out to be a common mistake
rather than a theoretical one — though `Pred[T]` still has to answer how a
join condition, which references two tables, could ever be `Pred[T]` for a
single `T`.

### Codegen is optional

Making the Go schema DSL the source of truth is a good end state and a poor
starting position: adopting it in an existing project would mean importing
the schema, handing over DDL control, and regenerating models that already
exist. The engine needs none of that — it reflects over struct tags and
derives column names from field names when no tag is present — so the
schema DSL and the generator are both optional. Metadata the builder cannot
infer is supplied at runtime instead:

```go
sqlb.Describe[Invoice]().
    PrimaryKey("id").
    Defaulted("id").
    Filterable("customer_id", "paid").
    Sortable("amount_due").
    Hidden("memo")
```

Descriptions merge onto struct tags, so a partly tagged model can be
completed, and naming a column that does not exist panics at startup,
listing the ones that do. Every capability the generator can emit has a
runtime form, including relations — `Relation("Customer", "customer_id")`
is the no-codegen half of `?expand` — which is the test this decision has
to keep passing: a capability reachable only from generated tags would
quietly make the generator mandatory again. This is what lets sqlb be
layered over structs another generator produced, without editing them,
turning adoption into something incremental rather than a migration; it
also keeps the engine honest, since anything it needs must be expressible
without importing the schema package at all.

The two routes can disagree, and nothing checks either against the
database. One consequence of allowing `Describe` at runtime was a real data
race: an early guard flag was read when a `Description` was constructed but
the writes happened in the chained calls after that, so a query built in
between could pass the guard and race the writes to the fields the request
path reads to decide what a caller may see. The fix keeps this decision's
constraint of no lock on the read path by inverting where the cost lands —
`Describe` now copies the model, writes the copy, and publishes it into the
model cache, so a published `*Model` is never written again and a statement
in flight always sees a consistent snapshot. Describing costs a copy once,
at startup; reading costs nothing. Revisit if the two routes drift
confusingly in practice — the fix would be having the generator emit
`Describe` calls rather than tags, collapsing to one mechanism.

### Actionable errors

Because capabilities are opt-in, requests get rejected routinely — that's
the design working as intended. The caller most likely to hit a rejection
is a program assembling a request against a schema it only partly knows: a
frontend, a client library, or an agent. For all three, `400 column is not
sortable` on its own is a dead end that costs a round trip and a guess. So
every rejection names both what was wrong and what would have worked:

```
filter: sort=body: column is not sortable (allowed: title, status, view_count, published_at, created_at)
```

Parsing collects every problem in a request rather than stopping at the
first, so a malformed request takes one round trip to fix rather than one
per mistake, and schema validation follows the same rule. The exception is
`Hidden` columns, which are reported as unknown and never listed in an
allow-list — the diagnostic must not become an oracle for probing what
exists.

This lets a caller correct itself from the response alone, and the
allow-list doubles as discovery, reducing how much schema a client needs up
front — part of why the API is pleasant to drive with an agent. The cost is
that error responses are larger and disclose the shape of the resource,
which is fine for something meant to be exposed but makes the exposure
decision itself carry more weight. The response shape is not free to change
later, either: a generated client's or an agent's retry logic depends on
the current structure, so renaming or renesting fields is a breaking change
even on an error path. Revisit if disclosing the filterable column set
turns out to be unacceptable for some resource — add a per-resource terse
mode rather than making terse the default — or if allow-lists get long
enough to be unhelpful, in which case truncate with a count and a pointer
to the OpenAPI document rather than dropping them outright.

### Change feed outbox

Dynamic data views need to know when their data changed, and the reference
points — Convex especially — set the expectation that a view can be live
rather than polled. Firing a notification from an in-process `AfterCommit`
hook loses events when the process dies between commit and publish, and
delivers phantom events when a transaction commits the notification but
rolls back the data. So every mutation that goes through sqlb writes a row
to an outbox table in the *same* transaction as the change. A dispatcher
tails that table — woken by `LISTEN/NOTIFY` rather than polling — and fans
out to subscribers. Subscribers receive invalidation events (table plus row
key), not recomputed results; clients refetch. The fan-out endpoint, event
shape and reconnection contract are a separate decision behind a
`rest.Source` seam; what belongs here is the outbox table, the trigger that
wakes the dispatcher, and the ordering guarantee underneath both.

The outbox row *is* the event; `NOTIFY` is only a doorbell carrying no
payload, which is why a lost notification degrades to latency rather than
lost data — the dispatcher also polls on a slow fallback interval, which is
what keeps the feed correct behind a connection pooler that silently
swallows `LISTEN`. An `AFTER INSERT` trigger on the outbox table rings that
doorbell, deliberately not a call from sqlb's own mutation path: issuing
`NOTIFY` from Go is one fewer database object but is forgettable — a new
mutation path that writes the outbox and omits the notify would work in
tests and lag in production. It's also deliberately not a trigger on every
domain table, since that captures row changes rather than domain events and
floods during backfills.

The hardest problem only appeared once there was something to be correct
about rather than describe: a bigserial primary key does not promise commit
order. Two transactions can take ids 5 and 6 and commit in the other order,
and a dispatcher reading `id > cursor ORDER BY id` would see 6, advance past
5, and lose it silently — exactly the failure the whole design exists to
prevent, arriving from inside the mechanism meant to prevent it. The fix is
`pg_advisory_xact_lock`, held from the outbox insert until commit, so id
order *is* commit order by construction and the dispatcher needs no
reasoning about visibility at all. The alternative considered and rejected
was gating the tail on a snapshot watermark (`pg_snapshot_xmin`), which has
no write-path cost but is wrong in a way that took a while to see: the xid
is assigned at a transaction's *first* write while the sequence value is
assigned at the outbox insert, so a transaction can hold an earlier id and
a later xid, and the watermark then admits the higher id first. Repairing
that means dispatching in `(xid, id)` order, which is no longer an order a
client's `Last-Event-ID` can name — the lock buys a position that is a
plain row id, and a row id is what makes replay across a restart possible
at all.

That correctness has a stated cost: writes to published models serialise
from the outbox insert to roughly the commit, bounding write throughput on
those models at about one transaction per commit latency — a real ceiling,
even though it's the same order Postgres's own WAL flush already imposes.
An application that publishes a write-heavy table pays for a feature its
clients may not even subscribe to, and the only remedy today is not
publishing that model. Retention (24 hours by default) is a delivery
guarantee rather than a disk setting — a subscriber resuming from a pruned
position gets a reset rather than a replay — and it's the piece of the
design chosen with the least confidence, since it was picked without a real
consumer. The dispatcher probes its own `LISTEN` at startup, ringing the
doorbell from a separate connection and reporting if it never hears back:
a pooled `LISTEN` is silently accepted and useless, which otherwise leaves
the feed correct, slow, and looking fine to everyone — the exact shape of
failure that earns a check rather than a paragraph. This is distinct from
`sqlb.AfterCommit`, which is in-process and at-most-once — fine for
invalidating a local cache, silently lossy as a change feed.

What's bought is at-least-once delivery that survives a process restart,
where only the dispatcher itself needs to be highly available; replay that
survives a rolling deployment, since a reconnecting client is caught up out
of the table rather than needing a full refetch; and two dispatchers over
one table both delivering, which is the horizontal-scaling story. Revisit
if the advisory lock turns out to be the binding constraint on write
throughput — the likeliest reason this gets revised, and unmeasured either
way — trading it against the `(xid, id)` dispatch order, which costs
exactly the row-id position that makes replay-across-a-restart possible.
Also revisit if outbox write volume becomes a measurable drag, which points
at logical replication (`pgoutput`, no write cost, but a replication slot
and decoded rows instead of typed domain events) — and if retention proves
the wrong knob, where the fix is likely a cheaper reset rather than a
bigger window.

### No internal split

Package `sqlb` exports a large number of identifiers: some are the daily
API, some are public only because another package needs them across a
boundary, some are the compiler's own vocabulary. Go offers `internal/` to
make that distinction compiler-enforced, and two facts decided against
using it. The genuinely internal machinery — compiler, scanning, model
building, escaping — is already unexported within the package, so
`internal/` would restate a boundary that already holds. And the obvious
extraction fails on its own terms: `Expr` and `Raw` have to stay public as
the documented escape hatch, so hiding the remaining node types would buy
only field renaming while forcing `Pred.Expr()` to return a type callers
cannot name.

So there is no `internal/` package. The layout stays flat, and the
distinction is expressed as documented tiers plus a `v0` version instead:
stable (the query builder, predicates, the typed facade, hooks, mutations,
`Describe`, `filter`, `schema` — changes here are breaking changes and
treated as such), provisional (`Model`, `ColumnInfo`, `Dialect` and similar
— public because `filter` and generated code cross a package boundary, and
expected to move), and escape hatch (`Expr` and the node types — use `Raw`,
`RawPred`, `RawSel` instead; the rest is compiler vocabulary that will
change without ceremony). This avoids import gymnastics and a premature
boundary in a library still moving pre-1.0, and tiers communicate intent
per identifier where `internal/` can only work per package. The cost is
that tiers are convention, not a compiler check — someone can depend on a
node type like `Binary` and be broken with only a doc comment to point at,
and a reader cannot tell the tiers apart without consulting the docs.

Introducing `internal/` later stays mechanically cheap inside the module,
but the cost lands entirely on external users: anything they imported that
moves becomes uncompilable with no deprecation path, since `internal/` is
absolute. If it's going to happen, it should happen before there are
external users to break. Revisit properly at v1.0 — promote provisional
identifiers to stable, or hide them — or sooner if someone outside the
module depends on a node type and is broken by a compiler change, which
would be the trigger to extract the AST behind `internal/` and accept the
`Pred.Expr()` awkwardness that comes with it.

### Migrations and import

Making the Go DSL the source of truth means something has to turn a schema
edit into DDL, and something has to turn an existing database into a
schema. A wrong answer here is destructive: a diff that mistakes a rename
for a drop-and-add loses a column of production data, and it cannot tell
the two apart from the schema alone. So migrations are generated, not
applied — sqlb emits files and stops; it does not own a runner, track
applied versions, or connect to a database to migrate anything. Goose is
the default output format, with golang-migrate and plain SQL selectable and
`Plain` as the escape hatch for runners sqlb doesn't ship. The format isn't
cosmetic: goose's `NO TRANSACTION` directive is file-level, so a migration
containing `CREATE INDEX CONCURRENTLY` would strip the rollback guarantee
from every unrelated change sharing its file — which is why index changes
get their own migration file, versioned to sort immediately after the one
they depend on.

The diff itself runs between two registries, not between a registry and a
live database. Introspection produces the same `*schema.Registry` the DSL
does, so `Diff(current, target) []Change` is a pure function, testable
without a database — and the same machinery works pointed in either
direction. Current state comes from replaying the checked-in migration
history into a scratch database and introspecting *that*, which validates
the history and catches drift as a side effect. Destructive changes are
opt-in: dropping a column or table, narrowing a type, or adding `NOT NULL`
without a default all render commented out, with the reason stated. A
change that depends on one of those commented-out changes is commented out
too — carrying `DependsOn` rather than `Destructive`, because it's
premature rather than dangerous — since without that, a commented `ADD
COLUMN` followed by a live `ADD CONSTRAINT` would fail the file partway
through instead of being the no-op the guard intends. Lock hazards, by
contrast, are stated rather than gated: a statement that rewrites or scans
a table is emitted live, with the lock it takes and an expand/contract
sequence named above it, because unlike a destructive change this is only
*occasionally* slow, and how slow depends on a row count the schema doesn't
have. `migrate.Unblock` can rewrite the lock-brief sequence — a scanning
`ADD CONSTRAINT` into `NOT VALID` plus `VALIDATE`, for instance — but the
caller decides whether to apply it, because the sequences aren't equivalent
under failure: they can leave a binding-but-unvalidated constraint or an
invalid index behind, where the plain statement leaves nothing.

Renames are declared, never inferred — `.RenamedFrom("old")` for one
release, and without it a rename is rendered as a drop and an add: lossy,
but never silently wrong. Adoption is `sqlb import`, which reads
`pg_catalog` and emits a `schema.go` with no capabilities, so the result
describes the database exactly and exposes nothing over REST until
capabilities are added by a deliberate edit; what import cannot represent,
it reports, and an empty report is the claim that the registry describes
the database completely. Reading the catalog is a separate package
(`introspect`) from writing DDL (`migrate`), which is what keeps the diff a
pure function, and formats are rendered in code rather than translated by
an agent — the variation between runners is only about fourteen lines of
syntax each, but what they share is semantics (file splitting, `Down`
reversing `Up`, destructive statements staying commented, multi-statement
delimiting), and a translation step would have to re-derive all of that and
get it right *most* of the time. A wrong migration is applied once, often
irreversibly, and nothing type-checks it — so agents are better spent
reviewing a destructive migration or supplying rename hints than generating
SQL text. No `USING` clause is ever generated for a type change, either:
Postgres refusing an implicit cast is the correct outcome, and a generated
cast nobody reviewed would truncate data silently instead.

The round trip is proven, not assumed: `pgtest` runs render, apply, read
back, diff against real Postgres in CI, and a stricter *fixpoint* —
import, re-render, apply, re-import, diff — is asserted unconditionally and
is empty, which is what makes adoption actually trustworthy rather than
merely plausible. Cost of change rises sharply once the first generated
migration is applied anywhere real: before that the diff engine is a pure
function and freely rewritable, but after, the migration history is
permanent, and the file format is the single most expensive thing here to
change later. Revisit if the shadow database proves too heavy for the inner
loop (replay into an in-memory model instead, losing validation against a
real parser), if people start uncommenting destructive changes without
reading them (meaning the guard isn't working and needs to become a
separate reviewed file rather than a comment), or if import silently drops
a construct that matters — the failure mode this design watches for
hardest, and the fix would be a raw-DDL passthrough.

### Module isolation

A target codebase arranged as independent fx modules — `auth`, `billing`,
`tenants`, `rag` — with a rule that no module imports another, each owning
its own migration directory, and cross-module foreign keys forbidden
outright, collided with sqlb in three ways: `schema.Table` registered into
one global registry, so two modules couldn't both own a table called
`events`; table names had no namespace, leaving prefixing to a discipline
that had already drifted; and `Ref` took a `*TableDef`, requiring exactly
the Go import the architecture forbids.

The fix makes a registry the unit of module isolation:
`schema.NewModule("billing")` returns a registry whose tables are prefixed
with the module name, while declarations use the local name —
`Table("invoices")` — so the prefix can never be forgotten and moving a
table between modules is a one-line change. Prefixing uses plain names
(`billing_invoices`), not Postgres schemas, and there's no abstraction
layered over the two: a Postgres schema is a deployment model, not a
rendering strategy, since only one of its four practical requirements is
about how a name renders — the others are `search_path` management,
ordering `CREATE SCHEMA` ahead of each module's first migration, and
per-schema goose version tables. A strategy interface covering rendering
alone would suggest switching between the two is just configuration, while
the hard parts stay entirely unbuilt. The prefix stays a storage concern
and never reaches the URL — a REST path defaults to the local name, so
moving a table between modules isn't a breaking API change — and
cross-module relationships are declared rather than enforced:
`ExternalRef("tenant", "tenants.id")` produces the column and a join index
but no `FOREIGN KEY`, with the target left as free text because resolving
it would require the very dependency this design avoids. A reference built
this way can't be `Expandable`.

Modules get to migrate and deploy independently this way, and either side
of a soft reference can move to its own database without dropping a
constraint; the relationship still shows up in the manifest as
`enforced: false`, so tooling and readers can see what the database itself
cannot. The cost is that referential integrity becomes the application's
job — nothing stops a `tenant_id` pointing at a deleted tenant, and no
cascade cleans it up — prefixed names run longer, and `ExternalRef` targets
are unchecked strings that rot silently when the other side renames its
table. Namespacing is the expensive half to reverse: adding a prefix to an
existing table is a rename, meaning a migration per table and a
coordinated deploy, though it's free before a module's tables exist.
Revisit if orphaned rows become a real operational problem (the answer is
likely a periodic reconciliation job per module, not foreign keys, which
would reintroduce the coupling this avoids), if a module needs to move to
a genuinely separate database (at which point prefixes stop helping and
Postgres schemas become worth their operational cost — the compiler
already renders qualified names, but `search_path`, schema-creation
ordering and per-schema version tables would still need building), or if
`ExternalRef` targets rot often enough to matter, which would justify a
lint pass checking them against a per-module manifest without adding a
compile-time dependency.

### Guards proven both ways

Three guards in this repository reported success while checking nothing, and
each was written deliberately and looked right on review: a dependency check
that grepped package paths for a dot and matched the standard library's own
vendored code, filtering everything away; a later version of the same check
that let `go list -m all` fail to stderr, so empty output read as "no
dependencies"; and a bisect check running under `set -e`, where the first
commit — legitimately without Go packages — killed the script before it
printed anything. A guard's failure path runs far less often than its success
path, so it can go unexercised until the day it matters, and a guard that
cannot fail is worse than no guard: absent tooling prompts caution, broken
tooling prevents it.

So a guard is not trusted until it has been observed failing on purpose.
Before a check joins the gate, both directions get demonstrated: it passes on
a clean tree, and it fails — naming the problem — on a tree broken in exactly
the way it exists to catch. Where the broken state is cheap to construct, a
test constructs it, so the failing branch runs on every CI run rather than
only once at review time; the migration diff engine's destructive-change guard
and codegen's dry-run check both work this way. Two narrower rules follow from
the specific failures above: a command whose own failure would empty its
result must have its exit status checked, since silence is not evidence of
cleanliness, and under `set -e` an expected failure must be guarded by `if`,
not read from `$?` afterward.

This buys the only real evidence that a green pipeline means something — every
guard's failing branch has run at least once under conditions someone chose —
at the cost of a slower add-a-check workflow, since the demonstration is
manual wherever the broken state isn't cheap to construct. Mechanically it's
cheap to abandon, since it's a practice rather than a structure; what dropping
it costs is confidence, and only gradually, because a guard that rots into
uselessness is invisible by construction. Revisit if a guard is found silently
passing despite this — the manual demonstration isn't working on its own, and
guards need a shared harness that constructs the failure for them.

### Enums as text and check

`schema.Enum("status", "draft", "live")` declares a column constrained to a
fixed set, and Postgres has a native type for exactly this — the obvious
choice right up to the point the list has to change, which is one of the most
ordinary schema edits there is. A native enum value cannot be removed; the
route is a replacement type, a rewrite of every column using the old one, and
a drop. Adding a value cannot happen in the same transaction that reads it, so
a change needing that drags every unrelated change sharing its migration file
out of its transaction too. And the type is schema-level, not table-level, so
under this project's module prefixing two modules declaring their own
`status` enum collide in a namespace neither owns. Against that, the native
type buys storage compactness, a defined sort order, and type-level rejection
at every call site.

So an enum column compiles to `text` with a named `CHECK` constraint —
`CHECK ("status" IN ('draft', 'live'))` — rather than a native type. Changing
the list becomes an ordinary `DROP CONSTRAINT` plus `ADD CONSTRAINT`, which
the diff engine already does for every other constraint: no special case, no
new object type, no transaction exception. Removing a value from the list
isn't marked destructive, because it can't lose data — Postgres rejects the
whole `ADD CONSTRAINT` if any existing row would violate it — so it renders
live, with a comment naming the values no longer permitted, since the fix
lives in the rows rather than the migration.

The cost is more storage than a native enum's four bytes, no implicit sort
order — `ORDER BY status` sorts alphabetically, and declaration order needs an
explicit `CASE` — and a bad value rejected at insert time by the constraint
rather than at parse time by a type system. The direction of that cost is
deliberately the cheap one to be wrong in: nothing outside the DDL renderer
knows the representation, so moving to a native enum later is confined and
mechanical, while moving away from one would mean rewriting every table that
used it. Revisit if declaration-order sorting is needed in more than one or
two places (the likely answer is an explicit ordinal column, not a native
enum), if a consumer's tooling reads `pg_enum` and can't be taught to read
`pg_constraint` instead, or if the text column's width becomes measurably
significant — at which point the right escalation is a lookup table, which is
also the natural move once a value list acquires attributes of its own.

### Tooling scoped to tracked files

Parallel agent sessions check this repository out into a worktrees directory
inside the repository itself, ignored via `.git/info/exclude` — git does not
see them, but a tool that walks the filesystem does. A format check that ran
`gofmt -l .` failed the gate naming a file belonging to an unrelated session
while every tracked file was clean, a failure the reader could neither
attribute nor act on; the write-mode equivalent, `gofmt -w .`, would have
rewritten a neighbouring session's checkout out from under whoever was working
in it. Every other gate happened to reach the code through `./...`, which
skips dot-directories, or through `git archive`, and that immunity was a
property of the Go tool rather than a decision this repository had made — it
silently covered for the absence of a rule.

So tooling that operates on "the repository" takes its file list from
`git ls-files`, never from a filesystem walk: what git tracks is the
definition of this repository, and anything else in the directory belongs to
someone else. `./...` remains an accepted equivalent where it applies, but
that's incidental immunity, so a check relying on it demonstrates the
equivalence rather than assuming it. Scoping this way adds two failure modes
that have to be handled explicitly — an empty file list checks nothing and
must fail, and the underlying command's own failure empties the result and
must be caught rather than read as clean. Formatting itself is checked by the
`gofmt` formatter built into the lint step and nowhere else; the standalone
tracked-files formatting check was strictly narrower than that (tracked files
versus every file in a package) and so was a second thing to keep in step
rather than a second gate, and was removed once lint was shown to catch the
same defect.

The gate's idea of the repository now matches git's, which is the only
definition it shares with CI, and a failure names a file the reader can act
on; nothing writes outside the tracked set, so a formatter can't damage a
neighbouring worktree. The cost is that a file that hasn't been staged sits
outside the set — not theoretical, since an unformatted, un-added file was
caught only by the separate lint step while this very decision was being
written. Neither the filesystem nor the index is automatically the right
scope, which is the argument for choosing deliberately rather than inheriting
whichever one a tool happens to walk. Revisit if a tracked Go file appears
that `go list ./...` doesn't cover — a test fixture, a build-tag-excluded
helper — since its formatting would then be checked by nothing; or if the
`gofmt` formatter is ever disabled in the lint config with no second gate
standing behind it.

### Pgbouncer in the path

The target deployment runs PgBouncer in transaction pooling mode, and sqlb had
assumed a direct connection everywhere without ever saying so — an assumption
invisible in the code, since sqlb takes an `Executor` from the caller and
would not notice a pooled one until it misbehaved in production. Transaction
pooling returns the server connection at transaction end, so anything whose
state outlives a transaction breaks: `LISTEN` for the change feed, session
advisory locks and `CREATE INDEX CONCURRENTLY` in migration runners, and the
driver's prepared-statement cache — the last being the entire query path, not
a corner case. Measured against a real PgBouncer in front of Postgres: the
query path works, but only because the pooler's default `max_prepared_statements`
happens to be non-zero, a deployment setting rather than a property of the
driver; `LISTEN` is accepted and the notification never arrives, silently;
`NOTIFY` works fine, including from inside a transaction, because it's
fire-and-forget with nothing to hold onto.

So the pooler is the default path, and the query path may assume nothing
session-scoped — no feature may rely on a `SET` outliving its transaction, a
session advisory lock, a temp table, or a cursor held across transactions.
Two components are named exceptions and connect direct rather than being
discovered by trial: the change-feed dispatcher's `LISTEN` connection, and the
migration runner. `NOTIFY` needs no exception, since it's transactional and
works from any pooled connection — the blast radius of getting it wrong is one
connection, not the application. sqlb does not manage any of this itself: it
takes a handle from the caller and does not grow a pooled-versus-direct
abstraction, since a pooler-aware sqlb would be deciding a deployment topology
it cannot see. What it owes users is documentation of which component needs
which connection, not a seam pretending to arrange it.

This buys an assumption that's now visible and testable instead of latent, and
forbidding session state is good hygiene independent of the pooler, since it's
what keeps the query path horizontally scalable with or without one. The cost
is two connection paths to operate, and a misconfiguration is silent: point
the dispatcher at the pooler and it never wakes, and the change feed's
fallback poll turns that into latency rather than data loss — which also means
it can hide the mistake indefinitely unless the dispatcher asserts its own
`LISTEN` at startup. Revisit if the deployment ever moves to session pooling
or a pooler that supports `LISTEN`, at which point every carve-out here
collapses; if a deployment turns up on a PgBouncer that doesn't track prepared
statements, which would make the driver's exec mode something sqlb sets in
code rather than a DSN setting every consumer has to get right; or if
generated writes opening a transaction around every mutation measurably raises
server-connection occupancy under the pooler, which is a real risk that was
identified but never measured.

### Transaction scoped handle

`Executor` is the two-method subset — `Query` and `Exec` — that a transaction
also satisfies, so a caller could always thread one through terminal calls,
but nothing sat on top of that: no `WithTx`, so every caller wrote its own
begin/commit/rollback and its own panic handling, and forgetting the
rollback-on-panic leaked a connection holding an open transaction. Worse, the
hook registry was a single package-level map, so hooks couldn't be scoped and
a hook had no way to learn it was running inside a unit of work — a
`BeforeQuery` needing rows written earlier in the same transaction would find
the plain pool instead. The obvious plan deferred this to a future Go release
that adds methods with type parameters, which is right about the ergonomics
of a fluent `db.Query[T]()` call but wrong about the scoping: a handle holding
an executor and a registry needs no new language feature, and waiting was not
neutral, since every month added call sites written against the process-global
registry.

So `*sqlb.DB` is built now, made additive by having it satisfy `Executor`
itself — no call site breaks, since every terminal already accepts the
interface. `WithTx` commits on nil return, rolls back on error, and rolls back
and re-raises on panic; it asserts the executor can begin a transaction rather
than requiring that of the two-method `Executor` interface, so that stays
frozen. Nesting joins rather than nests: `WithTx` called on a handle already
inside a transaction runs on that same transaction and leaves the commit to
the outermost call, so a function that opens a transaction stays callable from
inside one. And a hook joins the unit of work through an explicit
`TxFrom(ctx)` call rather than an implicit context read, which would make the
connection a statement runs on invisible at the call site.

This buys a multi-statement unit of work as one call with correct rollback
semantics including on panic, hooks that can be scoped to a handle rather than
reset globally between tests, and a hook that can tell it's inside a
transaction and read what that transaction already wrote — none of which was
expressible before. The cost is that the fluent ergonomics genuinely do need
the language feature that was deferred, so callers still thread the executor
through terminals explicitly; and hook resolution depends on the executor's
*dynamic type*, so passing a raw pool where a `*DB` was intended silently
falls back to the default registry, with nothing in the compiler able to
catch it — the price of not breaking existing call sites. One trigger has
already fired: the interface for "can begin a transaction" proved too narrow
once callers turned up already holding an open transaction handle of their
own, which is answered by redefining that interface alongside `Executor`
rather than widening it, since the narrowness belonged to the driver rather
than to this design. Revisit further if the dynamic-type hook resolution ever
causes a real bug, at which point terminals should stop accepting a bare
`Executor` for models with scoped hooks — a breaking change worth making only
once it's actually happened.

### Hooks receive an event

`BeforeQuery` gives a hook the builder itself, and that half of the hook
design held under a real worked example: a multi-tenant task manager scopes
six tables across twenty-five endpoints in one file, and no handler mentions a
tenant. The write hooks hadn't gotten the same treatment, and building that
example found three domain rules that couldn't be expressed as hooks at all —
a rule needing the row's data checked against the database, since
`BeforeCreate` receives the row and no executor; a rule needing `BeforeUpdate`
to read the assignments already staged on the update rather than only add to
them; and a rule needing two writes in one transaction, when nothing wrapped
generated writes in one, so every generated write ran under autocommit and
`AfterCommit` was unreachable from generated CRUD. The original proposal to
fix this reached for an event type carrying an executor and the pending
assignments — but wrapping every generated write in a transaction was built
first, and answered most of the complaint on its own.

So `rest.Resource` wraps every generated write in a transaction, and that's
the whole of what got decided: the hook's context now carries the
transaction, so an explicit `TxFrom` call finds it, answering the executor
half wherever a generated write is running, with no new event types. The
option controlling this defaults on, refusing to mount a resource whose
executor can't begin a transaction rather than silently falling back to
autocommit, since a silent fallback would restore the original gap in exactly
the callback meant to be the durable half. A callback failure after commit
must not become a server error, since the row is already durable and a retry
would write it twice — the framework logs it and returns the success it
achieved. Reads are left unwrapped, since a single `SELECT` is already atomic.
The event types themselves stay closed: no event value, no way to read the
executor off it, no way to read the pending changes — of the three motivating
rules, two are now expressible as hooks with the transaction in context, and
the third (reading an update's own staged assignments) is the one piece left
unaddressed, deliberately, since only one consumer ever asked for it.

This buys `AfterCommit` reachable from generated CRUD, which is the
difference between a documented feature and a decorative one, and
database-backed validation with no new API surface. The cost is a
begin/commit round trip on every generated write, holding a connection longer
— a real, unmeasured cost under transaction pooling, since it changes
server-connection occupancy rather than just latency. A hook that can query
the database is also a hook that can query it badly, since a per-request round
trip is now easy to write and invisible at the call site. Closing the events
question cost nothing, which is the argument for closing it now rather than
leaving it open on spec — nothing was built, no signature changed, no
consumer wrote against an event. The transaction is the expensive half to
reverse: shipping it default-on means off→on is harmless but on→off silently
breaks anyone whose `AfterCommit` callback stops running. Revisit the event
design if a second application hits the same `BeforeUpdate` gap — add a way to
read pending changes first, not the whole event — or if a hook genuinely needs
before-and-after correlation across a write, which nothing has produced yet.

### References declare their inverse

An outside review argued that a one-sided foreign key can't tell the runtime
its reverse cardinality, but that premise doesn't hold: the schema registry
is a runtime value, a reference carries a pointer to its target table, and
inbound edges are found by walking it, with cardinality following from
whether the foreign-key column is unique. Forward expansion is fully
determined by what the schema already records. Three things are genuinely
missing, and all sit on the reverse side. There's no name: deriving one from
the target table gives the same name to two different foreign keys pointing
at the same table, and the distinction between them exists only in the
schema author's head. There's no exposure decision: `Expandable` sits on the
referencing column and speaks for the forward direction only, and deriving
the reverse automatically would make it reachable by default, inverting this
project's rule that capabilities are opt-in. And a reference across a module
boundary carries no pointer at all, deliberately, since resolving one would
require the cross-module import that module isolation avoids.

So a reference may declare the name its target knows it by —
`.Inverse("posts")` — and that declaration is what makes the reverse relation
exist at all: absent it, the reverse isn't addressable, doesn't appear in the
manifest, and isn't an error to omit. One side declares, so a module can gain
an inbound relation without its owner changing a line, and exposure stays
opt-in in both directions independently. The reverse compiles to a correlated
subquery rather than a join, because joining a collection multiplies the base
rows and forces a `GROUP BY` over every selected column, while a subquery in
the projection composes by addition and stays one statement. The value
returned is a list envelope (`{items, has_more}`), never a bare array, since a
bare array can't say it's partial and it will be partial; the cap defaults to
50 and is declared, with the child's own filtered endpoint as the escape hatch
past it; and ordering carries the target's primary key as a tiebreaker, since
under a `LIMIT` a non-total order doesn't reshuffle the result, it decides
which children the caller never sees.

This buys reverse expansion as a genuinely expressible half of `?expand`, and
a manifest that describes a relation from both ends. The cost is a second name
to keep true — one that lives on the referencing table but shapes the
target's API surface, so reading one table's declaration in isolation no
longer tells you everything its own endpoint exposes — and a real per-row
cost forward expansion doesn't have: one subquery per base row per relation,
which is why the foreign key's index becomes load-bearing rather than merely
good hygiene. Revisit if forward expansion turns out to be all anyone asks
for in practice, or if the cap and order need to vary per request — which
would extend the wire format rather than the schema, a decision of its own
weight since the relation name is already part of a frozen response shape.

### Mixins carry behaviour

An outside review recommended a mixin mechanism — a user-defined bundle of
columns, hooks and capabilities — and half of that already existed. A column
mixin is exported and works today: it's an ordinary function returning a
bundle of fields, and the built-in timestamp and soft-delete helpers aren't
privileged, they're two such functions that happen to ship in the package,
composable with a user-written one. What doesn't exist is behaviour, and the
gap was concrete rather than theoretical: the soft-delete helper's doc
comment claimed the REST layer filtered out soft-deleted rows, and nothing
did — a table declaring it and exposing a list operation returned deleted
rows regardless, because a bundle contributes fields and nothing else, no
index, no check constraint, no hook. The schema package can't fix this by
registering the hook itself, for two structural reasons: it imports nothing
from the query engine, which is what keeps codegen optional, and hooks are
keyed by the generated Go model type, which doesn't exist yet at the point a
schema is declared — there's only the table definition, not the struct
codegen will produce from it. No loosening of the import graph changes that
ordering.

So the column mixin mechanism stays as it is and isn't extended to carry
hooks; if a mixin is ever to carry behaviour, codegen is the carrier, since
it's the only layer that knows both the declaration and the generated type,
and generated code is at least committed, readable and deletable. That's
recorded as a direction rather than built, so the next person doesn't
re-derive that the schema package is the wrong place for it. What's fixed now
is documenting plainly that the column mechanism is the extension point — it
was never advertised as one, which is why the review read the built-in
bundles as hardcoded rather than as an instance of something general — and
letting a bundle contribute table-level declarations like an index or a
check constraint alongside its columns, so a bundle wanting a partial index
on its own column doesn't leave the caller to remember it separately.

This buys a documentation fix at no design cost, since the underlying
mechanism already existed, and keeps the schema package a description of a
database rather than a place where runtime behaviour hides. The cost is that
soft deletion stays a two-part declaration — a column bundle plus a
separately hand-registered hook — and a table declaring only the first half
is still wrong in a way nothing currently catches; a lint rule is the obvious
fix and isn't written. Revisit if a second mixin wants behaviour, such as a
multi-tenancy bundle wanting to travel with its own scoping hook — one
instance is a bug fixed by documentation, two is a pattern that means the
codegen-as-carrier direction stops being deferred.

### No annotation slot

An outside review suggested an annotation slot on the table declaration,
pointing at ent's ecosystem of generators hanging their own config off it —
"that is why ent has an ecosystem and sqlb has a feature list." The
observation is correct and the causation runs the right way; the question is
whether the slot is the missing piece here. sqlb already has an annotation,
and it's typed: `Expose(schema.REST{...})` is config attached to a table,
meaningless to the database, read downstream — and it works precisely because
it crosses the boundary as a value with a known shape, so codegen can read
its fields and the manifest can describe what it means. An untyped bag
couldn't do either. And the slot is the smaller half of what ent actually
has: the thing that reads an annotation there is a generator with a plugin
and template system, while this project's code generation is a fixed
sequence of emitters named directly in the source, with no plugin interface
and no way to add a fifth. An annotation added today would be a field only
the in-tree emitters could read, which a typed field already serves better
than an untyped one — the extensible generator is the load-bearing half of
ent's feature, and it's far more work than the slot. The demand for either is
also inferred rather than observed: this is a pre-1.0 project with one author
and no third-party consumers yet, and the schema package sits in the stable
tier, where a mistaken commitment is expensive to walk back.

So there's no annotation slot, and the schema stays a closed, typed
vocabulary: new config on a table gets added the way the REST-exposure field
was, as a typed field with a consumer written at the same time in the same
repository. If third-party extensibility is wanted later, the order runs
opposite to what the review implied — an extensible generator first, with a
stable view of the registry for it to read, and the annotation slot second,
shaped by what that generator actually turns out to need. It may even turn
out the emitter interface is the whole feature and the slot is never needed
at all.

This buys every declaration a known meaning, so validation can reject an
incoherent one, linting can warn on it, the manifest can describe it, and the
schema can round-trip back out as source — the last of which is the adoption
loop, and it only works because the set of things a registry can hold is
closed; an opaque bag would break it, since the renderer can't emit Go source
for a value it doesn't know the shape of. The cost is that anyone wanting to
attach their own config today forks the project or opens a pull request, and
"send a patch" scales badly against one maintainer the day someone actually
wants this. Declining now is deliberately the cheap direction: adding the
slot later is additive and changes nothing existing, while narrowing or
removing one after it ships is close to impossible, since the moment one
extension writes into it, its shape is frozen in a stable-tier package.
Revisit the moment someone asks — one concrete request for config sqlb
doesn't understand, naming the consumer they intend to write, is deliberately
a low bar — or if a second in-tree consumer wants config that doesn't belong
in the existing typed fields, which is the same pressure arriving from
inside instead of outside.

### Expansion is one statement

`?expand=author` had been parsed since the filter grammar existed and refused
by every surface that could otherwise answer it, and building it forced three
decisions. The obvious implementation reads the page, collects the foreign
keys, and issues a second `SELECT … WHERE id IN (…)` — inheriting every hook,
filter and capability check on the target for free, and unable to be made
consistent: the second query runs at a later snapshot, so a row deleted
between the two produces a contradictory answer, the foreign key set but the
expanded object null, both in one response. Fixing that with a
repeatable-read transaction would make every list request pay for a problem
only expansion has. Whether the target's columns could be trusted wholesale
was the second question: building the expanded object with one call over
every column of the target row would carry hidden columns like a password
hash straight across the join — not a corner case, a leak in the shipped
example. And then, once a join-based version was built, tested across three
packages and green, Postgres refused the statement outright as an ambiguous
column reference, because the predicate-building code produced unqualified
column references that were fine in single-table SQL and invalid the moment
a second table entered the query — the in-memory test driver had been
validating what someone expected rather than what Postgres actually accepts.

So expansion compiles to one statement: a `LEFT JOIN` per relation, and a
`json_build_object` over the target's non-hidden columns built explicitly
column by column rather than with a wholesale row-to-json call, so a hidden
column can't survive the join by construction — verified by reading the raw
JSON the database produced, not the decoded struct, since a struct silently
drops an unmapped key and would pass even with the hash sitting in the
response. A join matching nothing yields `NULL`, not an object of nulls. An
unqualified column resolves to the base table, but only once something is
actually joined, so single-table SQL keeps the plain column names a human
reading a logged query wants. And expansion goes one level deep only.

This buys consistency by construction — one snapshot, so the foreign key and
the expanded object can never disagree — at no transaction cost, with the
projection staying exactly as wide as the base type. The cost fell mostly on
hooks: getting a soft-delete or tenant-scoping predicate to apply correctly
to the joined table required rewriting it onto the join's alias before
splicing it into the `ON` clause, and a predicate that can't be requalified
with certainty — a raw SQL fragment, or one naming a table that isn't part of
the join — fails the query outright rather than being silently dropped, since
a dropped scope predicate is the same leak arriving by a quieter route. The
join is also unconditional per named relation, with no query-time budget
beyond joining on a primary key, so an unindexed expandable foreign key is
something linting has to catch rather than something the runtime protects
against. Revisit if nesting is ever wanted — `?expand=author.org` reopens the
one-statement argument, and it's weaker for a nested relation whose
inconsistency window a client can barely observe anyway — or if expansion
turns out to be the measurably slow query on a real screen, at which point a
batched second query becomes worth its inconsistency on a per-resource basis.

### Vectors declare their index

A vector column had no support anywhere in the schema pipeline — the DDL
type mapping had no case for it, index creation had no operator class, and
introspection refused a `vector` column outright, which would drop an
embedding on adoption of any existing database using one. And yet a raw SQL
distance expression in an `ORDER BY` compiled and returned correct rows,
which is the dangerous half of the problem rather than the reassuring one: it
returns correct rows by sequentially scanning the table, and a feature built
on an escape hatch that happens to work looks finished right up until the
table is large. The type also can't be pushed off to a hand-maintained SQL
mirror file the way a window-function query can, because a vector's
dimension needs to be a value, not a parsed literal, and a real consumer of
this pattern was already hand-maintaining a mirror schema file with a text
sentinel its own SQL parser couldn't tolerate — the dimension wants to live
in the schema DSL as a Go expression or the arrangement breaks for any
project with an embedding column.

The harder argument is that an approximate nearest-neighbor index isn't like
a btree. A btree is an optimization the query planner may ignore without
changing the answer; an ANN index changes the answer — recall is a tuning
parameter, not a guarantee — serves only a narrow `ORDER BY <op> LIMIT k`
shape, and is built for exactly one distance metric, so an index built for
cosine distance queried with a different operator doesn't error, it
sequentially scans, meaning a metric mismatch is a correct-looking answer at
a thousand times the latency with nothing to notice it. Filters compound the
danger silently: an ANN scan finds its k candidates first and applies a
`WHERE` clause after, so a filtered search can come back short of what was
asked for with no error at all — and this isn't hypothetical, it's a
deployed shape found in a related codebase's production index, narrowed by
several per-request filters with no scan tuning anywhere. Measuring the
physical claims against a real pgvector installation confirmed the
mechanism, while also correcting the framing: recall varies with the corpus,
the planner's own costing, and the ANN index's own build randomness, so no
recall percentage measured here is a transferable fact about pgvector, only
about that one run. The planner may also simply decline the index under a
selective filter and fall back to an exact scan — which is not a
reassurance, since it makes the silent under-return conditional on
statistics nobody is watching, the same query complete on one database and
quietly partial on another.

So a vector column declares its dimension as part of its type, and an index
is a separate, optional decision that carries its metric with it. The
unindexed configuration is complete on its own — `Searchable()` with no
index is the right shape at small scale, giving an exact, correct, slower
scan — and adding an index is a second decision taken later, once exact
search stops being fast enough. The metric lives with the index, not with a
query, and asking for a metric no declared index serves is refused at build
time naming the ones that are, because the failure this prevents is invisible
in results and shows up only as a latency graph nobody thought to check. HNSW
is the default index kind, because an alternative one clusters nothing when
built against an empty table, and a migration-generated index runs at
exactly that moment. Which filters may accompany a search depends entirely
on whether there's an index: with none, any filter the model allows runs
against an exact scan; with an index and filters known at declaration time, a
constant predicate folds into the index's own `WHERE` and a variable one must
be declared up front, refused otherwise; and with an index and filters that
vary per request, the caller must explicitly opt into iterative scanning,
whose honest promise is "try harder, and it may still come back short" rather
than a fix — naming it as a fix would reintroduce the same silent failure
behind a configuration flag. A similarity search is also its own operation,
not a query parameter, both because a 1,536-dimension embedding doesn't fit
comfortably in a URL and because paging into an approximate neighbor set
isn't page six of anything meaningful. The embedding itself never crosses the
wire — a vector column is unconditionally hidden from REST responses.

This buys the schema staying the single source of truth for a project using
embeddings, closing the same adoption loop every other column type gets, and
it refuses the one mistake with literally no symptom — a metric that doesn't
match its index — at build time instead of at query time. The cost is real:
this is the largest surface added to the schema since relation expansion,
justified by keeping one source of truth rather than by vector search being
popular, and testing it honestly is harder than testing anything else here,
since a recall percentage isn't a stable fact to assert on — the tests that
exist assert on the shape of the query plan instead. Choosing to leave the
indexed half unbuilt initially, while shipping the unindexed configuration as
complete, means the riskiest part of the surface can be deferred or dropped
entirely if nothing ever outgrows an exact scan. Revisit if a second metric
per column is genuinely needed, which breaks the assumption that a distance
expression can bind its vector once and serve projection, predicate and
ordering together; if a caller needs the raw vector back over the wire for
client-side re-ranking; or if the added surface shows up as bugs elsewhere
faster than it earns its keep, in which case the fallback is cutting back to
an opaque passthrough type at a tenth of the code.

### Keyset pagination

`LIMIT n OFFSET k` was the only paging on offer, and its two defects are one
defect: Postgres has to produce and discard `k` rows to answer an offset
query, so page 500 costs five hundred times page 1 on exactly the endpoint
whose purpose is walking a filtered set, and because a page is addressed by
its distance from the start, a row inserted mid-walk shifts every later page
— the client sees a row twice or never, with nothing wrong with either the
query or the insert. Naming the boundary directly instead of by distance is
harder than it looks. An ordering that isn't total can't answer "the rows
after this one," and the same ambiguity was already silently making offset
paging skip and repeat rows. NULLs don't compare, so a naive boundary
predicate is wrong twice over — it drops every NULL row when NULLs sort past
the boundary, and a NULL boundary value makes the whole comparison NULL,
silently truncating the walk with no error. And the textbook-correct
boundary predicate, a full lexicographic expansion, is always correct and
also defeats the entire point: it's an `OR` of conjunctions that Postgres
answers with a bitmap of index scans rather than the single seek that was
the reason to page this way at all.

So a cursor names a position in an ordering, and the ordering is always
forced to be total: appending the primary key as an implicit final tiebreaker
happens automatically on every list request, so deterministic paging isn't
something a caller opts into — it's true of the very first page, since that
page's last row is what becomes the next cursor. The boundary predicate
compiles two different ways depending on what it can prove about the
ordering: when every term shares direction and no ordering column is
nullable, it compiles to a single row comparison — `(view_count, id) <
($1, $2)` — that Postgres turns into one index seek; otherwise it falls back
to the lexicographic expansion with NULL handled term by term, and that
fallback is triggered by a nullable *column* in the ordering, not by a null
*value* in a particular cursor, since the values being compared live in the
rows, not only in the cursor. The cursor itself is opaque by convention — a
plain, unencrypted encoding — rather than by encryption, and that's safe
because the boundary is validated against the ordering the current request
actually asked for, so an edited cursor can only move the boundary along a
column the caller could already sort by anyway. An ordering by anything other
than a plain column is refused for cursor paging by name, since the boundary
has to be read back off a returned column value — which is also why an
approximate nearest-neighbor ordering can never be cursor-paged: it isn't a
total order in the first place, so a boundary in it could skip or repeat
results no matter how carefully the distance were encoded. Paging is forward
only — there's no `?before=`.

This buys paging that costs the same at any depth and makes a concurrent
insert land unambiguously behind or ahead of the cursor rather than
duplicating, and it closes a determinism defect that predated cursor paging
entirely, since every list is now deterministically ordered whether or not a
caller uses cursors. The cost is that the forced tiebreaker can force a sort
an index on the primary sort column alone would have avoided — some list
endpoints got measurably slower in order to stop being wrong, and the fix is
the same composite index cursor paging wants anyway. There are also
genuinely two predicate-compilation paths rather than one, and the boundary
between them is a correctness argument about NULLs, not a style preference; a
cursor is coupled to the ordering it was issued under, so changing the sort
parameter while holding an old cursor is a rejected request rather than a
silently wrong one. Revisit if a client needs a genuine back button it can't
build by holding cursors, which offset paging still serves and a `?before=`
parameter could add additively; or if a cursor ever needs to carry something
a client shouldn't be able to choose for itself, such as a tenant or a
snapshot id, which would reopen the question of signing it.

### Typescript client

The obvious plan was to point a generic OpenAPI-to-TypeScript generator at
the emitted OpenAPI document and write no generator of its own, and three
things argue against it. The document is lossy exactly where the value is: a
filter parameter like `?status=eq.published` can only be documented as
`array<string>` with the operator vocabulary left to prose, so a generic
generator emits a bare string array type and a bogus operator compiles
clean. Two independently hand-written reference clients confirmed the shape
of that loss directly — both hand-roll their list endpoints with raw
`URLSearchParams` and untyped string parameters, with a comment explaining
which values happen to be legal, which is the single most generatable
function in either codebase and exactly where a typo compiles. And the
sharpest evidence was about cache keys: one client had to impose a key
factory and an architecture test to enforce it after two production bugs;
the other hand-writes over thirty string-literal keys across nine files, and
its change-feed subscriber and its mutation handlers keep two invalidation
lists that have quietly drifted apart by a single character — nothing catches
it, and it's the signature of a missing artifact rather than a missing rule.

So the TypeScript client is generated from the same model metadata the Go
code generator already reads, not from the emitted OpenAPI document, and
emitted directly into the consuming repository rather than published as a
package — a client generated against the exact server it talks to can't
drift from it. It's built as four independent layers, each usable without the
one above it: row and request-body types, with hidden columns absent from the
row type entirely rather than merely marked; typed request parameters, where
`where` admits only filterable columns with an operator set narrowed by
column type, and `sort`/`select`/`expand` are similarly narrowed; transport
functions encoding those parameters into the URL grammar, taking an injected
request function so auth, refresh, retry and redirect-on-401 stay the
application's problem; and a key factory plus query-options factories for
reads, with write support deliberately stopping at a bare mutation function.
That last boundary is deliberate rather than an oversight: the mechanical
half of a write — route, body type, response — is derivable, but what a write
should invalidate depends on which views a particular application keeps, and
a computed view isn't a table, so its cache key can't be generated at all; a
generated success callback would be a guess, and a guess in generated code is
exactly the kind of thing that gets copied out and silently edited. Wire
names keep their JSON tag spelling rather than being camelCased, since
camelCasing would need a runtime mapping layer this design refuses to carry.
An expanded reverse collection keeps its list envelope rather than being
typed as a bare array, which is the one shortcut that would reintroduce
silent truncation one layer further from where it was closed.

This buys a misspelled column, an illegal operator, or a sort on a column
that never opted in all failing at the TypeScript compile step instead of at
a runtime 400, and gives the change feed a consumer whose cache invalidation
is structural rather than a convention someone has to remember. The cost is
a second toolchain — Node, and a peer dependency on a query-caching library —
inside a repository whose pitch is a single Go module, and the generated
surface is honestly a minority of what a real client needs, five of twenty
methods in one observed service. Adoption is a migration rather than a
drop-in for any existing client, since both reference clients assumed offset
paging and an always-present total count that this design doesn't provide
the same way. Revisit if callers reach for the raw-parameters escape hatch
more often than the typed `where` builder, which would mean the typed layer
is an overlay rather than the primary interface; or if every real consumer
ends up hand-writing the same success callback anyway, which would argue the
policy actually is derivable, most likely as a keyed defaults mechanism
rather than a generated callback.

### Go cli

The TypeScript client's argument reaches a second consumer: a growing share
of traffic against an API is an agent working in a shell, which has to
discover what a resource accepts before it can ask for anything. `curl` is a
poor fit for this API specifically, because the filter grammar is
compositional and gated by declared capabilities, so the set of legal
requests isn't visible from the endpoint itself — a request either works or
comes back with a 400 naming the filterable columns, and the only way to find
out is to send it. Actionable error responses make that 400 recoverable,
which is most of a fix, but it's delivered only after a failed request, and
the caller who needs it most is the one for whom a round trip is a real cost.
A generic OpenAPI-driven CLI inherits the same loss as a generic TypeScript
client, since the filter grammar still arrives as free text either way. What
differs from the TypeScript case is where the vocabulary can land: a CLI has
no compile step, so nothing can refuse an illegal request before it's sent —
what it has instead is `--help`, read before the request rather than after
it.

So a cobra command tree is generated from the same registry the other
emitters read, into the consuming repository as one self-contained package,
and it speaks HTTP rather than SQL — it holds no database credential and
can't bypass what the HTTP layer enforces per request, such as the claims a
hook reads or a rate limit, while looking from outside like the same command
a direct-to-Postgres tool would be. The commands are exactly the exposed
operations, so an operation that isn't exposed has no command rather than
merely undocumented one. The flags are the capability vocabulary itself: one
flag per filterable column, taking the wire-spelled condition and repeatable,
since repeating a flag is what conjoins conditions, with the operator set on
each narrowed by column type exactly as the REST documentation and the
filter parser narrow it — which makes `--help` an exact, request-free
statement of what the resource accepts, not a refusal but a disclosure of
the same vocabulary the typed TypeScript facade carries in a form a shell can
use. Presence is read from whether a flag was passed, not from its value,
because a flag left out and a flag explicitly set to empty have to write
different SQL, and setting a column back to NULL gets its own explicit flag
rather than overloading emptiness. Walking every page (`--all`) walks by
cursor rather than by counting pages, since a script looping over numbered
pages re-reads rows the moment the underlying table is written mid-walk —
exactly the case a long-running walk is likely to hit.

This buys a vocabulary that's readable without sending a request, by a human
or an agent, and that can't drift from what the server enforces, since both
the flags and the server's own validation come from one declaration; a
column gaining a capability gains its flag at the next regeneration with
nothing else to update. The cost is a cobra dependency in the consuming
module, help output that scales with schema width rather than API surface —
around 1,800 lines of generated code for a six-table schema — and a CLI that
covers exactly the generated CRUD, so a hand-written endpoint like login has
no command at all, meaning the first thing an operator needs, a token, is
the one thing this tool can't get them on its own. Revisit if operators
reach for `curl` anyway despite the flags, which would mean discovery isn't
actually the friction point and the tool should shrink to one generic
request command handling only auth and paging; or if per-column flags make
`--help` unreadable past roughly thirty filterable columns on a wide table,
which would push the vocabulary into a `describe` subcommand backed by a
single structured filter flag instead.

### Declared scope is required

Hooks are offered as the answer to multi-tenant scoping and are described as
failing closed, which is true of a hook that runs and says nothing about a
hook nobody wrote. That's the actual hole: row-level security, the mechanism
hooks replace, is default-deny — a new table is unreachable until someone
writes a policy for it — while a query hook is default-open. Add a table,
expose it over REST, forget to register its scoping hook, and the resource
serves every tenant's rows with a 200. A worked multi-tenant example made
this concrete rather than theoretical: its hand-written hook file was
careful, well-argued, and enumerated five models by hand — and correct as far
as it went, while being structurally unable to notice a sixth table someone
adds later.

So a schema declaration that rows are confined — `.Scoped()` on the foreign
key naming the tenant column — becomes an obligation on any resource that
exposes the table, and mounting refuses a resource whose obligations aren't
satisfied by a registered hook. Neither the declaration nor the check writes
a predicate itself; `Scoped` is inert at runtime, exactly as the soft-delete
mixin already was, because a generated tenant predicate reading the wrong
context key would be worse than none — it would look like a boundary while
being one. What changes is what happens when nobody writes the hook at all.
The obligation follows the exposed operations rather than being one flat
requirement, since a query hook constrains what a request can see and says
nothing about what it can overwrite by id: listing or reading requires a
before-query hook, updating requires a before-update hook, deleting requires
a before-delete hook, and creating requires a before-create hook — the last
because the tenant column is read-only and therefore absent from the create
body, so without a hook the insert would reach the database carrying no
tenant at all. `Scoped` is defined to imply read-only, enforced by schema
validation, and a nullable tenant column is rejected too, since a row whose
tenant is NULL sits outside every scoping predicate and lands in everyone's
results the day someone writes an `IS NULL OR = $1` clause; marking the
column merely immutable isn't enough on its own, since that closes updates
while leaving create wide open. The check itself proves only that a hook
exists, not that it's correct — a hook that logs and returns success
satisfies it — deliberately, because the alternative, actually running the
application's hooks at mount time against a fabricated context, mostly just
observes those hooks correctly refusing a fake context, proving less than it
appears to while looking like it proves more.

This buys turning a silent cross-tenant read at runtime into a named error at
startup, listing every unmet obligation next to the registration that would
satisfy it — the same actionable-error property this project gives callers,
applied to the schema's own author instead. The cost is that a new table in a
multi-tenant schema now fails to start until its hooks exist, which is
friction by design and deliberately has no bypass flag: the way to say a
table's rows aren't confined is to not declare that they are. The check also
runs only at the REST mount, not on the query path itself, so
application code querying a model directly bypasses it entirely — a
deliberate choice, since a per-query check would cost something on every
read and legitimate unscoped administrative code paths genuinely exist.
Revisit if people start registering empty hooks purely to satisfy the check,
which would mean it's a tax rather than a real boundary and the fix is
making the check prove more, not relaxing it; or if a legitimate deployment
wants to scope through row-level security or middleware instead of hooks,
which today means stripping the declaration entirely and losing its
documentation value along with it.

### Dart client

The TypeScript client's argument — generate from the registry, not the lossy
OpenAPI document — is about the schema and the wire, so it transfers to Dart.
Three things about the language do not. Dart has no structural types, so a
projected row cannot merely carry an absent field the way a TypeScript row
typed as the full row does — a strict data class with `required String
title` cannot be constructed from a response that omitted it, so a
compile-time `select` narrowing is not just unwritten here, it's unwritable.
Dart has no implicit deserialisation, so mapping a wire key to a member isn't
a layer to add, it's the string literal already being hand-written, and
snake_case members fail the language's own naming lint. And Dart has no
keyed query cache — Riverpod and BLoC have no registry to invalidate
against — so a key factory would be vocabulary with no consumer.

So the emitted vocabulary is the TypeScript one, unchanged: `where` admits
only filterable columns with operators narrowed by type, `sort`/`select`/
`expand` are closed sets, hidden columns have no spelling. Four things
differ. Members are camelCase with the wire spelling kept beside them as a
string constant — the one place this contradicts the TypeScript client,
because the argument against a mapping layer doesn't hold once the mapping
is unavoidable string literals either way. A row is a view over the decoded
response rather than a copy of it: it reads columns on access, and a column
the request didn't select throws `MissingColumn`, naming the row, the
column and the fix, rather than silently returning null — which is what
makes a projection representable at all, at the cost of moving that failure
from compile time to runtime. The framework layer is a cursor pager,
`CursorPager<Task>`, holding rows and position and nothing about how
they're shown, so it drops into Riverpod, BLoC or a bare `StatefulWidget`
without preferring one — generating framework-specific providers would be
the same rejected idea as generating fetch hooks, under a different name.
And the emitter reproduces `dart format`'s output rather than deferring to
it, since this module carries no Dart toolchain to run the formatter with.

This buys the producer with the least type safety in this API's surface the
most from a generated client — every column, operator, sort term and enum
value in a mobile filter UI is now checked by the analyser, and a cursor
walk collapses to one function instead of several hand-rolled offset loops.
The cost is a third toolchain (a pinned Dart SDK, `pub get` in CI, a gate),
a runtime rather than compile-time failure for an over-narrow `select`, and
format stability asserted against one formatter version rather than
delegated to it. Revisit if applications end up wrapping every generated
call in a repository class anyway, which would mean the free-function shape
is wrong; if `MissingColumn` starts showing up in production rather than
being caught in development, meaning `select` is too sharp a tool for this
language; or if a Flutter app finds the row view awkward next to a
`freezed`-style value class, at which point plain constructor-built data
classes win and `select` leaves the client as a capability rather than a
spelling.

### Sqlb command

The schema is Go, not a file (see "Schema as Go DSL" above), and a table
only exists in a registry once the package declaring it is linked in — so
unlike `sqlc` or `atlas`, a prebuilt `sqlb` binary cannot read a project's
schema; only a program that imports it can. Each project first answered
this by hand-writing its own `cmd/gen/main.go`, and the copies drifted: a
`-dir` flag whose correct default changed depending on whether it ran from
a shell, `go generate`, or CI, and a `codegen.Options` literal that was the
only project-specific line in sixty. So `sqlb generate ./taskschema` writes
a three-line driver to a temporary directory outside the repository,
compiles it with the working directory at the module root, runs it, and
deletes it — no artefact to gitignore, none left behind on failure. The
driver itself stays three lines; everything it could contain lives in
`codegen.Main`, which is ordinary tested code, because generated code
cannot be tested but the package that generates it can. A project declares
itself by exporting a convention function, `SqlbProject() codegen.Project`,
rather than a config file — a config file would be a second declaration
language mirroring `codegen.Options` field for field, drifting whenever a
field is added and reporting mistakes at runtime, where the Go function is
compiler-checked and `go doc`-documented. Paths always resolve against the
module root, never the schema package or the working directory, which is
what deletes the need for a `-dir` flag at all: the command means the same
thing from all three callers that had to agree before.

Two verbs need no compile: `sqlb check` asks whether committed output
matches what the emitters would produce, a pure function of the schema, so
it gates every push with no database; `sqlb survey` introspects a live
Postgres and needs no schema package either. `sqlb migrate` is the verb
that does need one — it asks whether the committed migration history
*builds* the declared schema, answerable only by replaying it into an
empty database. Building `migrate` surfaced that a declared `CHECK`
constraint, and later a partial index's `WHERE` clause, never round-trip as
text: Postgres stores a parse tree and renders it back canonicalised, so
the author's spelling is unrecoverable and comparing declared-versus-live
as strings reports a phantom diff. Canonicalising both sides in Go was
rejected — stripping parentheses loses information, since `(a OR b) AND c`
and `a OR (b AND c)` reduce alike, so a heuristic can call two different
constraints equal and silently produce no migration where one was needed,
a failure quieter than the loud, visible churn it would replace. Instead
`shadow.Normalize` adds the declared expression to the replayed table,
reads back what Postgres actually stored, and rolls back — correct by
construction, one round trip per expression, available only because the
command already had a scratch database open at the right moment.

`sqlb survey` was originally its own binary, on the reasoning that it needs
no schema package to compile — but that's a fact about one verb's
arguments, not a reason for a second binary with its own help text and
install line. It was folded into `sqlb` as a fifth verb once the actual
boundary became clear: the compile is a consequence of the schema being
Go, not a property of the command. Cobra was considered for the merged
tree and rejected on the command's own shape: `cmd/sqlb` doesn't parse the
driving verbs' flags, it forwards them opaquely for `codegen.Run` to parse
on the far side, so cobra confined to `cmd/sqlb` would own a five-case
switch and none of the flags, while cobra pushed far enough to own the
flags would put a CLI framework inside `codegen`, a library package every
consumer imports — a dependency paid for by every consumer to improve the
help text of a binary only maintainers run, with no argument strong enough
to justify it. What this buys is a workflow that is one command with
byte-identical output to the generators it replaced; what it costs is a
real compile on every invocation — sub-second warm, the module's full
dependency graph cold — and a hard requirement that `go` is on the path,
so `sqlb` cannot be a binary dropped into a container with no compiler.
Revisit if a project needs something `Project` can't express and reaches
for a hand-written `main` again — widen the type rather than accept that,
since a working hand-written generator is one nobody comes back from — or
if the compile cost is felt in the inner loop rather than only in CI, which
argues for caching the built driver keyed on the module's build ID rather
than abandoning the mechanism.

### Array columns

Two outside evaluations named array columns the cheapest schema gap with
the most real call sites, and the gap was total rather than partial: with
no `text[]` mapping, `introspect` didn't drop an array column, it refused
one, and one array column anywhere in a module meant the whole module
failed the adoption loop's empty-diff test. jsonb doesn't answer this for
an adoption target that already has `text[]` — declaring it `jsonb` makes
the diff render a rewriting `ALTER`, destroying the zero-migration probe
adoption exists to offer, and it loses on the wire too, since a JSON type
emits `unknown` in the TypeScript client where an array of text could say
`string[]`.

So an array is its element type plus a flag — `FieldDesc.Type` keeps naming
the element and gains `Array bool` beside it, rather than a fused constant
like `TypeTextArray` — because the filter parser needs the element type
back: `?tags=has.urgent` binds a scalar `text`, and a fused type would have
to be split apart again at the one place that most needs to get it right.
`Nullable` still means the column may be NULL, but a NULL *element* is
refused at validation time, since `{a,NULL,b}` versus `NULL` is a
distinction neither generated client can express. The Go-side type is a
plain slice, `[]string`, not a named `sqlb.TextArray` — decided by the
adoption path, since sqlc in pgx mode emits a plain slice for `text[]`, and
a named type would make arrays absent from exactly the path meant to prove
the library cheaply. `Sortable` and `Searchable` are refused outright,
since sorting would put an array into a keyset cursor, and `Filterable`
requires a GIN index, since an array filter with no index is a sequential
scan — the same failure vector-index declarations exist to name, arriving
here at a fraction of the cost to get right.

The operator vocabulary tracks what a census of real usage actually
called for: `has` binds a scalar element (`$1 = ANY(tags)`) and covers the
large majority of observed queries; `hasany` (`tags && $1`) and `hasall`
(`tags @> $1`) bind whole arrays for the rest. `contains`, which reads
naturally, is deliberately not one of them — it stays text-only, since two
operators sharing one name and differing only by column type is exactly
the ambiguity the generated clients exist to remove, and a caller who
types it gets refused by name toward `has`. Negated forms (`nhas`,
`nhasany`, `nhasall`) were added once the JSON filter tree gained a general
`not` group: the URL grammar conjoins by design and has nowhere to put a
`not`, so without negated array operators the two filter frontends would
have compiled different vocabularies, breaking this project's rule that
they compile to the same predicate. Each negated form compiles to a plain
`NOT (…)`, not a hand-derived complement, so a NULL column matches neither
the positive nor the negative form — three-valued logic that only a real
Postgres could settle, and is asserted there rather than against rendered
SQL text.

The codec that first made binding and scanning a `[]string` possible — an
array-literal encoder and decoder written by hand under a stdlib-only
constraint — was later deleted outright once the driver dependency
[moved to pgx](#the-driver-is-a-dependency), which decodes and encodes
arrays natively; a `[]string` now binds and scans with nothing in between.
That deletion left every decision here untouched, because the element-type
shape, the plain slice, the operator set and the refusals never depended
on which library did the decoding — only on what the wire format and the
schema needed to say. What's bought is that a single unsupported array
column stops failing an entire module's adoption; what it costs is several
switch statements across the emitters that grow a case with no compiler to
check they all did — a missed arm in the TypeScript emitter would produce
a client type silently wider than the server, the one failure the
TypeScript client's own design claims cannot happen. Revisit if `has`
turns out to be the only operator anyone reaches for in practice, which
would mean the whole-array operators weren't worth building; if a real
sqlc struct doesn't present as a plain slice on a schema with nullable
elements, which would argue for a named type after all; or if the GIN
requirement is experienced as a tax — an index added only to satisfy a
check and never queried through — which would turn it from a refusal into
a lint.

### One column addresses a row

`schema.Validate` has always refused a second `PrimaryKey()` on a table,
but the refusal shipped unexplained, and an outside evaluation asked
plainly whether it meant "no composite primary keys" or an undocumented
"surrogate key required" stance — unable to tell which, and therefore
unable to tell whether the cost it was about to pay (twenty-six composite
primary keys, across fifteen tables in one real codebase) was permanent.
Five of the six places a composite key would touch have mechanical
answers nobody had written yet — a tuple cursor is a row-value comparison
Postgres plans as well as the scalar form. The sixth doesn't, and it's the
whole decision: the URL. `/tasks/{workspace}/{id}` collides with
sub-resource paths in a way that depends on what an application mounts —
a conflict surfacing at someone else's mount site — and `/tasks/{a},{b}`
invents an encoding that then needs an escape for a key containing a
comma. Either becomes wire format on the first response, alongside a
tuple cursor payload and generated client cache keys, so the real question
was never whether sqlb *could* carry a composite key but whether guessing
its URL spelling was worth freezing before anyone had asked for one.

So one column addresses a row, and what a table needs is bounded by what
it does rather than by a blanket refusal: a table queried only through the
builder needs nothing, since offset paging and an unaddressed model work
fine without one; a table that's cursor-paged or expandable needs one
column, since the cursor breaks ties on it and an expansion aggregates by
it; a table exposed for read, update or delete needs one column, since
`/resource/{id}` is one path segment. A join table with a genuine
composite key, queried through the builder and never exposed over REST,
needs no surrogate at all — `UniqueIndex("a", "b")` states the real key
and Postgres enforces it, and the constraint bites only on addressing.
Where a table does need addressing, the composite key doesn't disappear,
it changes job: a surrogate key carries identity and a `UniqueIndex`
keeps the real key real, a pairing already natural enough to appear in the
worked multi-tenant example for unrelated reasons. This is also narrower
than what shipped: the registry refuses a second `PrimaryKey()`
everywhere, while the actual argument only reaches tables that are
addressed, cursor-paged or expandable — a real gap between the record and
the code, since a table that is none of those three pays a refusal the
reasoning here never asked for. One deployment already hit exactly that
case and reported it "didn't block me", because an upsert's conflict
target can still name a composite key directly even without a declared
primary key — so the table loses the ability to *declare* the key it has,
not the ability to work, and narrowing the refusal to just the tables that
need it stays the right next change without being urgent.

What's bought is that a row has exactly one spelling everywhere it's
named — the URL, the cursor payload, the `?expand` aggregation, the
generated cache key — each of which is a frozen wire format that would
otherwise need its own independently-invented tuple encoding, permanent
from its first response. What it costs is real: an adopter with a
composite-keyed table too central to leave on hand-written SQL pays a
migration this project doesn't, and a table gains a surrogate column its
domain has no use for. The asymmetry runs the useful direction — widening
to tuple support later is additive, since a one-element tuple is exactly
today's behavior, while narrowing is impossible the moment a
composite-keyed row has a URL a client has already built a request
against. One alternative is underrated and worth reaching for first: a
join table with a natural composite key, queried by a couple of static
queries and never exposed, fits comfortably on the sqlc side of the
line this project already draws for tables it doesn't want to own.
Revisit if a real schema needs a composite key on a table that is
genuinely none of addressed, expandable or cursor-paged, which argues for
moving the refusal into the mount check and the cursor logic instead of
the registry; if a surrogate-key migration ever actually stops an
adoption, meaning the constraint is landing on the wrong party; or if a
second consumer needs tuple cursors regardless, at which point the
marginal cost of full composite-key support falls to the URL spelling
alone.

### Type overrides

`schema.Type.GoType()` was a closed switch — a `uuid` column is always
`string`, `numeric` is always `float64` — and both outside adoption
evaluations named this a real blocker: a codebase using
`github.com/google/uuid.UUID` for ids would have that type touch its
tenant middleware, every filter registry and every use-case signature, and
the only workaround, layering `sqlb.Describe` over structs sqlc already
generated, sidesteps the question rather than answering it, since it works
only by never generating a struct at all. So `codegen.Options` gains
`Types []TypeOverride`, matched by table+column, column, or type (most
specific wins; two overrides of equal specificity on one column is a
generation-time error, not last-one-wins), and an override changes the
emitted Go type and nothing else.

That "nothing else" is the whole decision, and it points two different
ways on purpose. The SQL type in DDL is decided by `schema.Type` alone and
an override never reaches `migrate` — the database doesn't care what a
consumer calls the type in Go. The wire — JSON, OpenAPI, the TypeScript
and Dart clients, the CLI — also stays governed by `schema.Type`, not the
override, because every client emitter maps from the declared type, so a
`uuid.UUID` still serialises as a quoted string and TypeScript still sees
`string`; this falls out of the emitters rather than being specially
enforced, which matters because "fixing" it later to reflect the override
would make a generated client disagree with the server it describes. Filter
coercion, in contrast, *does* follow the override, and that's correct
rather than an inconsistency: `filter.Coerce` reads the model's runtime
reflect.Type and already delegates to `encoding.TextUnmarshaler`, so an
overridden type that can't be coerced is one that can't be filtered, with
the existing error rather than a new one. Nullable and array compose
normally after the base type is chosen (`*uuid.UUID`, `[]uuid.UUID`), and
an enum column can't be overridden at all — the generated named string
type is the feature being sold, since it's what makes a value like
`PostStatusPublished` exist and carries the value set into the TypeScript
union and the CLI's `--help`.

The import a custom type needs lands in the consumer's generated code as a
string, so sqlb's own dependency budget is untouched, and a bad override —
a typo'd type name — fails loudly at the consumer's next `go build` rather
than being silently accepted. Overrides live in `codegen.Options`, which
is per-project, not in the schema, which is per-declaration: the same
schema generated into two repositories with different id conventions
should be free to differ, and putting the override in the schema would
make a rendering preference part of what `migrate` and `introspect` read.
Revisit if people reach for an override to change the wire rather than the
Go type — wanting camelCase JSON, say — which should stay refused, since an
override that changed both would make a generated client wrong about the
server; or if the enum refusal is experienced as a tax, in which case the
likely real want is a named enum type from a package the consumer already
has, a distinct feature from this one.

### The wire is the column name

Field naming and the list envelope shape were both decided in practice —
every JSON tag matched the column name, every list response shared one
envelope — and undocumented in [compatibility.md](compatibility.md), which
freezes the filter grammar for the same reason but said nothing about
either. That's the state that produces a "we never promised that"
argument later, and two adoption evaluations measured the cost of not
having settled it: one found 85% of a codebase's JSON tags already
snake_case with the rest to rename on both sides; another found camelCase
throughout plus its own `{data, total, cursor, hasMore}` envelope. Neither
found the cost unpayable, but neither was "no change needed" either. The
underlying argument for settling on one spelling is that a client which
renamed fields would need a mapping table, and a mapping table is the one
thing in otherwise-generated code with a reason to drift — the first time
it did, a column would arrive as `undefined` in a UI rather than failing
anywhere a test would see it.

So a column's wire spelling is one thing, reaching the response body, the
request body, the OpenAPI document, the filter grammar's parameter names,
and both generated clients identically — `?created_at=gte.…` names the
same thing the response does — and there is no per-field override, since
that's exactly the mapping table this design refuses. The list envelope is
one shape for every resource (`items`, `page`, `per_page`, `has_more`,
`next_cursor`, `total`), not one per resource, so a generated cursor pager
or query-key factory is written once against it. Both are frozen at 1.0.
The spelling was originally the identity function — the wire name is
exactly the column name, snake_case, no transformation — until a third
adoption data point landed on the far side of the earlier evaluations: a
68-table application at 100% camelCase, one of six applications in the
same position, meaning the rename cost wasn't paid once but six times.
That pressure forced two candidate escapes, and both turned out worse than
a per-field mapping. Renaming the Postgres columns themselves to camelCase
was withdrawn as advice outright — a camelCase identifier is reachable in
Postgres only double-quoted, so it breaks every hand-written query, every
`psql` session and every `pg_dump` a human reads, permanently, for a
Postgres-only library, which costs strictly more than the problem it
solves and isn't reversible the way a wire format choice is. And pushing
the transformation into the adopter's own transport layer just relocates
the exact drift risk the whole design exists to prevent, to hand-written
code with none of sqlb's own guarantees around it — one in-flight
adoption already had such a file and reported it as the least-checked code
in the port.

So the spelling became a declared total function of the column name,
`Verbatim` by default and `WireCase(Camel)` opt-in, applied by the same
pure function at every surface — still one spelling per deployment, still
derived, still no per-field override, and still additive: `Verbatim`
remains the default so no existing deployment is affected. Because
`snake → camel` isn't invertible over every possible column name (`pos_x_2`
round-trips to `pos_x2`, not itself), `Validate` computes each column's
wire name and its inverse and refuses the schema outright if any column
doesn't round-trip — an ambiguity becomes a compile-time error on a schema
nobody has deployed yet, rather than a wrong parameter name in a shipped
client. The derivation crosses into the runtime as data attached to the
generated model, the same way capabilities already cross as struct tags,
never as a policy the runtime evaluates — preserving the rule that the
request path may not import `schema`, so the DSL stays optional. Two
follow-on gaps surfaced after the amendment shipped, both from the same
cause: a surface that *describes* the wire rather than speaks it wasn't
among the five that were actually tested. The compatibility snapshot
matched fields by column name, so flipping a schema's `WireCase` respelled
every field of every resource while the compatibility gate stayed green,
blind to the one edit that renames everything at once; and the generated
manifest reported the column's own name rather than the derived wire name,
so a camelCase schema's worked example printed a request that would 400.
Both are fixed now, and both are evidence that "one setting, several
surfaces" has to be tested surface by surface rather than asserted.

`compatibility.md`'s frozen entry reads "one spelling per deployment,
computed from the column name by the schema's declared `WireCase`" rather
than "the column's own name, verbatim" — what's frozen is that there is
exactly one derived spelling, not which derivation a given deployment
picked; changing a deployment's `WireCase` afterward is a breaking change
for that deployment, exactly like renaming a column. Revisit if the
envelope's key names collide expensively with a common convention, which
would call for the same shape of answer — one choice per deployment, not
per resource — or if a genuinely second wire format is needed, which is a
second adapter rather than a configuration of this one.

### Search is ilike until it cannot be

`?search=ada` fans out across every `Searchable` column as a disjunction
of escaped `ILIKE '%ada%'`, and both outside adoption evaluations named
`tsvector` — Postgres's real full-text type — as a gap: it can't be
declared, `migrate` can't render it, and `introspect` refuses a table that
has one, the same "cannot be adopted at all" severity that got array
columns built. Full text doesn't get the same treatment, because ILIKE is
the right default rather than a placeholder for it, and the two are
different operations wearing similar names. A substring match is what a
filter box does — no index required to be correct, no configuration
needed to be predictable, no dictionary to explain itself; a user typing
`ada` gets rows containing `ada`, including `Nowlada`, which is
occasionally wrong and never surprising. Full text stems, drops stop
words, ranks, and depends on a text-search configuration that's a property
of the deployment rather than the column — better for prose, worse for
identifiers, and which any given `Searchable` column actually wants isn't
something the schema can say on its own.

So `?search` stays ILIKE, and a `tsvector` column doesn't ship. What makes
this a refusal rather than a plan is that full text isn't one feature the
way arrays were: beyond a type, a DDL arm and an `introspect` mapping, it
needs a GIN requirement, a named text-search configuration, an operator, a
ranking function, and — the sharp problem — an answer to generated
columns, since a `tsvector` is almost always database-maintained and
`migrate.Diff` renders neither generated columns nor triggers. A feature
that stopped at the column type would declare something the migration
layer can't actually maintain, which is worse than not declaring it at
all, because it looks complete. Nobody has asked for a specific shape
either — stemmed `?search`, a separately-ranked `?q=`, and a `Filterable`
`@@` operator are three different features the adoption census doesn't
distinguish between. A schema with a `tsvector` column today keeps that
column out of the registry, with `introspect` reporting it rather than
silently dropping it, while `?search` keeps working over whatever text
columns the schema does declare — the table can't be fully schema-first,
but the module adopts with an asterisk rather than not at all, a strictly
weaker gap than the one that justified building arrays.

The cost of change is asymmetric in the safe direction: nothing declares
a `tsvector` today, so a new operator or column type is purely additive.
The one expensive move would be redefining what `?search` already means —
if full text later took over the existing parameter, every deployed
search box would change behavior without its request changing — so
whatever ships lands as a new spelling, not a silent redefinition of the
old one. An opaque `tsvector` passthrough was considered and rejected for
the same reason an opaque vector-column passthrough was: the typed slot is
the small half of either feature, and a third record reaching that same
conclusion is itself evidence. Revisit if a real port can't be adopted
because its search box is genuinely a `tsvector` query rather than merely
missing a nicety — the arrays argument, applied here; if a specific
query shape gets named, such as stemming or ranking, since either is
smaller than the general feature and could ship alone; if `migrate` learns
to render generated columns for an unrelated reason, removing the sharp
dependency; or if ILIKE becomes a performance problem before an
expressiveness one, which argues for a trigram index rather than for full
text.

### Collections are flat

A resource mounts at one collection path, so a task belonging to a list is
fetched as `GET /tasks?list_id=eq.<id>`, never `GET /lists/{id}/tasks` — no
nested route is offered, and this record exists because a route shape is
wire format: nested paths arriving later would be an addition, but if the
flat form were ever going to move, that had to happen before the freeze,
and nobody had written down that it wasn't. A parent filter composes with
everything the request grammar already has — sorting, projection, search,
paging, expansion, disjunction — where a nested path is a second entry
point that would have to grow each of those separately or silently mean
something different through it; a caller can't tell from a nested URL
alone whether `?sort=` sorts the children. It's also already the
documented answer to a question the schema asks on its own: a capped
expansion says `has_more`, and the way to get the rest is the child's own
endpoint filtered by the foreign key — the exact request a flat path
already is. And a row commonly has more than one plausible parent — a task
belongs to a list, a workspace, and an assignee — so a nested path forces
a reader to guess which one is canonical, where a filter has no canonical
parent because there isn't one. Keeping capability checking in one place
matters too: `list_id` is reachable because the column itself declared
`Filterable`; on a nested path, exposure becomes a property of the route
instead, breaking the rule that capabilities live on the column.

What's given up is real: a URL no longer reads as a hierarchy, which
costs a human scanning an access log, and a request for a deleted parent's
children becomes a plausible-looking empty page rather than a 404 —
"no tasks yet" rendered for a list that no longer exists is the sharpest
consequence and the one most likely to surface as a bug report. The
nested form is refused outright rather than added as a convenience alias,
because an alias is a second spelling of one request, exactly the kind of
thing this project spends its refusals on elsewhere — every generated
client, `--help` text, OpenAPI operation and cache key would then have to
choose between the two, and readers would have to learn the choice didn't
matter. An alias also can't answer whether the nested route enforces that
the parent exists: if it does, it isn't really an alias; if it doesn't,
the extra path segment buys nothing. A hand-written nested route stays
entirely available to a project that wants one — write the handler, call
the same filter parsing and application any generated handler uses, and
add whatever parent check it wants — which is the escape hatch generated
REST handlers were designed to leave open.

The cost of change is asymmetric the useful way: adding a nested form
later is additive, since no request that works today would change
meaning and the flat path stays canonical, while removing the flat form
once shipped would be expensive and isn't on the table, since every
generated client, cache key and CLI command is already built on it — so
the freeze binds only the one direction. One alternative is worth naming
for what it would have cost: deriving nested routes automatically from a
declared inverse relation. Elegant on paper, and rejected because it would
make declaring a relation's inverse silently add a route, exactly the kind
of coupling between a capability and its exposure this project's opt-in
rule exists to prevent. Revisit if a port finds the flat form breaks a
client it genuinely can't change — a mobile app with hardcoded nested
paths and a slow release cycle, not merely "the URLs changed" — if the
missing-parent 404 causes a real defect, which argues for a nested form as
a genuinely different endpoint built with its own parent check rather than
an alias; or if nested `?expand` ever lands, since the parent/child
relationship would then gain a second representation and the ordering of
the two features is worth reconsidering together.

### A schema edit is an api edit

Two things called "compatibility" have to be kept apart: `compatibility.md`
freezes sqlb's own surface, while this decision is about the *generated
application's* REST contract — since sqlb re-derives that contract from a
schema on every edit, editing the schema is editing the API, and sqlb is
the one component that knows how. DB-safe and API-safe turn out to be
different judgments, and often inverted ones: a declared column rename is
the cleanest migration this project can emit — a reversible `RENAME` — and
a hard break for every client reading or writing the old name; unexposing
a column or dropping an operation produces no DDL at all and breaks
clients regardless; a nullable column going `NOT NULL` is safe for readers
and breaks the create body for writers; widening `int4` to `int8` is safe
in the database and can overflow a narrow client. That inversion is the
whole argument for a separate check: the cleanest migration can be the
sharpest API break, and the sharpest breaks emit no DDL a migration diff
could ever see.

So the contract check is a registry diff, `restcompat.Diff(old, new)`, a
sibling of `migrate.Diff` rather than built on it — a pure, DB-free
function over two registries that walks every exposed model's response
fields, filter and sort parameters, the create-body required/optional
split, the patch-body fields and the operation set, classifying each delta
`Breaking`, `Additive` or `Neutral`. It has to be a sibling rather than a
layer on top, because the load-bearing word is *capabilities*: a DDL diff
ignores them since they emit no SQL, which is exactly where the
un-expose-a-column and drop-an-operation breaks live, and building on top
of `migrate.Diff` would inherit that blindness. The check is scoped the
way migration generation is scoped, too — the moment a consumer writes a
custom handler, reshapes a response in a hook, or fronts the surface with
a gateway, the true contract stops being a function of the schema and
stops being sqlb's to judge. A break is stated by default rather than
gated, the same choice made for migration lock hazards: whether an API
break matters depends on whether there are deployed clients and whether
the deployment versions its API, something the schema itself can't know,
so a flag turns a `Breaking` result into a CI failure rather than the tool
assuming it always should. What "compatible" is measured against is a
checked-in contract snapshot, diffed against the current registry on
every codegen run — the same move the shadow database makes for migration
drift, checked in so the comparison is something a reviewer actually
reads rather than a claim taken on faith.

The classification has to be proven correct in both directions or it
lies: a nullable column going `NOT NULL` is compatible for readers and
breaking for writers, and a classifier reporting only one side "fires
sometimes" — the shape a guard takes when it looks like coverage it
doesn't have, since a green report that missed the writer-side break is
worse than no check at all, because it gets believed. Two real regressions
were caught by exactly that discipline rather than avoided by design:
`WireCase` — a schema-level property, not a per-resource one — passed the
early per-resource walk silently, since both snapshots being compared
recorded the same column names either way, which is what led to breaks
carrying an optional empty resource path and the snapshot recording the
schema's wire case directly; and a `ReadOnly`/`Immutable` change and
sort-null placement both slipped through the same way before being added
as their own facets. What's bought is that the break a schema edit causes
is legible at edit time, with no server running and no client deployed to
discover it later — including the two kinds of break nothing else can
catch, the clean-migration wire break and the no-migration-at-all break.
The cost is a second diff engine that must not quietly drift from
`migrate.Diff` over the same registries, and a checked-in snapshot format
that becomes a permanent artefact the moment any real team gates CI on it,
the same freeze cost a migration history carries. Revisit if nobody ends
up consuming the snapshot and teams just regenerate past a reported break
anyway, which would collapse the check to a warning printed at codegen
time with no file to maintain; if
gating by default should have been the answer because a real port shipped
a breaking change that scrolled past in a report, weighed against a
greenfield schema that breaks its own contract constantly and would train
people to pass the failure flag reflexively; or if the two diff engines
are ever found to have drifted on a shared kind of change, which would
mean their shared core should be one function both call rather than two
that happen to agree.

### The driver is a dependency

The engine depended on the standard library alone, unwritten and
unjustified anywhere, and the argument for it is genuinely good for a
library that merely extends the standard library: every dependency it
takes is one every consumer inherits. But that's not what sqlb had become
by the time this was written — `rest` already depended on huma by name,
exempted from the dependency check, because sqlb was turning into the
thing an application is *built with*, for a Postgres stack that already
runs pgx. Two structural costs of the `database/sql` bridge weren't
inconveniences, they were designs the driver choice was actively bending:
a vector column's binary codec needs to register on a pgx-specific
connection hook with no `database/sql` spelling, so the vector-index
design had already shipped as a text-form approximation of what it wanted
to be; and a caller holding a `pgx.Tx` and sqlb holding a `*sql.Tx` are
two separate transactions against one pool, since the bridge shares
connections, not transaction handles, and joining a transaction the
application already opened — the exact promise the transaction-scoped
handle makes — was structurally impossible across it. A real port counted
twenty-five call sites depending on exactly that.

Benchmarking narrowed rather than settled the performance argument.
Ordinary CRUD through the bridge ran about 30% slower than hand-written
pgx — real, but not by itself a reason to break a frozen interface, since
it's the honest cost of the case nearly every request is. The cost stopped
being incremental for a wide `float32` vector row, where a text literal
gets parsed element by element in Go: 2.7× the time and 21× the memory,
which is the pgvector case with a number attached and the strongest single
piece of evidence here. And the apparent bulk-insert gap turned out to be
an API gap, not a driver one — sqlb's multi-row `VALUES` path already ran
within about 10% of hand-written pgx, and the whole of the difference
belonged to `CopyFrom`, which the bridge simply can't reach at all. So the
engine now depends on pgx v5 directly and `database/sql` stops being the
contract: `Executor` is redefined over pgx's types, a breaking change to
the single most central public interface, landing before 1.0 specifically
because it could never land cleanly after. One driver, not two — carrying
both would mean two scanners' worth of type-mapping tests forever, a cost
that compounds where a one-time break doesn't. The scanners keep reading
an internal `rowSource` interface, now a test seam rather than a driver
seam, which is what turned the migration into an adapter rather than a
rewrite of `scan` and `mutate`. The closer alternative — an optional
interface sqlb type-asserts for, additive and freeze-preserving — was
rejected because it delivers the capability without the positioning: a
binary vector codec only helps if it's on by default, and supporting two
drivers indefinitely doubles the type-mapping matrix forever rather than
once.

What this buys: transactions an application already opened can now be
joined rather than merely wrapped, `CopyFrom` and batched writes become
reachable, one type-mapping matrix instead of two, and a vector index
built as designed rather than as a text-form compromise. What it costs:
`Executor` breaks for every caller with no path that preserves both old
and new, "importing sqlb costs nothing" stops being true and has to leave
the pitch rather than be softened, and sqlb becomes unusable on any driver
but pgx — a population this project's Postgres-only stance had already
made small, but not zero. The actual port cost less than estimated inside
the engine itself — an afternoon for `Executor` and the handle, since the
`rowSource` seam did exactly the job it was written for — and most of the
real work landed in test harnesses that had to fake a `pgx.Rows` and
`pgx.Tx` instead of a `database/sql` driver, and in examples that still
needed a `*sql.DB` for goose's migration runner over the same pool. Two
real bugs surfaced only once the flip reached a real Postgres, both cases
of pgx handing back exactly what the database sent where the old bridge
had been quietly tidying it: a catalog column read as a one-byte string
instead of empty, misclassifying every ordinary column as generated, and
a rejected write arriving as an empty result set with the real error left
on a separate field, misreported by the scanner as "no columns matched."
Neither was reachable from the database-free suite, which is the argument
for a Postgres-backed test module restated with a concrete example.
Revisit if the modules needing a truly shared transaction turn out to be
a short, containable list, which would make this an expensive answer to a
narrow problem; if pgvector support stops mattering because those tables
stay permanently outside the registry, leaving only the transaction
argument; or if a consumer arrives that isn't pgx-native for a real
platform reason rather than a preference — thin ground today, since every
application this library targets already runs pgx and `lib/pq` points its
own users at pgx now.

### Computed fields

sqlb's model *is* the row, so a value the row doesn't store had no slot at
all — one derived field pushed an entire entity off the generated path,
and an adoption review measured the cost as a 416-line hand-written view
plus a hand-written TypeScript type on top, ranking the gap first of six.
The fields a real application actually needed didn't stratify into one
feature: some were trigger-maintained real columns needing nothing beyond
`ReadOnly`; some were aggregates needing a correlated subquery; some were
row-local expressions; and one — "is this row starred by the viewer" —
depended on who was asking, which a static SQL string can't express at
all. So a computed field is a `*ColumnInfo` carrying an expression instead
of only a name, substituted wherever that column is rendered — `WHERE`,
`ORDER BY`, `?select`, the projection — so all four follow from one
change to the column-resolution path rather than four separate ones, and
the projection's `(expr) AS name` needs no new scan logic since name
matching already handles it.

Three tiers shipped. A row-local expression may be `Filterable()` and
`Sortable()` directly, since it's exactly a predicate the compiler already
knows how to emit. A correlated-subquery expression is projection-only
unless `Filterable()` is written explicitly, because a subquery in `WHERE`
runs once per row and the declaration is the author's acknowledgement of
that cost. A parameterised expression takes `Needs(key)` in the same shape
as a tenant-scoped table's obligation: the declaration supplies no value,
and mounting refuses to start until a `BeforeQuery` hook supplies the
bind — the same startup-time failure mode as an unscoped multi-tenant
table, chosen because an *unbound* parameterised expression renders as a
predicate that's always false and looks exactly like a working feature. A
fourth tier — a value computed in Go rather than SQL, requiring its own
hook — was designed and deliberately never built: the applications that
motivated this record expressed every real field in SQL once the
correlated and parameterised tiers existed, and a schema round-trip that
reads a database back into a declaration has no way to represent Go code
at all, so a tier the round trip can't express is a tier nothing could
adopt into. It stays documented as a considered-and-cut option rather
than silently dropped, since a taxonomy that quietly loses a row stops
being a record of what was actually weighed. `Sortable()` is refused on a
volatile expression, since keyset pagination breaks ties on the sort
column and an expression reading the current time isn't stable across
pages; `Needs` with no hook behind it refuses at mount for the same reason
`Scoped` does. No DDL is emitted in either direction — a computed field
never reaches `migrate` and a schema diff never sees one — and Postgres's
own `GENERATED ALWAYS AS … STORED` was set aside as a separate decision
rather than folded in here, since it requires `IMMUTABLE` and so can't
express the current-time-dependent fields that motivated this in the
first place.

Three corrections came from building it against real data rather than
from the design. Nullability had to invert: a computed column defaults to
nullable unless declared `NotNull()`, the opposite of a stored column's
default, because an unmatched correlated subquery is `NULL` by
construction and inferring non-null from the expression is wrong in the
unsafe direction whenever it's incomplete — the failure otherwise surfaces
as a scan panic on real data neither the migration diff nor generation
had any opinion about, since a computed column has no DDL for either gate
to check. Reads and writes both had to become opt-in per caller rather
than always-on: a correlated subquery evaluated on every read whether or
not a client asked for it, and worse, on every `INSERT`, `UPDATE` and
`DELETE` too, meant a per-write tax nobody asked for, a create whose
returned aggregate counted rows the same transaction hadn't committed yet
(always wrong, requiring the extra read this feature exists to avoid),
and a subquery naming another module's table riding into every insert of
the table that declared it. So reads and `RETURNING` both narrow to
exactly the computed columns a caller opts into, defaulting to none, and
an unrequested computed column is simply absent from the response rather
than serialised at its Go zero value — a definite `false` for "unknown"
being precisely the silent-failure shape this whole feature exists to
prevent. And a gap the design never named: `FromSQL` accepts a subquery
naming another module's table with no refusal at all, unlike
`ExternalRef`'s explicit one, because nothing parses the SQL string to
check — checking it would require exactly the cross-module dependency
`ExternalRef`'s free-text target exists to avoid. That's not a defect so
much as an undocumented asymmetry with a real consequence: a joined query
lives behind one handler, where a computed column sits in the projection
surface of every mount and the `RETURNING` of every write that opts into
it, so naming a foreign module's table there couples every statement
against the table to that module's presence — worth stating plainly next
to `ExternalRef` rather than discovering it from a failed isolation test.

What's bought is one declaration reaching the row type, JSON, both
generated clients and the CLI at once, with capabilities working
unchanged since a computed field is still an ordinary `ColumnInfo` for
every purpose but rendering. What it costs is real: `FromSQL` is raw,
unvalidated SQL sitting in the schema, only checked by actually running a
query; a correlated subquery makes a list's cost a function of how many
computed columns a request opts into; and `Needs` adds a third kind of
obligation the mount check has to track. Adding it is additive and safe —
a schema declaring no computed column compiles to the same SQL as before —
while removing a *filterable* tier is expensive once shipped, since a
filter expression a client can send became part of the REST contract this
project already freezes; that asymmetry is the argument for shipping
projection before filterability and holding a tier's `Filterable()` until
an application asks for it by name. Revisit if the correlated-subquery
tier is never filtered in practice, which would make its `Filterable()`
opt-in dead weight; if a second real application's fields don't fit this
same four-tier stratification, meaning the taxonomy itself is wrong rather
than incomplete; or if `Needs` starts carrying general request state
rather than a narrow per-column bind, at which point it has outgrown this
decision and needs its own.

### The exit is generated

The sharpest objection an adoption review raised against sqlb wasn't a
missing feature: sqlc and chi are cheap to reverse because they own almost
nothing — sqlc's output is Go functions you can stop calling, chi is a
mux — while sqlb owns the schema, the migrations, the wire format, the
clients and the CLI, and reversing that is not a day-4 change. Optional
codegen already makes the *runtime* half reversible — `Describe` over
structs you already own, a two-method `Executor`, stop calling the builder
without touching a model — but that leaves exactly the half the review
named unaddressed: the schema DSL owning migrations, and a generated wire
format two clients have already shipped against. A library with no
consumers yet can't answer a concentration-risk objection with a promise,
only with a working artefact — the way out, generated from the same
declaration as the way in, kept working by the same CI that keeps
everything else working.

So `sqlb eject` writes a package depending on pgx and the standard library
alone — DDL, row structs with the `sqlb` tags stripped, one function per
statement with the SQL spelled out, a small shared file of request
parsing and WHERE assembly, and plain `net/http` handlers — and deleting
sqlb from `go.mod` afterward is a supported end state, not a hack. The
fidelity line is drawn between the surface and the engine, not between
"important" and "unimportant": CRUD and list at the same paths and status
codes, the full one-fragment-per-operator set (`eq`, `ne`, `lt`, `lte`,
`gt`, `gte`, `in`, `nin`, `isnull`, `notnull`, `between`, `like`, `ilike`,
`contains`, `startswith`, `endswith`), sorting, search, paging and the RFC
9457 error shape with its allow-lists all come out whole. What doesn't —
keyset cursors, `?select`, `?expand`, the JSON filter tree, array and
document operators — is refused by name with a 400 rather than silently
dropped, because reproducing any of those in the exit would mean emitting
a copy of sqlb's own engine: not an exit, a fork under a different import
path. Two properties survive the loss of the machinery that implemented
them, on purpose: opt-in capabilities stay opt-in, so a column that never
declared `Filterable` still can't be filtered and a `Hidden` column still
has no spelling to probe; and the mount-time obligation for a scoped or
soft-deleting table stays compulsory, expressed as a required Go function
instead of a hook registration — the same seam with the generic machinery
removed, not a weaker version of it.

The load-bearing half is that the exit is tested against the thing it
left, not merely generated and trusted: a Postgres-backed test stands the
ejected package up beside the generated resource it came from, points
both at one database, sends both the same requests, and compares response
bodies byte for byte, with the two known intentional differences
subtracted explicitly rather than tolerated by a loose comparison. That
check runs as its own verb, deliberately not folded into the drift check
that gates ordinary generated code — generated code is stale when it
disagrees with the schema and there's exactly one right answer, but an
ejected package is *meant* to be edited, so the day a project takes the
exit is the day it deletes that gate rather than satisfies it, and keeping
the two checks as separate verbs keeps that distinction visible in CI
rather than blurred into one. What this buys is a concentration objection
answered with a demonstrably working server rather than a plan, plus a
free byproduct: the comparison test makes the generated resource's wire
independence checkable in a way this project's own compatibility promise
otherwise couldn't be tested at all. What it costs is a second emitter
that has to track the first — an operator added to the filter grammar and
not to the exit is a gap the comparison test will catch, and one added to
both is work done twice — and a fidelity boundary that has to stay
honestly documented, since the moment the "what's not out" list is wrong,
the feature is worse than not having it. Eject is a door, not a supported
dual-run mode: nothing keeps an ejected package and the generated
resources in step afterward, and nothing should, since the emitted code
is meant to be read and edited rather than regenerated. Revisit if nobody
ever actually runs it, which would make it a claim rather than a tool
still worth keeping for the objection it answers, but not worth its
current CI cost; if the very things refused as "engine" — cursors,
`?expand` — turn out to be exactly what a real eject immediately
hand-writes back in, meaning the fidelity line was drawn in the wrong
place; or if someone asks for the shared support file as a standalone
package, which is the design being rejected here and would deserve
reconsidering if asked for twice.

### Declared actions

The generated surface stops exactly where applications spend their code:
an adoption review measured a real application's domain verbs —
`POST /{id}/<verb>` routes like completing a task — at 780 lines against
464 for all of CRUD combined, because every verb opens the same way
before any actual domain logic runs: parse the id, fetch the row under
the tenant predicate, 404, decode an optional body, all written four
times over across the handler, the OpenAPI document, and two generated
clients. It's also the feature most likely to be the reason someone
leaves, because domain verbs are where logic is most idiosyncratic — the
place a DSL's expressiveness runs out first and most visibly — so a
feature that tried to *express* the transition itself, not just its
envelope, would get fought, worked around, and eventually ejected, taking
the parts that were working with it. The seam this needs already exists
and is already used: `BeforeCreate` is a plain Go function the generated
path calls, and an action is that arrangement moved one level out — the
framework owns the request, the fetch, the transaction and the response,
and calls a function the application wrote for the transition itself.

Three constraints from Go and this project's own rules forced the actual
shape more than the original proposal anticipated. A generic `.Body[T]()`
method isn't legal Go, so the question of whether an action's body comes
from a reflected application type or a declared one was never optional —
and it's declared, in the same field vocabulary a table's columns use,
because the value of this feature is reaching the client emitters, and a
body sqlb can't see produces a TypeScript method typed `unknown`, which is
exactly the drift this feature exists to close. A domain function can't
live in the schema declaration itself, since the schema is a value five
emitters read and `sqlb.json` serialises, and a function is neither
readable nor serialisable — so an action declares its envelope on the
table (name, an optional declared body, the columns it's allowed to
write) and binds its verb separately, at registration: codegen emits a
`Register(api, db, actions Actions) error` with one struct field per
action, so the compiler demands the exact function signature and an
action added to the schema is a build error at the call site rather than
a route answering 501 — though it can't demand the field be non-nil, so a
nil action refuses at mount instead, the same compiler-then-startup shape
already used for a table's scoping obligation. And the request path may
not import the schema package, so the action declaration crosses that
boundary as data the way exposure already does, never as code.

The generated envelope does everything an application wrote by hand
before: parse the id, fetch the row through the query hooks with a
row lock when the action declares writes — not optional, since a
read-modify-write across a network round trip is the classic lost update
this removes by construction rather than adds as a convenience — 404 on
no match (which, on a scoped table, is also the correct answer for a row
belonging to someone else), decode the declared body, call the
application's function *inside* the same transaction the envelope opened
so it can reach other tables through the transaction-scoped handle and
trigger its own `AfterCommit`, persist exactly the declared write columns
from the mutated row, then respond with it. `Writes` is enforced rather
than merely documented — the envelope persists those columns and no
others, so a verb that tries to mutate an undeclared column finds it
silently unwritten, a bug the first test catches rather than an
undocumented widening of what a route touches. A verb declaring an error
returns a typed problem answered with its own status — refusing "cannot
complete an archived task" is one line, deliberately the whole of what
this feature offers for preconditions, since a DSL that could express
*when* a transition is legal would be expressing the transition itself,
exactly the failure mode this design exists to avoid. A collection action
— no `{id}` in the path — gets none of this generated fetch, and
therefore none of the scoping safety a row-addressed action inherits from
the query hooks; it's plain Go with a transaction, the same position a
hand-written query already occupies, and roughly two in five of a real
application's verbs turned out to be exactly this shape — worth stating
plainly, since the safety argument doesn't reach the whole feature. A
later addition, `Touches`, names tables an action's escape hatch writes to
beside the declared column list, purely as documentation with no
enforcement behind it — added once it became clear that `--help`, the
OpenAPI document and the impact report all reported `Writes` as if it
were complete, inviting exactly the wrong inference that a verb touching
ten tables through the transaction was confined to the one row it fetched.

What's bought is route coverage rising from roughly 40% to roughly 90% of
a real application's endpoints, with the part that matters more being
that verbs now reach the generated clients and OpenAPI document, which is
where the drift was actually measured living; and the scoped, locked fetch
stops being remembered by hand on the majority of routes that CRUD alone
never covered. What it costs: the schema gains a second kind of
non-column thing after computed fields, a declared body wanting a shape
the column vocabulary can't hold is a real and expected future pressure,
and a verb's function runs inside the transaction the envelope opened, so
a slow third-party call inside it holds a connection under transaction
pooling — the answer is the same `AfterCommit` escape every write hook
already has, but it needs saying before someone discovers it the hard
way. Adding an action is additive — a schema declaring none generates
exactly what it generated before — but removing one isn't, once it's a
wire-format promise a deployed client is calling by name, the same
asymmetry every REST-facing capability in this project carries. Revisit
when a third real application declares actions, or on the first one whose
body genuinely can't be expressed in the column vocabulary, whichever
comes first — those are the two events actually capable of telling
whether the declaration's shape is right.

That revisit trigger fired on 2026-08-14: a quiz-grading action
(`Lesson.submit`, evidence from a real port, tracked by
[#196](https://github.com/jryannel/sqlb/issues/196)) grades a
`QuizAttempt` and wants to answer `{passed, score}` in the same round
trip, and it can't — `do` always returns the mutated row, never a
computed report beside it. The door opens, but only as far as the write
side needs it: a `GET` action is still declined, since the caching and
query-key questions a read RPC surface would raise are real and this
record still isn't answering them. A `POST`'s response outgrowing the row
is a smaller thing — the request was already a write, already uncacheable,
and the envelope already has the transaction open and the mutated row in
hand at the moment it marshals the answer, so "let the write's answer say
more than the row" is a report attached to a verb, not a new RPC surface.
The shape mirrors the read side's own widening: `do` becomes
`func(ctx, *T, In) (Out, error)` in place of `func(ctx, *T, In) error`,
with `Out` defaulting to `row[T]` so every action declared today keeps
compiling and keeps answering exactly what it answers now. Still
unsettled and left for the second and third action that actually reach
for it: whether `Out` needs a declaration in the field vocabulary the way
a body does, or stays outside the schema's reach at the cost of the
TypeScript client typing it `unknown`; whether `Writes` still persists
from the mutated `*T` when `do` also returns a separate `Out`, or the two
become one value; and whether a widened action is still one surface next
to CRUD or the first crack in that boundary.
[#218](https://github.com/jryannel/sqlb/issues/218) tracks the
implementation.

### The container is an adapter

`example/fxapp` answers the first question anyone building on a
dependency-injection container like uber-go/fx asks — where do the sqlb
pieces go when a container assembles the application — with roughly four
hundred lines of glue every fx adopter would otherwise rewrite: a pool and
migration runner assembled from a value group, a hook registry assembled
the same way, and an HTTP layer wiring a router, a Huma API and
middleware. The first answer to what to do with that glue was to publish
it as its own module, `sqlb/sqlbfx`, reasoning that an unowned de-facto
contract (a value-group name a third-party module would have to guess),
a boot-refusal guarantee that only reached as far as the example copied
it correctly, and a per-application auth convention for "who is calling"
all argued for a stable import path. That was reversed one day later, not
because the three observations were wrong but because they weren't one
problem: only one of them — the seam between "who is calling" and "what
confines the query" — had a real second consumer, since a hand-written
version of exactly that convention already existed elsewhere in this
repository's own examples, predating the kit. The other two were solving
for a hypothetical third-party author who didn't exist yet.

So the assembly and the seam split into different treatments. The
assembly — `example/fxapp/fxkit` — stays a package of the example: copy
it, adapt it, own it, with no import path anyone outside this repository
should use, because every line of it is a load-bearing opinion (chi,
humachi, goose, `slog`) and opinions that specific are the wrong thing to
put behind an import path a consumer can only take whole or refuse
outright. An application on a different router or migration runner
doesn't want a smaller version of this kit, it wants this same file with
four lines changed — which publishing would prevent by converting an
adaptable reference into a take-it-or-leave-it dependency, at the cost of
a second `go.mod`, a second release tag, and a compatibility surface with
no one yet depending on it. What a copy loses is enforcement: the boot
refusal, the deterministic migration ordering and the explicit middleware
order are written down as obligations in the kit's own doc comment and
proven by a test a copier is expected to take along, rather than
guaranteed by an import a compiler checks — weaker, and accepted as the
right instrument for as long as the number of people writing their own
shareable fx module stays at zero. The principal seam, by contrast, moved
the opposite direction, into the engine itself as `sqlb.WithPrincipal` and
`sqlb.PrincipalFrom[T]` — a stdlib-only context contract where middleware
resolves credentials and scoping hooks read them back by type, with
neither end naming the other. That's the half originally published in the
wrong place: it already had a real second consumer, and a seam is small,
opinion-free, and spelled directly by hooks that nobody regenerates, which
is exactly why moving it later would be expensive and having exactly one
of it is worth real cost to guarantee now. That split is the general rule
this settled into: publish the seams, copy the assembly.

A later trigger this record wrote for itself fired and, notably, didn't
force the reversal back: a real multi-module adoption reported dozens of
hand-written, near-identical migration-set providers and operation-set
literals — exactly the volume that should make codegen want to generate
the assembly. The predicted consequence — that generating against a
container needs a published, stable import path to generate against —
turned out false, because the proposed emitter shape names the *host's*
own types as fully-qualified strings in `codegen.Options` rather than
importing anything of the consumer's, so sqlb compiles against nothing
container-specific and a wrong name is simply a compile error in the
generated file. Generating for a container this way strengthens "publish
the seams, copy the assembly" rather than reopening it — the emitter
generates a fresh copy of the assembly for each consumer instead of
importing one shared instance. Revisit if a second author actually writes
a shareable fx-integration module meant to drop into someone else's
module list, which is the buyer the published contract never had the
first time; if two independent copies of the assembly drift into the same
bug, which would mean the obligations belonged in a type rather than in
prose; or if a non-fx consumer wants more than just the principal seam
from the engine, which would mean the seam/assembly line itself was drawn
in the wrong place.

### The stream is a seam

The [change feed outbox](#change-feed-outbox) decided durability — a row
written in the same transaction as the change, a dispatcher woken by
`LISTEN/NOTIFY` — and left everything downstream of it unbuilt: an
endpoint, a wire format, a reconnection story, and a decision about what a
subscriber is actually sent. An adoption review looking for what sqlb
offered a real application's live-update endpoint found nothing and
recommended building on infrastructure that already existed rather than
waiting. What changed the calculus was that `AfterCommit` had since
shipped and generated writes started wrapping themselves in a
transaction, which made the correct moment to publish a change — after
the commit, never inside it — reachable from every write sqlb issues,
without needing the outbox's durability to get there first. So the stream
splits into a transport with a `Source` behind it: `rest.Events` mounts
the endpoint and owns every HTTP concern — `Last-Event-ID`, heartbeats,
the retry hint, a per-subscriber filter — while knowing nothing about
where a delivery actually comes from, so the outbox dispatcher can later
implement `Source` and replace the interim implementation with the
endpoint, wire format and every client left untouched.

The first `Source` is deliberately in-process — `rest.Broker` fans out
only to subscribers connected to the same process, is at-most-once, and
is single-replica — and says so at the top of its own doc comment rather
than in a changelog, because the failure it produces (a client that never
learns a row changed and shows stale data forever) is invisible from the
outside and won't surface in a test the way it will in a two-replica
deployment behind a load balancer. A subscriber receives an invalidation —
`{table, key, op}` — never a row, the same choice the outbox decision
made, and building it sharpened why: a payload would have to be produced
per subscriber under that subscriber's own scope or the resource's query
hooks would never run on it, and a change feed that skips the tenant scope
hands one tenant's rows to another. Sending only the address of a change
keeps the ordinary read path the one and only thing that ever reads,
which is what lets every rule that path enforces keep holding. The tenant
value itself — read off whatever column the model declared as its scoping
column — travels only as far as the filter hook that decides whether an
event is this subscriber's; it's deliberately never serialised onto the
wire, since a subscriber already knows its own tenant id and putting it on
the wire would only enlarge a contract that's expensive to change later
for no benefit. Every failure converts into a disconnect rather than a
silent drop: a subscriber that falls behind its buffer is disconnected,
not skipped; a reconnection whose `Last-Event-ID` predates the retained
history gets an explicit reset event rather than silence; an unparseable
`Last-Event-ID` gets the same reset rather than being read as a fresh
connection. The rule behind all three is that a dropped event is a client
wrong forever, while a dropped connection is a client that reconnects and
converges — so when in doubt, the design drops the connection. Writes
reach the feed through the same hook mechanism as tenant scoping, not
through the REST handlers directly, because wiring the handlers would
leave the feed silent for exactly the writes most likely to matter — a
background job, a migration, an admin script — the identical argument for
why query scoping lives in a hook rather than in each handler.

This buys a live view working today, on one replica, with zero schema
change and zero new infrastructure, and it settles the client contract —
the endpoint, the two event types, `Last-Event-ID` behaviour — against a
running implementation now, so the outbox lands later as a source swap
rather than a client migration; when it did land, the contract held
exactly as hoped, with the dispatcher slotting in as a second `Source`
implementation and only one addition needed (an optional interface a
durable publisher can implement to record inside the writing transaction
while the in-process broker keeps announcing after it). The cost that no
amount of documentation fully retires is a feature correct on one replica
and quietly wrong on two — what documentation can do is put that limit
where a reader meets it first, which is where it sits. What's explicitly
not addressed: cross-table ordering, authorization beyond whatever the
filter hook chooses to check, WebSocket, and any delivery guarantee at
all while running on the in-process broker. Cost of change is sharply
split: swapping the source is one constructor call, cheap by construction
and the entire point of the seam, while the wire is deliberately the
smallest thing that works — three fields, two event types — because
changing its shape means a new version of the endpoint, not an edit. That
last argument rested partly on hand-written frontend code no generator
would ever update; the clients now emit the subscriber
([The subscriber is generated, the policy is not](#the-subscriber-is-generated-the-policy-is-not)),
which moves who writes the decode without moving the conclusion — a client
is emitted into the repository that consumes it, so a wire change reaches
a deployed one only when somebody regenerates. Revisit if a third source can't make its positions dense
— a per-partition Kafka offset or NATS sequence number would have to
either fake a position or reset on every reconnect, which the outbox's
own dense, commit-ordered ids sidestepped but a partitioned source can't;
if `Filter` turns out to be where authorization actually lives rather
than a hook a few applications happen to set, which would argue for
making it a declared, mount-time-checked obligation the way tenant
scoping already is; or if disconnect-on-overflow shows up as reconnect
storms under a real write burst, answerable by a larger buffer or by
collapsing queued invalidations per table before the buffer fills, sound
specifically because two invalidations for the same table really are one.

### A negation is sqls

Every negation in the filter surface — `neq`, `nin`, the containment
complements `nhas`/`nhasany`/`nhasall`/`nhasdoc`, and the JSON tree's group
`not` — uses SQL's three-valued logic: a row that is NULL in the column under
test matches neither an operator nor its complement. That reads as a bug to
anyone expecting set semantics, where the complement of a set is everything
else, and the fix keeps getting proposed as `IS NOT TRUE` on the group form
alone. That fix is rejected structurally, not on taste: a leaf complement's
negation lives inside one token (`nhas` cannot be decorated), so `IS NOT
TRUE` could only ever apply to the group spelling, leaving
`?labels=nhas.urgent` and `?not=(labels.has.urgent)` — the same logical
filter — returning different rows depending on which of two equivalent
spellings a caller happened to write. That disagreement has no
documentation home; it belongs to neither the operator, the column, nor the
group, only to which spelling was picked, which is strictly worse than the
surprise it set out to fix.

Set semantics everywhere — `neq` as `IS DISTINCT FROM`, `nin` as
`NOT IN (…) OR col IS NULL`, and so on — would dissolve the dilemma
entirely and is arguably the better filter language. It is foreclosed
anyway: `compatibility.md` freezes the filter grammar as a wire format on
the promise that existing spellings never change meaning, and a change to
what `neq` returns is invisible to a deployed client, to an agent replaying
`sqlb.json`, and to `restcompat`'s capability diff alike — no parameter is
removed, no request is rejected, so nothing can detect or migrate the
break. So negation stays SQL's negation everywhere, documented at each
place a caller meets it, with the escape hatch spelled out beside it —
`?or=(labels.nhas.urgent,labels.isnull)`. The one asymmetry in the shipped
behavior, `OneOf` translating a nil member into `IN (…) OR col IS NULL`
while `NotOneOf` does not mirror it, is deliberate: that translation
repairs a filter that could never match anything, which is a different act
from choosing which rows a well-formed negation returns. The door left open
is a second, additive vocabulary — null-inclusive operators landing
alongside the existing ones rather than redefining them — provided every
new spelling lands in both the URL grammar and the JSON tree at once, so
the same seam doesn't reopen one level up. Revisit if the escape hatch
turns out to be unspellable in some position, most likely inside a nested
negated group — that would be a correctness argument, and it would outrank
the compatibility freeze.

### No default hook registry

Hooks used to have ambient state at their centre: `sqlb.On[T]()` registered
into a package-level default registry, `sqlb.New(exec)` handed every handle
that same registry, and the form that named where the rules land —
`OnIn[T](r)` — carried the longer name, a recommendation written backwards.
That surfaced as a real incident: an adopter migrating from the default
registry to a per-application one left one module still calling
`sqlb.On[T]()`, whose rules then landed on a registry no handle it queried
through ever carried. It compiled, it mounted, it answered — with every
tenant's rows — and nothing in the API could have caught it, since both
spellings were valid and the shorter, more obvious one was the wrong one.
The examples had already voted with their feet: both worked applications in
this repository built their own registry rather than use the default,
because two servers in one process otherwise stack each other's predicates.

The default registry is deleted. `New(exec)` gives each handle an empty
`Registry` of its own; `On[T](r)` is the only registration form, and the
short name now takes the registry argument rather than omitting it. A bare
`Executor` that is not a `*sqlb.DB` — a raw pool, a borrowed `pgx.Tx` — has
no rules at all, which is honest for a handle-less statement and visible at
the call site: the alternative is a handle, and a handle carries its rules.
Two applications sharing a process are now independent by construction
instead of by care. This was rejected in gentler forms first: inverting only
the names while keeping a default (`On[T](r)` alongside `OnDefault[T]()`)
still leaves ambient state a handle can acquire without asking; a shim that
panics on second use trades a compile-time break for a production panic;
documenting the hazard harder was tried already, in compatibility.md, and
the boundary still switched off silently under an author who had read that
page. The migration broke every existing registration call site in one
edit, deliberately, with no deprecation window, because a shim would be the
same ambient registry wearing a different name.

One narrower failure survives: `On[T](reg)` still compiles whether or not
any handle ever carries `reg`, so hooks can be registered into a registry
nothing attaches. But it is local rather than action-at-a-distance — the
registry and the handle are usually a few lines apart — and a model
declaring `Scoped` is refused at mount if the handle's registry has no hook
for it, which closes the specific case this record was written about.
Revisit if naming a registry turns out to cost more than one line at
startup in real adoption, which would suggest the seam is in the wrong
place, or if bare-executor statements are found to have depended on
inheriting global rules — the fix then is making a bare executor refuse a
`Scoped` model outright, not restoring the default.

### Auto incrementing keys

Neither `BIGSERIAL` nor `GENERATED … AS IDENTITY` could be declared, because
`introspect` correctly refused to import a serial as an ordinary column
whose default happens to name a sequence — that produces a diff proposing
to drop the default outright, since nothing renders the `CREATE SEQUENCE`
behind it. Across a real multi-application platform this made exactly one
kind of table undescribable: an append-only log using the serial as a
tiebreak so `ORDER BY occurred_at DESC, id DESC` stays a total order, with
an index built over that same column. "Use a UUID instead" isn't a
substitution here — a monotonic counter is the thing being asked for, and
widening the densest key Postgres offers to sixteen bytes on the
highest-volume tables in the system, just to satisfy the declaration
language, is the same trade `SmallInt` and `PrimaryKeyColumns` existed to
retire.

Auto-ness is declared as a property of a column — `Auto`, alongside `Array`
and `Size` — rather than as a distinct `Type`. A `bigserial` column *is* a
`bigint`: that's what the catalog reports and what an `ALTER COLUMN TYPE`
has to name, so a separate type constant would have split the filter
grammar and sort machinery the same way array types were refused a type of
their own, and would have turned a column that stops auto-incrementing into
a table rewrite instead of what it actually is, a default going away. Both
Postgres spellings are declarable, and the older one — serial plus a named
sequence — is not deprecated in favor of the newer, cheaper-to-own identity
column, because the entire demand for this feature came from databases that
already have serials; a DSL that could declare only the modern spelling
would propose rewriting those columns on its first diff. Everything
downstream asks one question, not "is this a sequence" — `DatabaseSupplied`
folds a plain default, a serial and an identity together, which is what
lets the existing `default` struct tag make the runtime work unchanged: the
engine already omits a zero value on such a column, so the write path never
has to learn what a sequence is. A `GENERATED ALWAYS` column marks itself
read-only at declaration, the same move made for computed columns, so an
insert naming it is rejected once rather than every write path having to
know the feature exists.

Two things are deliberately left as named hazards rather than automated
away. The sequence's name is derived (`<table>_<column>_seq`) and not
recorded, so a database whose sequence is actually named something else
round-trips correctly but a database rebuilt from a diff gets the
conventional name — a small, one-directional lossy edge, accepted because
nothing compares the name so pinning it would add a field nothing reads.
And when a column *becomes* auto-incrementing on a populated table, the
migration names the `setval` or `RESTART WITH` that has to run first but
does not generate it, because the row count isn't in the schema and a
generated `setval` scanning the column's own max would be a full-table scan
written into a migration by something that can't see the table's size —
getting it wrong is a duplicate key on the next insert, which is loud
enough that it should stay a person's decision rather than a quiet guess.
Revisit if a project's sequence name turns out to be load-bearing —
referenced by `nextval()` in application SQL or by a grant — which would
earn `Auto` a name field the way a check constraint earned `CheckName`.

### The skill is generated

An agent briefing itself off sqlb's docs and ADRs has a corpus, not a
briefing, and the fact it most needs — which tables exist, which columns
declared which capability, which resources are mounted where — is not
knowable from any static file, because the schema *is* the surface and
differs in every repository. That rules out the ecosystem's usual answer
(Supabase ships two static skills that work because Supabase's Postgres is
the same in every project) and argues instead for treating an agent skill
like every other derived artefact: `sqlb generate` emits a project-specific
`SKILL.md` from the same declaration everything else reads, and
`generate-check` gates it the way it gates the TypeScript client or the
manifest. Being gated is the load-bearing half — a skill that has drifted
from the schema is worse than none, since it is confidently wrong about the
one thing it exists to know, and generating it is the only version of the
idea that structurally cannot drift. Two things stay hand-maintained rather
than generated, precisely because they are not per-project facts: the
adoption procedure (a census run in the wrong order is the failure mode,
not a wrong ratio) and the query boundary where the builder degrades and a
model's instinct is to keep pushing rather than drop to `Raw` — nothing
compiles that judgment call away, so it's written down because there's no
check to prefer. A third hand-maintained skill, covering the DSL's general
vocabulary (does `Col[T]` have `Lt`, what does `Scoped` enforce), joined
later on the same reasoning: that vocabulary changes at release cadence,
not per-project, so it belongs with the other static skills rather than as
a second generator reading a registry for facts no single project's schema
would exercise.

The emitter carries structure — names, types, capability flags, paths —
and deliberately not free text, because `introspect` pulls column comments
off a live database, and an adopted database is not first-party source; a
comment written by someone outside the project would otherwise become an
instruction sitting in an agent's context. This is the one place the skill
emitter differs from every other one, since DDL and OpenAPI are read as
data and a skill is read as instructions. A test injects an
instruction-shaped string as both a table and a column comment and asserts
it never appears while the column is still described — proven the way a
guard has to be, by first carrying the comment through on purpose and
watching the test catch it.

Measurement narrowed the claim this decision was built on rather than
confirming it: across several rounds of agent A/B testing, a generated
skill produced no measurable accuracy improvement over an agent reading the
schema declaration directly — the "an agent gets this wrong" premise did
not reproduce against a capable model given the source. What survived is a
cost effect, consistently: roughly 3× fewer tool calls and 2-6× less wall
clock, which is a smaller and more honest justification than the one this
record started with, and the reason the emitter is kept is that it's cheap,
gated, and buys latency — not that it prevents mistakes. One gated-but-wrong
failure did surface in real adoption: a camelCase registry's skill told an
agent that column names were the wire's JSON field names, which was false
under that project's `WireCase`, and the gate stayed green because the
skill faithfully rendered a manifest that was itself wrong — proof that
being gated shows the file matches the schema, not that it's correct about
it. Revisit if the latency benefit stops mattering (the emitter should go),
if the generated content collapses toward "read the manifest" (point at the
manifest instead of restating it in prose), or if free text turns out to be
what actually changes an agent's output, which would mean the trust
boundary has to be paid for rather than sidestepped by omission.

### Reachability is a property of the mount

A table has one model and one `Expose`, which is the right shape for a
table served one way — nearly all of them — and the wrong shape for a
table served two: a storefront and an admin panel over the same
`products` row, where the admin surface exists precisely to serve columns
(`cost_price_minor`, `supplier`, `internal_notes`) the storefront must
never see. `Hidden` can't make that split, because it hides a column from
every reader of the model, and there's only one model; `Expose` can't add
a second resource either, since the schema only carries one REST
declaration per table. The fix reuses a mechanism that already existed for
a different reason — `rest.Options.Computed` narrows per-resource column
reachability for query cost — and generalizes it: `rest.Options.Columns`
says which columns a given mount serves, at every layer a column touches —
response, `SELECT`, filter, sort, search, `?select`, write body, and the
allow-list a rejection offers back — so what a request may name and what
the database is asked for can't disagree. Empty means every column, which
is what a generated resource emits and every existing mount already
relies on.

This keeps `Expose` singular and the emitters at one resource per table;
the narrowed, privileged mount is a hand-written `rest.Resource` call over
the same generated model, keeping the typed facade, the manifest and the
drift gate for the wide half while paying only for the one hand-written
mount. The honest cost is that narrowing lives at the mount rather than in
the schema: a model with no field for `cost_price_minor` has no code path
that could ever return it, while a model that has the field and a mount
that declines it is one `Options` value away from serving it — a real,
weaker guarantee than a schema-side split would give, and the one the
report that prompted this asked not to be true. Two smaller costs follow
the same shape: the OpenAPI response schema is the model's Go type,
registered once and shared by every mount, so a narrowed resource's
document still lists columns its runtime responses omit; and reused create
or update body types document fields a narrowed mount will not actually
write.

The trigger to watch for was named directly rather than left implicit —
whether a second surface needs its own generated clients, at which point
the schema should carry a `Columns` allowlist per declared `Expose`
appended rather than replacing, with each surface on its own drift gate —
and it has since been observed, just not inside this codebase: an external
Huma-based application (no sqlb involved) hit the same consumer/superadmin
split and didn't stop at narrowing a mount, building a second independent
`huma.API` with its own document and client-generation surface entirely.
That isn't evidence `Options.Columns` failed anywhere it's actually been
used — it's evidence that the shape of the need, when it gets big enough,
is two whole surfaces rather than one narrowed one, which is closer to two
schemas sharing tables than to one schema with a mount-time restriction,
and is the shape a future `Expose`-appending design would have to take.

### A gap in the declaration is reported

Below the schema sit three layers that can each say something the
declaration cannot — the mount, the migration, the caller — and that's by
design; not everything belongs in a declaration. Five reports in two weeks
shared a shape that had nothing to do with a missing field: a
`rest.Options` narrowing that no `Expose` could express, a scan bound with
no field on `schema.REST`, a `BeforeQuery` hook scoping a read that
`Builder.SQL` rendered unscoped anyway, a hand-deferred constraint that
passed `sqlb check` clean because the declaration and the database were
blind to the same property, and — the one case that *was* loud — a
predicate against a hidden column caught by the compiler. Each individually
had a workaround the reporter already knew about; what was missing in the
other four was that nothing said the gap existed. A drift check reporting
"no difference" is a claim, and a check that cannot see a property reports
no difference about it whether or not one is there — which is the same
failure named elsewhere as silent dropping, one layer further down: a check
that can't see a property isn't a weak check, it's a check answering a
question it was never asked.

So where a layer below the declaration can express something the
declaration can't, the gap gets closed if that's cheap, and made visible
if it isn't — never left inferable from a workaround documented somewhere
else. Visibility has an order of preference: a refusal at the boundary
first, where the declaration gains the word and something fails when it's
absent or wrong (a scan bound moved onto `schema.REST`, a negative ceiling
rejected outright); failing that, a report from the tool that reads the
database, naming what it saw and cannot declare (`introspect` reads
deferrability on every constraint and lists what the DSL can't express as
a `Skip`, with the definition attached, rather than staying silent about
a property neither side models); failing that, a sentence at the point
where the reader is standing, not a pointer four screens away. What all
three rule out is the same failure that produced the five reports: correct
behavior, an existing workaround, and the two facts living in different
files. The corollary for the schema itself is narrower than "declare
everything" — a declaration that cannot say a thing is fine; a *tool*
whose job is to report differences and cannot see the thing is not.

This makes previously-green checks red wherever they were quietly blind —
adopting a database that defers a foreign key now produces report entries
where it produced none, which is the change working, not a regression —
and it does not reverse the decisions that chose the weaker, mount-time or
hand-written answer in the first place, like reachability living at the
mount; it only insists that wherever that weaker answer is taken, its gap
gets reported rather than found by experiment. Revisit if the reports
turn to noise a routine adoption learns to ignore, which argues for a
severity on `Skip` rather than more entries, or if a closed gap's
declaration goes essentially unused by anyone but the importer that
motivated it — evidence the next gap of this shape should stop at a report
rather than earn a schema field.

### A singleton is an op that removes the id

A table keyed by the column that scopes it has exactly one row per
caller — billing settings per org, a profile per user — and neither
existing shape fits it. `OpList` answers `{items:[…]}` for a resource that
is definitionally singular, leaving every client to unwrap `items[0]`
forever with nothing in the document saying so. `OpRead` at
`/resource/{id}` makes the client send back its own tenant id, a value the
server already holds and the scope hook already enforces, so the segment
is redundant when it matches and a lie when it doesn't — a mismatch reads
as a 404 that actually means "you typed your own name wrong." Real
adopters facing this kept a hand-written handler rather than generate
either shape, which is the evidence that a third op belongs in the
vocabulary rather than that the table is unusual.

`OpSingleton` removes the `{id}` segment from the whole resource instead
of adding a route beside it: `GET <path>` answers the caller's one row as
a bare object, `PATCH`/`DELETE <path>` replace the id-scoped forms, and
`OpList`/`OpRead` are refused alongside it — the first because it's the
same route, the second because it's the exact question this op exists to
delete. It needs no primary key, which is what lets a table keyed only by
its tenant column be a resource at all, and it's refused outright on any
model with no `Scoped` column, both at generate time and at mount. That
refusal is the whole design, not a side constraint: a singleton's row is
the row the scope hook leaves — there is no key in the path and no key
predicate in the statement, so the handler issues an unfiltered `SELECT`
and relies entirely on `BeforeQuery` to narrow it. Without the
`Scoped`-requires-a-hook chain already enforced elsewhere in the system,
an unconfined singleton table would answer an arbitrary row on read and
reach every row on write — the default-open failure the scoping design
exists to close, arriving through a door that op alone didn't cover. It's
also the strongest form of that check: elsewhere a missing hook merely
widens an answer the client could still narrow by naming a row; here it
picks one at random, and two matching rows is a 500 rather than a silent
first-of-two, because serving one of them is exactly the wrong answer this
package refuses to give quietly anywhere else.

A `Singleton bool` modifier on the existing read op was rejected because
the manifest and `restcompat` are keyed by operation, and a document
saying `operations: ["read"]` at `/settings` would no longer say whether
an `{id}` exists — the distinction has to live in the operation, not a
flag beside it. A second op just for the singleton write was rejected as
needless multiplication, since "this resource has no id" is one fact, not
one per HTTP verb. And allowing `OpRead` to coexist with `OpSingleton` was
refused for now on the general rule that refusals are cheap to relax
later and expensive to tighten once someone depends on them. A singleton
also reports no filterable, sortable or searchable columns anywhere,
regardless of what the columns themselves declare, since the one `GET`
rejects every query parameter but `?expand` and publishing that vocabulary
would document requests that only ever 400. Revisit if adopters keep
reaching for a hand-written handler anyway because the `Scoped`
requirement doesn't fit their table, which would mean the requirement
needs revisiting rather than the op, or if a resource turns out to want
both a singleton and a collection route at once, which would mean the
modifier design was right and this was the wrong axis.

### The manifest describes what cannot be guessed

Django's admin is the one thing sqlb had no answer to, and the obvious
move — a fifth emitter generating a UI beside the TypeScript and Dart
clients — is wrong: a UI is HTML, CSS, a component vocabulary, a theme and
a browser support matrix, none of which exist in this repository, and
carrying one would tie a release cadence to frontend churn instead of the
schema. The seam that already existed, `sqlb.json`, was missing a rule for
what belongs in it: every field there happens to be something a client
would otherwise get wrong, but nothing said so, which left the next
addition with no test to pass. The test turns out to hinge on who the
consumer is. An agent authoring an admin against the generated SDK is in
the loop at authoring time and picks `title` over `id` without being told,
baking the choice into ordinary source a human can edit and `tsc`
reguards on every schema change; a generic runtime renderer pointed at
`sqlb.json`, with no human in the loop, has no such guess available to it.
So the manifest carries what a competent author cannot guess and would
get wrong silently — opt-in capabilities (a guess is a 400), the derived
wire spelling (a guess is a 400 that reads like a typo), an inverse
relation declared on the far side of a reference (invisible from this
table alone), a declared action (invisible in a CRUD surface, and its
absence leads an agent to PATCH the status the action exists to own) —
and carries nothing an author can decide and the compiler will check: no
row label, no field order, no widget hint. What a caller may actually do
also stays undescribed, and deliberately: hooks are code, not data, so no
static document can report a caller's rights, and because a hook adds a
predicate rather than vetoing a request, a row in another tenant reads as
a 404, not a 403 — there is no permission registry to consult.

A later request for a Convex-dashboard-style admin (schema, data,
actions) tested this rule rather than reversing it. Convex's dashboard is
a grid over raw rows addressed by primary key, with no `__str__` to
guess — column types and enums render a cell, `references` renders a
foreign key as a link, and the typed `Body` a declared action already
carries is enough to render an invoke form without guessing a field the
curated case would have had to guess. Every fact that shape needs was
already in the manifest, on the same admission test as before: nothing
authorable became required, because a grid over raw rows never asked for
a row label in the first place. So sqlb carries this one — an uncurated
data/schema/action browser, built and released as its own module off the
engine's cadence, the same way `pgtest` already is. It calls the
generated REST API rather than the database, which is what makes it safe
to carry at all: fetching through the same endpoints inherits row
scoping for free, exactly as `?expand=` does, in a way Django's admin
does not give you without a per-model `get_queryset` override. That
inherits the permission question rather than answering it — a browser
authenticating as the caller shows only what the caller could already
fetch, and an operator wanting a cross-tenant view needs a credential
this design does not build. Logs stay out of scope for both the manifest
and the browser on the same "instrument, don't carry" posture: `Executor`
is already a traceable interface, and an application points the result
at whatever OTel-reading tool it prefers rather than sqlb rendering spans
itself.

A curated admin — one that guesses a row label, a field order, a widget —
stays authored per application either way; carrying one is the
expensive, least-reversible direction, not technically but socially,
since an admin that ships gets adopted and removing it breaks whatever
was built around it. The uncurated browser is shipped deliberately as a
separate module for the same reason, so that if the claim it rests on
doesn't hold up, deleting it costs a module rather than an unwind inside
the engine. Revisit if an admin written against `sqlb.json` — curated or
the uncurated browser alike — reaches for a fact that isn't there, which
names the field to add, or if two different clients guess the same thing
differently (a TypeScript admin and a Dart app disagreeing on which
column names a row), which would mean a label is a contract after all
and this record sits on the wrong side of its own line.

### A named scope is releasable at the mount

[Reachability at the mount](#reachability-is-a-property-of-the-mount)
settled which columns a mount may reach and admitted what it didn't
solve: the same public/admin split also differs in *rows*, not just
columns — a storefront reads published products, an admin panel reads
drafts of the same table — and row visibility lives in a `BeforeQuery`
hook keyed by the model's Go type, reaching every statement against that
type including the ones an `?expand` issues. A rule registered to confine
the storefront confines the admin panel too, and the only way out used to
be a second Go type over the same table — the exact alternative that gives
up the model, the typed facade, the manifest and the drift gate all at
once. This was never a missing knob, though: the global reach of a scoping
hook is the feature, since one registration confining every generated
handler and every hand-written query is what makes the scoping guarantee
worth anything, and an escape hatch a mount could simply omit would hand
back the same ergonomics as the confinement — the failure mode a `Scoped`
model's mount-time obligation check exists to close.

So a rule is releasable only if it was named at registration, and only by
that name — `sqlb.On[Product](reg).Scope("storefront").BeforeQuery(...)`
can later be dropped with `handle.WithoutScope("storefront")` or, at a
mount, `rest.Options{Unscoped: []string{"storefront"}}`; an unnamed
`BeforeQuery` registration stays absolute, unreleasable by anybody, and
the short spelling — the one every existing registration in every codebase
already uses — stays the safe one by construction. The author of a rule
decides whether it can ever be escaped, by choosing whether to name it,
and that decision sits next to the rule rather than at whichever mount
would like to be out from under it. The obligation check that refuses to
mount an unconfined `Scoped` model runs *after* release, against the
handle a resource will actually serve from, so releasing one of two
confining rules still leaves the other counted — this is what keeps the
mechanism from being the flag an earlier decision already declined to
add, since it doesn't get a resource past the check, it changes what the
check can see. A scope name is a property of the registry, not of a
single model, deliberately: "a shopper sees the published catalog" is one
rule spanning products, variants, categories and collections, and naming
it once lets one release reach all four — including models a request
reaches only through `?expand` — rather than requiring the admin handle to
name the same rule four separate times and silently miss a fifth table
that joins in later. `BeforeCreate` is not releasable at all, on purpose:
it stamps a row on the way in rather than confining a set, so there's
nothing for a reader to be released from.

The cost is a second way to spell a hook, so reading a registration now
means checking whether it's named to know whether some mount can opt out
of it — `Registry.ScopeNames` and `DB.Released` exist so that's answerable
without a grep — and two more ways a mount can fail to come up at startup,
which is the right place for that failure to land. `sqlb.Query[T]()`
issued directly against a released handle in application code is released
too, exactly as a bare pool is unscoped today; the obligation check lives
at the REST mount, not on the query path, and that boundary is unchanged.
Revisit if scope names in practice collide across modules in a
multi-registry deployment — they're registry-wide, so two modules both
naming a rule `"tenant"` share it for release purposes, which is right for
one application and wrong for a library shipping its own hooks — or if
codebases start naming every scope, which would erode the asymmetry the
whole design leans on and argue for a registry-level assertion that a
`Scoped` model keeps at least one unnamed, unreleasable rule.

### A nested query runs nobodys hooks

A query being a value everywhere else in this design still had one gap: a
value couldn't stand inside another query. `Field.OneOf` takes values, so a
set the database itself computes had to be written as `RawPred` — the
escape hatch whose contents go unvalidated, in exactly the position where
a mistake produces a wrong answer rather than a syntax error — and
enumerating the set instead runs into a hard ceiling of one bind parameter
per member. The real obstacle was never the SQL, though. A nested query is
*compiled*, not *run*, and hooks apply when a query runs — so a model whose
reads a `BeforeQuery` scope is supposed to confine would contribute its
rows to somebody else's `WHERE` clause with the confinement silently
absent, and nothing in the response would show it: the outer query returns
the right shape, computed over a set an unscoped read produced.

So a `*Builder[T]` can now be nested — `Exists`, `NotExists`,
`Field.InQuery`, `Field.NotInQuery` compile it directly into the
surrounding statement's compiler, sharing its bind numbering the way only
a closed `Subquery` interface with unexported methods can — but a nested
query that would have run confined is refused unless it has already been
resolved into a value with `.Resolved(ctx, db)`. The refusal is computed
at each nesting site rather than looked up from a registry entry, because
asking "does this model have hooks registered anywhere" would refuse every
nested query in any application using hooks at all; what actually has to
be asked is whether this handle, in this context, would apply a predicate
to this model — which correctly finds nothing missing for a handle that
released a named scope. The check walks every clause that can carry an
expression, recursing into each nested query's own nested queries, because
a clause left out is a nested query that skips the check silently; `Raw`
holds text rather than nodes and stays invisible to the walk the same way
it's invisible everywhere else `Raw` is used. Auto-resolving the inner
query automatically, rather than refusing and asking the caller to resolve
it, was rejected because resolution has to produce a new value without
touching the caller's original expression tree, and a rewriter that missed
a node type would fail open in exactly the way this guard exists to
prevent — refusing needs only the same walk, and nothing more.

The cost lands exactly where people will first try nesting: a confined
model takes two steps instead of one, with an error message that teaches
the second. A `resolved` flag on `Builder` survives `Clone` and could
follow a query onto one that's since been widened — sound today only
because adding predicates narrows a conjunction, and not something that
would stay sound if a disjunctive `Where` were ever added. And nesting a
query inside itself is now expressible and is caught by a depth backstop
and a visited set rather than being structurally impossible. Revisit if
the two-step resolve gets worked around in practice — callers reaching for
`RawPred` specifically to dodge it — which would mean the friction buys
nothing and auto-resolution is worth building the rewriter for after all,
or if a nested query turns out to be wanted somewhere the walk doesn't
reach, such as inside a `Raw` fragment or a computed column's declared
SQL, which would mean the refusal belongs where that text gets assembled
instead.

### A junction is a table

Every ORM in this space has a many-to-many keyword — bun's `m2m` tag, ent's
and GORM's and Drizzle's own spellings — and sqlb deliberately has none. A
reference is a column with a declared inverse, so relations are one hop
and forward-or-inverse; a junction table is an ordinary table with two
references, and the far side is reached by querying the junction directly,
either by expanding the relation on the junction row or by selecting the
far-side foreign key out of it and nesting that as a subquery against the
target table. There is no plan to add sugar over that two-hop. A junction
is almost never empty in practice — `added_at`, `role`, `position`,
`added_by` are the columns that are the actual reason the relationship is
a table rather than an array, and every one becomes filterable, sortable
and projectable the moment the junction is modelled as what it is. An
`m2m` tag would hide that table and then need its own way to say "let me
at the junction row after all" — the table it just hid — so modelling it
directly costs one struct and buys the whole grammar rather than a second,
narrower one. It also sidesteps a second version of the opt-in-capability
argument: a declared traversal across the junction would make the far
table reachable through the near one as a capability nobody wrote down,
and the scope obligations that apply to a mounted table would then need
to apply to a path no declaration names.

The real cost is that a post can't reach its tags in one hop — expansion
is one level, so `Expand("tagged.tag")` is an error, and a post's tags in
one response is either two queries or one query returning junction rows
rather than tag rows. A caller arriving from bun or ent writes a struct
where they expected a tag and has to learn why. This was written down
because the position had been implicit and split across other records
that each stated part of it without ever answering the question someone
new actually asks — how do I get from a post to its tags — which had led
a comparison document to wrongly list many-to-many as something sqlb
lacks. Revisit if nested expansion is ever built, which would turn the
second hop into part of the same query rather than a separate one; if
several adopters model a junction with no columns beyond its two keys,
weakening the "a junction is always more than a link" premise this rests
on; or if forgetting the scope obligation across a junction turns out to
be common in practice, which would argue for a declaration specifically
so a check has something to attach to.

### A read is a query and a row scoped write is a mutation

[Declared actions](#declared-actions) gave a row-scoped write a generated
envelope and explicitly deferred the read case as needing its own record,
since it raises different questions about caching and a query key. A read
is now `Query{Name, Path, Params, Reads, Summary, Description}`, declared
with `TableDef.AddQuery` and mounted as a `GET` with no fetch, no lock, and
no obligation check — its `Do` function is free to call `sqlb.Query[T]`
itself and inherits whatever hooks the executor it's handed carries, which
was proven rather than assumed: a query mounted against a workspace-scoped
table stayed correctly isolated between two tenants with no scoping code
of its own. `Query.Reads` names tables besides the one a query is declared
on, typed as `[]*TableDef` rather than as strings the way `Action.Touches`
is, because a table is already a named Go value in this schema style — a
seam a future table-scoped cache-invalidation feature can read without
inventing anything new.

The row-scoped write, by contrast, stays `Action` in its existing item
form rather than gaining a second declaration type. A same-named
`Mutation` type was actually built, wired into codegen, and shipped for
exactly this shape — then retired the same release cycle, when an
independent consumer reported the precise finding this decision's own
revisit trigger had named in advance: swapping `Mutation` for `Action` on
a live table changed the generated code by exactly one identifier and
nothing about behavior, route, or response shape. The split-by-name
argument that justifies `Query` as its own thing never applied to
`Mutation`, because a read and a row-scoped write are genuinely different
shapes while `Mutation` was never a different shape from `Action`'s item
form at all — it borrowed a three-way query/mutation/action split from a
query-language RPC surface, naming distinctions that don't exist in sqlb's
REST surface, where a row-scoped write is already exactly one verb and one
envelope. Retiring it on the day it shipped was a small, real pre-1.0
break for the one schema that had adopted it, accepted because nothing
else had, and the lesson carried forward is procedural: "the form may
still change" is a reason to defer introducing a second name for the same
shape, not a reason to ship one and see. Revisit if a generated `Query`'s
fixed `[]T` result shape — every row of the table it reads, filtered —
turns out to be the wrong default often enough that a narrower or wider
result type is worth declaring; a query wanting something else stays
hand-mounted today, the same as before codegen knew about `Query` at all.

### Serve owns the boilerplate mount is the seam

Every sqlb server's `main.go` repeated the same ~134 lines — open a pool,
ping it, run migrations, start an `http.Server`, shut it down gracefully
on `SIGINT`/`SIGTERM` — none of which depends on the schema; measuring a
real application found only the resource-mounting half (which tables,
which `huma.Group`, which middleware) was actually application-specific.
This is the same seam an earlier decision drew for the fx container case —
publish only what needs no opinion attached, keep opinionated glue like
"which router" or "which migration runner" as copy-paste rather than an
import — applied here to the plain, no-DI-container path most sqlb
applications actually take. `rest.Serve(ctx, ServeConfig, mount)` owns the
boilerplate: it opens the pool, pings it, runs `ServeConfig.Migrate` if
one was supplied, builds a `*Server`, calls the caller's `mount(*Server,
sqlb.Executor) error`, and serves until the context is cancelled. `Migrate`
is deliberately a caller-supplied function rather than a migration runner
`rest` owns — owning one (goose, atlas, anything) would be exactly the
opinion that stays out of a published import, and a project that migrates
as a separate deploy step passes `nil` and pays nothing for the feature.
`mount` is the seam for which resources get registered and whether one
needs a `huma.Group`, none of which is inferable from a schema value
alone, and `Serve` doesn't try to infer it. Middleware for the whole
server was not drawn as its own seam at first, which meant wrapping it
meant assigning `srv.Handler` from inside `mount` and trusting that
`Serve` read the field back afterward — correct, but load-bearing on an
ordering nothing stated (issue #301). `ServeConfig.Middleware` closes that
gap: `Serve` applies it to whatever `mount` left on `srv.Handler` once
`mount` returns, so establishing a principal — upstream of every
guarantee a `Scoped` hook's guard makes — has a supported place to run
that does not depend on statement order.
`sqlb init` applies the identical boundary to project scaffolding — it
writes a working `go.mod`, schema, and `main.go` built on `rest.Serve`,
but deliberately does not run `go mod tidy`, `go generate`, or
`sqlb migrate` itself, since each depends on something a scaffolding step
can't promise, like network resolution or a prior step's output.

The measured result was a real application's `main()` shrinking from
~134 lines to 29, with no pool code, HTTP server setup, or signal handling
left in application code at all. The cost is that `Serve` fixes the shape
of what it owns — one pool, one `*Server`, one `http.Server` on one
address — so an application wanting two independent `huma.API`s in one
process, the shape a related decision about mount-time reachability
flagged as a real pattern, doesn't fit inside one `Serve` call and has to
wire both servers by hand, exactly the boilerplate `Serve` exists to
remove. This decision carries lower confidence than most here and says so
plainly: the mechanism itself is a mechanical, measured-equivalent
extraction, but it has been built and live-tested against exactly one
application, so the open question isn't whether it works but whether the
seam is drawn in the right place. Revisit if a second application's
`main.go` needs a shape `Serve` can't express — two servers, two ports, a
non-HTTP listener — often enough that the single-server assumption is the
wrong default rather than the common case, or if every real project's
`Migrate` function turns out to be the same ten lines, which would mean
withholding a shipped migration adapter is optimizing against a dependency
nobody actually minded.

### A Verifier composes with the principal seam

An earlier decision already answered how sqlb publishes an extension point
without owning the assembly around it
([ADR-0044](#the-container-is-an-adapter)): a minimal, stdlib-only context
contract — `WithPrincipal`/`PrincipalFrom[T]` in `principal.go` — with
anything opinionated, which router, which provider, kept as a copy-paste
example rather than an import. That record also named its own
reconsideration trigger: a second author writing an sqlb-shaped module.
Four such authors arrived at once — applications wanting WorkOS, Clerk,
Zitadel, or a self-hosted JWT/session scheme, each about to write the same
shape of middleware, extract a credential, verify it, call `WithPrincipal`,
independently. `example/tasks/auth/` and `example/fxapp/access/` already
proved the pattern works; what neither proved is that it needs restating by
hand every time.

`Verifier[T]`, `Middleware[T]`, `TransientError`, and `BearerToken` are
published in `auth.go` at the module root, next to `principal.go`, with
zero new dependencies. `Verifier[T]` stays generic over the application's
own principal type rather than sqlb defining a canonical `Principal`
struct — the same choice `PrincipalFrom[T]` already made, extended to the
thing that produces a principal rather than just the thing that carries
one. Credential extraction is a separate, composable piece from
verification (`CredentialExtractor`, with `BearerToken` as the
stdlib-shaped default): WorkOS's AuthKit and Clerk's hosted UI commonly
hand back session state via a cookie for browser flows, while Zitadel and
self-hosted JWT are bearer-token-shaped, so hardcoding one extraction
strategy into `Middleware` would make it wrong for half its named targets.
A `Verify` failure is 401 unless it opts into `TransientError`, which
answers 500 instead — a provider outage and a rejected credential are
different failures for an operator paging on 5xx and for a client deciding
whether to retry, and collapsing them loses that distinction. This is
opt-in rather than a required interface method, because a `Verifier` with
no network call to fail, local JWT verification, has no transient failure
mode to report; return `TransientError` by value, since `Middleware`'s
`errors.As` check targets the value type and a pointer silently falls
through to 401. Provider adapters — WorkOS, Clerk, Zitadel — stay worked
examples under `example/`, each its own Go module, not published
`go get`-able packages: sqlb core is dependency-locked to pgx only
([ADR-0040](#the-driver-is-a-dependency)), an adapter needs its provider's
SDK, which must never reach sqlb's own `go.mod`, and this is ADR-0044's
rule exercised for auth specifically rather than a new one. `Middleware[T]`
also carries no public-path allow-list the way
`example/tasks/auth/middleware.go` does — not a gap so much as the same
ADR-0044 rule applied to path matching: `rest.Server.Handler` and any
router already let an app mount `Middleware[T]` on only the subtree that
needs protection, so an allow-list opinion in core would duplicate the
app's own router and still have to pick subtree-vs-exact-match itself;
`example/tasks/auth/middleware.go`'s allow-list stays the worked example
for an app that wants deny-by-default-with-exceptions, and core stays
silent on the policy.

Four independent auth integrations share one seam instead of four
reimplementations of "extract, verify, `WithPrincipal`," and the 401-vs-500
split is testable and tested once in core rather than once per adapter.
The cost is that `Verifier[T]` commits to a one-credential, one-call shape:
an app needing multi-factor verification, or a provider whose check
genuinely needs two round trips, doesn't fit `Verify(ctx, cred) (T, error)`
directly and has to compose its own `Verifier[T]` around the parts it
needs — multi-provider chaining within one realm was scoped out
deliberately, not an oversight. Widening (`Middleware` gaining an optional
parameter, a second failure taxonomy) is free; narrowing was cheap before
the first adapter depended on it — a first one now exists,
`example/auth-workos`, so the honest cost of changing the signature is no
longer zero. Two smaller calls made along the way, both left for later
evidence rather than built speculatively:
`Middleware` doesn't set `WWW-Authenticate`, since it's generic over
`CredentialExtractor` and doesn't know the extractor is bearer-shaped (an
app-specific wrapper can add it), and `Verify`'s error carries no typed
reason beyond transient-or-not, since the detail message deliberately
never echoes the underlying error and a richer type would have nowhere
visible to surface today.

Revisit if a second application's `Verifier[T]` needs a signature
`Verify(ctx, cred) (T, error)` can't express, or if a real adapter finds
`TransientError` insufficient, for instance a provider whose rate-limit
response is neither "reject the credential" nor cleanly "unreachable."
`example/auth-workos` is that first real adapter, and it already answered
the `TransientError` question, just not the way the rate-limit scenario
above speculated: golang-jwt and keyfunc collapse "JWKS unreachable" and
"unrecognized signing key" into the same wrapped error, so `Verify` has no
reliable signal to report as transient and correctly never returns one —
the outage case that matters most, WorkOS being unreachable at startup, is
caught by `New` failing at construction instead; an outage that begins
*after* startup surfaces at request time as an indistinguishable rejected
credential, the same collapse this paragraph already describes.
`TransientError` holds up; it was just never going to be exercised from
inside `Verify` for this adapter.

### Fx wiring is generated, not a runtime library

sqlb emits models, the manifest, the REST resource, three clients and a
skill from a schema declaration, but nothing that makes a schema-owning
module a *unit* in the host's dependency-injection graph — even though two
of the three things a module contributes there are already fully
determined by the declaration: the migration history and the resource
mount. A real 38-module uber-go/fx consumer measured what that costs by
hand: 78 byte-identical migration providers, 183 operation-set literals,
209 `fx.Module(...)` declarations. This was tried once already as a
runtime library, and rejected on the merits rather than the idea: its
value-group vocabulary matched the consumer's contracts name for name, but
importing it pulled chi, goose and huma into a graph that already had
them, duplicating contracts the platform already held — a runtime
dependency can't avoid dragging sqlb's own choice of router and migration
runner along, no matter how well the vocabulary lines up. A generated file
doesn't have that problem: it references the host's own types by a
fully-qualified string and imports only what the consuming module already
depends on, at the cost of sqlb needing to be told two names a schema
can't derive — which fx value group a mount joins (an access-surface
decision, not a schema property), and a module's fx identity when its
registry is genuinely unnamed on purpose.

`codegen.Options` grows `WiringMigrations` and `WiringOperations`, each a
`WiringSet{Type, Group, Name, EmbedDir}` — two named fields rather than a
slice, because the two contributions have different shapes (`EmbedDir`
means "the directory of `.sql` files to embed" on one and is refused on
the other) and one `codegen.Options` never has a second migration history
or a second resource mount to contribute. `Type` is a fully-qualified
`"import/path.TypeName"` string, so a wrong name is a compile error in the
generated file rather than a provider that silently joins no group. What's
emitted is one `fx.Option` value, `FxModule`, never a wrapped
`fx.Module(name, ...)` — the hand-written module composes it
(`fx.Module("store", FxModule)`), so a module with more to say adds it
beside the generated value instead of inside it, and `FxModule` never
carries a hand edit to lose when the schema changes and it regenerates.
Nothing is emitted for hooks, since a `Scoped` column already refuses to
mount without one and generating hook wiring would mean sqlb generating
unreviewed authorization policy; nothing is emitted when the schema
declares actions or queries either, since `Register` would need
hand-written funcs the emitter has no way to supply — an honest refusal
rather than a generated call that fails to compile.

The measured result was `example/fxapp`'s `store/module.go` shrinking from
a hand-written provider pair to one line composing `FxModule`, the exact
shape the 38-module consumer's copies were measured at. The cost is a
fifth emitter over the same declaration, a documented limitation in
`WiringSet.Type`'s assumption that a package's name is its import path's
last segment, and coverage that's partial by design — a module declaring
actions or queries wires its resource mount by hand and gets no operations
contribution at all. Revisit if a second DI container shows up in a real
consumer wanting the same two contributions, if the actions/queries
refusal turns out to be reachable often enough that most modules with a
REST surface hit it, or if a host package's name diverging from its
directory turns up in practice — untested against a real consumer, and the
failure today is an import that doesn't exist rather than a clean
rejection at generate time.

### A unique foreign key is already one-to-one

`docs/compatibility.md` freezes the list envelope — `{items, page, per_page,
has_more, next_cursor?, total?}` — for every `Inverse` relation, because a
capped reverse expansion has always needed to say it might be partial
([ADR-0022](#references-declare-their-inverse)). That shape is right when
more than one child row can exist. It was never right for a
`Ref(...).Unique()`: the constraint that makes a foreign key column
one-to-one already lived in the database, but nothing downstream read it, so
a unique `profiles.user_id` still generated `sqlb.Collection[Profile]` in Go
and `{items, hasMore}` in both clients for a relation that can never hold
more than one row, and every caller had to know out of band that `.items`
would never exceed length one and unwrap it by hand.

`Registry.Inverses` now sets `InverseRelation.OneToOne` wherever the
referencing field it is walking carries a single-column `Unique` alongside
its `Reference` — no new schema verb, and no state stored on `Reference`
itself. That is deliberately not the same move as `Filterable`, `Sortable`
or `Expandable` ([ADR-0006](#capabilities-are-opt-in)): those are policy a
schema author opts into, because "can a client ask about this column" has no
single right answer. Whether a foreign key can address more than one row is
not a policy question — it is a fact the `UNIQUE` constraint already states,
and inferring `OneToOne` from `.Unique()` keeps the DSL from asking an
author to declare a second time what the database already enforces.
`RelationInfo.Reverse` carries the same distinction into the query compiler,
separating "the foreign key lives on the target" — true of both a capped
collection and a one-to-one reverse relation — from `Collection`'s narrower
"and there may be more than one": a one-to-one `Inverse` now joins through
the same `LEFT JOIN` machinery a forward reference already used, with the
`ON` clause's two sides swapped, rather than the correlated-subquery-with-cap
path built for the general reverse case
([ADR-0025](#expansion-is-one-statement)). Go, TypeScript and Dart codegen
follow the same signal: a one-to-one `Inverse` field generates a bare
pointer, `Target | null`, and a nullable getter, never the paged-collection
type, and `ExpandOrder`/`ExpandLimit` on a unique-backed `Inverse` is now a
schema validation error, since neither means anything once at most one row
can ever match.

That breaks the Frozen list-envelope guarantee for this one relation shape,
the same move [ADR-0040](#the-driver-is-a-dependency) made for `Executor`: a
pre-1.0-or-never trade, because breaking a Frozen surface only gets more
expensive once a version tag turns the same edit into a major version and a
hand migration for every consumer. Unlike `Executor`, there was nothing here
to migrate away from inside this codebase: no schema anywhere in this
repository declared a unique forward reference before this feature — the
only one, `profiles.user_id`, was added specifically to prove this shape
end to end — so nothing here reads `.items`/`.has_more` off a one-to-one
relation today, because that shape has never been correct for that case.
The cost this decision does carry belongs to a hypothetical outside adopter
who declared `Ref(...).Unique()` before this shipped and generated clients
against the old collection shape: regenerating flips the field's type, and
every call site reading `.items[0]` or checking `.hasMore`/`.has_more` needs
the mechanical edit `compatibility.md`'s carve-out names.

The inference is narrower than "structurally one-to-one," on purpose. A
table-level `Unique(a, b)`/`UniqueIndex(a, b)` that happens to include the FK
column does not set `OneToOne` — only a single-column `Field.Unique()`
directly on the `Ref`'s own column does, since that is the only case where
the column itself cannot repeat. And a FK column that is *also* the table's
own primary key — `profiles.id IS user_id`, the idiomatic shared-PK one-to-one
SQL pattern, Django's `OneToOneField(primary_key=True)` — does not set
`OneToOne` either, even though a primary key is exactly as repeat-proof as a
`Unique()` column: `Field.PrimaryKey()` sets `PrimaryKey`, `ReadOnly` and
`Filterable`, not `Unique`, and `Registry.Inverses` reads only `d.Unique`.
This is a real gap the design spec never considered, not a deliberate
exclusion the way the composite case above is — it is recorded here rather
than closed because extending the derivation to `d.Unique || d.PrimaryKey`
is a behavior change reaching every schema anywhere that declares a
PK-backed FK today, silently flipping their generated shape on the next
`sqlb generate`, which is not a change to make without its own review. No
schema in this repository happens to use the pattern, so nothing here would
regress — but that is not the same claim as "safe for every adopter."
Revisit if a PK-backed FK turns up in a real schema wanting this shape:
either extend the derivation (auditing every downstream consumer of
`Registry.Inverses`/`InverseRelation.OneToOne` first, the way this entry
had nothing to migrate only because nothing here used the shape yet) or add
`.OneToOne()` as the explicit opt-in this feature deliberately did not
build, scoped to exactly the PK-backed case the inference misses.

Revisit also if a one-to-one relation ever needs to say more than "the row,
or none" — a soft-deleted child's tombstone, say — which the bare nullable
shape cannot express and the list envelope's `has_more` never could either,
arguing for a third shape rather than reusing either existing one.


### DDL is declared, never inferred

`ExternalRef` used to imply a single-column index, on an argument that is
correct as advice: a soft foreign key exists to be joined on, and one without
an index scans the table. The index was synthesised when a table's index set
was read, from nothing but the column's shape.

That works while the only thing building registries is a declaration. It stops
working the moment one is built by reading a database, because an inference
cannot be checked against reality. `introspect` imports a self-referencing
foreign key as an enforced `ExternalRef` — deliberately, since when that code
was written a self-reference had no other spelling — so the registry
*describing* a live database claimed an index that database did not have. The
declared side, using the `AddField` form `example/catalog` documents, asked for
no such index. `migrate.Diff` resolved the disagreement the only way it could:
`DROP INDEX CONCURRENTLY` for an index nothing had created, on every run, and
applying it failed with `42704`
([#259](https://github.com/jryannel/sqlb/issues/259)). Hand-correcting the
migration did not help — the phantom was regenerated from the same inference
the next time anything else in the schema changed.

So no DDL is inferred from a column's shape. An index is `Field.Indexed()` or a
table-level `Index`/`AddIndex`, and a registry read out of a database reports
the indexes that are in it. The advice the inference was carrying moves to the
register that already carries this kind of advice: `schema.Lint`'s
`unindexed-ref`, beside `unindexed-filter`, `unindexed-expand` and
`unindexed-inverse-expand`, all four suppressed to one warning per column since
one word fixes all of them ([ADR-0006](#capabilities-are-opt-in) is the same
principle one layer up — nothing is filterable, sortable or selectable unless
the column says so).

What this costs is a schema that declared `ExternalRef` before v0.15.0 and got
its index for free: the next diff proposes dropping it, which is clean SQL that
silently removes an index a join depends on. That is a real break and it is
taken deliberately, pre-1.0, with the mechanical edit — add `.Indexed()` — in
the release notes and in `docs/compatibility.md`. The alternative, teaching
`introspect` not to synthesise while leaving the inference in place, fixes the
one reported symptom and leaves two spellings of the same reference producing
different DDL, which is the mechanism rather than the instance.

Revisit if unindexed foreign keys start showing up in real schemas despite the
diagnostic — that would mean a warning is too quiet for DDL this load-bearing,
and the answer would be to make `Ref` refuse to be silent rather than to make
it guess again: an explicit `.NotIndexed()` for the case where the index is
genuinely unwanted, so the declaration has to say which it means.

### Generated output is replaced, never rewritten

`sqlb generate` used to write every file it rendered, whether or not the bytes
had changed. Nothing in a build could tell the difference — a build reads the
tree once, after generate has returned — so for two years the only cost was
some wasted I/O.

A language server is not a build. gopls invalidates on the filesystem event,
not on the content: `snapshot.clone` marks the package containing a changed
file, and every package that imports it, as needing a re-typecheck for any
watched write, and only the heavier `go list` reload is skipped when the file
hash turns out to be unchanged. Generated code is by construction what the rest
of a project is built on — in the layout `sqlb init` produces, `models_gen.go`
sits in the package every other package imports — so a `go generate` that
changed nothing still threw away the type information for the whole module.
Schema authoring is a loop of small edits and a regenerate after each, and most
of those regenerates touch one file out of eight. The report was a consumer
project whose editor spent its time re-indexing
([#269](https://github.com/jryannel/sqlb/issues/269)).

So a file whose rendered bytes match what is already on disk is not written at
all, and one that differs is replaced by a rename rather than truncated and
filled. The rename matters for the same reader: `os.WriteFile` leaves a window
in which the file is a prefix of valid Go, and a language server that reads on
the event can land in it and report a syntax error against code nobody wrote.

Three alternatives were considered and rejected, because they all try to make
the *tool* ignore the churn instead of not causing it:

- **Move generated code to its own directory.** It would still be a package the
  rest of the project imports, so its reverse-dependency cone is invalidated
  exactly as before. This buys nothing and costs the import path.
- **Flag the files as generated.** gopls already detects generated files — it
  parses the `// Code generated … DO NOT EDIT.` header — but only to suppress
  code actions and refuse edits to them. There is no marker that means "do not
  index", and there could not be one: the symbols in these files are what the
  hand-written code resolves against.
- **`build.directoryFilters`.** It excludes a directory from the workspace
  entirely, so the generated package would be unresolvable everywhere it is
  used. That is not a quieter editor, it is a broken one.

The one thing left that is genuinely per-event, rather than per-change, is the
driver: `sqlb generate` compiles a program inside the module in order to import
the schema package ([The driver is a dependency](#the-driver-is-a-dependency)),
and a Go package appearing and vanishing in the workspace on every run would be
worse than any rewrite, since an existential change forces the `go list` reload
that a content change does not. It costs nothing because the scratch directory
is named `.sqlb-driver-…`: Go's package loader skips directories beginning with
`.`, so `go list ./...` never sees it and gopls has no package to invalidate.
That was chosen for `.gitignore`'s sake rather than for this, and it is worth
recording that it is now load-bearing twice.

Revisit if a consumer reports churn that survives this — the next suspect would
be the TypeScript and Dart output, whose language servers were not measured
here, and the answer would be the same treatment rather than a different one.

### The subscriber is generated, the policy is not

[The stream is a seam](#the-stream-is-a-seam) settled the endpoint, the wire
format and the reconnection contract, and left the last mile to whoever
consumed it: a client got `keysByTable` — a table name mapped onto the query
keys that read it — and wrote the `EventSource` listener itself. The
[vision](vision.md) recorded the consequence in advance and it was right: a
feed that delivers a table and a row key is only mechanical if something
derives the cache key from that pair, and otherwise every consumer hand-writes
an invalidation list that drifts from the one its own mutations use.

The example was the evidence. `example/tasks/web/src/board.ts` carried the
listener this argument predicts — a `keysByTable[event.table as keyof typeof
keysByTable]` lookup with a cast to make the index compile, an `undefined`
check, `lists()` and `detail()` by hand, and no case at all for an event whose
key is empty, which is what an unattributable delete looks like. It was written
by the people who wrote the generator, in the repository that documents the
rule, and it was still one of the two lists this design exists to collapse.

So the decode is generated, in both clients, and the line it stops at is the
one [the TypeScript client](#typescript-client) already draws for mutations. A
`mutationFn` gets no `onSuccess`, because what a write invalidates is a policy
question — whether a new row belongs in a list filtered the way this screen
filters it is not answerable from a schema. A change event is not that: it is
an *address*, and turning an address into the queries that read it is a lookup
the schema fully determines. One file, two answers, one reason.

What each client emits follows from what its ecosystem has. TypeScript gets
`subscribeChanges`, which owns the stream, narrows `table` to a `TableName`,
and hands the caller the keys — a keyed event resolving to that row's detail
queries plus the lists and infinite walks it may have moved in or out of,
rather than to the table, so a change to one row does not refetch every other
row on screen. That precision needed one addition to the key factory, an
`infinites()` prefix beside the `lists()` and `details()` that already existed;
without it the only way to reach a table's infinite walks is `all()`, which
takes every detail query with it. Dart gets `TableChange.from`, the same
narrowing with no keys, because Riverpod and BLoC have no keyed cache to hand
them to — the asymmetry [the Dart client](#dart-client) already records, applied
to one more thing.

Two mechanical consequences are worth recording because neither is obvious from
the outside. Dart's `FeedEvent` is sealed so that a `switch` over it is
exhaustive — a subscriber that handles a change and forgets a reset does not
compile, and the reset is the case that matters — and a sealed hierarchy must
be one library, so the whole stream had to go in the shared runtime rather than
in each client. And the *connection* is generated in neither: `EventSource`
cannot carry an `Authorization` header, so a bearer-token deployment passes its
own opener, while Dart has no `EventSource` at all and the application opens the
request and passes `jsonDecode` in. That is the `Transport` seam again, and
keeping it is what lets both runtimes go on importing nothing.

Three alternatives were rejected. **Generating the invalidation itself** — a
subscriber wired straight into the cache — would put a write policy in the one
place that has no way to ask about the screen. **Generating a subscriber for
the Go CLI** was declined for the reason Dart gets no keys: a command has no
cache and no view, and a `sqlb watch` command that prints events is a different
feature with a different argument. **Emitting cache keys for Dart** stays
rejected on the same ground it always was.

The cost is that the wire format is now written down in four places — the
publisher, the endpoint, and a decoder in each client — against a shape
[ADR-0045](#the-stream-is-a-seam) deliberately froze. That is why the Go, the
TypeScript and the Dart tests all assert the same frames: `rest/events_test.go`
from the server end, `web/src/api/events.test.ts` and `mobile/test/events_test.dart`
from the client end, so a drift in one of the four is a failure rather than a
field nobody reads.

Revisit if an application reports that the derived key set is wrong for its
query layout, which would argue for making the derivation declarable per
resource rather than fixed; or if the stream acquires an authorization
obligation the way tenant scoping has one, since the opener seam is where a
token would have to be threaded and it is currently a callback rather than
something a mount can check.

### A create body may carry what the row does not

A create body is derived from the columns, and the request that creates a thing
with a secret in it carries one thing that is not a column: the plaintext. The
column stores a digest. Before this there was nowhere to put the property, and
the two available shapes each lied to somebody
([#309](https://github.com/jryannel/sqlb/issues/309)).

Marking the digest column `WriteOnly` puts it in the body under its own name,
so the client sends a plaintext PIN in a property called `pin_hash` — and
[the wire is the column name](#the-wire-is-the-column-name) means there is no
per-field override to rename it, deliberately. Renaming the column to `pin`
fixes the wire and moves the lie into the DDL, where a `VARCHAR(255)` called
`pin` holds a bcrypt digest and every reader of the schema concludes it is
plaintext. A collection action can declare an honest body and answers 204, so
the client does not get the row it created. The honest option left was to
hand-write the create, and with it the DTO, the route, the OpenAPI operation
and all four clients — which is exactly the door
[hooks as domain seam](#hooks-as-domain-seam) exists to close, arguing that a
generated create *is* the placement for the rule.

So `schema.REST.CreateInput` declares those properties, in the field
vocabulary [a declared action's body](#declared-actions) already uses.
The body is still the columns; this is the part of it that is not.

**The value reaches the hook through the context**, as `sqlb.CreateInputFrom`,
which is [the principal seam](#a-verifier-composes-with-the-principal-seam) applied a
second time
and for the same reason: `BeforeCreate` is handed the row and the context and
nothing else, and that is what lets one registration cover every insert of the
model. Widening the hook signature was the alternative and it is the one that
does not compose — the input's type is per-resource and `Hooks[T]` is generic
over the model, so a typed second parameter cannot be spelled, and an untyped
one would hand every existing hook a `map[string]any` it never asked for. The
cost is that a hook has to fail closed when nothing was supplied, which the
doc comment says in the imperative: every insert runs the hook, including one
from a job that never saw a request, and treating the absence as "nothing to
do" writes the row with an empty digest in the column that authenticates.

**A property is spelled as declared**, not through `WireCase`. The case is a
declared function of a *column* name, and this is not a column — an action's
body has always worked this way, and the two disagreeing would be a second rule
for one vocabulary. What the schema does refuse is a property whose name is a
column's, in either spelling, since the two share one JSON object and the
generated body would carry both under one tag.

**The exit carries it too.** An ejected package has no hooks, so the seam is a
function field — `Derive`, beside the `Confine` and `Assign` that
[declared scope is required](#declared-scope-is-required) already put there —
and `Register` refuses a nil one rather than serving a create that silently
drops the property. That is the same shape [the exit is generated](#the-exit-is-generated)
uses everywhere: keep the property, drop the machinery.

Revisit if the same argument arrives for a patch body. It has not: the cases
collected so far — a password at signup, an invite's token, a list of ids
resolved into rows of another table — are all creates, and a second declaration
before there is a second case would be a vocabulary invented for symmetry.

### A verb may declare what it answers with

[Declared actions](#declared-actions) gave a domain verb a route and a generated
envelope, and fixed the response: an item action answers with the row it acted
on, and a collection action answers 204. Both are right for the verb that
*transitions* a row, which is the case that decision was written for.

Neither is right for the verb whose answer is the point. Grading a quiz returns
a score; a score is not a Lesson, and 204 is not a score. An audit of a
nine-vertical application ([#312](https://github.com/jryannel/sqlb/issues/312))
put one operation in that shape after its own correction, and
[#310](https://github.com/jryannel/sqlb/issues/310) is the collection half of
it — a verb that created a row and could only answer 204, leaving the client to
re-list and guess which row was new. Both stayed hand-written, and a
hand-written route takes the OpenAPI operation and four clients with it.

So `Action.Returns` declares the response in the same field vocabulary
`Action.Body` uses, and the func grows a return value. Everything else is the
envelope as it was: the same scoped fetch, the same row lock, the same write of
the declared set. The declaration *replaces* the default response rather than
adding to it, because one operation has one body — a client that needs the row
as well re-reads the id it already sent.

**The runtime grows two functions rather than a flag.** `ActionReturning` and
`CollectionActionReturning` sit beside `Action` and `CollectionAction`, because
the response type is a Go type parameter: what a route answers with is fixed at
`huma.Register`, so it cannot be a value the spec carries. That also keeps the
existing signatures untouched, which matters more than the symmetry — a mount
written against the old ones is a mount that still compiles.

**The status is 200, never 201.** A collection verb that creates a row is
describing `OpCreate`, and since
[a create body may carry what the row does not](#a-create-body-may-carry-what-the-row-does-not)
that operation can take an input with no column behind it, which was the reason
#310 reached for a collection action in the first place. Two ways to spell one
create is the ambiguity that issue was one half of; this closes the half that is
about the answer, and leaves creating to the operation named for it.

**What the response says is diffed.** `sqlb impact` records the declared result
and classifies it as a reader-side contract, which is the mirror of the request
side: a property leaving the response breaks the client that reads it, a value
set that *grows* breaks the client that switched on it, and acquiring or losing
a declared result is a change of return type rather than of one property.

Revisit if a verb needs to answer with the row *and* something computed. The
shape that would take is a declared result with a property whose type is the
model, and it is not built: the cases collected so far each want one or the
other, and a response type that sometimes embeds a row is the thing that makes
a generated client's return type conditional.
