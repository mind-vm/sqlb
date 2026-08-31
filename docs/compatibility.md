# Compatibility

What a tagged release promises, and what it deliberately does not.

sqlb is pre-1.0 and has one author. It no longer has no consumers, and that is
what has changed since this document was written at `v0.1.0`: the breaks below
are not hypothetical any more. `v0.7.0` removed the default hook registry
because the ambient one had switched a real adopter's tenant boundary off
without a compile error, and the five unannounced breaks recorded at the
bottom of this page all came out of consumer reports rather than a plan.

That is the reason this document exists, and the reason it is maintained rather
than archived: an unreleased `main` reads as unknown risk, whereas a tag with a
stated blast radius is something a reader can decide against.

Semantic versioning applies from `v1.0.0`. Until then a minor bump may break a
surface listed under **Will move**, and each break is described in [the release
notes](releases.md) with the mechanical edit that fixes it.

What has to be true before that version — and why the gating item is evidence
rather than features — is [the road to 1.0](release-1.0.md).

## Frozen

These are the surfaces worth freezing early, because they are the ones other
code and other *systems* couple to. Breaking them would invalidate stored data
or deployed clients, not just call sites.

- **`Executor`** — `Query` and `Exec` over pgx's types, and nothing more.
  `*pgxpool.Pool`, `*pgx.Conn` and `pgx.Tx` satisfy it as they stand, and so does
  any wrapper written over them. Widening this interface would break every
  implementation at once, so it grows by adding optional interfaces that are
  type-asserted for, never by adding methods.

  **This entry broke once, deliberately, before 1.0.** It used to be
  `QueryContext` and `ExecContext` over `database/sql`.
  [ADR-0040](architecture.md#the-driver-is-a-dependency) redefined it, because the
  driver was structurally blocking two things sqlb had already committed to.
  That was a pre-1.0-or-never change: after the tag the same work is a major
  version and a hand migration for every consumer. What it bought, and what it
  cost, is [below](#the-driver).
- **The filter grammar** — the URL syntax (`?status=eq.draft`, `?order=`,
  `?select=`, `?limit=`) and its operator names. This is a wire format: a
  deployed client or an agent driving the API off `sqlb.json` has requests built
  against it. New operators are additive; existing spellings do not change
  meaning. `has`, `hasany` and `hasall` were added for array columns
  ([ADR-0033](architecture.md#array-columns)) and are frozen from here on, as are
  their negations `nhas`, `nhasany`, `nhasall` and `nhasdoc`, added later for
  the frontend-parity reason that record's 2026-08-01 revision gives; `contains`
  was *not* extended to mean array containment, and will not be — one name
  meaning two things depending on the column it is applied to is the ambiguity
  the generated clients exist to remove.
- **The wire spelling of a column** — the column's own name, verbatim, in the
  JSON body, the OpenAPI document, the filter grammar's parameter names and both
  generated clients. There is one spelling and no way to configure a second, so
  `?created_at=gte.…` names the same thing the response does. A `Hidden` column
  has no spelling at all. Renaming a column is therefore an API change as well
  as a schema change; `RenamedFrom` makes the database half mechanical and the
  client half is a regeneration plus a compile error per call site
  ([ADR-0036](architecture.md#the-wire-is-the-column-name)). More precisely: there is
  **one spelling per deployment**, computed from the column name by the schema's
  declared `WireCase` — `Verbatim` unless the schema says otherwise, so this
  reads as "the column's own name" for every schema that has not chosen. What is
  frozen is that there is exactly one and that it is derived, not which
  derivation a deployment picked; changing a deployment's `WireCase` is a
  breaking change for that deployment, exactly as renaming a column is.
- **The list envelope** — `{items, page, per_page, has_more, next_cursor?,
  total?}`, one shape for every resource. `next_cursor` is absent when there is
  no next page and `total` only when `?count=exact` asked for it. The key names
  and which of them may be absent are frozen; *adding* an optional key is
  additive and breaks no client that ignores unknown ones.

  **This entry broke once, deliberately, before 1.0, for one relation shape.**
  A reverse `Inverse` relation backed by a single-column `Ref(...).Unique()`
  foreign key can never match more than one row — the constraint already says
  so — so it no longer expands to `{items, has_more}` at all. It expands to
  the target row, or `null`.
  [ADR-0060](architecture.md#a-unique-foreign-key-is-already-one-to-one)
  decided it: the shape was wrong for exactly this case since before the
  envelope was frozen, and the fix was pre-1.0-or-never for the same reason
  `Executor` was — after the tag, the same edit is a major version and a hand
  migration for every consumer. Nothing in this codebase's own examples
  needed that migration, because no schema here declared a unique forward
  reference before the feature that made this decision. The mechanical edit,
  for anyone who did: regenerate the generated Go model and the TypeScript
  and Dart clients, then fix any call site reading `.items`/`.has_more` (Go)
  or `.items`/`.hasMore` (TypeScript, Dart) off that relation's expansion to
  read the value directly — nil-check the pointer in Go, null-check it in
  TypeScript and Dart. Every other relation shape keeps the envelope
  unchanged.

  Building this also closed a gap in `restcompat`/`sqlb impact`: `Capture`
  had never walked `Registry.Inverses` at all, so an exposed reverse relation
  of *any* shape — capped collection or one-to-one — was invisible to the
  contract diff before this work, not merely misdescribed. An adopter with a
  checked-in `restcontract.json` predating this change sees every exposed
  `Inverse` reported as newly additive (`response.<name> field added`,
  `expand.<name> expand relation added`) the first time `sqlb impact` runs
  afterward — additive, so it does not fail `-error`, but it needs one
  `sqlb impact -write` to bring the baseline current.
- **The generated DDL's shape** — `migrate.Diff` output for a given pair of
  schemas may improve, but a migration already written and applied is never
  reinterpreted.
- **The cursor payload** — `?cursor=` and the `next_cursor` field are wire
  format for the same reason the filter grammar is, and so is what a cursor
  decodes to: a client holds one across requests, so changing the payload's
  shape breaks a request already in flight rather than a call site. It is
  base64url of JSON and has room for a version field, but that field has to
  arrive before it is needed. Nothing today reads the payload, which is not a
  reason to treat it as private. See
  [ADR-0027](architecture.md#keyset-pagination).

## Will move

Named in advance, so the break is a documented plan rather than a surprise.

- ~~**Hook registration.**~~ Moved twice, and the second time it broke on
  purpose. After `v0.1.0` it became `sqlb.On[T]()` over a process-default
  `Registry` with `OnIn[T](r)` for a scoped one
  ([ADR-0020](architecture.md#transaction-scoped-handle)). The subtlety this entry
  used to warn about — which registry a statement uses is decided by the
  *dynamic type* of the executor, so passing a raw pool where a scoped
  `*sqlb.DB` was meant silently used the default — turned out to be the whole
  problem rather than a footnote to it, and it switched a real adopter's tenant
  boundary off without a compile error or a failing request.

  So the default registry is **gone** ([ADR-0047](architecture.md#no-default-hook-registry)).
  `On[T](r)` takes the registry and `OnIn` is deleted; `rest.PublishChanges[T](r, p)`
  takes it too and `PublishChangesIn` is deleted; `sqlb.New` gives each handle an
  empty registry of its own, which `DB.WithHooks` is how you fill. Every call
  site that did not name a registry is now a compile error, deliberately: the
  failure this prevents is silent, so its migration must not be. The mechanical
  edits are `On[T]()` → `On[T](reg)`, `OnIn[T](reg)` → `On[T](reg)`,
  `PublishChangesIn` → `PublishChanges`, and a `WithHooks(reg)` on the handle.
- ~~**A computed column's nullability.**~~ Landed: `schema.Computed` now
  defaults to nullable, so the generated field is a pointer unless the
  declaration calls `NotNull()`. It moved because the old default was the one
  reading the expression cannot satisfy — a correlated subquery that matches
  nothing is `NULL`, and the failure was a 500 at scan time on rows a fixture is
  unlikely to contain, from a declaration `generate` and the drift gate were
  both happy with ([#147](https://github.com/mind-vm/sqlb/issues/147)). The
  mechanical edit is `NotNull()` on every computed column whose expression
  genuinely cannot produce a `NULL`; leaving it off is the safe direction, since
  a pointer scans a non-null value fine. Stored columns are untouched, as is the
  structs-first path, where the Go field's own type has always carried this.
- **An index is never inferred from a column's shape.** `ExternalRef` used to
  imply a single-column btree, so a schema that declared one got an index it
  never wrote down. It is gone: `Field.Indexed()` is how a column asks for its
  own index, and `schema.Lint`'s `unindexed-ref` gives the advice the inference
  was carrying ([ADR-0061](architecture.md#ddl-is-declared-never-inferred)).

  It broke because an inference a declaration does not state is one a registry
  read back out of a database cannot honour: `introspect` imports a
  self-referencing foreign key as an `ExternalRef`, so the registry describing a
  live database claimed an index that database did not have, and every migration
  after the first proposed `DROP INDEX` for something nothing ever created
  ([#259](https://github.com/mind-vm/sqlb/issues/259)).

  The mechanical edit is `.Indexed()` on every `ExternalRef` that was relying on
  the implication. **Check before applying the first migration generated after
  upgrading**: for a schema that does not make that edit, the diff proposes
  dropping the index — clean SQL that silently removes an index a join depends
  on, which is why this entry says so rather than leaving it to be discovered.
  `sqlb check` names every unindexed reference under `unindexed-ref`, which is
  the fastest way to find the ones that need the word.
- **A bare date given to `eq`, `ne`, `in` or `nin` on a timestamp column is now
  a 400.** It used to compile to an equality against midnight and match almost
  nothing, returning 200 with an empty page — a "what's on this day" view that
  shipped and answered nothing
  ([#241](https://github.com/mind-vm/sqlb/issues/241)). The refusal names the
  two ways to say what was meant, and the new `day` operator is the first of
  them: `?starts_at=day.2026-09-01` matches that whole calendar day, as a
  half-open range an index can serve.

  The mechanical edit is `eq.` → `day.` where a whole day was meant, or a full
  timestamp where an instant was. A client relying on the old behaviour was
  relying on receiving no rows. `date` columns are untouched, the ordering
  operators are untouched, and a model that does not declare its column types —
  hand-written, without `Describe.SQLType` — is untouched too, since an unknown
  type is not evidence of a mistake.
- **Terminal call signatures**, when Go 1.27 arrives. `sqlb.Collect[R](ctx, db,
  b)`, `filter.Apply(b, q)` and the `db` threaded through every terminal call
  all gain method forms, because a method on a concrete type cannot introduce a
  type parameter before then. These are additive — the functions stay.
- **Nested `?expand`.** One level resolves today. If nesting lands it arrives as
  a longer name — `?expand=list.workspace` — under a depth limit, so nothing a
  request can send today changes meaning.
- ~~**`schema.Action`, the referential one.**~~ Landed with declared actions
  ([ADR-0043](architecture.md#declared-actions)), which needed that noun for a
  domain verb. The foreign-key type is now `schema.RefAction`; the constants
  every call site actually writes — `schema.Cascade`, `schema.SetNull` and the
  rest — are unchanged, so a schema breaks only if it named the type. The
  mechanical edit is `schema.Action` → `schema.RefAction` in a foreign-key
  position.
- **Backwards cursors.** Paging goes forward only. If `?before=` lands it is a
  new parameter alongside `?cursor=`, so again nothing a request can send today
  changes meaning. [ADR-0027](architecture.md#keyset-pagination) says what would
  make it worth building.
- **The `sqlb` command's verb names**, which are still settling. `v0.8.0` moved
  one: `sqlb-survey` became `sqlb survey`, a second binary folded into the one
  command tree ([ADR-0032](architecture.md#sqlb-command)). The mechanical edit is
  `./cmd/sqlb-survey …` → `./cmd/sqlb survey …`, and it is the cheap kind of
  break — a script that invoked the old name stops with "no such file" rather
  than doing something subtly different.

  Listed here rather than under *Not covered* because a command line is
  something a person memorises and a CI file hardcodes, so it deserves the
  announcement even though the code behind it is a build-step tool. What it
  does not get is the *Frozen* promise: the boundary ADR-0032 states is that
  needing no schema package is a fact about a verb's arguments rather than a
  reason for a separate binary, and any verb still on the wrong side of that
  moves the same way this one did.

- **The emitted agent skill** — its path, and everything about the document's
  shape. `Options.SkillDir` writes `<SkillDir>/<SkillName>/SKILL.md`, defaulting
  to `sqlb-schema`, and both halves of that are expected to move: the `SKILL.md`
  frontmatter and directory convention belong to the agent tooling rather than to
  sqlb, so a change there is a change to this output with no deprecation window
  sqlb is in a position to offer
  ([ADR-0049](architecture.md#the-skill-is-generated)).

  This is the cheapest kind of break to own, and that is the reason it is
  allowed to be here at all: the file is generated, so the mechanical edit is
  `sqlb generate`, and `sqlb check` names the file when it has drifted. The
  emitter is also opt-in — a project that never sets `SkillDir` has no exposure.
  What would *not* be cheap is the reverse, so it is stated here rather than
  discovered: if this emitter is ever removed, the verb has to delete the file
  rather than stop writing it. A stale skill still loads, and an instruction file
  that is confidently wrong about a schema it no longer describes is worse than
  an absent one.

- **The emitted fx wiring** — `wiring_gen.go`'s exact shape, though not the
  fact that `FxModule` is a composable `fx.Option` value, which is the load-
  bearing property and is frozen. `Options.WiringMigrations` /
  `WiringOperations` and the `WiringSet` field names are new and still
  settling — this is the first release carrying them
  ([ADR-0059](architecture.md#fx-wiring-is-generated-not-a-runtime-library)).

  Same reasoning as the skill: generated, opt-in, and `sqlb check` names the
  file when it drifts, so the mechanical edit is `sqlb generate`. A project
  that never sets `WiringMigrations` or `WiringOperations` has no exposure.

- ~~**`rest.Serve`'s mount callback.**~~ Landed: `mount` is now
  `func(*rest.Server, *sqlb.DB) error` rather than taking a `sqlb.Executor`.
  `Serve` builds the handle itself, so it always knew the concrete type; handing
  out the interface meant every mount that attaches a hook registry — which is
  the seam's own headline use, since `WithHooks` and `WithTx` live on `*sqlb.DB`
  and not on `Executor` — opened with a type assertion to recover what the
  caller of the callback held one frame up
  ([#277](https://github.com/mind-vm/sqlb/issues/277)).

  The mechanical edit is deleting that assertion, and nothing else: `*sqlb.DB`
  satisfies `Executor`, so a mount that only passes `db` on to a generated
  `Register` compiles unchanged. Listed here after the fact rather than before
  it — the entry is owed either way, and the break is the cheap kind, a compile
  error at one line in one function per application.

- **A declared action that writes obliges a `BeforeUpdate`.** An action on a
  `Scoped` model whose `Writes` is non-empty now refuses to mount unless a write
  rule is registered, where it previously needed only the `BeforeQuery` its
  fetch runs
  ([#308](https://github.com/mind-vm/sqlb/issues/308)).

  The mechanical edit is registering the hook — the same one such a schema
  already has when it exposes a `PATCH`, which is why the break is invisible to
  most tables and loud on exactly the ones it is about. A verb declaring no
  `Writes` is unaffected, and so is any model that is not confined.

  It broke because the fetch's confinement answers "whose row is this" and was
  read as answering "who may write it". Those differ wherever a tenant has more
  than one kind of member: a consumer's child set the parent's PIN through a
  route that mounted cleanly, and the sibling route was safe only because a hook
  registered for its own `PATCH` happened to cover it.

- **Format rules on a value narrow what a request may carry.** `Pattern`, `Min`
  and `Max` are new and additive — a schema that declares none is unchanged —
  but *declaring* one on an existing column is a wire break, and a quiet one:
  the type does not move, so nothing about the column's shape gives it away
  while every request carrying a value outside the rule starts getting a 422
  ([#311](https://github.com/mind-vm/sqlb/issues/311)).

  `sqlb impact` reports it, which is the point of recording the rules in the
  contract snapshot rather than only in the generated tags. Tightening is
  breaking; loosening is reported as *unknown* rather than neutral, because a
  generated client still enforcing the old rule refuses input the server now
  accepts.

  These validate a request and write no DDL. A row arriving from a migration, a
  seed or a job is unchecked, so a rule the table must hold is still `Check`.

- **A grouped query read with `All` is refused.** `Builder.All` on a query that
  declared `GroupBy` now returns an error when the projection carries a column
  the model has no field for, where it used to scan the rest and drop that one
  ([#306](https://github.com/mind-vm/sqlb/issues/306)).

  The mechanical edit is `sqlb.Collect[R](ctx, db, q)` with an `R` carrying a
  `db` tag per projected column — which is what the code wanted in the first
  place, since the dropped column is the aggregate. A grouped query whose
  projection the model can hold is unaffected, and so is every ungrouped partial
  select.

  It broke because the old behaviour was silently wrong rather than merely
  permissive: the rows came back the right length, with the grouped column set,
  a zero where the count should be, and a nil error. A consumer read that as
  "the builder can express GROUP BY and cannot read what it returns" and shipped
  an N+1 loop instead.

- **`schema.Field.Scoped` takes an optional scope name.** `Scoped()` is
  unchanged and every existing schema compiles; `Scoped("workspace")` groups a
  column with the others answering the same question, which is what lets
  codegen emit one resolver for the sixteen tables carrying one tenant
  reference ([#274](https://github.com/mind-vm/sqlb/issues/274)).

  A new `scopes_gen.go` appears for any schema with a scoped column that is not
  an unnamed primary key. Nothing calls it until you do, so the file arriving
  changes no behaviour — but it is a new generated artefact, so `generate-check`
  fails until the schema is regenerated.

- **`schema.Query` takes a `Returns`.** Additive: a query declaring none still
  generates `func(...) ([]T, error)` and every existing declaration is
  unchanged. Declaring one changes that field's signature to a slice of the
  emitted result type, so the func assigned to it has to change with it — a
  compile error at the call site, which is the point
  ([#240](https://github.com/mind-vm/sqlb/issues/240)).

  It is what a read whose answer is not rows of the table needs: a rollup, a
  per-bucket sum, a count per group. Those were hand-mounted beside the
  generated `Register` because codegen always emitted `[]T`; `rest.Query` was
  already generic over its result, so nothing about the runtime changed.

- **`schema.Map` declares a map-shaped body property.** Additive: a new
  constructor and a new `schema.TypeMap`, neither reachable by an existing
  declaration ([#327](https://github.com/mind-vm/sqlb/issues/327)).

  It is body-only. `Registry.Validate` refuses it on a table, no DDL renders
  one, and `schema.Types()` does not list it — a `jsonb` column stays `jsonb`.
  Changing a declared map's *value* type is a break, and `sqlb impact` reports
  it: the value type is not in the type string, so every client's generated
  type changes while `map` stays `map`.

### Six that broke without being listed here first

`v0.6.0` broke three surfaces that were not under *Will move*, `v0.11.0` broke
a fourth — which is the first one finishing — `v0.13.0` broke a fifth, and the
sixth is a server that had been more permissive than its own contract. The
honest version is that all five came out of consumer reports rather than a
plan. The release notes — [v0.6.0](releases.md#v060),
[v0.11.0](releases.md#v0110) and [v0.13.0](releases.md#v0130) — carry each
with its mechanical edit, which is the other half of the promise above; this
is the half that was missed, recorded where the announcement should have
been.

- **A computed column is opt-in on reads** (`v0.6.0`). `sqlb.Query[T]()` no
  longer projects declared computed columns; `WithComputed(names…)` asks for
  them, and `rest.Options.Computed` is the hand-written mount's form. A
  generated resource opts into its own table's, so generated endpoints are
  unchanged. It broke because the default charged every reader of a shared model
  for one screen's aggregates, and made an existence check by id fail for want
  of a bind it had no business supplying.
- **And on writes** (`v0.11.0`). `Insert`, `Update` and `Delete` evaluate no
  computed column in `RETURNING` unless `WithComputed(names…)` asks, and
  `rest.Options.Computed` narrows a write's `RETURNING` as it already narrowed
  the read's projection — so a `POST` or `PATCH` response carries a derived
  field only where the mount declared it, and the key is absent rather than
  zero. A column carrying `Needs` is refused there rather than skipped.

  This is the entry the section exists for. The write path was the same break
  and it was not revisited when the read path flipped, so the announcement was
  owed at `v0.6.0` and is being made five releases late. It broke for the
  reasons the read half did and one more: a create whose subquery counts rows
  the same transaction has not written yet returns a value that is *always*
  wrong, so the second read the clause existed to delete had to come back
  anyway ([#164](https://github.com/mind-vm/sqlb/issues/164)).
- **The generated Go client is its own package.** `cli.New` takes a
  `*client.Client` from the emitted `client` package. Regenerate, then the edit
  is in the four-line main. It broke because a program wanting the typed encoder
  could not take it without also taking cobra.
- **A nil member of `OneOf` widens the set** rather than binding a `NULL` that
  could never match. Hand-written Go only — the filter grammar has a separate
  `isnull` operator and never routes a nil through `OneOf`.
- **`schema.Mutation` is gone** (`v0.13.0`). Folded back into `Action`'s
  existing item form, the shape it was always byte-for-byte identical to.
  `AddMutation(schema.Mutation{...})` becomes `AddAction(schema.Action{...})`;
  `rest.Mutation[T,In]` becomes `rest.Action[T,In]`; `rest.MutationSpec`
  becomes `rest.ActionSpec`. It broke because an independent port confirmed,
  with a diff rather than a hypothesis, the redundancy `v0.12.0`'s own release
  notes had already named as an open question rather than a settled one.

- **A required query parameter is now enforced.** A parameter of a declared
  read that is neither `Nullable` nor defaulted is emitted with
  `required:"true"`, so omitting it is a 422 naming the parameter. It used to
  be emitted without the tag, and huma treats a query parameter as optional
  unless told otherwise — so the read was handed the zero value and had no way
  to tell it from a caller who meant midnight on the first of January year one.
  The observed failure was a 200 carrying rows nobody asked for, which is worse
  than the 422 that replaces it.

  Listed here because a deployed client that was omitting such a parameter now
  gets a refusal. It is not a change to the *declared* contract: the parameter
  was already neither nullable nor defaulted, `restcompat` has always recorded
  it as required, and `sqlb impact` reports nothing because nothing about the
  schema moved. What moved is that the server stopped being more permissive
  than the contract it publishes — which is why no contract diff can announce
  it, and why it is written down here instead.

  The mechanical edit, if a caller was relying on the old behaviour: declare
  the parameter `.Nullable()`, or give it a `.Default(...)`. Either says in the
  schema what the server used to do by accident.

One behavioural change landed with cursors and is worth stating plainly, because
it affects requests that do not use them: **every list is now ordered
deterministically**, since `filter.Apply` appends the primary key when the sort
does not already settle ties. Responses that were previously in an arbitrary
order within a tie group now have a defined one, and paging no longer repeats or
skips rows across pages. No request changes meaning; some get a different — and
correct — row order.

## Not covered

Anything under `introspect`, `migrate`, `codegen` or `pgtest` that is reached
only from a build step or a test. These are tools, not a runtime surface, and
they change with less ceremony.

What is *not* in this category is the set of files a generator writes into the
repository, because those are checked in and reviewed: `v0.8.0` adds
`runtime.gen.ts` and `runtime.gen.dart` beside the clients, which arrive on the
next regeneration and want committing with it. Nor are the command's verb
*names*, which are [above](#will-move).

## The driver

**sqlb depends on pgx v5, and `database/sql` is not the contract.**
[ADR-0040](architecture.md#the-driver-is-a-dependency) decides it and the engine is
built that way: `Executor` is `Query` and `Exec` over `pgx.Rows` and
`pgconn.CommandTag`, and there is one driver rather than two.

This document previously said the opposite, in as many words, and that reversal
is the point rather than an embarrassment: the answer it gave was correct for a
library that extends the standard library, and sqlb turned out to be aiming at
something else. Every evaluation of sqlb so far has asked the driver question
first, which is why it is answered here at length in both directions.

**What you pass.** A `*pgxpool.Pool` is the ordinary case. A `pgx.Tx` is the
interesting one: it is an `Executor` like any other, so sqlb writes join a unit
of work the application opened itself, rather than opening a second transaction
against the same pool. `sqlb.New(tx)` knows it is inside one — `InTx` reports
true and a `WithTx` on it joins rather than nesting — and deliberately does not
take over the boundary, so `AfterCommit` refuses there rather than queueing
callbacks behind a commit sqlb will never perform.

**What it bought**, since the bill below is not free:

- **A shared transaction.** This was impossible before and is the largest
  mechanical cost of adoption that disappears. A library holding a `pgx.Tx` and
  sqlb holding a `*sql.Tx` were in two transactions even against one pool —
  `stdlib.OpenDBFromPool` shares connections, not transaction handles.
- **Arrays at no cost.** `array.go` was 449 lines of array-literal codec written
  because `database/sql` has no array case in either direction
  ([ADR-0033](architecture.md#array-columns)). It is deleted. A `[]string` binds as
  `text[]` and scans back from one, and `sqlb.EncodeArray` — a public function
  whose only job was rendering `{a,b}` — is gone with nothing in its place.
- **Per-connection type codecs.** Registering a binary codec on `AfterConnect`
  is a pgx API with no `database/sql` spelling, and
  [ADR-0026](architecture.md#vectors-declare-their-index) had to specify a vector
  column over pgvector's *text* form for exactly that reason. Measured, a 50-row
  page of 1536-dimension embeddings cost 2.7× the time and 21× the memory
  through the bridge, because the text literal is parsed element by element in
  Go.
- **`CopyFrom` and `pgx.Batch` are reachable**, through `DB.Tx()`. Reach rather
  than speed: sqlb's multi-row `VALUES` already ran within ~10% of the same
  statement hand-written over pgx, and the whole gap was `CopyFrom`'s absence.
  sqlb still has no builder for either.

**What it cost**, stated plainly:

- **Every consumer inherits pgx.** "Importing sqlb costs nothing" is no longer
  true and has been removed from the pitch rather than softened. What holds
  instead is that the list is short and checked: `mise run deps-check` fails on
  any dependency of the engine that is not pgx or something pgx itself pulls in,
  and on `rest` anything that is not huma.
- **sqlb does not run on another driver.** A population
  [ADR-0001](architecture.md#postgres-only) had already made small, but not zero.
- **`Executor` broke, and there was no deprecation path** that preserved both,
  because the point was to have one.

**What would change this.** The optional-interface path this document used to
name as the growth path was the rejected alternative rather than the plan:
type-asserting a narrow capability delivers the capability without the
positioning, and pgvector's binary codec only helps if it is on by default. What
would send it back to `database/sql` is in ADR-0040's *What would change our
mind* — and it is now an expensive question rather than a cheap one, since
reversing costs a second `Executor` break on top of the first and `array.go`
would have to be written again.
