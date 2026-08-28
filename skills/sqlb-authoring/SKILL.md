---
name: sqlb-authoring
description: Use when writing or editing a sqlb schema.Field declaration in Go, or asking a question about the DSL's own vocabulary — does Col[T] have Lt/Gt, what does Scoped actually enforce, how do you read a Hidden column from trusted code, how do you get an unhooked handle inside a hook. Covers column types, capability flags (Filterable/Sortable/Searchable/Expandable/Hidden/ReadOnly/Computed/Scoped), predicates, hooks and escape hatches — the DSL's whole surface, not one project's schema.
---

# Writing a sqlb schema

This is the *authoring* direction. It answers "what can this DSL do" — the
same question every session pays a source read to answer, because prose in
`docs/` and doc comments isn't indexed the way a lookup table is.

It is **not** the *consuming* direction: which columns *this project's*
schema actually declared `Filterable` on is a different, per-project question,
answered by the generated `sqlb-schema` skill (`Options.SkillDir`,
[ADR-0049](../../docs/architecture.md#the-skill-is-generated)) — load that one
instead when the question is "can I filter `tasks.author_id`", not "does
`Filterable` exist". This skill can be hand-maintained where that one cannot:
the DSL's vocabulary is the same in every repository, so there is no
per-project fact to drift out from under it the way capability lists drift.
Every symbol below is checked rather than trusted: `mise run skill-check`
fails if a name here is not declared where this file says it is, and if a
declaration method exists that no table below mentions. It said "grounded at a
file:line" for months, and by the time anyone looked almost every line number
was wrong — `Filterable()` pointed at a blank line, `Hidden()` at
`Field.Comment` — while a fifth of the vocabulary was missing outright. Line
numbers are gone: they rot on any edit and a reader greps for the name anyway.

Also not this skill: **where the builder ends and `Raw`/sqlc begins**, and the
four traps that compile and are wrong at runtime — that's
[`sqlb-queries`](../sqlb-queries/SKILL.md). This one is about declaring
columns; that one is about querying them once declared.

## Column types

Constructors live in `schema/field.go`, each returning `*Field` for chaining.

| Constructor | Postgres type | Notes |
|---|---|---|
| `Text(name)` | `text` | |
| `Varchar(name, size)` | `varchar(size)` | |
| `Int(name)` / `BigInt(name)` / `SmallInt(name)` | `integer` / `bigint` / `smallint` | |
| `Serial(name)` / `BigSerial(name)` / `SmallSerial(name)` | serial variants | legacy auto-increment; prefer `Identity()`/`IdentityAlways()` below |
| `Float(name)` / `Real(name)` | `double precision` / `real` | |
| `Numeric(name, precision...)` | `numeric` | |
| `Bool(name)` | `boolean` | |
| `UUID(name)` | `uuid` | |
| `UUIDv7(name)` | `uuid`, server-generated v7 | a `UUID` with a default; the spelling follows `migrate.MinPostgres` — do not hand-write `Default(Expr("uuidv7()"))`, which the `raw-default-has-helper` lint refuses |
| `Timestamp(name)` / `Date(name)` / `Time(name)` | `timestamptz` / `date` / `time` | |
| `JSON(name)` | `jsonb` | |
| `Bytes(name)` | `bytea` | |
| `Vector(name, dim)` | `vector(dim)` (pgvector) | no `Col[T]` and no comparison methods; `sqlb.Near(f, v)` is how it is queried — see "Not expressible" below |
| `Enum(name, values...)` | `text` + `CHECK` | values become the skill/manifest's `Values:` line |
| `Computed(name, type, expr)` | generated column | `FromSQL(sql)` builds the `ComputedExpr` |
| `Ref(name, target)` | foreign key to `target` | `ExternalRef(relation, target)` for a table outside this registry |
| `Map(name, valueType)` | not a column | declares a body property on an action or query — `map[string]V` in Go, `Record<string, V>` in TypeScript. No DDL renders it |
| `Timestamps()` | `Group` — `created_at`/`updated_at` pair | |
| `SoftDelete()` | `Group` — one nullable `deleted_at`, `ReadOnly` | see the capability table below |

`Identity()` / `IdentityAlways()` (`field.go`) are the declarable
auto-incrementing primary key ADR-0048 added — prefer these to `Serial` for a
new table; they're what makes the skill emitter's introspect round-trip keep
the primary key.

## Predicates — what `Col[T]` and `Field` can do

`Col[T]` (`expr.go`) is the typed facade a generated model exposes;
`Field` (`expr.go`) is the untyped one `sqlb.F("name")` returns. Every
`Col[T]` method below is a one-line forward to the matching `Field` method, so
the two have the same repertoire — **yes, `Lt`/`Gt` exist**, on both:

| Method | Renders | Where |
|---|---|---|
| Method | Renders | On `Col[T]` too |
|---|---|---|
| `Eq` / `Neq` | `=` / `<>` | yes |
| `Gt` / `Gte` / `Lt` / `Lte` | `>` `>=` `<` `<=` | yes |
| `Between` / `NotBetween` | `BETWEEN` / `NOT BETWEEN` | `Between` only |
| `OneOf` / `NotOneOf` | `IN` / `NOT IN` | yes |
| `IsNull` / `NotNull` | `IS NULL` / `IS NOT NULL` | yes |
| `EqField(other)` | column-to-column `=` | as `EqCol(other)` |
| `Has` / `HasAny` / `HasAll` (+ `NotHas` / `NotHasAny` / `NotHasAll`) | array `@>` / `&&` variants | no — `Field` only |
| `Contains` / `StartsWith` / `EndsWith` | `ILIKE '%v%'` etc. | no |
| `Like` / `ILike` | `LIKE` / `ILIKE` | no |
| `ContainsJSON(doc)` / `NotContainsJSON(doc)` | jsonb `@>` | no |
| `OnDay(t)` | a half-open day range, index-usable | no |
| `Cast(typ)` | returns `Expr`, not `Pred` | no — reach it as `c.Field().Cast(...)` |
| `Asc()` / `Desc()` | `ORDER BY` | yes |
| `Column()` / `Qualify(alias)` / `Name()` / `Table()` | the expression and its address, for a builder | `Column` / `Qualify` / `Name`; `Field()` returns the untyped one |

The right-hand column is the answer to the question this table exists for: the
typed facade is not a reduced set. Everything a request can express through the
filter grammar has a `Col[T]` method, and the text and array operators hang off
`Field` because they are reached through `sqlb.F` from trusted code.

## Capability flags — effect, and where it's enforced

Every flag below is opt-in: undeclared means the wire rejects the request and
names what would have been accepted (ADR-0006/ADR-0011). All are chainable
methods on `*Field` in `schema/field.go` unless noted.

| Method | Effect | Notes |
|---|---|---|
| `Filterable()` | column usable in `?filter=` | |
| `Sortable(nulls...)` | column usable in `?sort=` | optional `NullsFirst`/`NullsLast` overrides Postgres's direction-following default |
| `Searchable()` | column joins the `?search` fan-out | **implies `Filterable`** |
| `Expandable()` | a `Ref` column resolves inline via `?expand` | refused if the column is not a `Ref` |
| `Inverse(name)` / `InverseExpandable(opts...)` | names the relation pointing *back* from the target, and lets `?expand` follow it | on a `Ref` column. `ExpandOrder(column)` and `ExpandLimit(n)` are the options |
| `Hidden()` | dropped from every REST response **and** from the generated typed-column facade — `Col[T]` for it does not exist, so a predicate against it does not compile | **`sqlb.F("name")` still reaches it**, untyped, from trusted server code. `Hidden` closes the compiled path and the wire; it grants no reach `F` did not already have |
| `WriteOnly()` | same response-omission as `Hidden`, but stays in the generated create/update bodies and the typed facade | for a value written once and never read back through REST |
| `LookupKey()` | declares a `Hidden` column is found *by* its value — `WHERE token_hash = $1` is intended, not a leak | refused on a non-`Hidden` column |
| `ReadOnly()` | unwritable through REST create/update bodies | application code, hooks and actions are unaffected |
| `Immutable()` | writable at create, not at update, through REST | same "REST only" boundary as `ReadOnly` — pair with a `BEFORE UPDATE` trigger for a real guarantee |
| `Scoped()` | declares this column confines every row to one tenant; **every exposed operation must be constrained by a hook** or the resource refuses to mount | see the next section — one of the four named gaps |
| `PrimaryKey()` | implicitly `ReadOnly` + `Filterable` | refused if also `Hidden`/`WriteOnly` — a response needs it to address the row |
| `Nullable()` / `NotNull()` | whether the column may be `NULL` | `Nullable` is refused together with `Scoped` |
| `Unique()` | unique index | `ConstraintNamed(name)` fixes the constraint's name |
| `Indexed()` | a plain index on the column | what the `unindexed-filter` and `unindexed-sort` lints ask for. An index is declared here or it is not there — nothing implies one |
| `Default(d)` | column default | build it with `Value`, `Now`, `GenUUIDv7`, `Expr` — see the `raw-default-has-helper` lint before reaching for `Expr` |
| `Identity()` / `IdentityAlways()` | declarable auto-incrementing key | prefer to `Serial` on a new table; what keeps the introspect round-trip's primary key |
| `Array()` | the column is `T[]` | what `Has`/`HasAny`/`HasAll` operate on. Scalar elements only, one dimension, never `Sortable` or `Searchable`, and a `Filterable` one needs a GIN index |
| `OfType(t)` | overrides the column type | for an `ExternalRef` whose target key is not the conventional UUID |
| `Pattern(re)` / `Min(v)` / `Max(v)` | format rules, carried to every surface | rendered into OpenAPI, the generated bodies and each client — not just checked server-side |
| `Needs(keys...)` | names the binds a `Computed` column's expression takes, in placeholder order | such a column is absent from a write's response, which has no request to resolve it with |
| `OnDelete(a)` / `OnUpdate(a)` | the `Ref`'s referential action | `Cascade`, `Restrict`, `SetNull`, `SetDefault`, `NoAction` |
| `Comment(s)` | documents the column | reaches the DDL, the OpenAPI document and every generated client |
| `Named(column)` | the column's name in the database, where it differs from the field's | |
| `RenamedFrom(old)` | tells `migrate` this column is a rename, not a drop-and-add | the one declaration that makes a migration keep data |
| `SharedAs(name)` | this column is the same column another module declares | a codegen-time claim only: it changes no runtime reach, since `sqlb.F` and a struct tag see a Go string either way |
| `Deferred()` | the column's unique constraint is `DEFERRABLE INITIALLY DEFERRED` | refused without `Unique()` — there is otherwise no constraint to defer |
| `Enforced()` | an `ExternalRef` gets a real foreign key rather than none | gives up the module independence ADR-0015 bought; `?expand` is still refused, since a constraint does not hand this schema the target's columns |
| `CheckNamed(name)` | pins the name of the check constraint this column declares | separate from `ConstraintNamed`, which pins a unique or foreign-key name. Both are for adopting a database whose names are not the ones this package generates |
| `SoftDelete()` (Group) | adds a nullable `deleted_at`, `ReadOnly` | writes no predicate itself — a hook must filter it, the same obligation shape as `Scoped` |

`Name()` and `Desc()` are accessors rather than declarations, and are the only
two `*Field` methods with no row here.

## The four gaps this skill exists for

### `Col[T]` has the full comparison set, not just `Eq`

Covered above — `Lt` `Gt` `Lte` `Gte` all exist on both `Field` and `Col[T]`
in `expr.go`. There is no reduced facade; if a comparison
doesn't compile, the column's type is the reason (comparing a `T` the
operator doesn't accept), not a missing method.

### What `Scoped` enforces, and where

`Scoped()` (`field.go`) is a declaration that the
column confines the table's rows to one tenant. It **writes no predicate
itself** — same as `SoftDelete` — what it changes is what happens when the
confining predicate is *missing*: [`rest.Resource`](../../rest/rest.go)
refuses to mount the resource at startup rather than serve every tenant's
rows with a 200 (ADR-0030). The check is at `rest/scope.go` and the
refusal fires from `rest/rest.go` (`"%s exposes %s but %s declares no
Scoped column"`). It checks that a hook *exists*, not that it does anything
correct — a `BeforeQuery` that logs and returns nil satisfies it.

A table may declare at most one `Scoped` column. It must be
`ReadOnly` — otherwise a create request names its own tenant — and must not be
`Nullable`, since a NULL tenant is outside every tenant's predicate: visible to
nobody today and to everybody the day someone writes `IS NULL OR = $1`. It
cannot be declared on an array, computed or vector column. Every one of those is
a refusal in `schema/registry.go` with a message naming the column, so the way
to learn them is to try one, not to remember this paragraph.

`OpSingleton` (the caller's-one-row resource) refuses to mount without a
`Scoped` column at all — `schema/registry.go` and `rest/singleton.go` —
because a singleton addresses "the caller's row" entirely through the scope
hook; with no scope, GET answers an arbitrary row and PATCH reaches every
row.

### Reading a `Hidden` column from trusted server code

`Hidden()` removes the column from the typed facade and every REST response,
but grants no less reach than was there before: `sqlb.F("column_name")` —
untyped field access — still resolves it, same as any other column. This is
stated directly in `LookupKey`'s doc comment in `field.go`: "a
declaration about Go, on the writer's side of the boundary, where `sqlb.F`
already grants the same reach untyped." Use it from a hook or action that
needs to compare against a hashed credential, for instance — the thing
`Hidden` closes is the compiled path (`Col[T]` doesn't exist for it) and the
wire (filter grammar 400s on it), not application code.

### An unhooked handle inside a hook

A hook's signature hands it only its own model — no `*DB`, no `Executor` — so
[`sqlb.TxFrom(ctx)`](../../db.go) is how it reaches
another model in the same transaction. The handle it returns carries **the
current request's registry**, so a write through it still runs that
request's hooks — including, if writing a *different* model, that model's
own scoping hook. To write past the current rules entirely — an inventory
decrement from an order hook that must not be narrowed by the buyer's own
scope — attach a second, empty registry:

```go
system := sqlb.NewRegistry()  // nothing registered — no rules apply
tx.WithHooks(system)
```

Documented at `db.go` and demonstrated in
[the transaction-scoped-handle decision](../../docs/architecture.md#transaction-scoped-handle)
(`tenant := db.WithHooks(sqlb.NewRegistry()) // hooks: scoped to this
handle`). `db.go` prefers `Update.One` over `Update.Exec` on the escalated
handle so that matching nothing refuses rather than silently committing
(#159).

`rest.Resource` also exposes a narrower version of the same idea —
`Options.Unscoped` releases one *named* scope for one mount, still refused at
startup if the release leaves a `Scoped` column with nothing confining it
(`rest/rest.go`). An unnamed `BeforeQuery` can never be released this
way; naming a scope is what makes it negotiable at all.

## Hooks — the full set

`hooks.go`, in the root package (not `schema`) — hooks are runtime seams, not
declarations. A `*Registry` (`NewRegistry()`,
`hooks.go`) holds per-model hook sets; there is no process-wide default
(ADR-0047) — build one, register into it, attach with `db.WithHooks(reg)`.

| Hook | Signature | Runs |
|---|---|---|
| `BeforeQuery` | `func(ctx, *Builder[T]) error` | before every read of `T` — including reads issued by generated REST handlers. The one that carries tenant scoping |
| `BeforeCreate` / `AfterCreate` | `func(ctx, *T) error` | around insert |
| `BeforeUpdate` | `func(ctx, *Update[T]) error` | before update |
| `AfterUpdate` | `func(ctx, []T) error` | after update, over the affected rows |
| `BeforeDelete` | `func(ctx, *Delete[T]) error` | before delete |
| `AfterDelete` | `func(ctx, int64) error` | after delete, given the row count |
| `AfterDeleteRows` | `func(ctx, []T) error` | after delete, given the rows themselves — separate from `AfterDelete` so a bulk delete only pays to materialise rows when something asked for them |

All defined at `hooks.go`. Hooks registered on one model run in
registration order; a hook returning an error aborts the operation and
reaches the caller unwrapped.

## Escape hatches, gathered

The three ways to reach past what the declared surface normally allows, in
one place because each is documented separately and none of the three
individual doc comments cross-reference the others:

| Need | Hatch | Cost |
|---|---|---|
| Read/write a `Hidden` or otherwise-undeclared column from Go | `sqlb.F("name")` | untyped — a typo compiles and fails at the database |
| Write SQL the builder has no spelling for (window functions, `WITH RECURSIVE`, jsonb edge cases) | `Raw` / `RawPred` / `RawSel` | contents unvalidated; see `sqlb-queries` for the four traps this invites |
| Run a statement past the current request's hooks (a different model's scoping, from inside a hook) | `tx.WithHooks(sqlb.NewRegistry())`, or `rest.Options.Unscoped` for one named scope on one mount | a fresh registry still can't defeat ADR-0030's mount-time check — a `Scoped` column with nothing confining it refuses to mount regardless of which handle asks |

## Not expressible — declared but unqueryable through the builder

A `Vector(n)` column has no `Col[T]` and no comparison methods, and reaching it
with `Raw` is **not** the answer: `sqlb.Near(field, vec)` is, and it exists
because the distance expression appears three times in one statement — as a
projected score, as a threshold, and as the ordering — and three hand-written
copies that disagree sort rows by something other than the number beside them.
`near.Similarity()`, `near.AtLeast(x)` and `near.Nearest()` are those three, and
the vector is written once. The metric is cosine and is deliberately not an
argument (ADR-0026), and the scan is exact — there is no index kind in the DSL
yet, which is the staged second half of that decision.

Composite primary keys are not representable: a row is addressed by one column,
and `UniqueIndex` is the named workaround. `tsvector` full-text search is a gap
too — `?search` is `ILIKE`, not `to_tsquery`. Range types are declarable only as
the operand of an exclusion.

`EXCLUDE USING gist` is **not** a gap: `TableDef.AddExclude(schema.Exclusion{…})`
declares one, with `Elements` and `Where` as hand-written SQL for the reason
`Check` takes hand-written SQL — Postgres stores a parse tree and renders it back
in its own spelling, so a structured form would have to reproduce that spelling
or every diff would propose replacing a constraint nobody changed. It is the one
construct where not declaring it loses a *correctness* property rather than a
convenience. An exclusion over a scalar with `=` needs `btree_gist`, which no
generated DDL creates.

The remaining gaps are ADR-backed decisions rather than oversights — check
architecture.md's Decisions section before proposing a builder extension for
one. `sqlb-queries` has the fuller list and the reasoning.

## What this file does not say

- **Which columns *your* schema actually declared these flags on.** That's
  per-project and this file can't carry it without becoming the thing
  ADR-0049 argues against generating twice. Load the generated `sqlb-schema`
  skill for that.
- **Where the builder ends and `Raw`/sqlc begins**, and the four traps that
  compile and pass tests anyway. That's `sqlb-queries`.
- **Whether an existing codebase should adopt sqlb at all.** That's
  `sqlb-adoption`.
- **Migrations, introspection, or the REST/OpenAPI mount itself.** Those are
  `migrate`, `introspect`, and `rest/doc.go` — read those packages' own
  `doc.go` first, per `CLAUDE.md`'s orientation order.
