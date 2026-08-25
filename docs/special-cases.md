# Special cases — a census, and the examples that would settle them

What shapes real Postgres applications actually use, how sqlb handles each one
today, and which worked examples would turn the open questions into answers.

*Written 2026-07-30. The status column is against the tree at that date and will
rot; the census will not.*

*Revised the same day. Every status below that could be settled against a
running Postgres now is, in [`pgtest/census_test.go`](../pgtest/census_test.go)
and [`pgtest/smallcases_test.go`](../pgtest/smallcases_test.go) — fourteen tests,
each one row or one proposed example. Cells marked **measured** were checked
rather than read; four of them came back different from what reading the source
suggested, and those are collected under [What the tests
changed](#what-the-tests-changed).*

*Revised again 2026-08-15. All six proposed examples are now built —
[`tasks-evolved`](../example/tasks-evolved), [`meter`](../example/meter),
[`outbox`](../example/outbox), [`rooms`](../example/rooms),
[`vault`](../example/vault), [`catalog`](../example/catalog) — each entry below
says what its example confirmed and what it corrected. Three corrections are
worth reading before the rest of this document, because they are exactly the
kind of drift the introduction above warns will happen: the arithmetic upsert
(§2) closed in [#90](https://github.com/mind-vm/sqlb/issues/90), the `EXCLUDE`
constraint (§4) shipped in [#121](https://github.com/mind-vm/sqlb/issues/121),
and the self-referencing `Ref` (§6) is now expressible via `TableDef.AddField`.
This document's own age is the demonstration of its first sentence: fifteen
days was enough for three of the six "not expressible" verdicts below to stop
being true.*

## Why this document

The evidence behind sqlb's coverage claims is four builds — `blog`, `tasks`,
`library`, `exchange` — plus two anonymised port reports
([subject-go](review-adoption-port.md),
[subject-mono](review-adoption-port-multi-app.md)). Every one of those was
chosen to prove something, which means the set is biased toward what was already
believed to work. A census is the other direction: count what a codebase that
never heard of sqlb actually writes, then ask what happens to each shape.

The asking is now done against a database rather than against the source, which
is the second half of the same correction: reading the DSL tells you what a
construct *can* express, and only running it tells you what happens when somebody
writes the obvious thing instead.

**Method.** `subject-mono` — the monorepo of the second port report: 40 shared
modules, 12 applications, 334 SQL files (164 goose migrations, 170 sqlc query
files across 75 query directories), Postgres only. Counts below are *matched
lines* from `grep -rIniE … --include="*.sql"`, not tables and not call sites, so
read them as "this shape is common here", not as a workload profile. Status was
first written from reading the DSL and query surface directly
(`schema/field.go`, `schema/table.go`, `expr.go`, `mutate.go`, `builder.go`),
not from the docs; the cells marked **measured** were then checked against a
running Postgres, and say what it did rather than what the source implies.

## The census

| Shape | Lines | sqlb today |
|---|---|---|
| `ON CONFLICT` upsert | 53 | **Partial,** measured. `OnConflictDoNothing` / `OnConflictUpdate(target, cols…)` copy `EXCLUDED.<col>`. An update that is an *expression* — `SET n = t.n + EXCLUDED.n` — has no spelling: upserting 3 over an existing 5 leaves 3, silently and without error |
| `CHECK` constraint | 58 | **Works.** `Table.Check(name, expr)`; the drift check compares Postgres's own spelling, which one commit already had to fix |
| jsonb column | 96 | **Stores fine,** measured. Filtering *into* a document has no operator; `RawPred` reaches both `->>` and `@>` and is the only way in. The corpus never does, which is itself the finding |
| Array column | 26 | **Works** — [ADR-0033](architecture.md#array-columns), `Has`/`HasAny`/`HasAll`, GIN index enforced by lint |
| Composite primary key | 26 | **Not expressible** — [ADR-0034](architecture.md#one-column-addresses-a-row). 15 tables in this corpus; every m2m link table is one. Measured: the refusal is a `Diff` error that names its own workaround — `UniqueIndex`, plus a surrogate key if the table is exposed |
| `expires_at` / `revoked_at` lifecycle columns | 73 | **Works,** and is where the missing bind-parameter cast bites. Measured, and worse than reported: a day filter against a `timestamptz` matches **zero rows and returns no error**, and `Field.Cast` cannot be used to fix it by hand — it returns an `Expr`, which a projection takes and no comparison does |
| Relative time window (`now() - interval …`) | 34 | **`Raw` only,** measured. No interval literal, no `now()`. Computing the instant in Go and binding it works through the typed API and quietly moves the boundary onto the application's clock |
| `GROUP BY` / `date_trunc` rollup | 19 / 5 | **Half,** measured, and worse than reported. `GroupBy`, `Having`, `Sum`, `Count`, `Coalesce` exist, but the natural `Sel(Call{…})` bucket *fails*: a `Param` unit is numbered `$1` in the projection and `$2` in the `GROUP BY`, so Postgres sees two expressions and refuses the query. The unit has to be a `Raw` literal. The REST surface has no aggregate shape at all |
| Polymorphic owner (`tenant_kind` + `tenant_id`) | 9 | **Not expressible as a reference.** Works as two plain columns; loses `?expand` and the FK |
| `LEFT JOIN LATERAL` per-row aggregate | 8 | **Mostly covered** by inverse expansion ([ADR-0022](architecture.md#references-declare-their-inverse)) for count/latest-child; anything else is `Raw` |
| Partial index carrying an invariant | 6 | **Works** via `AddIndex(Index{Where: …})` — but only the struct form, and the predicate is a hand-written string the diff compares textually. Measured, and better than reported: the violation arrives as a `*ConstraintError` carrying the index name, so a 409 *can* say which rule was hit, with nothing to register |
| `soft delete` (`deleted_at`) | 5 | **Works** — `SoftDelete()` + a hook. [FEEDBACK](../FEEDBACK.md) finding 2 argues a view is the better answer |
| `FOR UPDATE` | 4 | **Works,** plus `ForShare` and `SkipLocked` — measured under contention, not merely compiled: four workers over twelve rows claim each row exactly once, and dropping the two calls makes every row claimed four times. Still undocumented; still no example walks it |
| `vector(n)` | 3 | **Not expressible** — [ADR-0026](architecture.md#vectors-declare-their-index), and `introspect` refuses the column, so it cannot even round-trip |
| Range overlap / `EXCLUDE USING gist` | 2 | **Not expressible.** No range types, no exclusion constraints |
| `tsvector` | 1 | **Deliberately out** — [ADR-0037](architecture.md#search-is-ilike-until-it-cannot-be). The blocker is the generated column, not the type |
| `DISTINCT ON` | 1 | **`Raw` only,** measured. `RawSel` reaches it, but only as the *first* projection item — a positional convention nothing enforces, so getting it wrong is a syntax error at the database rather than a build error in Go |
| Idempotency key | 28 | **Works,** measured, and not the way it reads. `OnConflictDoNothing` skips the row, so `One` has no row to return. It answered `ErrNotFound` — a retried payment arriving as "not found" — and since [#146](https://github.com/mind-vm/sqlb/issues/146) it refuses instead, naming both routes out. What returns the first call's row is `OnConflictUpdate(target, target…)`: a write that changes nothing is still a written row, and a written row is a returned one |
| Self-referencing parent (`parent_id`) | 0 here, universal | **Not expressible.** `Ref(name, target *TableDef)` needs the target value, which does not exist yet inside its own `Table(…)` call, and there is no `AddField`. `ExternalRef` compiles but gives up the type and `?expand` — and, measured, the foreign key too: a `parent_id` naming a row that is not there is accepted |
| `WITH RECURSIVE` | 0 | **`Raw`, by design** — [vision](vision.md) non-goals |
| Generated column / trigger / backfill | 1 trigger, 12 backfills | **DDL not rendered.** Hand-written migrations interleave; the "one source of truth" story keeps its asterisk |

Three things stand out from the counts rather than from opinion.

**Upserts are the second most common construct in the corpus** (53 lines, 37 of
them `DO UPDATE`), and the one form sqlb cannot write is the one a counter needs.
`SetExpr` covers increment-on-update — `example/tasks` uses it — so the gap is
narrow and shaped exactly like the metering table that hits it.

**Composite keys are not an edge case here.** 26 lines, ~15 tables, and the
pattern is not exotic: it is `(a_id, b_id)` on a link table and
`(tenant, day, bucket)` on a rollup. ADR-0034 already concedes the refusal is
wider than its argument; the census says how much wider.

**jsonb is stored, not queried.** 96 lines declare it; zero filter into it. That
is a real permission to *not* build jsonb operators, and worth writing down
before someone builds them on intuition.

## What the tests changed

The census's status column was written by reading the DSL and the query surface.
Most of it survived contact with a database. Four rows did not, and the pattern
in the four is worth more than any one of them: **reading the source tells you
whether a construct is expressible, and not whether the expressible spelling is
the one a reader will reach for.** Three of these four have a working spelling
that the obvious attempt is not.

**A `date_trunc` bucket is not "needs `Sel(Call{…})`" — the obvious form does not
run.** Handing the same `Call` to `Select` and to `GroupByExpr` looks like the
one correct answer and is a hard error. The compiler numbers bind parameters per
occurrence, so a `Param{"day"}` unit becomes `$1` in the projection and `$2` in
the `GROUP BY`; Postgres matches `GROUP BY` entries structurally, sees two
different expressions, and rejects the query for not grouping by the timestamp
column. The unit has to be a `Raw{SQL: "'day'"}` literal — safe, because the unit
is a developer-chosen constant, and undiscoverable, because nothing says so. This
raises the `meter` example's value: it would have hit this on its first chart.

**The invariant in a partial index is half-solved already.** The argument below
is that nothing lets the REST layer turn a violation into a 409 naming the rule.
The runtime half is there: the violation arrives as a `*ConstraintError` whose
`Constraint` is the index name the schema chose, so a handler can match on
`one_pending_invitation_per_email` rather than on "duplicate key". Reaching the
name used to need a driver-aware `SetErrorClassifier`, because the standard
library defines no way to read a constraint name off an error; sqlb reads
`*pgconn.PgError` itself since
[ADR-0040](architecture.md#the-driver-is-a-dependency), so that catch is gone. The
open question is therefore narrower than it looked: not "can the layer know
which rule was hit" but "should the schema declare the rule by name, the way
`Check` does".

**The missing bind-parameter cast produces a silent wrong answer, and cannot be
worked around in the builder.** `?day=eq.2026-07-30` against a `timestamptz`
does not fail — it matches zero rows and returns no error, because Postgres
infers the parameter as `timestamptz`, parses the date as midnight, and compares
it for equality against timestamps that are never exactly midnight. There is
nothing to notice. And the hand-written fix is unavailable: `Field.Cast` renders
`"at"::date` correctly but returns an `Expr`, which the projection accepts and no
comparison does, because every comparison hangs off `Field`. `RawPred` is the
only route, and the REST filter parser cannot reach it. That is a stronger case
for the port report's ranking than the report itself makes.

**An idempotency key needs a spelling that looks like a mistake.**
`OnConflictDoNothing` is the obvious call and the wrong one: a skipped row is
absent from `RETURNING`, so `One` had no row to return and answered
`ErrNotFound`, leaving the caller's struct at its zero value — which
`Insert.writeBack` documents and defends, and which turns a retried payment into
a 404. What works is `OnConflictUpdate([]string{"key"}, "key")`, updating the
conflict target to itself: a write that changes nothing is still a write, and a
written row is returned. It is correct, it is one line, and it reads like a typo.

*Updated after [#146](https://github.com/mind-vm/sqlb/issues/146):* the pairing
is now refused at the terminal rather than answered, with a message naming
`Exec` for "make sure it exists" and the line above for "give me the row either
way". The finding stands; what changed is that it is no longer discovered from
production.

Two further numbers the census did not have. `InsertRows` renders one statement,
so bulk insert has a hard ceiling at 65535 bind parameters divided by the columns
actually written — 32767 rows at two columns, with 32768 failing in a driver
message that names neither count. And `DISTINCT ON` is reachable through
`RawSel`, but only as the first projection item.

Everything else the tests touched confirmed the reading: the arithmetic upsert
gap, `FOR UPDATE SKIP LOCKED` holding under real contention, jsonb through
`RawPred`, the composite-key refusal, the empty-set aggregate trap and its
`Coalesce` fix, and `ExternalRef` rendering a self-reference without a foreign
key.

## The shapes worth arguing about

Four of the rows above are not "missing features" but design questions the
project has not had to answer yet.

**An invariant that lives in an index.** `CREATE UNIQUE INDEX … WHERE status =
'pending'` is how this corpus says *one pending invitation per email*, *one
production deployment per project*, *one active passport per agent*. sqlb can
render it, but the predicate is a string, the diff compares it as text, and
nothing in the schema tells a reader that a business rule is in there. This is
the same class of problem as `Check`, and it deserves the same treatment: a
named declaration whose violation the REST layer can turn into a 409 that says
which rule was hit.

The test narrowed this. The 409 half already works — the violation carries the
index name, so a handler can answer with the rule — which leaves the argument
about the *declaration*: whether a business rule should be legible in the schema
rather than inferable from an index name, and whether the diff should compare a
predicate as text. Those are worth arguing about on their own terms, and the
"the layer cannot know" framing above was doing the argument's work for it.

**A write that is an increment.** Metering, rate limits, counters, tallies. The
row may not exist; the update is arithmetic; the concurrency is real. Today that
is `InsertRows(...).OnConflictUpdate(...)` for the shape it cannot express, so
it falls to `Raw` — which is fine as an escape hatch and not fine as the answer
for the second-most-common construct in the corpus.

**A row whose payload only Go may write.** `Hidden()` is the best-tested
capability in the repository — `blog` asserts it against `select`, filters and
the projection, `tasks` against responses, sorts and the OpenAPI document, the
CLI against its own flag set, and `expand_test.go` in both join directions. What
is *not* answered is the other half: a hidden column cannot be written through
the generated create body either (`rest/item.go` reports it as unknown), and the
typed update facade omits it too — finding 2 of the exchange report. So a table
whose payload is entirely hidden keeps a generated read surface and loses its
generated write surface, and nothing says what is left.

**A schema after its first year.** `example/tasks` does evolve — two later
migrations add task labels, and `migrations/drift_test.go` replays the history
and diffs it against the declaration, which is a real gate. But both later
migrations are *additive*, and the feedback reports say plainly that their own
schemas were generated once with `-force` and never changed. The untested half is
the ambiguous and destructive one: renames, backfills, narrowed types, dropped
columns a generated client still names, a constraint added against data that
violates it.

## Proposed examples

Six, in the repository's existing form: each settles one question, is tested
against real Postgres, and says out loud what it deliberately is not. Ordered by
what they would settle per unit of work, not by size.

The tests took the mechanical questions off four of these — whether a lock
holds, what an upsert leaves behind, which predicate a day filter compiles to.
What is left in those four is the part an example exists for and a test cannot
reach: a schema, a generated surface, and a reader following it end to end. The
other two, `tasks-evolved` and `vault`, are untouched, and are now the largest
items on the list. Each entry below says what remains.

### 1. `tasks-evolved` — the second year

**Built:** [`example/tasks-evolved`](../example/tasks-evolved) — but not this
entry alone. `example/evolve` already existed, undiscovered when this document
was written, and independently settles the additive-plus-rename-plus-destructive
shapes described here. `tasks-evolved` covers what `evolve` doesn't: an enum
widened against existing rows, a `NOT NULL` addition with a hand-written
backfill, an array column split into a join table, and — the finding neither
this entry nor `evolve` anticipated — a partial unique index whose `CREATE …
CONCURRENTLY` fails against violating data and leaves an *invalid* index
behind, so a naive retry fails a second time with "already exists" rather than
the constraint violation that actually explains it. See both READMEs.

Not a new application. `example/tasks` already carries two additive migrations
and a drift gate that replays them; this carries it through the six changes that
are *not* additive:

1. rename `status` → `state` (the change `Diff` cannot infer)
2. widen the state enum by one value (a `CHECK` rewrite, in Postgres's spelling)
3. add `assignee_id NOT NULL` with a backfill (DDL sqlb renders, plus DML it does not)
4. split `task_labels` out of an array column (the ADR-0033 shape, reversed)
5. drop a column the generated TypeScript client still selects
6. add a partial unique index that the existing data violates

**Settles:** whether the generated loop survives change, what `migrate` comments
out and what a human must uncomment, whether `check` and the drift gate catch
each one, and what happens to a client generated one commit ago. **Deliberately
not:** zero-downtime choreography beyond what `Diff` can render.

Highest value in the list, and it needs no new feature — only the willingness to
run the loop a second time.

*Nothing here measures any of it.* A second year of migrations is not reducible
to a test: what is in question is a sequence of human decisions and what the
tooling does between them. This entry is unchanged, and is now the largest
untouched item on the list.

### 2. `meter` — the write is an increment

**Built:** [`example/meter`](../example/meter). Leads with a correction: the
arithmetic upsert this entry calls "the reason to build it" is no longer
missing — `OnConflictSet` landed in [#90](https://github.com/mind-vm/sqlb/issues/90),
and the example's `TestArithmeticUpsertUnderConcurrency` is its demonstration
under real concurrent writers, not merely a sequential proof. The composite
key, the `date_trunc` bucket's parameterisation trap, and the empty-range
aggregate's `NULL` trap are all still exactly as described below.

Per-tenant usage: one row per `(tenant, day, kind)`, written by many concurrent
producers, read as a chart.

**Settles:** the arithmetic upsert, whose absence is finding 5 of the exchange
report and whose shape the census confirms; the composite key it wants (and what
the surrogate-PK workaround costs); a `date_trunc` bucket through `Sel`; an
aggregate response over a REST surface that today only returns rows; and
aggregates over an empty range, which today fail in a way one undocumented method
fixes. **Deliberately not:** billing, invoicing, or price.

This is the example that would force one concrete missing feature rather than
document its absence.

*Now measured:* the arithmetic upsert and both its workarounds, the composite
key it wants and the surrogate-plus-unique-index it settles for, the `date_trunc`
bucket — including the parameterised form that does not run — and the empty-range
aggregate. *Still open, and still the reason to build it:* the aggregate REST
response, which has no shape at all today, and whether a chart endpoint over a
metering table can be generated rather than hand-written.

### 3. `outbox` — each row is handed to exactly one worker

**Built:** [`example/outbox`](../example/outbox), and worth a naming note: a
*different*, first-class `outbox` package now exists at the repository root —
a change-feed `Dispatcher` implementing [ADR-0012](architecture.md#change-feed-outbox)
for `rest`'s event stream, unrelated to this entry's competing-consumers job
queue beyond sharing a name and a table-plus-worker shape. The example's
README disambiguates the two explicitly. What this entry asks for — the lock
holding under contention, a retry/backoff/dead-letter policy — remains an
open, explicitly-not-frozen shape, exactly as ADR-0012 says it should.

A transactional outbox and a pool of competing consumers: claim with `FOR UPDATE
SKIP LOCKED`, retry with backoff on a relative time window, dead-letter after n
attempts, wake on `LISTEN/NOTIFY`.

**Settles:** that `ForUpdate`/`SkipLocked` work under contention rather than
merely compile; the relative-time predicate that today needs `Raw`; and — the
real reason to build it — it is the shape of
[ADR-0012](architecture.md#change-feed-outbox). The record says freezing an outbox
format on a guess is the mistake 1.0 exists to avoid. An example is how you stop
guessing without shipping the format. **Deliberately not:** a scheduler, cron, or
exactly-once delivery.

*Now measured:* the claim itself. Four workers over twelve rows take each row
exactly once, and removing `ForUpdate().SkipLocked()` makes every row claimed
four times — so the test has teeth, and the lock has to be held by a `WithTx`
boundary rather than by the statement. The relative-time predicate is measured
too, in both spellings. *Still open:* the outbox record's actual subject — a
format, a retry policy, a dead-letter rule, and `LISTEN/NOTIFY` — none of which
a lock test says anything about.

### 4. `rooms` — two bookings cannot overlap

**Built:** [`example/rooms`](../example/rooms). Leads with a correction: the
`EXCLUDE USING gist` constraint this entry says is "not expressible" now is —
`schema.Exclusion` and `TableDef.AddExclude` shipped
([#121](https://github.com/mind-vm/sqlb/issues/121)), and its own doc example
is almost this exact scenario. The example is the demonstration under real
contention (8 goroutines racing an overlapping confirmed booking; exactly one
wins), not a discovery. The timestamptz day-filter trap below is still real
and still uncorrected.

A room-booking service. The invariant is not a unique key and not a check: it is
that no two confirmed bookings on one room may overlap in time.

**Settles:** what sqlb does with an `EXCLUDE USING gist (room_id WITH =,
tstzrange(starts_at, ends_at) WITH &&)` — today, nothing, so the honest outcome
is either a raw table-level constraint the schema can carry and the diff can
compare, or a documented refusal. It also puts the missing bind-parameter cast
under a test: a day filter arriving as `?day=eq.2026-07-30` against a
`timestamptz` column is the exact `$1::date` case the port report ranks as the
highest-value single addition, and the bug it produces is a timezone bug, which
is the kind that ships. **Deliberately not:** recurrence rules, or calendar sync.

*Now measured:* the day filter, and it is worse than this entry assumed — zero
rows, no error, and no hand-written fix available inside the builder. That much
no longer needs an example. *Still open, and now the whole reason to build it:*
the exclusion constraint. Whether sqlb carries a raw table-level constraint the
diff can compare, or refuses in writing, is a decision nothing here can make.

### 5. `vault` — the row whose payload only Go may write

**Built:** [`example/vault`](../example/vault). Confirms most of this entry
and corrects the sentence just below — "the typed update facade omits it
too" — against what `sqlb generate` actually emits: `SecretUpdate.SetCiphertext`
et al. exist and work, because `Hidden` strips the REST create/update bodies
and the typed *predicate* facade (`SecretCols`), not the typed `Update`
setter facade, which is exactly the trusted-code path a real key rotation
would use. See the example's README, "A third thing, and it is easy to
conflate with the other two."

Secrets for two kinds of owner. Ciphertext, nonce and key material are `Hidden`,
so the generated create and update bodies cannot name them and the typed facade
does not carry them; the owner is polymorphic (`owner_kind`, `owner_id`) with no
foreign key, because the two owner tables live in different modules; every read
is recorded through a hook.

**Settles:** what is left of a generated surface when the payload is unwritable
through it — which is exchange finding 2 turned into a worked answer rather than
a complaint — and gives the polymorphic-owner shape (9 lines in the corpus, no
`Ref` that fits, `ExternalRef` gives up `?expand`) something to point at.
**Deliberately not:** a real KMS, or key rotation.

Not a leak-hunt: `Hidden` is already the best-tested capability in the
repository, in five places and both join directions. This is about the write
side, where nothing is written down.

*Nothing here measures it either.* The question is what a generated surface
*looks like* once its payload is unwritable through it, and that is a shape a
reader has to see rather than an assertion a test can make.

### 6. `catalog` — the tree, and where search stops

**Built:** [`example/catalog`](../example/catalog). Leads with a correction:
this entry and `pgtest/census_test.go`'s own comment both say self-reference
"cannot currently be named at all" because there is "no `AddField`" — there
now is (`TableDef.AddField`), which makes `Category.AddField(schema.Ref("parent",
Category))` expressible, with a real, enforced `FOREIGN KEY` — the opposite of
what `ExternalRef` gave, measured below. Nothing about the search-escalation
half changed.

A product catalog with self-referencing categories and a search box that outgrows
`ILIKE`.

**Settles:** the self-reference, which the DSL cannot currently name at all — the
fix is small (a deferred `Ref`, or an `AddField`) and the shape is universal; and
[ADR-0037](architecture.md#search-is-ilike-until-it-cannot-be)'s escalation path
written down as code rather than prose: `Searchable` → trigram index → the
`tsvector` the record puts out of 1.0 → the vector column
[ADR-0026](architecture.md#vectors-declare-their-index) leaves open. **Deliberately
not:** a ranking model, or relevance tuning.

Last because most of what it would prove is already decided; it becomes a 1.1
example the moment either record moves.

*Now measured:* what the `ExternalRef` fallback costs. It renders the column and
its index, so a tree is storable — and it renders no foreign key, so a
`parent_id` naming a row that does not exist is accepted. The loss is not just
the type and `?expand`; it is referential integrity, on the one shape where a
dangling pointer means an unreachable subtree.

## Smaller cases — a test, not an example

Each of these is one shape, adequately covered by existing features, and
demonstrated nowhere until now. All five are written, in
[`pgtest/smallcases_test.go`](../pgtest/smallcases_test.go) — which is also where
"adequately covered" stopped being a safe assumption: one of the five is covered
by a different call than the obvious one, and two more are covered only up to the
point where an HTTP status has to be chosen or a batch has to be sized.

- **Idempotency key** — 28 lines in the corpus. Written, and the assumption in
  this line was wrong: `OnConflictDoNothing` does not return the first call's
  row. `OnConflictUpdate(target, target…)` does. It returned `ErrNotFound`, and
  after [#146](https://github.com/mind-vm/sqlb/issues/146) the pairing is
  refused outright. See [What the tests changed](#what-the-tests-changed).
- **Optimistic concurrency** — a version column, `Update…Where(version.Eq(n))`,
  and the zero-rows-affected path. Written, and the mechanism is entirely there.
  What is not is the distinction a 409 needs: a stale version and a missing row
  both produce `ErrNotFound`, so telling a conflict from a miss costs a second
  query. The REST layer still has no `If-Match` story.
- **Bulk insert** — written, with the number this line asked for. A thousand
  rows is one statement and unremarkable; the ceiling is 65535 bind parameters
  divided by the columns actually written, so 32767 rows at two columns pass and
  32768 fail in a driver message that names neither count.
- **`DISTINCT ON`** — written. `RawSel` reaches it, but only as the first
  projection item, and the ordering has to be built to match — two conventions
  a caller has to know and nothing enforces.
- **Empty-set aggregate** — written, and it is exactly the trap the exchange
  report names: a bare `Sum` over a filter matching nothing fails to scan, with
  an error naming a column index rather than the empty range. `Coalesce` is the
  fix. `Count` is the exception, which is why the shape survives review until
  the first quiet week.

## Where the answer should stay "no"

Naming these keeps the census from reading as a backlog.

Recursive CTEs, window functions, reporting queries, cross-tenant admin planes
and anything whose shape is a report belong in hand-written SQL, and the
[vision](vision.md) already says so. The census counted them (0 recursive, 1
window function in 334 files) and they are rare precisely because they are the
part of an application nobody generates. `Raw`, `RawPred`, `RawSel` and living
alongside sqlc are the answer, and [with-sqlc.md](with-sqlc.md) is where it is
written.

## What this census does not cover

Stated so it is not read as broader than it is. One corpus, one team, one
architecture — a modular monolith with sqlc and goose, which biases toward
per-module schemas and away from anything a single large schema would show. It
counts SQL text, so a shape expressed in Go (a transaction, a retry loop, a
serialisable isolation level) is invisible to it. It says nothing about
frequency at runtime: a construct on one line may run a million times an hour and
a construct on fifty lines may run at deploy time. And it is a census of what a
codebase *wrote*, which includes what it wrote because its tools made the
alternative hard — the same bias this document exists to correct, one level down.

The tests have their own limit, and it is the same one in a different coat. Each
asks whether a shape *works*, against a schema written to make the question
askable — so they say nothing about whether the shape is discoverable, whether it
composes with the generated REST surface, or what it costs a reader to find. That
is the gap the six examples exist to close, and all six still do — four of them
now on narrower ground, two of them on exactly the ground they started. Several of
the tests assert that something does *not* work; those are written to fail loudly
when a gap is closed rather than to quietly keep passing, which is the only way an
absence stays a decision instead of becoming folklore.
