# Vision

What sqlb is for, who it is for, and what would make it a success. This is the
most speculative document here — it is a statement of intent, not a plan of
record, and it should be edited whenever the intent changes.

*Last reviewed: 2026-08-08.*

## The problem

Modern application screens are dynamic. A single data view filters on several
columns, sorts by whichever one the user clicked, runs a free-text search,
groups for a summary row, and paginates. The set of queries such a screen can
issue is combinatorial.

Static query generators cannot express that. They compile a fixed SQL string
into a typed function, which is exactly the right tool for a known query and
exactly the wrong one for a view whose shape depends on what the user does.
Teams hit this within weeks of adopting one, and the standard workaround —
building SQL by concatenation for the dynamic parts — gives back every
guarantee the typed generator was adopted for.

So a large fraction of the code between an HTTP handler and the database ends up
being written by hand, repeatedly, per resource: parse the query string, map it
to filters, validate what a client may filter on, assemble SQL, paginate, count,
serialise, and thread authorisation through all of it. It is high-volume,
low-novelty, and a good place for security bugs to hide.

The alternatives each solve part of it. PostgREST and prest make the database
the API, which removes the boilerplate but leaves nowhere for domain logic and
puts the whole schema behind a policy layer. ent gives a language-driven schema
and generated queries, but not a dynamic REST surface. Convex gives schema plus
reactivity, in a different language and a different runtime.

## What sqlb is

A data layer where **the schema is declared once and everything else is derived**
— migrations, models, typed queries, a dynamic REST API, an OpenAPI document, a
typed client — with **explicit seams for domain logic** so that generated code
stays useful when the rules get specific.

Three properties define it:

**Queries are composable values.** Predicates can be added conditionally, which
is the thing static generators cannot do and the reason the project exists.

**One predicate AST, two producers.** A filter arriving over HTTP and a query
written in Go compile through the same path, so bind-parameter discipline and
tenant scoping are enforced once rather than twice.

**Exposure is opt-in per column.** A column is not filterable, sortable or
visible unless it says so. The blast radius of exposing a table is legible from
the schema file, which is what makes a dynamic API safe to point at a real
database.

### Derive, refuse, hand back

The shorter version, and the one to reach for when someone asks what this is:

> Declare once. sqlb **derives** what must agree across surfaces, **refuses**
> what the declaration left ambiguous, and **hands back** the rest — through
> seams for the logic that was always going to be yours, and a supported exit if
> you want the whole thing back.

Each verb is doing work, and the middle one is the one that gets left out.

**Derive** is not "generate most of the code". It is narrower and more useful
than that: what gets derived is everything that must not disagree with itself
across five surfaces at once — a column's spelling, its capabilities, its
nullability, the contract a deployed client couples to. That is the set where a
human keeping two copies in step is the actual failure mode, and it is why the
line is drawn by *kind* rather than by proportion. Nothing here claims a
percentage of an application.

**Refuse** is where most of the design effort has gone, and it produces no code
at all. A schema that says rows are confined obliges a hook, and a resource
without one does not mount ([ADR-0030](architecture.md#declared-scope-is-required)).
A capability nobody declared is a 400 naming what would have been accepted. A
hook predicate that cannot be requalified onto a join alias fails the query
rather than being dropped. A gap a layer below the declaration can see is
reported rather than worked around
([ADR-0051](architecture.md#a-gap-in-the-declaration-is-reported)). This is the
property that separates a dynamic API from pointing PostgREST at your database,
and it is the reason a release can consist almost entirely of things the library
now declines to do.

**Hand back** covers two things worth keeping apart. *Seams* — hooks, declared
actions, [`rest.Reads`](rest/README.md), `Options.Columns` — are where an
application is meant to write code, and reaching for one is not a shortfall. The
port that motivated `rest.Reads` withheld the writes on six of its seven
resources and no two withheld for the same reason; on the write path the
hand-written fraction is routinely the majority, by design. *Exits* are the other
thing: the raw `pgx.Tx` behind `db.Tx()`, the [sqlc pairing](with-sqlc.md) for
the query that wants to be a real query, and [`sqlb eject`](eject.md), which
is a supported end state rather than a hole — it has a CI gate of its own, and
the day a project takes it is the day it deletes that gate.

## Why agents work well with it

This was a design goal, not a retrofit, and it comes down to feedback loops.

**One file to edit.** Schema, capabilities, exposure and relations live in one
declaration. An agent changing the data model edits one thing and runs one
command, rather than reconciling a migration, a model, a handler and a spec.

**Mistakes are compile errors where possible.** The typed column facade turns a
misspelled column, a wrong comparand type, and a text operator on an integer
into build failures. A compile error is a fast, local, unambiguous correction
signal; a runtime error found in a rarely-hit branch is none of those.

**Rejections say what would have worked.** `column is not sortable (allowed:
title, status, view_count)` lets a caller correct itself in one step. Schema
validation behaves the same way, reporting every authoring mistake at once with
the valid alternatives named.

**The reasoning is written down.** The [decision records](architecture.md#decisions) exist so that
someone arriving cold — human or agent — can tell not just what the system does
but which constraints are load-bearing and which are incidental. That is the
difference between a change that fits and a change that has to be reverted.

None of this is specific to agents. It is what makes a codebase pleasant for a
new colleague, and it happens to be exactly what makes one tractable for a
model.

## Non-goals

Being explicit about these is as useful as the goals.

- **Not an ORM.** No identity map, no lazy loading, no object graph persistence.
  You write queries; sqlb makes them safe and composable.
- **Not portable across databases.** Postgres only, deliberately
  ([ADR-0001](architecture.md#postgres-only)).
- **Not a replacement for hand-written SQL.** Reporting queries, recursive CTEs
  and window functions belong in SQL. `Raw` is a supported escape hatch, not an
  admission of failure, and sqlb should stay useful alongside sqlc rather than
  demanding all of a codebase. [with-sqlc.md](with-sqlc.md) says what that looks
  like concretely, and [example/withsqlc](../example/withsqlc) tests the half of
  it that can be tested.
- **Not a way to expose your database.** The opt-in capability model is the
  whole difference between this and putting PostgREST in front of production.
- **Not a framework.** No router, no dependency injection, no opinion about how
  the rest of the application is organised. `Executor` is two methods, and
  `rest` mounts onto a `huma.API` you built on the router you chose, rather than
  handing you one.

## Where it goes

Roughly in order of leverage. Everything below the first item is honestly
speculative and should be reordered as understanding improves.

**1. The generator.** The keystone, and mostly landed: models, typed column
sets, REST request bodies, the registration that mounts them, a TypeScript
client, a Dart client, and a cobra CLI. What remains is migrations — the generator can render
DDL but nothing tells it what changed. `example/blog` is generated end to end,
so every behaviour test in it tests the generator.

That client was described here as the thing the OpenAPI document exists to feed.
It is not, and the correction is the whole reason it needed a generator rather
than an off-the-shelf tool: the document can only say `array<string>` about a
filter parameter, with the operator vocabulary in prose, so anything pointed at
it produces a client where `?status=bogus.x` compiles. It is generated from the
model instead, and stops at a key factory the change feed can consume — see
[ADR-0028](architecture.md#typescript-client) and
[the guide](typescript/README.md). `example/tasks/web` is the worked one,
and its refusals file asserts what must not compile.

The same vocabulary reached Dart next, for a Flutter app
([ADR-0031](architecture.md#dart-client), [the guide](dart/README.md)). Three of
ADR-0028's decisions had to go the other way there — camelCase members, a row
view instead of a data class, and a cursor pager instead of a key factory — and
each is a language fact rather than a preference, which is what makes the split
worth reading if a third language ever follows.

The REST handlers turned out not to need generating at all: one generic function
serves every model, while the *document* is built per resource from the model's
capabilities. See [ADR-0007](architecture.md#generated-rest-handlers), which reversed
on this after it was built.

**2. Migrations.** DDL emission plus a diff against a live database. The
hardest correctness problem in the project, because a wrong diff is
destructive. Expect this to need a review-before-apply workflow and a way to
express what cannot be inferred, such as a column rename.

**3. Relation expansion.** Both directions of one level are built and wired end
to end by codegen. Forward is a LEFT JOIN and a `json_build_object` in the same
statement ([ADR-0025](architecture.md#expansion-is-one-statement)); the reverse is a
correlated subquery per relation, capped and told whether there was more, because
joining a collection would make a page's row count depend on the data
([ADR-0022](architecture.md#references-declare-their-inverse)). A board of lists each
showing its first few tasks is one request rather than an N+1 the client writes.

What is left is nesting under a depth limit, and — the likelier ask — an order
and cap chosen per request, which is an extension to a wire format
[compatibility.md](compatibility.md) freezes rather than a parameter to add
quietly.

Paging, which used to sit here as the other half of "a real screen", is done:
`?cursor=` names a position rather than a distance, so a walk costs the same at
any depth and does not repeat rows when the table is written to underneath it
([ADR-0027](architecture.md#keyset-pagination)). Backwards paging is deliberately
not built; the record says what would change that.

**4. The change feed.** Built, in both halves. The SSE endpoint, the wire format
and an in-process source landed first ([ADR-0045](architecture.md#the-stream-is-a-seam)),
and the transactional outbox behind them — a table written by the same
transaction as the change, a dispatcher woken by `LISTEN/NOTIFY`, at-least-once
across replicas — is the `outbox` package
([ADR-0012](architecture.md#change-feed-outbox)). This is what closes the loop from
"dynamic views" to "live views".

It remains the piece most likely to change shape once it meets real traffic, and
the reason is now specific rather than general: the ordering guarantee is bought
with an advisory lock that serialises writes to published models, and nobody has
measured what that ceiling is under load.

It used to be worth less on its own than the ordering suggests, and that is now
closed. A feed delivers table plus row key and expects the client to refetch,
which is only mechanical if something derives the cache key from that pair —
otherwise every consumer hand-writes an invalidation list that drifts from the
one its own mutations use. Both clients now emit that derivation and the
subscriber around it
([The subscriber is generated, the policy is not](architecture.md#the-subscriber-is-generated-the-policy-is-not)),
so what a consumer writes is which cache to invalidate rather than what to
invalidate in it.

**5. Introspection for agents.** A `sqlb.json` manifest and a generated
`AGENTS.md` describing the schema and its capabilities, plus `sqlb explain` to
print the SQL a query compiles to without running it. Possibly an MCP server
over the manifest. Small work, disproportionate effect on how well the system
can be driven by a model.

Part of this is built, and arrived from the other direction. The generated CLI
([ADR-0029](architecture.md#go-cli)) is introspection an agent can *act* on rather
than only read: one flag per filterable column, the operators its type accepts
in the usage string, so `--help` is a statement of what the resource will accept
and it costs no request. The manifest still describes more than the CLI exposes
— the CLI covers generated CRUD and nothing hand-written — so the MCP server is
the piece that would close the gap, and it should read the manifest rather than
shell out to the binary.

## How we would know it worked

Success is not feature count. Concretely:

- A new dynamic list endpoint — filtered, sorted, searched, paginated, tenant
  scoped — costs a schema edit and no handler code.
- Adding a filterable column is one line and a regeneration, and it is obvious
  from the schema file what the API now exposes.
- Nobody concatenates SQL to work around the query layer.
- Someone unfamiliar with the codebase can read `docs/` and make a correct
  architectural change without asking which constraints matter.
- It composes with sqlc rather than requiring its removal, so adoption can be
  incremental and reversible.

The failure mode to watch for is a generator that produces code people fight.
If generated handlers get copied out and edited by hand, the seams are in the
wrong place, and that is a signal to revisit
[ADR-0007](architecture.md#generated-rest-handlers) rather than to add options.
