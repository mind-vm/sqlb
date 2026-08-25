# The road to 1.0

What has to be true before `v1.0.0`, and how we would know it is.

This is a plan, not a schedule. It has no dates in it, because the gating item
is not work — it is evidence, and evidence arrives when it arrives.

## What 1.0 means here

[compatibility.md](compatibility.md) says semantic versioning applies from
`v1.0.0`. That is the whole promise and it is worth stating plainly, because
everything below is downstream of it:

**After 1.0, every surface under *Frozen* stops being revisable.** Not "revisable
with a migration note" — revisable only by a major version, which for a library
with real consumers is a thing you do approximately once. The filter grammar,
the cursor payload, `Executor`, the shape of generated DDL: these are already
frozen in spirit. 1.0 is where the spirit becomes a bill.

So the question 1.0 answers is not "is it feature-complete". It is: **have we
found the mistakes that are expensive to keep?** A missing feature after 1.0 is
an additive release. A wrong wire format after 1.0 is permanent.

That framing decides the ordering below. Anything that would change a frozen
surface is a 1.0 blocker. Anything additive is not, however much someone wants
it.

## The proof gate

The library's own [readiness review](review-adoption-readiness.md) and both
outside evaluations reach the same conclusion by different routes: the thing
sqlb is missing is not a feature, it is **elapsed time under someone else's
traffic**. One author, no observed consumers, and a design that reads well is
still a design nobody has been hurt by.

**1.0 ships when two real applications have been ported onto a sqlb branch and
that branch has run.** Not a pilot endpoint, not a spike — a branch that a
person could merge.

Two, specifically, and not one:

- **A single multi-tenant product** — the shape the first evaluation describes.
  It exercises depth: one schema, a tenant boundary that must hold, a web client
  and a mobile client both regenerated, and a wire-format cutover that has to be
  sequenced across all three.
- **A multi-app monorepo** — the shape the second describes. It exercises
  breadth: many schemas beside a shared platform layer, coexistence with sqlc
  rather than replacement of it, and the question of whether module isolation
  ([ADR-0015](architecture.md#module-isolation)) survives contact with a `core/`.

The two fail differently, which is the point. A port that only proves the depth
case leaves the isolation claim untested; one that only proves breadth leaves
the client-regeneration claim untested. Neither alone is evidence for the other.

### What "ported" has to mean

A port counts as evidence when all of these hold. Anything less is a spike, and
spikes are useful but they are not the gate.

1. **The schema is the source of truth for the ported modules.** `sqlb check`
   passes in the consuming repository's CI, so the generated code cannot drift
   from the declaration.
2. **`sqlb migrate -check` is green against the real migration history.** The
   history builds the declared schema — that is the claim
   [ADR-0014](architecture.md#migrations-and-import) makes, and the port is where it
   either holds against a seven-month history or does not.
3. **Every generated client compiles and its refusals file holds.** A widened
   type is the one failure [ADR-0028](architecture.md#typescript-client) claims
   cannot happen, and a real schema is a much harder test of that than
   `example/tasks`.
4. **The tenant boundary is enforced at startup, not by review.** Every `Scoped`
   model mounts or refuses ([ADR-0030](architecture.md#declared-scope-is-required)).
5. **The existing test suite passes**, including whatever architecture tests the
   consuming repository has. Where a rule had to be rewritten, that is a finding
   and goes in the report.
6. **A written report**, in the shape the two evaluations already use: what
   worked, what was friction, what was a gap, and what the port had to route
   around. A port that produced no findings is a port that was not honest.

### What the ports are allowed to conclude

**"Do not adopt" is a passing result for the gate.** The gate is evidence, not
endorsement. If a port concludes that sqlb is the wrong tool for that codebase
and says clearly why, 1.0 can still ship — the surfaces were exercised and the
mistakes were found. What 1.0 cannot ship on is *no port*, or a port that
stopped early because something was missing.

## What has to change before the freeze

Ordered by whether it touches a surface that stops being revisable. Everything
in stream A and B is a blocker. Everything below is negotiable.

### A. The hole that is a security bug, not a gap — **closed**

**`?expand` did not run the target's hooks.** Both evaluations found it
independently, and [ADR-0030](architecture.md#declared-scope-is-required) recorded
it under Consequences: a `BeforeQuery` hook confines a model's own reads, and an
expansion reached the target through a join that hook never saw. On a
tenant-scoped schema that was a cross-tenant read behind a capability the schema
declared safe.

It is fixed. The expansion runs the target's hooks and requalifies their
predicates onto the join alias, so the hook that satisfies the mount check is
the hook the join carries. Neither of the two answers this plan proposed — carry
the scope predicate, or refuse `Expandable` on an unprotected `Scoped` target —
turned out to be the cheap one; the first is what landed, and it needed no
judgement about which schemas are confined by something the package cannot see.

A predicate that cannot be requalified with certainty fails the query rather
than being dropped, which is the one new refusal. The composite foreign key
remains the stronger arrangement and is now belt-and-braces rather than the only
thing holding.

### B. The driver question — **decided, and it goes the other way**

This was the one item the plan deliberately refused to pre-decide, on the
grounds that doing the work on a guess is how you acquire a permanent interface
for a problem you did not have. The gate was both ports run and both reports
exist. That gate is met, and the answer is
[ADR-0040](architecture.md#the-driver-is-a-dependency): **the engine depends on pgx
v5, `database/sql` stops being the contract, and there is one driver rather than
two.** It lands before the freeze or not at all, because it breaks `Executor`.
It has landed; Phase 4 below is closed.

Neither branch this plan wrote down is what happened, and the reason is worth
recording. The two branches were "the flip is cheap, so the answer stands" and
"the flip degrades something, so `Executor` grows an optional interface." The
ports split the first branch in half: the *bridge* is cheap — one report calls
the pgxpool bridge "a non-event" and finds pgx's `pgtype` values scanning
through it with zero model edits — while the *flip*, moving a platform onto
`database/sql` so a transaction can span a sqlb module and a pgx-native one, is
the expensive half. And the second branch's remedy lost on positioning rather
than on capability: an optional interface delivers pgvector's binary codec only
to callers who opt in, and a codec that is not on by default does not fix the
module that could not port.

What settled it was the multi-app port classifying the driver split as
*architectural* — its first finding — with two pools per process, no shared
transaction, and pgvector's `AfterConnect` codec absent from the sqlb handle, so
its `rag` and `memory` modules "can't port this way". A benchmark
(`mise run bench-pg`) then narrowed the performance claim rather than supporting
it: ordinary CRUD is ~30%, bulk insert is an API gap rather than a driver one,
and the real number is wide float arrays at 2.7× time and 21× memory. The case
rests on the two structural blockers, not on speed.

**What this obliges.** `Executor` is redefined over pgx's types, the
`database/sql` path is removed rather than kept alongside, `deps-check` is
rewritten to enforce pgx-and-nothing-else, and
[compatibility.md](compatibility.md#the-driver) and
[with-sqlc.md](with-sqlc.md) both need rewriting rather than amending —
`compatibility.md` is already updated to say the contract is changing and why.

### C. Type overrides — **closed**

`schema.Type.GoType()` was a closed switch with no override mechanism, and the
first evaluation made the cost concrete: `uuid → string` where the codebase uses
`uuid.UUID` touches middleware, every filter registry, and every use-case
signature.

`codegen.Options.Types` is the sqlc `overrides:` equivalent
([ADR-0035](architecture.md#type-overrides)). The record is mostly about the
boundary rather than the mechanism, because that is the part that would have
been easy to get wrong: an override reaches the models, the typed facade, the
REST bodies and the manifest; it reaches filter coercion too, and must; and it
does not reach the SQL type or the wire, which falls out of every client
emitter mapping from `schema.Type` rather than from the Go type.

### D. The wire format, stated as policy — **closed**

Three things ship as wire format. The filter grammar was already frozen in
`compatibility.md`; the other two were decided in practice and undecided in
writing, which is the state that produces a "we never promised that" argument
later.

Both are now stated, with their cost and their escape
([ADR-0036](architecture.md#the-wire-is-the-column-name)):

| | Rule |
|---|---|
| Field naming | The column's own name, verbatim, on all five surfaces. One spelling, no configuration |
| List envelope | `{items, page, per_page, has_more, next_cursor?, total?}`, one shape per resource |

The record is explicit that the rename cost for a camelCase front end is real
and undiscounted — one evaluation counts 334 tags — and names the two positions
an adopter can take instead, both of which keep exactly one spelling per
deployment. It also names the escape it *would* build if a port stalls on this:
a deployment-wide naming policy, not a per-field mapping layer, because every
guarantee [ADR-0028](architecture.md#typescript-client) makes is about the generated
client having no contents to be wrong.

### E. Schema gaps, and which of them 1.0 needs

Not all of these block. The test is whether a port can complete without them.

| Gap | Blocks a port? | Position for 1.0 |
|---|---|---|
| **Array columns** | Was yes | **Done** — [ADR-0033](architecture.md#array-columns) |
| **pgvector** | Yes, for one module — **confirmed**, and the blocker is not this row | **Not in 1.0.** The port scoped `rag`/`memory` out, and what stopped them is the driver rather than the missing DSL: a text-form vector cannot host pgvector's binary codec however well the schema declares its index. [ADR-0026](architecture.md#vectors-declare-their-index) records the closure; stream B owns the fix |
| **`tsvector` / full text** | Probably | **Decided: not in 1.0** ([ADR-0037](architecture.md#search-is-ilike-until-it-cannot-be)). The blocker is not the column type, it is that a `tsvector` is database-maintained and `migrate` renders neither generated columns nor triggers |
| **Composite primary keys** | ~~Yes, ~15 tables~~ **No** — the multi-app port hit one and it "didn't block me" | **Not in 1.0, and no longer a candidate to hold it.** [ADR-0034](architecture.md#one-column-addresses-a-row) stands as written; the narrowing it concedes is additive and can land after the tag |
| **Generated columns, triggers, backfills** | No | `migrate.Diff` renders DDL only; hand-written migrations interleave. Document the asterisk rather than close it |
| ~~**`Security` on `rest.Options`**~~ | No | **Done** — every generated operation carries the resource's requirement. It documents; middleware still enforces |
| **Parent-scoped routes** (`/projects/{id}/tasks`) | No, but every consumer notices | **Decided: flat, deliberately** ([ADR-0038](architecture.md#collections-are-flat)). The one real cost is that a missing parent is an empty page rather than a 404 |

**The row this section expected to argue about was composite primary keys, and
the ports settled it the cheap way.** The prediction was that ~15 tables would
block. What the multi-app port found instead is that its composite-key table
ported fine — the upsert names the conflict target explicitly, so the missing
declaration cost it a helper rather than the table. It is ranked Medium, third
of its findings, and its own recommendation is a doc note now and the
declaration later.

So the narrowing ADR-0034 concedes — a table never addressed, expanded or
cursor-paged needs no key at all — stays worth doing and stops being a 1.0
question. It is additive in both directions: nothing that compiles today breaks
when it lands.

What is left over is the doc note, which is smaller than the feature and is what
the port actually asked for: a composite-key table can be written against —
`OnConflictUpdate` takes multiple columns and the upsert worked — but it cannot
*declare* its real key, so every PK-derived affordance is off, starting with REST
single-row addressing. Today a reader discovers that per affordance instead of
once. That belongs in the schema docs before the tag.

### F. ADR hygiene — the records must be true at 1.0 — **closed**

Six records sat at **Exploring**, and they were not the same kind of thing. Four
described shipped behaviour and now say so; two are genuinely unbuilt and now say
*that*, with "not in 1.0" in the status rather than left for a reader to infer
from a confidence line.

| ADR | Was | Now |
|---|---|---|
| [0004](architecture.md#schema-as-go-dsl) schema as Go DSL | Exploring | **Working** — every artefact is generated from it |
| [0014](architecture.md#migrations-and-import) migrations by diff | Exploring | **Working** — the loop closes and CI enforces the fixpoint |
| [0019](architecture.md#pgbouncer-in-the-path) PgBouncer | Exploring/Low | **Working** — the carve-outs are tested against a real pooler, not reasoned |
| [0023](architecture.md#mixins-carry-behaviour) mixins | Exploring | **Working as a decision** — the column half ships, the behaviour half is out of scope |
| [0012](architecture.md#change-feed-outbox) change feed | Exploring/Low | **Not in 1.0**, said in the status |
| [0026](architecture.md#vectors-declare-their-index) vectors | Exploring/Low | **Not in 1.0.** A port did need it; what it needed was the driver, not this — see stream B |

Four decisions had **no record at all**. All four now have one: type overrides
([0035](architecture.md#type-overrides)), the wire-format policy
([0036](architecture.md#the-wire-is-the-column-name)), full-text search
([0037](architecture.md#search-is-ilike-until-it-cannot-be)) and parent-scoped routes
([0038](architecture.md#collections-are-flat)).

**The index now has no Exploring row whose subject is shipped**, which was Phase
1's gate.

### G. Port findings, triaged — two land, four are written down

Phase 5's gate asks that every finding from both ports be triaged: fixed before
1.0, scheduled for 1.1, or written down as a known limitation. This is that
triage. Three findings are already answered elsewhere and are listed for
completeness rather than re-argued; one belongs to the platform, not to sqlb.

| Finding | Port | Their rank | Disposition |
|---|---|---|---|
| **Bind-parameter cast** — no `$1::date`; `cmp` wraps even an `Expr` in `Param{}` | single-app | #1, "highest-value single addition" | **Before 1.0** |
| **Expressions in `ON CONFLICT DO UPDATE`** — only `EXCLUDED.<col>` | multi-app | "highest-value sqlb change surfaced by this port", High | **Done** — `OnConflictSet` with `Excluded`/`Current` ([#90](https://github.com/mind-vm/sqlb/issues/90)) |
| **Null-aware negation** — `IsDistinctFrom`, NULL-inclusive `NotOneOf` | single-app | #2 | 1.1, documented now |
| **`$N`-form raw predicate** — `RawPred` is positional, `$N` is referential | single-app | #4 | 1.1, documented now |
| **Registry-aware coercion** — `filter.Coerce` is string→type only | single-app | #5 | 1.1 |
| **`Describe` panics after first use** (`InUse`) | multi-app | Low | 1.1, documented now |
| Module-graph MVS bump (huma/chi, `go` directive) | both | #3 / Low | Answered — [ADR-0007](architecture.md#generated-rest-handlers). The `go` directive half is **fixed**: it was patch-pinned at `1.25.7` for no reason and is now `1.25.0`. The remaining 1.25 floor is huma's, not the engine's — the non-`rest` packages build at `1.21.0` |
| `pgtype` scanning unverified in sqlb's own tests | single-app | — | **Done** — `pgtest/pgtype_test.go`, both directions and NULLs |
| Composite primary key | multi-app | Medium | Stream E above; not a blocker |
| Local `replace` is machine-specific | multi-app | landing blocker | The platform's, not sqlb's |

**Why those two and not the other four.** Both are additive, so neither is
pre-1.0-or-never in the way stream B is — the reason to take them now is that
each was independently named the top ask by a port that had just spent real
effort, which is the strongest evidence this plan set out to buy. The bind cast
is also the only one of the six that the report ties to a *wrong answer* rather
than an inconvenience: it names "a latent non-UTC-TZ date bug" on a ported
endpoint, and the workaround it forced — `RawPred` — discards the typed facade
at precisely the filterable-column case the facade exists for.

**One of these is a second sighting, and that is the argument for it.** The
[2026-07-28 review](review-2026-07-28.md) left "an arithmetic upsert" among its
still-open ergonomics items — parked, unscoped, LOW. A port then hit it in *both*
of its upsert modules and rated it High, because the workaround moves a timestamp
from the database clock to the application clock and forces the column into the
INSERT list. A thing dismissed as ergonomics once and re-found as High by someone
doing real work is a thing that was mis-ranked the first time, and that is a
better reason to schedule it than either report alone gives.

**The four deferred ones are not free**, and the honest cost is documentation
rather than code. Null-aware negation and the `RawPred` positional-vs-referential
mismatch are both *traps*: the first silently drops NULL rows, and the second
500s in production if a `$N` repeats — the port caught it only in a conformance
test, not in its happy-path units. Deferring the API is defensible; leaving the
behaviour unstated is not, so the doc note lands before the tag even though the
feature does not.

## Deliberately not in 1.0

Named so that "it is missing" is not mistaken for "it was forgotten".

- ~~**The change feed** ([ADR-0012](architecture.md#change-feed-outbox)). The biggest
  unbuilt item in [the vision](vision.md), and the one most likely to change
  shape on contact with traffic. Freezing an outbox format on a guess is exactly
  the mistake 1.0 exists to avoid.~~ **Built**, in two halves and in that order —
  the endpoint and the wire format first
  ([ADR-0045](architecture.md#the-stream-is-a-seam)), the transactional outbox behind
  them second, which is what stopped the format from being a guess: the wire was
  settled against a running client a fortnight before the durable source existed,
  and the source then landed without changing it.

  The deferral's reasoning still applies to one thing, so it moves rather than
  disappears: **the delivery semantics** are what a consumer builds against, and
  at-least-once with a replayable position is now a promise. The `outbox` package
  is additive and nothing depends on it, so what 1.0 should be careful about is
  not the format but a client written to skip its own refetch because the stream
  promised catch-up.
- **Nested `?expand`, and backwards cursors.** Both are already named in
  `compatibility.md` under *Will move*, both are additive, and neither changes
  the meaning of a request that can be sent today.
- **Declared actions** ([#18](https://github.com/mind-vm/sqlb/issues/18)). The
  largest item left on the roadmap, and additive — a schema that declares no
  action is unaffected.
- ~~**Computed fields** ([#17](https://github.com/mind-vm/sqlb/issues/17)).~~
  Built, additive, and in: `schema.Computed` with `FromSQL` and `Needs`, per
  [ADR-0041](architecture.md#computed-fields). It was deferred as the strongest
  candidate for 1.1 rather than a blocker; it landed early because it is purely
  additive — a schema that declares no computed column compiles to the same SQL.
  The record's `FromGo` tier is **cut** rather than pending: ADR-0041 set the
  condition "if the first two applications express everything in SQL", and both
  did ([#17](https://github.com/mind-vm/sqlb/issues/17)).
- ~~**`sqlb eject`** ([#19](https://github.com/mind-vm/sqlb/issues/19))~~ Built:
  the schema as SQL and the resources as plain `net/http` handlers over pgx, with
  what it does not carry refused by name and a comparison test against the
  generated resources it replaces ([ADR-0042](architecture.md#the-exit-is-generated)).
  It was deferred as an adoption argument rather than a feature; it landed early
  because the argument is the one thing a pre-1.0 library with no consumers can
  actually answer.
- **`sqlb impact`** ([#21](https://github.com/mind-vm/sqlb/issues/21)). Half of
  it is built — a REST-contract diff against a committed baseline
  ([ADR-0039](architecture.md#a-schema-edit-is-an-api-edit)) — and what the issue
  asks for beyond that is the blast-radius report: which endpoints, which client
  types, which DDL statements one edit touches.
- ~~**A pgx-native `Executor`** — unless stream B says otherwise.~~ Stream B said
  otherwise. It is now the opposite of a deferral: it is the one item that has to
  land **before** the tag, because it breaks `Executor` and afterwards that is a
  major version. See stream B above and
  [ADR-0040](architecture.md#the-driver-is-a-dependency).

## Sequencing

Four phases, each with a gate. The point of the gates is that work stops if one
fails, rather than continuing on momentum.

### Phase 1 — Make the records true — **done**

Stream F plus the two cheap items. Nothing here needed a port and nothing here
was risky, which is why it went first: it is the work that makes the next phase
legible to someone who is not the author.

- ~~Every ADR reaches Working or documents its scope.~~
- ~~ADRs written for type overrides, wire-format policy, full text,
  parent-scoped routes.~~
- ~~`Security` on `rest.Options`.~~
- ~~`compatibility.md` gains the envelope and naming rule under *Frozen*.~~

**Gate: passed.** The ADR index has no Exploring row whose subject is shipped.

### Phase 2 — Close the hole and unblock the ports

- ~~**Stream A**: the `?expand` scope fix.~~ Done.
- ~~**Stream C**: type overrides in `codegen.Options`.~~ Done.
- ~~Whichever of stream E's rows the two target codebases actually hit — decided
  by reading them, not by guessing here.~~ Array columns shipped
  ([ADR-0033](architecture.md#array-columns)) and were the row that had to move
  first. The rest were answered by the ports rather than before them — see the
  stream E table, which now records which predictions held.

**Gate: passed.** Both ports began and completed. That is the only real proof
this gate could have: it asks for the absence of a blocker, and a blocker would
have stopped Phase 3 rather than been spotted here. What the ports found is
friction plus one architectural finding (stream B), not a wall.

### Phase 3 — The two ports, in parallel

They are independent and answer different questions, so running them in sequence
buys nothing but calendar time. Each produces a written report.

**Gate: passed.** Both branches ran, both reports exist
([port](review-adoption-port.md), [multi-app](review-adoption-port-multi-app.md)),
and stream B is decided by what they measured — see B above.

### Phase 4 — The one sanctioned break — **done**

Stream B's answer breaks a *Frozen* surface, so it gets its own phase rather than
being folded into the freeze. Doing it the other way round would mean tagging and
then breaking, which is the failure the freeze exists to prevent.

- ~~`Executor` redefined over pgx, the `database/sql` path removed, `deps-check`
  rewritten to enforce pgx-and-nothing-else with its positive controls intact~~
  ([ADR-0040](architecture.md#the-driver-is-a-dependency)). Done, and it took two
  things with it the phase did not list: `array.go`'s 449-line codec, and
  `sqlb.EncodeArray` — a public function that existed only because
  `database/sql` had no spelling for an array.
- ~~`compatibility.md` and `with-sqlc.md` rewritten to match — `with-sqlc.md`
  inverts, since the advice it gives pgx-generated sqlb users now applies to
  `database/sql`-generated ones.~~ Both done, and `example/withsqlc` is now
  generated for pgx so the inversion is asserted by a compiler rather than by a
  paragraph.
- ~~ADR-0040's revisit triggers checked *before* the work, not after~~ — they
  were, and none fired.

**Gate:** ~~ADR-0040 reaches Working~~ — it reached Accepted, and no other frozen
surface moved. The whole gate passes on it, `pgtest` included.

What the phase did not anticipate, recorded because the next estimate should
know: the engine was the small half. The work was in the three test harnesses
that had each been a registered `database/sql` driver, and in the examples,
where goose still wants a `*sql.DB` and now gets one over the application's own
pool. ADR-0040's *What the port actually cost* has the detail, including the two
bugs the flip introduced.

### Phase 5 — Freeze

- ~~Every finding from both ports triaged: fixed before 1.0, scheduled for 1.1,
  or written down as a known limitation.~~ **Done** — stream G above. Two land
  before the tag, four are written down.
- The two that land: a bind-parameter cast, and expression assignments in
  `ON CONFLICT DO UPDATE`. Both additive, so they can follow Phase 4 rather than
  wait for it, and neither touches a frozen surface.
- The documentation the four deferred findings owe: NULL semantics per operator,
  and `RawPred`'s positional-versus-referential contract. Both are traps today,
  and a trap that is deferred silently is a trap that is shipped.
- `compatibility.md` rewritten from "what `v0.1.0` promises" to the 1.0 contract.
- Tag.

**Gate:** nothing in the *Frozen* list has changed since Phase 4 — the driver
break is the one exception and it is spent by then. If anything else has moved,
the freeze restarts. That is the whole purpose of the phase.

## What would delay 1.0

Stated in advance, so the decision to slip is a decision rather than a drift:

- **A port cannot complete** for a reason that is a sqlb design flaw rather than
  a missing feature. A missing feature is a scheduling problem; a design flaw
  found at 1.0 is the system working.
- ~~**Stream B goes the other way** — the ports show `database/sql` is not enough.
  `Executor` gains an optional interface, and that has to settle before a
  freeze.~~ **This happened**, and further than written: the remedy is not an
  optional interface but a redefinition, which is Phase 4. The delay is therefore
  real and is now scheduled rather than hypothetical. What would delay 1.0
  *further* is that work finding something the benchmark and the ports did not —
  the honest candidate is the scan path's NULL and type-mapping behaviour
  differing from `database/sql`'s conversion rules in a way the existing tests do
  not pin.
- **A frozen surface is found to be wrong.** The filter grammar, the cursor
  payload and the response envelope are the three that would hurt most, and all
  three get their first real exercise during the ports.
- **Only one port happens.** One is a pilot. The gate asks for two because they
  fail differently.

## What this plan does not claim

It does not claim 1.0 makes sqlb mature. It makes sqlb *finished enough to stop
changing underneath people*, which is a smaller and more honest thing. The
readiness review's own exit criterion — six months of someone other than the
author running it against production traffic — is not met by two ports on
branches, and 1.0 does not pretend otherwise.

What two ports do buy is the difference between "the design is good" and "the
design survived contact with a codebase it did not choose." That is the evidence
1.0 is short of today, and it is the only item on this page that cannot be
written.
