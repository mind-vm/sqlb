# Schema and API practices, and which ones sqlb enforces

This page is written to be argued from. Each practice below states what to do,
**why it is right whether or not you use sqlb**, what goes wrong in real schemas
without it, and how sqlb enforces it — or admits that it does not.

The independent reason is the point. A practice justified by "the tool wants it"
is special pleading, and an adopter is right to reject it. Every entry here
should survive sqlb disappearing tomorrow; where one would not, it is in
[Gaps](#gaps-not-opinions) instead.

**Stance markers**

| | |
|---|---|
| **Enforced** | the DSL cannot express the alternative |
| **Recommended** | sqlb imports either; the survey reports on it |
| **Gap** | sqlb refuses something for no reason but that it is unbuilt |

## Where the evidence comes from

Counts come from `sqlb survey` over two production schemas: **valiro-go**
(68 tables) and **mind-vm/studio-apps** (ten independent app deployments,
233 tables) — 301 tables. Practices without a count say so; a practice can be
right without being measured, but it should not pretend to evidence it lacks.

---

# 1. Table and column names

## A module's tables carry its prefix, applied mechanically — **Enforced**

[ADR-0015](architecture.md#module-isolation). `schema.NewModule("billing")` returns a
registry that prefixes everything in it, so declarations use the local name and
the prefix cannot be forgotten:

```go
billing := schema.NewModule("billing")
var Invoice = billing.Table("invoices")   // → billing_invoices
```

**Why, independent of sqlb.** Modules sharing one database collide on the
obvious names — `documents`, `events`, `members`, `deployments`. The collision
does not appear when the table is written; it appears when a second module wants
the same word, which may be years later and someone else's commit.

**What goes wrong without it.** studio-apps documents this rule itself — *"new
modules MUST prefix every table with `<module>_` … `rag`'s tables were renamed
after colliding with an app's `documents` table"* — and enforces it with a
`module-check` CI target. It still drifted: across its core modules, 6 tables
conform, 8 violate the rule, and 16 are permanently grandfathered. The 6
non-grandfathered violations are `agentdeploy`'s `deployment*` tables and
`agentloop`'s `agent_sessions`/`agent_steps` — and those are exactly the tables
that collided when the survey built one database per deployment.

That is the argument in one line: **a rule enforced by review drifts; a prefix
applied by the registry cannot.**

**Prefixes, not Postgres schemas.** ADR-0015 chooses `billing_invoices` over
`billing.invoices` deliberately. Schemas are a deployment model, not a naming
strategy: adopting them means `search_path` management, `CREATE SCHEMA` ordered
ahead of each module's first migration, and per-schema migration tables. Naming
is the easy quarter of that problem, and a tool offering only the naming quarter
implies the rest is configuration when it is not.

## The storage name is not the URL — **Enforced**

The REST path defaults to the *local* name, so `billing_invoices` is served at
`/invoices`. Moving a table between modules is therefore not a breaking API
change.

**Why, independent of sqlb.** Prefixes exist to disambiguate storage. Leaking
them into the public interface exports an internal decomposition that consumers
then depend on, and re-modularising later becomes a breaking change for reasons
that have nothing to do with the API.

## Columns are named the way you want the JSON named — **Enforced**

[ADR-0036](architecture.md#the-wire-is-the-column-name). One spelling from the column
to the JSON key. In practice: `snake_case` in both.

**Why, independent of sqlb.** The alternatives relocate drift rather than
removing it. Renaming columns to camelCase buys quoted identifiers in Postgres
forever. Putting a mapping layer in the transport moves the drift into
hand-written code that sits outside every gate you have.

**The honest cost.** This is the most expensive opinion sqlb holds. One
evaluation counted 1,848 snake_case JSON tags against 334 camelCase — 85%
already matching, with a residual to rename on both sides. Another found
camelCase throughout. For a camelCase front end this lands on every route at
once, and it is tracked as [#116](https://github.com/mind-vm/sqlb/issues/116),
which argues the single spelling could be *derived* rather than literal.
Unresolved — do not present this one as settled.

---

# 2. SQL and schema design

## Timestamps are `timestamptz`, always — **Enforced**

`schema.Timestamp` renders `timestamptz` and nothing else, and `introspect`
accepts `timestamp with time zone` while refusing `timestamp without time zone`.
You cannot declare a naive timestamp.

**Why, independent of sqlb.** `timestamp without time zone` stores a wall-clock
reading with no offset. It is correct only while every writer, reader and server
agrees on a zone, and it silently stops being correct at a DST boundary, on a
server move, or the first time a client in another region writes. The failure
mode is wrong data that looks fine.

**Evidence.** Across all 301 tables, application tables use `timestamptz`
universally — the 82 naive `timestamp` columns in the corpus are *all* inside
goose's own `*_schema_migrations` bookkeeping tables. The practice is already
what good schemas do; enforcing it costs nothing and closes the hole.

`schema.Timestamps()` adds `created_at`/`updated_at` with `Now()` defaults,
read-only and sortable.

## An enum is `text` with a `CHECK`, not a Postgres `ENUM` — **Enforced**

[ADR-0017](architecture.md#enums-as-text-and-check).

**Why, independent of sqlb.** A native enum cannot drop a value: there is no
`ALTER TYPE … DROP VALUE`, only a replacement type, a rewrite of every column
using it, and a drop. `ALTER TYPE … ADD VALUE` cannot run in the transaction that
reads the new value, which drags every unrelated change in that migration out of
its transaction too. And the type is schema-level, so two modules each declaring
`status` collide in a namespace neither owns.

**The honest cost**, stated in the ADR: you lose storage compactness, a defined
sort order, and type-level rejection at every call site.

## A row is addressed by one column — **Enforced**

[ADR-0034](architecture.md#one-column-addresses-a-row). A composite key becomes a
`UniqueIndex`; a table that must be addressed carries a surrogate for identity
*and* the unique index that keeps the real key real.

**Why, independent of sqlb.** A row has one spelling, and it appears in the URL,
the cursor payload, the `?expand` aggregation and the generated cache key. Each
freezes on its first response. `/tasks/{a},{b}` invents an encoding, then needs
an escape rule for a key containing a comma, then a rule for what an unescaped
one means.

**Be precise about the scope, because this is where the argument is usually
overstated.** A table queried only through the builder needs no key at all — a
join table with a genuine composite key, never exposed, is fine as it is, and
`UniqueIndex("a","b")` states the real key. The constraint bites on *addressing*.

**And the trigger has fired.** ADR-0034 says its refusal currently sits in the
registry, which is wider than its own justification, and names the fix: move it
into `rest.Resource`'s mount check and `keysetTerms`. studio-apps supports that —
composite primary keys in 4 of 10 apps, including `agentdeploy`'s
`(deployment_id, key)` configuration tables that no REST resource mounts. If you
are arguing this to another team, argue the narrowing, not the refusal.

## A natural key is a `UNIQUE` constraint, not a unique index — **Recommended**

```go
t.Unique("tenant_kind", "tenant_id", "name")     // UNIQUE (…)  — a constraint
t.UniqueIndex("org_id", "code")                  // CREATE UNIQUE INDEX — an index
```

**Why, independent of sqlb.** Both enforce the same rule, and Postgres backs the
constraint with an index anyway — so they look interchangeable and are not:

- only a constraint can be the target of `FOREIGN KEY … REFERENCES t (a, b)`
- only a constraint can be named in `ON CONFLICT ON CONSTRAINT`

So a key another table references, or one the write path names as its conflict
target, has to be the constraint. Reach for the index when the uniqueness is a
rule you are enforcing rather than a key anything else refers to — and when you
want a *partial* one (`WHERE deleted_at IS NULL`), which a constraint cannot be.

sqlb declares the two separately and will not quietly substitute one for the
other: since [#108](https://github.com/mind-vm/sqlb/issues/108) the constraint
imports as a constraint, named by Postgres's own `<table>_<cols>_key` convention
so an adopted database diffs to nothing.

## A vector declares its dimension; the index is a separate decision — **Enforced**

[ADR-0026](architecture.md#vectors-declare-their-index).

```go
schema.Vector("embedding", ragcfg.Dim).Searchable()
```

**Why, independent of sqlb.** The dimension is a property of the embedder, so
binding it to the same Go constant the embedder uses means the two cannot
disagree silently. studio-apps does it the other way — a
`vector(%%EMBEDDING_DIM%%)` placeholder substituted at deploy time from
`RAG_EMBEDDING_DIM` — and its own config notes that once migrated, changing the
variable does *not* alter the column. That gap between config and column is the
drift this closes.

Confirmed working: `vector(1536)` columns round-trip intact, with no skip note
anywhere in 233 tables.

## Index the leading column of every foreign key — **Recommended** (partly automatic)

`ExternalRef` adds a join index automatically, because it deliberately emits no
`FOREIGN KEY` and the index is the only thing making the join affordable. A local
`Ref` emits a real foreign key and **no** index — you add `.Index("author_id")`.

**Why, independent of sqlb.** Postgres indexes the *referenced* side, never the
referencing one. Without an index on the child column, every `ON DELETE`/`ON
UPDATE` check on the parent scans the child table, and the ordinary "fetch the
children" query does too. It degrades with data volume, so it passes review and
fails in production.

**Evidence.** 8 foreign-key columns across the fleet have no index with that
column leading.

## `text`, not `varchar(n)`, when a CHECK already bounds the value — **Recommended**

**Why, independent of sqlb.** Postgres stores them identically — there is no
performance argument for the cap, only a validation one, and the CHECK is already
doing that job. Two overlapping validations disagree the moment the set changes.

**Evidence.** 227 `varchar` against 481 `text` across the fleet. In valiro this
was four status columns, and converting them took the round-trip residual to
**0**.

## `gen_random_uuid()`, not `uuid_generate_v4()` — **Recommended**

**Why, independent of sqlb.** `gen_random_uuid()` is built in from Postgres 13.
`uuid_generate_v4()` needs the `uuid-ossp` extension present in every database
anyone creates — including scratch and CI databases, where its absence fails once
per table naming a missing *function* rather than once naming the missing
extension.

In valiro this was 30 columns, and removing it dropped a dependency.

Prefer **UUIDv7** for keys you generate: time-ordered, so inserts land at the end
of the index instead of scattering across it.

## Prefer identity over `serial` — **Recommended**, and sqlb declares both

`GENERATED … AS IDENTITY` is the SQL-standard spelling. `serial` is a legacy
pseudo-type that silently expands into three objects — a column, a sequence, and
a default — leaving you owning a sequence you did not write.

```go
schema.BigInt("id").Identity().PrimaryKey()  // recommended
schema.BigSerial("id").PrimaryKey()          // what your database probably has
```

Both are declarable ([ADR-0048](architecture.md#auto-incrementing-keys)), and that
is deliberate rather than indecision: the recommendation is for a column you are
writing now, and the serial exists because moving an existing column between the
two is a migration rather than a rename. Declaring what the database actually
has is what keeps the first diff after an adoption empty.

Neither is a type. A `bigserial` column *is* a `bigint`, so the Go type, the
filter operators and the sort machinery are the plain integer's.

---

# 3. REST

## A page is a position, not a distance — **Enforced**

[ADR-0027](architecture.md#keyset-pagination). A cursor names a position in an
ordering, and the ordering is always total: `Stable()` appends the primary key
unless the ordering already ends with it, and `filter.Apply` calls it on **every**
list, so deterministic paging is not opt-in.

**Why, independent of sqlb.** `OFFSET n` re-scans and discards `n` rows, so page
500 costs 500 pages of work, and any insert or delete between requests shifts
every subsequent page — callers silently skip or duplicate rows. Keyset paging
has neither property.

Where every ordering term runs the same direction and no ordering column is
nullable, the boundary compiles to a row comparison — `(view_count, id) < ($1,
$2)` — which Postgres turns into a single index condition.

## One collection, one path; the parent is a filter — **Enforced**

[ADR-0038](architecture.md#collections-are-flat). `GET /tasks?list_id=eq.<id>`.
`GET /lists/{id}/tasks` is not offered.

**Why, independent of sqlb.** A flat collection composes with the grammar you
already have — sorting, projection, search, paging, expansion, disjunction. A
nested path is a second entry point that must grow each of those separately, or
accept them and mean something subtly different; a caller cannot tell from
`/lists/{id}/tasks?sort=` whether the sort applies to lists or tasks.

## A schema edit is an API edit, and the break is diffed — **Enforced**

[ADR-0039](architecture.md#a-schema-edit-is-an-api-edit).
`restcompat.Diff(old, new *schema.Registry) []Break` is a pure function over two
registries — no database, golden-testable — classifying each delta to response
fields, filter and sort parameters, the create-body required/optional split, the
patch body and the `Op` set.

**Why, independent of sqlb.** If the API is generated from the schema, then a
column rename *is* a breaking API change, and the only question is whether
anything tells you before the client does.

**Note the scope, and do not oversell it.** sqlb checks the contract it
generates. The moment someone writes a custom handler, reshapes a response in a
hook, versions their own `/v1` or fronts it with a BFF, the true contract is no
longer a function of the schema and no longer sqlb's to judge.

## A declaration that rows are confined is an obligation — **Enforced**

[ADR-0030](architecture.md#declared-scope-is-required). Declaring a scope obliges the
machinery to honour it, including through `?expand` — which is where
multi-tenant leaks otherwise live, since an expansion traverses a foreign key
that the row filter never sees.

---

# Gaps, not opinions

These are refusals with no design argument behind them. They are listed on the
same page deliberately: an argument that hides its weaknesses loses the room the
moment an engineer finds them.

| gap | seen in |
|---|---|
| [#121](https://github.com/mind-vm/sqlb/issues/121) `EXCLUDE` constraints | 1 app — no near-miss; dropping it loses an invariant |
| [#114](https://github.com/mind-vm/sqlb/issues/114) `smallint` / [#120](https://github.com/mind-vm/sqlb/issues/120) `real` | 2 apps, 6 valiro columns |
| [#115](https://github.com/mind-vm/sqlb/issues/115) `Diff` renders no `CREATE EXTENSION` | every bootstrap |

**An unsupported column type costs more than one column.** The CHECKs and indexes
over it cannot be declared either — **four of the nine** distinct skip messages
left across studio-apps are cascades of this kind rather than gaps in their own
right, so the counts above understate the cost. With composite `UNIQUE` closed,
cascades are now the largest single category of what remains.

---

# Finding out where a schema actually stands

`sqlb survey` answers all of this against a real database — what imports clean,
what imports partially, what a round trip fails to reproduce. Read-only against
the source, seconds to run.

```bash
go run github.com/mind-vm/sqlb/cmd/sqlb@main survey "$SRC" "$SCRATCH" > survey.md
```

The scratch database must carry the same extensions as the source
([#115](https://github.com/mind-vm/sqlb/issues/115)), and a project whose
migration runner keeps per-module bookkeeping tables needs `-exclude`
([#123](https://github.com/mind-vm/sqlb/issues/123)).

# The rule to triage by

When a survey turns up a difference between a schema and what sqlb can declare,
one question decides which side moves:

> **A schema change must be defensible if sqlb vanished tomorrow.** Where it is
> not, sqlb moves instead — and where the shape is common across projects, sqlb
> moves regardless, because otherwise the tax is paid by every adopter.

Everything in sections 1–3 passes that test on its own merits. Everything under
Gaps fails it, which is why it is sqlb's problem and not yours.
