# A library that ships tables

sqlb documents how an *application* declares a schema. A library is a different
problem: its tables have to coexist with tables it does not own and cannot see.
This example is the shape that works, and the tests are the argument.

```bash
go test ./example/libtables/
```

No database. Every claim below is about the DDL a composed registry produces or
the SQL a model compiles to, and both are values.

## The shape

`sessionkit/` is the library. It declares one table and never imports the
application:

```go
func Declare(reg *schema.Registry, opts Options) Tables
```

`appschema/` is the host. It owns the registry, declares its own tables, and
hands the library both the registry and the table its rows should point at:

```go
var Registry = schema.NewRegistry()
var User = Registry.Table("users", …)

var Session = sessionkit.Declare(Registry, sessionkit.Options{
    Users: User,
    Scope: "workspace_id",
}).Sessions
```

One registry, so one `sqlb generate`, one migration sequence, one drift check,
one manifest — and a foreign key that crosses what would otherwise be a
boundary.

## Why not let the library own its migrations

It is the first thing a library author reaches for and the one answer that is
always wrong. A library with its own sequence has its own tracking table, so its
tables are created in an order nothing coordinates with the host's. A foreign
key across that line then works only when the referenced table happens to be
created first — fine for `accounts` before `companies`, broken for `academy`
before `companies` — and it fails at deploy rather than at compile.

Declaring into the host's registry makes the ordering question disappear rather
than answering it: every `ALTER TABLE … ADD CONSTRAINT` is emitted after every
`CREATE TABLE`, which `TestConstraintsFollowEveryTable` pins.

What it costs to get this wrong is not abstract. Without the key, deleting a
user leaves their sessions behind and nothing but application code says
otherwise.

## The six questions, answered

| | Answer | Pinned by |
|---|---|---|
| **Naming** | A fixed, library-owned prefix — `sessionkit_`, a package constant. Not a host-supplied option: a model names its table statically, so a configurable prefix has to be applied with `sqlb.Describe` at startup, which turns a naming choice into an initialisation-order rule that fails at runtime | `TestACollidingNameIsRefusedAtDeclaration` |
| **Optional references** | One function returning `*schema.Field`. `schema.Ref` when the host supplies a target, `schema.UUID` when it does not — same column name either way, so nothing downstream knows which it got | `TestStandaloneDeclarationDropsTheKeyAndKeepsTheColumn` |
| **Confinement** | The library declares it and the host is obliged. `sqlb:"scope"` on the model means a mount refuses to start until a `BeforeQuery` confines it — the library knows the rows are confidential, only the host knows what confines them | `TestConfinementObligesTheHost` |
| **Extension** | A host may `AddField` onto a library's table and it lands in the migration like any other. It does not reach the library's own model, so the added column is the host's to read through the host's own struct | `TestTheHostCanExtendALibraryTable` |
| **Migrations** | The host owns the sequence. Always | `TestTheLibrarysReferenceIsEnforced` |
| **Models** | The library says which package generates them — `.ModelsIn("sessionkit")` — so the host's codegen skips the table and emits no unhooked shadow of it. Migrations are untouched | `TestTheHostsCodegenEmitsNoModelForTheLibrarysTable` |

## Two halves, neither needing codegen

`Declare` is the declaration half. `sessionkit.Session` — a plain struct with
`db` and `sqlb` tags — is the runtime half, and it matters more here than in an
application: the host owns codegen, so a library cannot import its output. The
engine reflects over the tags, so the library queries its own tables with
nothing generated anywhere.

**The host's `sqlb generate` emits no model for the library's table**, because
`Declare` says where its models live: `.ModelsIn("sessionkit")`. It used to emit
one, and that was a hazard rather than a redundancy. Hooks are keyed by Go type,
so a query written against a host-generated `Session` runs with none of the
hooks registered on `sessionkit.Session` — same table, same rows, no
confinement, no error — and the shadow sits in the package the host's author is
already importing ([#284](https://github.com/mind-vm/sqlb/issues/284)).
`TestTheHostsCodegenEmitsNoModelForTheLibrarysTable` pins it, along with the
half that makes it safe: the table is still migrated.

A host model written *on purpose* is a different thing and still works. The
model cache is keyed by Go type, so a narrower hand-written struct addresses the
same rows independently — `TestAHostModelAndTheLibrarysCoexist` asserts that
with two projections over one table. The distinction is whether its author knows
both exist.

The library's query takes an `sqlb.Executor` rather than owning a handle, so the
host decides whether it carries the hook registry. Passing the hooked handle is
how a library's rows end up confined by rules written for tables the library has
never heard of — `TestTheLibrarysQueryRunsThroughTheHostsRules`.

## When a library should not depend on sqlb at all

The convention above assumes the library can take sqlb as a dependency, which is
almost always right: a storage abstraction with one real implementation is cost
without a buyer, and its glue is where the consumer's bugs come from.

The exception is a library with a genuine reason to run with no database — one
that legitimately works in a CLI, a test harness or a one-shot script, where a
pgx dependency would change what the library *is*. That library ships a small
storage interface, and a *separate bridge module* carries the `Declare` above.
The test is not "will consumers use sqlb"; it is "does this library have a real
life without a database". Two interfaces and six methods can be worth it; forty
methods across seven interfaces, bought to gain one implementation and a test
fake `sqlbtest` already provides, is not.

See [docs/schema/libraries.md](../../docs/schema/libraries.md) for the prose
version, and [#281](https://github.com/mind-vm/sqlb/issues/281) for the port
that produced it.
