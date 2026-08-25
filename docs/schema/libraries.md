# A library that ships tables

Everything else in this documentation is written for an application: one schema,
one registry, one migration sequence, and the whole database in view. A library
is a different problem. Its tables have to coexist with tables it does not own
and cannot see, and the author has to decide what to ship before knowing
anything about the host.

With nothing written down, three libraries in one real application each chose
differently and all three chose wrong
([#281](https://github.com/mind-vm/sqlb/issues/281)):

| What it did | What it cost |
|---|---|
| Owned its schema — its own goose files, tables `documents` and `chunks` | No foreign key can cross to the host, and unprefixed names collide with any content-bearing application |
| Shipped store interfaces and adapters, no schema | Every host re-derives the column set by hand, and the glue is where the bugs were |
| Shipped nothing — no schema, no bridge | Every consumer writes the same Postgres store again |

[`example/libtables`](../../example/libtables/) is the shape that works, with
each claim below pinned by a test.

## First: should the library depend on sqlb at all?

The question that decides this is **not** "will consumers use sqlb". It is:

> Does this library have a genuine reason to run with no database at all?

**Almost always no → depend on sqlb directly.** Declare the tables into the
host's registry, as below. A storage abstraction with one real implementation is
cost with no buyer, and its glue is where consumers' bugs come from. A library
with seven store interfaces and forty methods is buying exactly two
implementations, one of which is a test fake
[`sqlbtest`](../queries/testing.md) already provides.

**Occasionally yes → a storage interface, and a separate bridge module.** A
library that legitimately runs in a CLI, a test harness or a one-shot script —
where taking pgx would change what the library *is* — keeps its core
database-free and ships the declaration from a second module that depends on
sqlb. This is worth it when the storage surface is two interfaces and six
methods. It is not worth it at forty.

State both halves, because the exception reads as the default: most libraries
should just depend on sqlb.

## The convention: declare into the host's registry

A library exports a function taking the host's registry, and the options only
the host can answer:

```go
// in the library
func Declare(reg *schema.Registry, opts Options) Tables
```

```go
// in the host's schema package
var Registry = schema.NewRegistry()
var User = Registry.Table("users", …)

var Session = sessionkit.Declare(Registry, sessionkit.Options{
    Users: User,               // so the reference is real
    Scope: "workspace_id",     // so the rows are confined like everything else
}).Sessions
```

One registry, so one `sqlb generate`, one `sqlb migrate`, one drift check, one
manifest — and references that cross what would otherwise be a boundary.

## Never ship your own migrations

This is the answer an author reaches for first, and the only one that is always
wrong.

A library owning its own sequence owns its own tracking table, so its tables are
created in an order nothing coordinates with the host's. A foreign key across
that line works only when the referenced table happens to be created first —
fine for `accounts` before `companies`, broken for `academy` before `companies`
— and it fails at deploy, not at compile.

Declaring into the host's registry does not answer the ordering question, it
deletes it: every `ALTER TABLE … ADD CONSTRAINT` is emitted after every `CREATE
TABLE`, so no declaration order can make a reference fail to apply.

The cost of getting this wrong is concrete. A session table with an unenforced
`user_id` leaves rows behind when the user is deleted, and a corpus table with
an unenforced `company_id` keeps an indexed copy of a deleted tenant's
documents — which is a retention requirement, not an aesthetic one.

## Table naming: a fixed, library-owned prefix

Prefix every table with the library's own name — `sessionkit_sessions`, not
`sessions`, which would collide with any host that has its own. Make it a
package constant rather than an option.

The reason it is not an option is worth knowing, because the flexible version
looks free. A model names its table statically, through a `TableName` method, so
a host-configurable prefix has to be applied at runtime with
`sqlb.Describe[T]().Table(name)`. That call must happen before any statement
runs against the type and panics if it does not — so an overridable prefix turns
a naming choice into an initialisation-order rule the host can get wrong with no
compile error. `Describe` is still there for the host that genuinely must rename
(adopting a database that already has the table under another name); it should
not be the price of the default.

`schema.NewModule("name")` is the *application's* form of the same idea — it
prefixes every table in a registry — but a library declaring into the host's
registry cannot use it, because the registry is not its own.

## Optional references, in one code path

Whether the reference can be real depends on the host, and both constructors
return a `*schema.Field`, so the choice is a function and not a fork:

```go
func userRef(users *schema.TableDef) *schema.Field {
    if users == nil {
        return schema.UUID("user_id").Filterable().Indexed()
    }
    return schema.Ref("user", users).OnDelete(schema.Cascade).Filterable().Indexed()
}
```

The column is named the same either way, so nothing downstream — a hook, a
filter, the library's own model — has to know which one it got. The library
stays usable standalone, and gains a real key the moment a host has something to
point at.

`schema.ExternalRef("user", "users.id")` is the third option: a relationship
declared by name, carrying no foreign key. It is for a reference that genuinely
cannot be enforced — across a module boundary the application has drawn on
purpose — and not a substitute for the above. See
[References](references.md).

## Confinement travels with the declaration

A library's tables are usually confidential, and the library knows that. What it
cannot know is what confines them — so it declares the obligation and lets the
host discharge it:

```go
type Session struct {
    UserID string `db:"user_id" json:"user_id" sqlb:"filter,scope"`
    …
}
```

`sqlb:"scope"` — the struct-tag form of `schema.Scoped` — means a REST mount
over the model refuses to start until a `BeforeQuery` hook confines it, naming
the column that asked. The obligation travels in the right direction: the
library states that these rows belong to someone, and the host says who. See
[Capabilities](capabilities.md#scoped-so-the-missing-hook-is-caught).

## The runtime half needs no codegen either

A library cannot import the host's generated models — the host owns codegen. It
does not need to: the engine reflects over `db` and `sqlb` tags, so the library
ships a plain struct and queries its own tables with nothing generated anywhere
([Using your own structs](../start/structs-first.md)).

The host's `sqlb generate` will emit a model for the same table. That is not a
conflict: the model cache is keyed by Go type, so two structs address the same
rows independently, and the host's generated resource and the library's own
queries coexist.

Take an `sqlb.Executor` rather than holding a handle:

```go
func Live(ctx context.Context, exec sqlb.Executor, now time.Time) ([]Session, error)
```

That leaves the host deciding whether the statement carries the hook registry —
which is how a library's rows end up confined by rules written for tables the
library has never heard of, including rules written after the library shipped.

## Extension: the host may add a column

A host can `AddField` onto a library's table, and it lands in the migration like
any other column. What it does not do is reach the library's own model, so the
added column is the host's to read through the host's own struct.

Prefer a host-owned table joined by id where the data is really the host's. Use
the added column where it is genuinely a property of the library's row and the
library has no opinion about it.

## Versioning: the declaration is public API

A library's declaration is as public as its Go API, and breaking it is worse:
the host has already migrated. So

- **Adding a column** is a minor change. Give it a default or make it nullable,
  the way any additive migration must.
- **Renaming or dropping one** is a breaking change, and it is the host's
  migration to run — say so in the release notes with the mechanical edit, the
  way [compatibility](../compatibility.md) does here.
- **Changing the prefix** is the worst of them, because it is a table rename
  across every host at once. It is the other reason the prefix is a constant:
  a constant is something a reader can see is load-bearing.

The host's drift check is what catches a library that changed its declaration
without saying so — `sqlb check` compares the composed registry against the
database and names what moved.

## Related

- [`example/libtables`](../../example/libtables/) — the worked version, nine
  tests, no database
- [References and relations](references.md) — `Ref`, `ExternalRef`, and what
  each enforces
- [Hooks](../queries/hooks.md) — what the host registers to discharge the
  confinement obligation
- [Using your own structs](../start/structs-first.md) — the reflection path a
  library's own model uses
