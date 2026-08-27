# How sqlb compares

Every evaluator builds this table silently. Better to build it here, including
the places where sqlb loses.

Read the [status](compatibility.md) first, because it outranks everything below:
sqlb is pre-1.0, has one author and no observed consumers. Every tool on this
page is older, more used, and more likely to still exist in three years. Nothing
here argues otherwise.

## The short version

Most of this page is not either/or. Three of the five tools below sit happily
beside sqlb in one codebase, and one of those pairings is tested here rather
than asserted.

| Tool | Strongest at | How sqlb relates |
|---|---|---|
| **sqlb** | Filter/sort/search endpoints whose predicates vary per request, and one constraint applied to every read of a model | — |
| **sqlc** | Static queries typed against the real schema, and anything expressible in SQL | **Complementary**, and tested that way: sqlc owns the static queries, sqlb the dynamic list endpoint, both on one transaction |
| **Atlas** | Migrations as a product — multi-engine, declarative and versioned, linted in CI | **Complementary**: sqlb writes migration files, Atlas is a better tool for applying and linting them |
| **Bun** | A mature multi-dialect query builder and light ORM | **Overlapping** at the builder. sqlb adds the URL grammar and the capability model above it |
| **ent** | A mature graph model — traversal, M2M, nested eager loading — with an extension ecosystem | **Overlapping**, and the more complete answer today. sqlb layers over structs ent cannot touch |
| **Drizzle** | Schema-as-TypeScript with compile-time inference, for a TS backend | **The same philosophy in another language.** Not a rival for a Go backend — the question it decides is whether the backend should be Go at all |
| **PostgREST** | An API with no Go code at all | **An alternative**: the same job with the rules in RLS rather than in Go |

The sections below take each in turn, leading with what it does better.

### The rest of the table an evaluator builds

These are not compared at length below, because sqlb's relationship to each is
one line. They are here because leaving them out does not stop anyone reaching
for them — it only stops this page being useful when they do. Star counts and
importer counts checked August 2026.

| Tool | What it is | How sqlb relates |
|---|---|---|
| [**GORM**](https://github.com/go-gorm/gorm) | The default Go ORM. ~40k stars, ~87,000 importers — the most-used database library in Go by a wide margin | **An alternative**, and the first comparison most evaluators make. GORM is a full ORM with associations, callbacks and auto-migration; sqlb is a builder plus an HTTP surface and declines to be an ORM. If your team wants an ORM, this is the one, and the [when not to use sqlb](#when-not-to-use-sqlb) list already says so |
| [**go-jet/jet**](https://github.com/go-jet/jet) | Type-safe SQL builder generated from the live database. ~3.8k stars | **Overlapping at the builder**, and stronger there: jet's column references are generated types, so a misspelling is a compile error rather than sqlb's runtime one. No HTTP layer, no capability model |
| [**stephenafamo/bob**](https://github.com/stephenafamo/bob) | Query builder and ORM generated from the schema, the modern successor to SQLBoiler. ~1.8k stars, ~69 importers | **Overlapping at the builder.** Its importer count is in sqlb's range rather than ent's, which is worth saying plainly after this page's opener claims everything here is more used |
| [**squirrel**](https://github.com/Masterminds/squirrel) | The classic fluent builder, usually paired with sqlc for the dynamic half. ~8k stars, no commit since April 2024 | **Overlapping**, and the pairing sqlb is proposing to replace: squirrel gives conditional predicates and nothing above them, and its maintenance has stopped |
| [**prest**](https://github.com/prest/prest) | "PostgREST in Go" — a standalone binary exposing tables over HTTP. ~4.6k stars | **An alternative to `rest`**, and an honest contrast: [GO-2025-3941](https://github.com/advisories/GHSA-p46v-f2x8-qp98) is a systemic SQL-injection advisory against it, found by an independent review. sqlb's answer to that class is structural — every value is a bind parameter and every identifier is validated against the model — which is a claim worth checking rather than believing |
| [**Supabase**](https://github.com/supabase/supabase) | A hosted Postgres platform built on PostgREST — point it at a table and get a REST and realtime API immediately, plus auth, storage and edge functions. ~99k stars | **The productised form of the [PostgREST row](#postgrest) above**, so the same trade applies: authorization lives in RLS policies rather than in application code sqlb would give you a place to put. `introspect -out` gets sqlb to "point it at an existing database" too, but the columns it emits carry no capabilities and no exposure until a person adds them ([capabilities are opt-in](architecture.md#capabilities-are-opt-in)). Not either-or: [Running sqlb on Supabase](supabase.md) is the arrangement for using the platform's Postgres, Auth and Storage with sqlb serving the tables |
| [**PocketBase**](https://github.com/pocketbase/pocketbase) | A realtime backend in one Go binary — an admin UI defines a "collection" and it is a REST API immediately, no schema code. SQLite by default. ~60k stars, ~544 importers | **An alternative, and the sharpest capability-model contrast on this page**: a PocketBase collection is reachable the moment it exists, where a sqlb column is unreachable until it declares `Filterable`, `Sortable` or similar ([capabilities are opt-in](architecture.md#capabilities-are-opt-in)) and a rejection names what would have been accepted ([actionable errors](architecture.md#actionable-errors)). Also [Postgres only](architecture.md#postgres-only) against PocketBase's SQLite, which is a different foundation, not just a different API layer |
| **The incumbent** | Huma or oapi-codegen for the handlers, hand-rolled query-parameter parsing for the filters, openapi-typescript or hey-api for the client | **This is the workflow sqlb replaces**, and the one most projects are actually on. It works. What it costs is that the allow-list, the rejection messages and the client are three hand-maintained things that drift from the schema independently — which is the whole of sqlb's argument, and why `rest` is built *on* Huma rather than against it |

## sqlc

**What it does better.** sqlc reads your actual SQL and generates types from it,
checked against the real schema at build time. That is a stronger guarantee than
sqlb offers and it is not close: `sqlb.F("titel")` is a runtime error, and the
[typed column facade](queries/typed-columns.md) narrows that without closing
it. Anything expressible in SQL — CTEs, window functions, `DISTINCT ON` — sqlc
handles by definition, where sqlb hands you `Raw`.

**Where it structurally cannot help.** sqlc requires complete, static SQL. A
`WHERE` clause that exists only when a search box is filled in is not a query it
can generate, and the [documented workarounds](https://dizzy.zone/2024/07/03/SQLC-dynamic-queries/)
are `sqlc.narg` with `coalesce`, chains of `(@x::text IS NULL OR col = @x)`, or
"use a query builder". The last one is the honest answer, and it is where sqlb
starts.

**These are not competitors.** [`docs/with-sqlc.md`](with-sqlc.md) is the
coexistence story, and `example/withsqlc` tests it against real sqlc output:
sqlc owns the static queries, sqlb owns the dynamic list endpoint, and
`DB.Tx()` lets both share one transaction. If you already use sqlc, that is the
cheapest way to try this, and
[Refactoring a sqlc endpoint](refactoring-from-sqlc.md) is the step-by-step
version of moving the first one — including what it costs, which at the first
step is more code rather than less.

## ent

The comparison that matters most, because ent overlaps the largest part of the
pitch and does it with Meta's name, years of production use and an extension
ecosystem.

**What ent does better.**

- **Relations are a graph, not a column.** `edge.To`/`edge.From` with `Ref` and
  `Unique` express O2O, O2M, M2O and M2M, and generate traversal predicates
  (`HasPetsWith(...)`) and chained traversal. sqlb has one-directional
  references and one level of forward `?expand`; there is no reverse expansion,
  no M2M vocabulary, and no relation-spanning predicate — see
  [ADR-0022](architecture.md#references-declare-their-inverse) for what is missing
  and why.
- **Maturity.** ~17.2k stars and ~4,000 importers on pkg.go.dev, production use
  at scale, and an extension for this exact job: [`entrest`](https://github.com/lrstanley/entrest)
  generates an OpenAPI spec *and* an HTTP handler implementation with filtering,
  pagination and eager-loaded edges, which covers a large fraction of what
  `rest` does here.

  Two corrections to what this page used to say, both of which cut against
  sqlb's case in one direction and for it in the other.
  [`entoas`](https://github.com/ent/contrib/tree/master/entoas) is spec-only —
  *"Generate a fully compliant, extendable OpenAPI Specification document"*, with
  the README pointing elsewhere for a server — so only entrest generates
  handlers. And entrest is one maintainer, 41 stars and created in June 2024,
  which is a thinner "ecosystem" than the word implies, and thinner than ent
  itself by a wide margin.
- **The privacy layer is the same idea as `BeforeQuery`**, arrived at first.
  Opt in per schema, and it then applies to every query and mutation of that
  type regardless of call site.

**Where sqlb differs, and one place it is genuinely better.**

`Describe[T]` has no ent equivalent. ent cannot be layered over structs it did
not generate, which makes it all-or-nothing per project. sqlb can be pointed at
structs another tool produced — including stock sqlc output — and adds
capabilities without editing them.

Capability opt-in also lives in the core schema here rather than in an
extension's annotations, so one vocabulary drives both the SQL layer and the
HTTP surface. In ent, `field.Sensitive()` is roughly `Hidden`, but the
filterable/sortable exposure that shapes a REST endpoint is entrest's
annotations — a separate extension with its own release cadence.

**One concrete difference worth knowing, because it cuts both ways.** ent's
eager loading issues additional queries: *"it is not possible to load all
associations in a single `JOIN` operation. Therefore, Ent executes additional
query to load each association."* sqlb expands in one statement, a `LEFT JOIN`
and a `json_build_object` per relation
([ADR-0025](architecture.md#expansion-is-one-statement)).

sqlb's version is consistent by construction — one snapshot, so a foreign key
and its expansion cannot contradict each other. That used to come at a real
cost: an early version did not run the target's `BeforeQuery` hooks on an
expansion of it, which is the shape of ent's advantage — **ent's second query
inherits its privacy rules on the target for free**, because it is an ordinary
read of that type. sqlb has since closed that gap rather than accepted it:
the target's hooks now run, requalified onto the join's alias before they
splice into its `ON` clause (or into the collected subquery's `WHERE`, for a
reverse relation) — a scoped-out target nulls out rather than silently
returning unscoped data, and a predicate that can't be requalified with
certainty (raw SQL, one naming a table outside the join) fails the query
rather than being dropped. So this is no longer a trade sqlb makes: one
statement and the target's own policy, together, which ent's N+1 approach
does not get at the same time — its second query is consistent with its own
policy but not with the first query's snapshot.

## PostgREST

**What it does better.** No Go code, no application layer to write, and years of
production use. If the API you want is a faithful projection of your tables and
the rules are expressible as row-level security, PostgREST is less work than
anything on this page.

(This page used to say "no deployment beyond the database", which is not true:
PostgREST is its own Haskell server process to deploy, configure and operate.
The argument does not need the overstatement — what it saves you is the
*application*, not the process.)

**The trade.** Authorization is Postgres roles and RLS; there is no application
layer, so business rules become RLS policies, `SECURITY DEFINER` functions or
views. That is a legitimate architecture and it is the one sqlb declines: the
schema sits one policy mistake away from being public, and there is nowhere to
put Go domain logic.

sqlb's answer is the inverse. A column is unreachable unless it declares a
capability, and the failure is a 400 naming what would have been accepted rather
than a leak — and `BeforeQuery` is a place for the rules that RLS would
otherwise have to carry.

## Atlas

**What it does better.** Nearly everything about migrations. Atlas is a
language-independent schema tool with declarative *and* versioned workflows,
diffing, destructive-change detection, lock-aware linting, ORM integration and
CI/CD across engines. It is a company's entire product.

**What sqlb's `migrate` actually is.** A diff between two registries, rendered
as Postgres DDL, written as files for a runner you already have — with
[lock-aware sequencing](migrations/rollout.md) for the changes whose remedy is
mechanical. It does not apply migrations and does not track which have run.

If migrations are the problem you are solving, use Atlas. sqlb's migration layer
exists so that a schema declared in Go can reach the database at all, not to
compete with a dedicated tool. The two are compatible: Atlas can consume the SQL
files sqlb writes.

**One thing that changed, and that this recommendation should carry.** Since
v0.38 (October 2025), [`atlas migrate lint` is Atlas Pro only](https://atlasgo.io/versioned/lint)
— $9 per developer per month plus $59 per CI project per month. The linting is
the part of Atlas this page was pointing at hardest, so "use Atlas" now has a
price tag on it. Everything else above still holds, and sqlb's own
[lock-aware sequencing](migrations/rollout.md) covers a much narrower set of
changes than a linter does.

## Bun

**What it does better.** A mature, widely used query builder and light ORM,
across Postgres, MySQL, MSSQL and SQLite. It solves the conditional-predicate
problem the same way sqlb does — that is what a builder is for — and it has
years of use behind it.

**What it does not have.** No HTTP layer, no OpenAPI generation, and no
per-column capability model. Building a filter endpoint on Bun means writing the
parameter parsing, the allow-lists and the rejection messages yourself. That
work is the thing sqlb is trying to delete, and it is also the work where a
missing allow-list becomes a leak.

**The sharper difference, for a Postgres project.** Bun is built on
`database/sql` — you hand `bun.NewDB` a `*sql.DB` and a dialect. sqlb is
pgx-native since [ADR-0040](architecture.md#the-driver-is-a-dependency). So Bun cannot
join a `pgx.Tx`: an application already holding one for its pgx or sqlc code
cannot put a Bun query inside it without going through `database/sql` for
everything. That is the whole of the sqlc coexistence story above, and it is not
available on the other side.

## Drizzle

The one entry on this page that is not a Go library, and it earns its section
by being the closest *philosophical* relative here — closer than anything in
Go. [Drizzle](https://github.com/drizzle-team/drizzle-orm) (~35k stars, checked
August 2026) is "headless ORM for NodeJS, TypeScript and JavaScript": the
schema is ordinary TypeScript in your codebase, and
[drizzle-kit](https://orm.drizzle.team/docs/kit-overview) diffs migrations from
it (`generate`), applies them (`migrate`/`push`) and introspects an existing
database back into a declaration (`pull`). Schema as code in the application's
own language, migrations derived rather than written, adoption by
introspection — an evaluator who likes sqlb's shape will recognise all of it,
which is why the comparison belongs here even though nobody chooses between
them for one backend. What this section decides is a prior question: **if your
whole stack is TypeScript, should the backend be Go at all?**

**What Drizzle does better, and Go cannot match.** Type inference. A Drizzle
schema types every column, every query result and every insert at compile
time, for free — no codegen step, no generated files to gate. `db.select()`
against a misspelled column is a squiggle in the editor. sqlb's equivalent is a
runtime error narrowed by the [typed column facade](queries/typed-columns.md)
and caught by `Explain`-as-a-gate in CI, and the jet/bob answer is codegen.
None of the three is as good as what TypeScript's type system hands Drizzle
for nothing. It is also multi-dialect — Postgres, MySQL, SQLite and a long
list of serverless drivers — where sqlb is
[Postgres only](architecture.md#postgres-only), and it has the ecosystem this page
keeps conceding: Drizzle Studio, first-party integrations, and a community
sqlb does not have.

**Where Drizzle stops, and sqlb's argument starts.** Drizzle derives types and
migrations from the schema and nothing else. The HTTP surface is explicitly
your problem — the filter parsing, the allow-lists, the OpenAPI document, and
the API-boundary validation are assembled per project from pieces
([drizzle-zod](https://orm.drizzle.team/docs/zod) for schemas at the boundary,
tRPC or a framework for the transport). There is no per-column capability
model, so "which columns may a request filter by" is a hand-maintained list
with all the drift that implies, and there is no counterpart to the `Scoped`
obligation — a forgotten tenant predicate is a leak, not a boot failure.

**The sharper difference is who the derivation protects.** Drizzle's inference
protects one language, end to end, superbly: a TS schema change breaks the TS
code that contradicts it, including the frontend if types are shared. It
protects nobody else — a Dart app, a Go worker, an agent reading the API are
all outside the inference boundary and drift silently. sqlb inverts the trade:
weaker guarantees inside the backend language, and generated, drift-gated
clients for four languages outside it. Which trade is right is a fact about
your consumers, not about the tools.

## What is actually unique here

Stripped down, one thing on this page is not done elsewhere:

> A single predicate AST that a Go query builder and an HTTP query grammar both
> compile into, with per-column capability opt-in, so a `BeforeQuery` hook
> constrains reads from both — and an unlisted column is a 400, not a leak.

- PostgREST gives the grammar and no place for Go domain logic.
- sqlc cannot express a conditional predicate at all.
- Bun and the typed builders give the builder and nothing above it.
- Drizzle gives the schema-as-code and the derived migrations, in TypeScript,
  and stops before the HTTP surface — the allow-lists, the boundary validation
  and the transport are assembled from pieces per project.
- ent plus entrest is the one real counter-example, and the honest version of
  this claim has to name it: entrest compiles URL filters into ent predicates
  that pass through ent's privacy layer, which is the same shape. What is left
  is narrower — not done elsewhere *in one tool, over structs you did not
  generate, with the capability vocabulary in the core schema rather than in an
  extension's annotations*.

Second, smaller: `Explain`-as-a-gate. It validates a query against the live
schema without running it, so it fails on a migration that was written and never
applied — which a compile-time column check cannot see. It costs a database in
CI, which sqlc's approach does not.

## When not to use sqlb

Stated plainly, because a comparison page that cannot answer this is marketing:

- **You need it to still exist in three years.** One author, no consumers, no
  production history. Use ent.
- **Your queries are mostly static.** The thing sqlb is for does not arise, and
  sqlc gives a stronger guarantee for that work.
- **You need a graph.** Traversal predicates and expansion more than one level
  deep — ent has them and sqlb does not. Reverse expansion it does have
  ([ADR-0022](architecture.md#references-declare-their-inverse)), and many-to-many is
  a junction table queried directly rather than a declaration
  ([ADR-0056](architecture.md#a-junction-is-a-table)); if a graph is the shape of
  your domain rather than an edge of it, ent is still the answer.
- **Migrations are the problem.** Atlas.
- **You are not on Postgres.** sqlb is Postgres-only and
  [will stay that way](architecture.md#postgres-only).
- **Your team wants an ORM.** This is not one, and does not intend to be.
- **Your whole stack is TypeScript and so are all your consumers.** Drizzle
  gives the same schema-as-code philosophy with the compile-time inference Go
  cannot have, and sqlb's remaining edge — generated clients for the languages
  inference does not reach — is an edge you would not be using.

---

*Claims about other projects were checked against their documentation, their
repositories and pkg.go.dev in August 2026, and carry links. They will go stale;
if you find one that has, please open an issue — an out-of-date comparison is
worse than none. [Issue #79](https://github.com/mind-vm/sqlb/issues/79) is what
that looks like working, and four of the six things it corrected made sqlb's
case weaker rather than stronger.*
