# Glossary

sqlb coins a fair amount of vocabulary, and most of it is defined wherever it
first became relevant rather than in one place. This is that one place: a
sentence each, and a link to the page that argues for it.

## The declaration

**Schema** — the declaration itself: tables as ordinary Go values. It is the
source of truth every other surface is derived from, and there is no separate
schema language to keep in sync. See [Declaring tables](../schema/README.md).

**Table definition** — one `schema.Table(…)` call: its columns, indexes,
constraints, exposure and the relations it declares.

**Registry** — the set of table definitions a program has declared, as one
value. `migrate.Diff` compares two of them, which is why generating a migration
and adopting an existing database are the same machinery pointed in opposite
directions.

**Column constructor** — a function naming a column and fixing its type —
`Text`, `BigInt`, `Timestamp`, `Vector`. Every one is listed in the [column type
reference](column-types.md).

**Group** — several columns inserted as a unit, so a recurring set is declared
once. `Timestamps()` and `SoftDelete()` are built in; returning your own
`schema.Group` factors any other.

**Module** — a group of tables that ship and migrate together, carrying a shared
table-name prefix applied mechanically. A reference across a module boundary is
an `ExternalRef` — a column and an index, and deliberately no foreign key.

**Capability** — a per-column opt-in to what the outside world may do with it:
`Filterable`, `Sortable`, `Searchable`, and their absence. Nothing is reachable
unless the column says so, and a request naming a column that did not opt in is
rejected with a message that lists what would have been accepted. See
[Capabilities](../concepts/capabilities.md) and the [capability
reference](capabilities.md).

**Hidden** — a column with no wire spelling at all: it is absent from the JSON,
from the generated client types, and from every request grammar. A `Vector`
column is hidden and not optionally so.

**Exposure** — what publishes a table over HTTP, declared with
`Expose(schema.REST{…})`. Without it a table is reachable from Go and has no
REST surface at all.

**Wire spelling** — the single name a column has outside the database — in JSON,
in the filter grammar, in the generated clients. It is derived from the column
name by the schema's `WireCase`, and there is no mapping layer and no per-field
override in either direction. See [The wire is the column
name](../architecture.md#the-wire-is-the-column-name).

**Declared scope** — a tenant boundary the schema requires rather than trusts a
hook to remember. A hook that nobody wrote is default-open, which is the hole
this closes: a module declares its scope, and a resource that would serve
unscoped rows refuses to mount. See [Declared scope is
required](../architecture.md#declared-scope-is-required).

## Querying

**Predicate AST** — the intermediate form every query condition takes before SQL
is rendered. It has two producers — hand-written Go and the URL filter grammar —
and one compiler, which is why an HTTP filter is subject to the same
bind-parameter discipline as Go code. See [One grammar, two
producers](../concepts/one-grammar.md).

**Query is a value** — nothing runs when a query is built, so predicates can be
added on a branch and a hook can amend a query it is handed. The terminal
methods (`All`, `One`, `Count`) are what execute. See [A query is a
value](../concepts/queries-are-values.md).

**Model** — the described form of a Go struct, its columns, key and
capabilities, cached after first use. A published `*Model` is never written
again, which is how the read path avoids locking.

**Hook** — the seam where domain logic attaches to a model's queries and
mutations. `BeforeQuery` is the load-bearing one: a single registration
constrains *every* read of that model, including reads issued by generated REST
handlers. Hooks are registered against a named registry rather than process-wide
state. See [Where domain logic goes](../concepts/domain-logic.md).

**Executor** — the two-method interface sqlb runs against: `Query` and `Exec`
over pgx types. It grows by optional interfaces that are type-asserted for,
never by adding methods, so a `*pgxpool.Pool`, a `pgx.Tx` and a test double all
satisfy the same thing.

**Typed column facade** — the generated `columns.go`: one typed `Col[T]` per
column, so a comparand that does not fit does not compile. A nullable column is
typed as its base type there, with NULL expressed by `IsNull`. See [Typed
columns](../queries/typed-columns.md).

**Verifier** — the one-method interface an authentication provider satisfies,
`Verify(ctx, cred) (T, error)`, so the identity type is the application's, not
sqlb's.

## Serving

**Resource** — one exposed table's set of HTTP operations: list, read, create,
patch, delete, as far as its `Ops` bitmask allows.

**Expand** — following a declared relation in a single request, for a column
marked `Expandable`. `Inverse` names the relation from the target's side. See
[Expanding relations](../rest/expand.md).

**Rejection** — sqlb's refusal of a request, served as an RFC 9457 problem
document. A rejection names what would have been accepted rather than only what
was wrong. Every message one can carry is in the [rejection
reference](rejections.md).

**Keyset cursor** — the opaque token that positions a page. A page is a position
rather than a distance, which is why offset paging is not offered and why an
array column cannot be `Sortable()`. See [Pagination](../rest/pagination.md).

## Changing things

**Diff** — the pure function over two registries that yields the changes between
them. Rendering those changes as Postgres statements is a separate layer, and
rendering them as the files a particular runner expects is a third.

**Hazard** — a warning attached to a generated change, naming what it costs on a
table that already has rows — the lock it takes, or the `setval` to run first.
The row count is not in the schema, so nothing in the diff can size it.

**Introspection** — reading a Postgres schema out of `pg_catalog` and returning
the same registry the DSL produces. It is how an existing database is adopted.
See [Adopting a database](../migrations/adopting.md).

**Shadow** — building a registry by replaying the checked-in migration history
into an empty database, so the current state is *what the history produces*
rather than what production happens to look like. A hand-applied hotfix is
invisible to the second and caught by the first.

**Impact** — the diff of the REST contract two schemas generate, with each delta
classified as breaking, additive or neutral for a deployed client. A sibling of
the migration diff, not a consumer of it: the sharpest API breaks — un-exposing
a column, dropping an operation — produce no DDL at all. See [REST
compatibility](../rest/compatibility.md).

**Ejection** — the generated way out: the schema as plain SQL and the resources
as `net/http` handlers over pgx, with no sqlb import left. It is checked in CI,
because an exit nobody exercises has quietly stopped working by the time it is
needed. See [Ejecting](../eject.md).

## The documentation

**Surface** — one of the things derived from the declaration: schema, queries,
REST, TypeScript, Dart, CLI, migrations. Each is independently optional, and
each is a section of this documentation.

**Decision** — a load-bearing architectural choice, recorded as a section of
[architecture.md](../architecture.md#decisions) with the alternative it
rejected and, usually, the condition that would justify revisiting it. These
were once a directory of individually numbered files; `git log -G` over the
heading is how a decision's history is read now.
