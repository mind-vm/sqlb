# Releases

Every tagged release, newest first.

Each entry is the annotated tag's own message, marked up for the web — `git show
v0.5.0` prints the original. That is the arrangement rather than a hand-kept
changelog because a tag message cannot go stale: it is written where the release
is made and is immutable once pushed, so the only way this page can be wrong is
by missing a version, which is one failure rather than five.

[Compatibility](compatibility.md) says what is frozen and what is expected to
move. Semantic versioning applies from `v1.0.0`; until then a minor bump may
break a surface listed there, and the break is described here with the
mechanical edit that fixes it. [The road to 1.0](release-1.0.md) says what has
to be true before that promise becomes permanent.

## v0.20.0

2026-08-26 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.20.0)

The licence is MIT, and the documentation site is published again. No Go API
change: nothing this release touches is code.

### MIT, and what the previous licence actually cost

[v0.18.0](#v0180) replaced MIT with an all-rights-reserved notice, reasoning
that MIT is irrevocable per copy and said the opposite of what a private
repository intends. That was right for a private repository and stops applying
to a public one, which this now is.

The cost while it stood was never the licence text. It was that every
application and every CI runner building against sqlb needed `GOPRIVATE`, an
`insteadOf` rewrite and a credential that expires, and that resolution went
around `proxy.golang.org` and `sum.golang.org` entirely — no immutable
versions, no checksum verification, and a build that failed when a token did.
A restricted licence also gives a coding agent something legitimate to hesitate
over, which is a strange price to pay in a repository whose `docs/`, `skills/`
and generated agent skill all exist to make it legible to one.

**The tags before this one are not relicensed and cannot be.** pkg.go.dev reads
the licence out of the module zip, and a module version is immutable once the
proxy has cached it. v0.19.0 and earlier stay what they were; this is the first
MIT zip.

### The documentation site is back

Published at [mind-vm.github.io/sqlb](https://mind-vm.github.io/sqlb).
v0.18.0 deleted `site/` because Pages cannot serve a private repository on this
plan, so the deploy was permanently red and nothing it built ever reached a
reader. Both halves of that are gone.

It did not come back as it was. When the site was deleted its two hand-written
directories — `examples/` and `reference/`, MDX that existed only under
`site/` — were folded into `docs/` as plain markdown. Restoring the MDX would
have recreated the second copy that deletion removed, so both are now entries
in `sync-docs.mjs`'s `SOURCES` and generate from `docs/` like every other
section. `HAND_WRITTEN` is empty; `index.mdx` is the only hand-written page
left, and stays one because a card grid has no equivalent in a checkout.
`docs/` is the single copy, and it still reads on GitHub.

The deploy job is gated to `main`. A pull request still builds, because
`npm run build` is the link check and that is the half that ever caught
anything, but it has nothing to publish.

### Also

- `site-dev`, `site-build` and `site-check` are back in `mise.toml`.
- `example/tasks/mobile/build/` was untracked-but-not-ignored, which is the
  state that ends with build output in a commit. It now has a `.gitignore`.

## v0.19.0

2026-08-26 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.19.0)

Four independent additions, all additive. No API change moves or removes
anything v0.18.0 exposed.

### Right/full/cross joins, and a scoped SELECT CTE

The builder gains `RightJoin`, `FullJoin` and `CrossJoin` beside the existing
`Join`/`LeftJoin`, and `Builder[T].With` gives `SELECT` the single-named-CTE
capability `Update[T].From` already had for the Postgres queue-claim idiom —
one CTE, no recursion, referenced with `Join`. Comparing the builder against
Drizzle is what surfaced both gaps. Drizzle's free choice of pgvector
distance metric per query was compared too and deliberately not copied:
[ADR-0026](architecture.md#vectors-declare-their-index) ties the metric to
the index declaration specifically to close that footgun, not leave it open.

### Read-only views, diffed and introspected

`schema.View` declares a read-only view — the one genuinely unclaimed gap the
same comparison named. It reuses `TableDef` rather than a second declaration
type, so it carries none of a table's own DDL machinery and every
codegen/rest emitter that already reads `Name`/`Fields`/`Rest` off a
`*TableDef` needs no new type-handling; `Registry.Views()` is the new,
additive accessor. `migrate` diffs a view in its own pass — `DROP VIEW IF
EXISTS` + `CREATE VIEW`, unconditionally, since there is no `ALTER VIEW` in
v1 and a view holds no rows of its own for one to preserve — and
`introspect` reads it back with the `relkind='v'` queries table/column
introspection never needed. Confirmed against a real Postgres 18: create,
apply, introspect back, change the definition, drop and recreate, query the
result.

One open gap came out of that run and is named rather than silent, in
`Diff`'s own doc comment: a view's query has no `shadow.Normalize`
equivalent yet, so diffing a declared registry against an *introspected* one
proposes recreating every view, every time, because `pg_get_viewdef`
reformats the text. Two registries built from the same Go declaration are
unaffected.

### studio's grid uses its own filter grammar

studio's grid now uses the filter/sort/search grammar its own manifest page
already quoted but never wired up: a `Filterable` column gets an operator and
value field, a `Sortable` one a place in the sort select, any `Searchable`
column a search box — and the applied filter now carries across
Previous/Next instead of resetting. Export (JSON/CSV/SQL) and import
(JSON/CSV) round it out; import has no transaction across its rows, which the
README now says next to the existing "No optimistic concurrency" line.

### `go doc` doc-comment checks are on

`go doc` is the API reference (CLAUDE.md says so), and golangci-lint's own
default had quietly disabled the staticcheck checks that keep that promise
true — a missing package comment, or an exported symbol's comment not
starting with its own name, never failed the gate. They are back on
(`ST1000`/`ST1020`/`ST1021`/`ST1022`; `ST1003` and `ST1016` stay off, being
naming style rather than doc coverage). Eleven pre-existing violations came
with them, all comment-form issues rather than missing prose — the more
interesting one was in `sqlb eject`'s own generator, whose banner comment
always sat a blank line above `package X`, which is exactly what stops
go/doc from crediting a comment as the package doc. No ejected package has
ever actually had one until now.

**Housekeeping.** `ci.yml` and the rest of `.github` are gone — an earlier
release already reduced it to a release-tag-only re-check of what `mise run
preflight`/`mise run ci` already cover locally, and that was the whole of
what it was still buying. A stale line in `docs/comparisons.md` crediting ent
with an advantage sqlb no longer lacks — hooks applying to an expansion — is
corrected, and `docs/superpowers/specs` carries a design memo on whether
`?expand` should go past one hop, not a decision.

## v0.18.0

2026-08-25 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.18.0)

The repository moved from `github.com/jryannel/sqlb` to `github.com/mind-vm/sqlb`,
and the module path moved with it — the only way that change could compile.
Several feature additions and fixes came along in the same window, so this is a
minor bump on its own merits and not only because of the move.

**BREAKING: the import path is `github.com/mind-vm/sqlb`.** Every `go.mod`
requiring the old path and every import statement naming it now says `jryannel`
where the repository no longer does. The mechanical edit is the same string
everywhere: replace `github.com/jryannel/sqlb` with `github.com/mind-vm/sqlb`
across `go.mod` and every import, then `go mod tidy` to settle `go.sum`. A
`GOPRIVATE` or `GONOPROXY` entry naming the old org needs the new one added
beside it, not in place of it, if anything else still lives under
`github.com/jryannel`. There is no compatibility shim and none is planned — a
module path is not a runtime value an alias could paper over, so the next pull
is this edit or a build failure naming it.

**Housekeeping alongside the move.** The documentation site (Astro, published
nowhere — the repository is private and a private repository on this plan
cannot serve GitHub Pages) is deleted; every page it held is now plain markdown
under `docs/`, readable straight from a checkout or on GitHub with no build
step. `ci.yml` — `vet`, `lint`, both test jobs, the whole doc-drift check set —
now runs only when a release tag is pushed rather than on every push or pull
request; `mise run preflight` and `mise run ci` locally are the gate during
regular development, and a pull request shows no CI status at all between
opening and merge. And `LICENSE` changed: MIT is irrevocable per copy and said
the opposite of what a private repository intends, so it is now an
all-rights-reserved notice; nothing under `example/` is exempted.

New surface, all additive — none of it is wired into an example schema yet, so
`sqlb check` has nothing to regenerate and `sqlb impact` reports every example's
REST contract unchanged.

### A declared verb can answer with something other than the row

A declared action had two response shapes: an item verb answered with the row
it acted on, and a collection verb answered `204`. Neither is right for the verb
whose answer is the point — grading a quiz returns a score, and a score is
neither a row nor nothing
([#310](https://github.com/mind-vm/sqlb/issues/310),
[#312](https://github.com/mind-vm/sqlb/issues/312)).

`Action.Returns` declares the response in the field vocabulary `Action.Body`
already uses, built with `schema.Result`. The runtime gains `ActionReturning`
and `CollectionActionReturning` beside the existing pair, since the response
type is a Go type parameter fixed at registration rather than a value the spec
carries. One declaration reaches the Go result type, the TypeScript and Dart
clients, the CLI's `--help`, the skill's actions table, and `sqlb impact`, which
now diffs a result the way it already diffs a request.

### A create body can carry what the row does not

A create body is derived from the columns, and a request that creates something
with a secret in it carries one thing that is not a column — the plaintext
behind a stored digest, an invite token, an id list resolved into rows of
another table ([#309](https://github.com/mind-vm/sqlb/issues/309)).

`schema.REST.CreateInput` declares those properties, and the value reaches
`BeforeCreate` as `sqlb.CreateInputFrom` — the same context seam a principal
already uses. One declaration, six surfaces: the Go body and a
`Create<Model>Input` type, TypeScript, Dart and the CLI send it, the manifest
and skill name it, `restcompat` records it as breaking if a required one is
added, and an ejected package refuses to serve a create that would silently
drop it.

studio's write forms follow. `buildFormFields`, `parseFormBody` and the
rejected-submission redisplay all carry the declared properties now, not only
the columns — the redisplay path matters most, since it rebuilds from the
submitted form and dropping the property there takes its input away on exactly
the attempt the error is about. A pre-existing bug came out with it: the shared
body encoder was keying a declared property by its `WireCase` spelling, which
only a column has, so under `WireCase(Camel)` studio was sending `completedAt`
to a handler that only knows `completed_at`.

### Server-wide middleware has a documented seam

`Server.Handler`'s doc comment always said to wrap it with application
middleware; `Serve` offered no field to put one in, only an ordering nothing
stated — assigning `srv.Handler` inside `mount` and trusting `Serve` to read it
back afterwards ([#301](https://github.com/mind-vm/sqlb/issues/301)).
`ServeConfig.Middleware` is that field now. It is also the one thing here that
cannot become a hook: establishing the principal is upstream of every guarantee
a `Scoped` hook makes.

### A table's generated name can differ from its SQL name

`board_columns` singularising to `BoardColumn` can collide with a name a
different table's codegen already derives
([#262](https://github.com/mind-vm/sqlb/issues/262)). The only existing fix was
`RenamedFrom`, which actually renames the table — a live-data migration for a
naming problem that has nothing to do with the data model.
`TableDef.TypeName(name)` pins the generated Go/TypeScript/Dart identifier
without touching storage, and the collision refusal now names both fixes.

### A client directory outside the module states where it is

`TSDir` and `DartDir` still default to resolving against `Dir` for every
project configured before this. Two escapes remove the arithmetic instead of
warning about getting it wrong
([#290](https://github.com/mind-vm/sqlb/issues/290)): an absolute path is used
verbatim, and a path prefixed `"//"` resolves against the directory holding the
nearest `go.mod` instead of against `Dir` — `"//web/src/api"` means that at any
`Dir` depth, for a client three levels outside the module, without simulating
`filepath.Join` by hand to get there.

### Fixes

**A request body now speaks one vocabulary throughout, not two that
occasionally disagreed.** `WireCase` is one setting applied by one function at
every surface, and three places emitting a request body were not calling it:
the generated create/patch struct tagged its JSON key by column name while the
TypeScript and Dart clients beside it sent the wire spelling, the ejected
decoder matched its `case` labels the same wrong way, and the CLI's
`--set-null` wrote the column key after converting the flag from kebab-case.
Fixed throughout, plus the same fault in the ejected read surface's `Column`,
which now carries `Name` (SQL) and `Wire` (the request) separately rather than
one string doing both jobs — an exit under `WireCase(Camel)` was accepting
`?created_at` while its own generated clients sent `?createdAt`. An unrelated
compile failure surfaced proving it: `sqlb eject`'s fixed import set predicted
which packages a schema's conditions would need and drifted from the templates
it was predicting for, so the plain shape (neither `Scoped` nor `SoftDelete`)
emitted `"fmt"` imported and unused. Imports are now read off the rendered body
instead of predicted ahead of it.

**`sqlb impact` now sees a declared query, not only a declared action.**
Between the release that added `TableDef.AddQuery` and this one, adding,
removing or repathing a declared read was a REST contract change the tool could
not see, while its report looked complete regardless. Removing or moving a
query is now reported breaking; adding one is additive.

**`sqlbtest.Fresh`'s scratch database names no longer collide.** The old
suffix was a nanosecond clock reading, and on a host whose clock advances in
whole microseconds roughly four calls land on the same tick — a
ten-thousand-call loop produced 2629 distinct names and 7371 duplicates, worse
across the two package binaries a `go test ./...` run starts together. The
suffix is now a per-process random tag plus an atomic counter, neither of which
depends on when anything started.

**`ScopedHooks.BeforeCreate` exists now, and only to panic at registration**
([#289](https://github.com/mind-vm/sqlb/issues/289)'s second report). The
reasoning for keeping create out of a released scope was already written down;
what was missing was the method itself, so the error a caller got was "has no
field or method BeforeCreate" rather than anything naming the decision.

**The generated Dart client's `withCursor` wraps past 80 columns like every
other signature this emitter writes**
([#302](https://github.com/mind-vm/sqlb/issues/302)). A long table name no
longer produces a file `dart format` immediately rewrites.

Deliberately not done: no release-time contract for a declared query's result
type, which is always `[]T` for its table and would record a constant, and no
client emitter reads a declared query yet, which is the larger gap behind that
one. No `UpdateInput` to sit beside `CreateInput` — every case collected so far
is a create, and a second declaration before there is a second case is
vocabulary invented for symmetry. No `BeforeCreate` release under a scope; the
panic is louder, not different.

## v0.17.1

2026-08-23 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.17.1)

One fix to the generated Dart client, and the gate that should have caught it.
Regenerate to pick it up; no API change and nothing else moves.

**`ChangeFeed`'s private field is named for what it holds**
([#299](https://github.com/mind-vm/sqlb/pull/299), reported by a consumer).
v0.16.0's change-feed subscriber emitted a constructor that Dart 3.12 reports
and 3.11 does not:

```dart
ChangeFeed({String? lastEventId}) : _lastEventId = lastEventId;
// info • Use an initializing formal to assign a parameter to a field
//        • prefer_initializing_formals
```

From Dart 3.12 a *private* field counts as the same name as a *public*
parameter, because `private-named-parameters` makes `this._lastEventId` spell a
parameter called `lastEventId`. Below 3.12 that does not parse, so the rule
cannot fire — which is why this reached a consumer rather than the emitter's own
suite, and why it landed on generated code, the one file a project cannot fix
for itself.

Neither obvious fix works, and they fail in opposite directions. `this._position`
— the rule's own advice — is a hard error below 3.12, so taking it would put an
SDK floor of 3.12 on every project that generates this file. An
`// ignore: prefer_initializing_formals` fails the other way, on
`unnecessary_ignore` below 3.12 where the rule cannot fire at all. Renaming the
field is the one spelling that analyses clean on every SDK: the rule compares
the parameter's name to the field's, and `_position` is not `lastEventId` on any
version. The public API is untouched — `ChangeFeed(lastEventId: …)` still
constructs it and `.lastEventId` still reads it.

**`dart-sdk-check` is the mechanism.** `test-dart` analyses
`example/tasks/mobile`, whose pubspec says `sdk: ^3.9.0` — and a pubspec
constraint fixes the *language version*, so that gate cannot see a diagnostic
introduced after 3.9 no matter which SDK runs it. It was green through the
entire window in which this shipped. The new task copies the package, rewrites
the constraint to the version the pinned SDK actually offers, and runs the same
analyzer again.

The floor is derived from `dart --version` rather than written as a literal,
because a literal is exactly what went stale here: the example's `^3.9.0` was
current when it was written and silently stopped covering anything after it.
Bumping `dart` in `[tools]` now widens the check on the same commit.

Proven the way this repository asks a guard to be proven: with v0.17.0's
`ChangeFeed` restored, `dart-sdk-check` fails naming the rule and the line while
`test-dart`, against the same file with the same analyzer, still reports "No
issues found".

## v0.17.0

2026-08-23 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.17.0)

One change, and it is to what the linter *says* rather than to what it catches.
No API change, and nothing to regenerate.

**`unindexed-filter` and `unindexed-sort` read the table's scope before they
speak** ([#296](https://github.com/mind-vm/sqlb/issues/296), found reviewing a
real multi-tenant port on v0.16.0). On a table whose reads are confined, both
halves of the diagnostic were wrong, and the fix was the worse half:

```
[warn] unindexed-filter: tutor_sessions.subject: column is filterable but is not the
       leading column of any index, so filtering on it scans the table
    fix: add .Index("subject") to the table, or drop .Filterable() from the column
```

The predicate is never that column alone. Every read of a scoped table carries
`family_id = $1`, and that column is indexed — so the scan is one tenant's rows,
not the table, and the index that serves the query is `(family_id, subject)`. A
reader who followed the advice literally built a single-column index Postgres
will mostly decline to use, and concluded the linter was not worth following.
That is the expensive outcome, and it is the one #267 already recorded the shape
of: three warnings judged not worth acting on is three warnings that teach a
reader to skip the block they are printed in.

Lint does not read a registry's hooks to know this, which was the obvious
objection. `Scoped` is an obligation rather than a note: `rest.Resource` refuses
to mount a resource whose exposed operations have no hook behind them
([ADR-0030](architecture.md#declared-scope-is-required)), so an exposed table
with a `Scoped` column either carries that predicate on every generated read or
the server does not start. The declaration is sufficient.

Three conditions bound it, and each is a case that would otherwise produce
nonsense. The scope column must itself be indexed — an unindexed one means the
scope predicate *is* the scan, and the diagnostic naming that column is the one
to read first. The table must be exposed, because that is where the obligation
bites; a `Scoped` column on an internal table states the same intent and is
checked by nothing. And a scope column that is also the primary key or unique
selects at most one row, so neither rule fires: the composite would be
`(id, subject)`, which adds nothing to a unique column.

The diagnostics are *not* suppressed on scoped tables. An unindexed filter
inside a large tenant is still a sequential scan of that tenant's rows, and a
schema with one big customer has a real problem the linter should still name.
What was wrong is the arithmetic and the suggested index, not that the rule
fires. `search-without-trigram` is unchanged for a related reason: a trigram GIN
index does not compose with a scope column the way a btree does.

## v0.16.0

2026-08-23 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.16.0)

No breaks. Two new packages' worth of surface — a scratch database for tests and
a generated change-feed subscriber — and, from a single consumer port, a check
for each of the three mistakes that port made and shipped.

The theme of the second half is worth stating because it decided what got built.
[#293](https://github.com/mind-vm/sqlb/issues/293) tabulated every authoring
mistake made porting a nine-table application onto `v0.15.1` against whether a
mechanism caught it, and the split was total: **everything with a check cost
minutes, everything without one shipped.** The generated `sqlb-schema` skill was
loaded from the first turn throughout and changed none of it. So each of the six
issues that came out of that port is answered with a mechanism where one was
possible, and the release notes say plainly where one was not.

**Upgrade: regenerate.** Two emitters produce more than they did, so `sqlb check`
reports drift until `sqlb generate` runs. The TypeScript and Dart clients gain a
typed change-feed subscriber, and the generated skill gains a section naming its
siblings. Nothing else moved; `impact-check` reports the REST contract unchanged.

### A scratch database is a DSN and a function call

Every suite wanting a real Postgres wrote the same eighty lines — read a DSN,
open an admin connection, derive a legal name, drop, create, connect, apply the
schema, drop again. Nine copies were in this repository alone, and two of them
carried comments explaining that the fix would not be worth it
([#292](https://github.com/mind-vm/sqlb/pull/292)).

`sqlbtest.Fresh` creates a database per test on a server the caller names,
applies each option in order, and drops it afterwards; `FreshDSN` is the same for
a caller that opens its own connection. Options are one list because the order
they are written in is the order the database is built in — an extension before
the schema that needs it, the pool size beside both.

It takes a DSN and starts nothing, which is the decision rather than an
omission: this repository ran the other experiment, and `pgtest/doc.go` records
the reversal. There is no skip-when-absent path either — `sqlbtest.DSN` fails the
test naming the variable that was unset, because a suite that passes quietly
when it cannot reach a database reports coverage it does not have.

### The change-feed subscriber is generated

The feed shipped its client half as raw material: `keysByTable` in TypeScript,
`TableName` in Dart, and the listener left to whoever consumed it
([#285](https://github.com/mind-vm/sqlb/pull/285)). `example/tasks`' own board
was the evidence — an index into a lookup table with a cast to make it compile,
an undefined check, the query keys by hand, and no case at all for an event whose
key is empty, which is what an unattributable delete looks like on this wire.

A change event now resolves to the queries that read it, in both clients. The
line it stops at is the one the queries file already draws: a mutation gets a
`mutationFn` and no `onSuccess`, because what a *write* invalidates is a policy
question while what a *read* depends on is an address. SSE was already a
supported surface as of `v0.13` — `rest.Events` mounts the stream and
`outbox.Dispatcher` makes it durable — and every page a reader would have checked
said otherwise or said nothing. They now say what it is and how to subscribe.

### A one-function verifier is a func

`Verifier[T]` is a one-method interface and most real verifiers are a single
function closing over one dependency. Writing one still cost a named struct plus
a `Verify` method that did nothing but call through.
`VerifierFunc[T]` is the `http.HandlerFunc` shape for it
([#279](https://github.com/mind-vm/sqlb/issues/279)). The interface stays: a
verifier with real state — a JWKS refresher with a background goroutine — is
better as a named type, and the interface is what lets both spellings mount.

### Three mistakes that shipped, and now cannot

**A raw default that spells out what a helper renders.** The reporter believed
`GenUUIDv7` needed the `pg_uuidv7` extension and wrote `Expr("uuidv7()")`. It
does need the extension — until `MinPostgres(18)`, which is exactly the question
the helper was still asking and the raw string has now answered once, for every
target. The new `raw-default-has-helper` lint rule names the constructor.

It cannot fire on the canonical spelling, because `GenUUIDv7()` and
`Expr("uuid_generate_v7()")` produce identical values and every other reader
already treats `Raw` as the helper's identity. What it fires on is the spelling
no helper produces but the renderer emits — which meant the rule and the renderer
had to be reading the same table, so `schema.TargetDefaults` is now that table
and `migrate` reads it instead of its own copy. A rule that had drifted would
advise against SQL `migrate` itself writes.

**A hook's plain error answering 500 where 403 belonged.** `rest/errors.go`
already said in a comment that a deliberate refusal should carry its status. The
person who needs that sentence is reading a log, not that file, so the
unclassified-error line now names `huma.Error403Forbidden`. Nothing reaches the
response: it is advice for whoever wrote the hook, not for whoever provoked it.

**A client directory with one `../` too few**
([#290](https://github.com/mind-vm/sqlb/issues/290)). `TSDir` resolves against
`Dir` rather than the module root, so one level short wrote a complete, correct
TypeScript client into `server/web/src/api`, where nothing imported it while the
real frontend went on building against the client it already had. `tsc` stayed
green, `vite build` stayed green, and the first symptom weeks later was a
generated client describing a schema that had moved.

The reported path could not have helped and never could: `filepath.Join` cleans
`sqlbdata/../web/src/api` down to `web/src/api`, a correct module-root-relative
path that reads exactly like the repository's real `web/`. What differs between
the two cases is that the *tree* was new, so `generate` now warns when a
TypeScript or Dart client directory **and its parent** both had to be created,
and carries the absolute path. Scoped to the two clients whose consumer is not a
Go compiler — a misplaced `CLIDir` is still a Go package and the import says so
on the next build.

### Two things an adopter could not find

`sqlbtest` was undiscoverable from the front door
([#287](https://github.com/mind-vm/sqlb/issues/287)): `sqlb init` emitted six
files, none of them a test, and the emitted `sqlb.md` did not contain the word.
A consumer wrote an entire tenant boundary and its whole suite against a real
Postgres without learning the package existed. `init` now emits a passing
`predicate_test.go`, and the two tests it writes are the two questions a round
trip cannot answer *at all* — did the hook's predicate reach the statement, and
did the refusal issue no statement. Zero rows and no query look identical from
outside, and they are the difference between a boundary and a filter that
matched nothing today. The emitted test is compiled and run in CI against a
freshly initialised, freshly generated project, because it names symbols on both
sides and a rename would break every project created afterwards.

The static skills were undiscoverable for the same reason
([#291](https://github.com/mind-vm/sqlb/issues/291)). The generated
`sqlb-schema` skill is the only sqlb artefact guaranteed to be in a consumer's
repository and in front of an agent from the first turn, and it named the others
zero times — so an agent held this project's capability lists with no pointer to
the vocabulary those lists are written in. It now names them, generated so it
cannot drift, and checked against `skills/` in both directions.

### A refusal that could not be acted on

The nested-query guard told the reader to resolve the inner query with
`Resolved(ctx, db)`. That needs an `Executor`, and a `BeforeQuery` hook is handed
the query and nothing else — so at the one place the guard is most likely to
fire, inside a scoping rule, the message read as actionable and was not
([#288](https://github.com/mind-vm/sqlb/issues/288)).

`Resolved` already walked the statement's subqueries after running the hooks;
walking before as well makes anything new provably a hook's, at no cost to a
statement that nests nothing. Both refusals stay — the caller-written one keeps
`Resolved`, which is right for it, and the hook one names what a hook can
actually do: denormalise the column onto the table being confined, or register
the rule on the other model. `Update` and `Delete` get the same split.

### Where a check was not possible

`WithoutScope` cannot release `BeforeCreate`, so every create goes through a hook
wanting the request's claims — including the creates with no request behind them:
a fixture, a seed, an import, a job
([#289](https://github.com/mind-vm/sqlb/issues/289)). The rule is right, and
sharper than the existing comment gave: a released *read* fails visibly, a
released *stamp* does not, and the row is still wrong tomorrow.

The fallback that answers for it is now written down beside the `WithoutScope`
worked example and grounded in a test of all three branches, because the branch
carrying the load is the one a reader is most likely to write as an
unconditional `return nil` — and that mistake passes any test which only ever
creates rows with claims present.

### Fixes

`rest.Serve` handed `mount` an `Executor` rather than the `*sqlb.DB` it had
built, so every mount function opened with a type assertion
([#277](https://github.com/mind-vm/sqlb/issues/277)). A `TransientError`
returned by pointer read as a rejected credential rather than a provider outage
([#278](https://github.com/mind-vm/sqlb/issues/278)).

### Gates and documentation

`lint-check` and `map-check` join `column-check` and `adr-check`. Both earned
themselves on the commit that added them: the lint reference table had gone four
rules stale, and every count in `CLAUDE.md` was wrong at once — 58 decisions
against 64, 19 root files against 22, 36 tasks against 38, six Go modules against
fourteen. A map with wrong numbers is worse than one with none, because it is the
document a reader has no reason to doubt.

New pages for the auth seam and the second stage it does not cover
([#280](https://github.com/mind-vm/sqlb/issues/280)), for how a library ships
tables ([#281](https://github.com/mind-vm/sqlb/issues/281)), and for the
boundary a hand-written CLI endpoint sits behind
([#257](https://github.com/mind-vm/sqlb/issues/257)). `WithoutScope` gained the
worked example it went unfound for
([#276](https://github.com/mind-vm/sqlb/issues/276)).

`example/attachments` is presigned direct-to-S3 uploads, and it exists to answer
whether sqlb should grow a Django-style `FileField`. It should not: the database
half of an attachment is three ordinary columns, and everything actually hard is
the ordering between a row Postgres owns and bytes it never sees. The row is
written first and born pending; the size comes from a `HEAD` against the storage
rather than from the client; the object is deleted in an `AfterCommit` callback,
because inside the transaction a rollback would leave a row pointing at bytes
that are gone. Its `s3/` is SigV4 against the standard library, cross-checked
byte for byte against `aws-sdk-go-v2` rather than trusted.

## v0.15.1

2026-08-19 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.15.1)

Five fixes, four of them reported against v0.15.0 and one a gate correction
found by review. No API change.

`sqlb check`'s advisory output defaulted to every note it had ever printed,
so a fourteen-table schema produced 102 lines with the one line it was run
for at the bottom of them
([#267](https://github.com/mind-vm/sqlb/issues/267)). `-lint` now takes a
floor — `off`, `summary` (the new default), `warn` or `all` — and `runCheck`
records each failure rather than printing a verdict wherever it happens to
find one, closing with a single line naming what failed or `sqlb: check
passed` as the last line every time.

`sqlb migrate` proposed a clean, correct-looking `DROP INDEX CONCURRENTLY`
for an index sqlb never built, byte-identical to a drop the author intended
and to the phantom v0.14 used to emit
([#268](https://github.com/mind-vm/sqlb/issues/268), upgrade from v0.14).
`migrate` and `check -database` now annotate a drop with why: no
header-bearing migration ever created the index, or its shape still matches
what the old inference would have produced.

`Hooks`' doc comment said only that a hook's error "reaches the caller
unwrapped," which reads as the caller seeing that error — what the caller
actually sees, absent a status, is a 500 with a sentence
([#255](https://github.com/mind-vm/sqlb/issues/255)). The doc comment now
states the rule and points at `example/tasks/app/errors.go` for the pattern.

The generated Go client had one seam for a header the schema cannot
derive — replacing `Transport`, which reimplements all of `Do` — and a
custom `Client.HTTP` silently dropped `--timeout` with it
([#254](https://github.com/mind-vm/sqlb/issues/254)). `Request` gained a
`Header` field, applied caller-wins after the derived headers, and `Do` now
wraps the context with `Client.Timeout` directly regardless of what HTTP
client is in play.

`sqlb generate` wrote every rendered file unconditionally, so a no-op
regenerate still touched `models_gen.go` and forced gopls to throw away and
rebuild the whole module's type index
([#269](https://github.com/mind-vm/sqlb/issues/269)). `generate` now skips
a file whose rendered bytes already match disk, and replaces one that does
change via temp-file + rename, so a language server reading on the event
never observes a syntactically broken half-write.

`adr-check` now fails when a citation number names two decisions, or a
decision is cited under two numbers. ADR-0059 had quietly meant both "A
Verifier composes with the principal seam" and "Fx wiring is generated, not
a runtime library" for two releases before anyone read the two citations
side by side; `example/tasks/README.md` had separately mislabelled the Go
CLI decision as ADR-0030 against 28 citations giving that number to
"Declared scope is required." Both are fixed and the gate now catches a
repeat.

## v0.15.0

2026-08-18 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.15.0)

Three deliberate breaks, more than any tag before it has carried at once. Each
is a shape that was wrong before the surface it sits on was frozen, and each is
fixed now because pre-1.0 is when the mechanical edit is still a paragraph
rather than a major version. All three are named in
[compatibility.md](compatibility.md) with the edit that fixes them. Alongside
them: a pluggable authentication seam with its first provider adapter, generated
`fx` wiring for a schema-owning module, and the 58 architecture decisions folded
into one document.

**BREAKING: a bare date given to `eq` on a timestamp column is a 400, and `day.`
is what to write instead.** `?starts_at=eq.2026-09-01` against a `timestamptz`
column compiled to an equality against midnight in the session's time zone. A
stored timestamp is almost never exactly midnight, so the request a caller
writes for "what is on this day" returned `200` and an empty page — a booking
calendar's front screen, shipping and answering nothing, with no error to notice
([#241](https://github.com/mind-vm/sqlb/issues/241)). Both halves are fixed,
because either alone leaves the trap open: `?starts_at=day.2026-09-01` compiles
to `starts_at >= $1::date AND starts_at < $2::date + 1`, a half-open range an
index on the column can serve, and the old spelling is now a refusal naming both
ways to say what was meant. `Field.OnDay` is the builder half. The date is bound
as text and cast in Postgres rather than parsed in Go, deliberately: a
`time.Time` is an instant carrying a zone that is not the session's, so binding
one answers a different question either side of midnight. Scoped to where a date
is silently wrong — the ordering operators, `date` columns and models that do
not declare their column types are all untouched. The mechanical edit is `eq.` →
`day.` where a whole day was meant; a client relying on the old behaviour was
relying on receiving no rows. Reaches the consumers: the OpenAPI parameter
description names the operator and the refusal, and the TypeScript client offers
`day` on timestamp columns only.

**BREAKING: an index is declared, never inferred.** `sqlb migrate` proposed
`DROP INDEX CONCURRENTLY` for an index nothing had created, on every run, for a
self-referencing reference declared the way `example/catalog` documents;
applying it failed with `42704` and hand-correcting the file did not help,
because the next unrelated change regenerated the same phantom
([#259](https://github.com/mind-vm/sqlb/issues/259), found building an
application on `v0.14.0`). The chain ran through an implication: `ExternalRef`
implied a single-column btree, so a registry read back out of a live database
claimed an index that database did not have, while the declared side asked for
no such index. The inference is what broke, not `introspect`'s choice — an
implication a declaration does not state is one that cannot be checked against
reality. `Field.Indexed()` is how a column asks for its own index now, and the
advice the implication was carrying moves to `schema.Lint`'s `unindexed-ref`
([ADR-0061](architecture.md#ddl-is-declared-never-inferred)). The mechanical
edit is `.Indexed()` on every `ExternalRef` that relied on it. **Read the first
migration generated after upgrading before applying it**: without that edit the
diff proposes dropping the index — clean SQL that silently removes an index a
join depends on. `sqlb check` names every unindexed reference.

**BREAKING: a unique foreign key expands to an object, not a collection.** A
reverse `Inverse` relation backed by a single-column `Ref(...).Unique()` can
never match more than one row — the constraint already says so — and yet its
expansion was a capped `{items, has_more}` envelope in the wire shape and in all
three clients. It is now the target row, or `null`. `OneToOne` is *derived* from
`Field.Unique` rather than declared, because a unique foreign key is a
structural fact and not a policy opt-in like `Filterable`, and the break is
recorded against the frozen list envelope on purpose:
pre-1.0-or-never, following the `Executor` precedent
([ADR-0060](architecture.md#a-unique-foreign-key-is-already-one-to-one)).
The mechanical edit is to regenerate the Go model and the TypeScript and Dart
clients, then read the expansion directly — nil-check in Go, null-check in
TypeScript and Dart — instead of unwrapping `items`/`has_more`. Building it
closed a gap in `restcompat`/`sqlb impact` as well: `Capture` had never walked
`Registry.Inverses` at all, so an exposed reverse relation of *any* shape was
invisible to the contract diff. An adopter with a checked-in
`restcontract.json` predating this needs a one-time re-record.

**A pluggable auth seam, and a WorkOS adapter that proves it composes.** Four
providers were reinventing the same middleware. `sqlb.Verifier[T]` is the seam:
a `Verify(ctx, token) (T, error)` an adapter implements, `Middleware[T]` puts
the verified credential in the request context as the principal the scoping
hooks already read, `BearerToken` extracts a token the same way everywhere it is
trusted, and `TransientError` is what separates a provider outage from a bad
credential — a rejected token answers `401`, an unreachable identity provider
answers `500`, because collapsing the two teaches a caller to retry the one
thing retrying cannot fix
(["A Verifier composes with the principal
seam"](architecture.md#a-verifier-composes-with-the-principal-seam)).
`example/auth-workos` is the first adapter, for WorkOS AuthKit, and it is its
own Go module so the WorkOS SDK and the JWT/JWKS dependencies never reach sqlb
core's `go.mod`. `TransientError` must be returned by value, not by pointer;
its doc comment says so, because that is the file an adapter author reads.

**A module's `fx` wiring is generated, not hand-copied.** sqlb emitted models, a
manifest, a REST resource, three clients and a skill, and nothing that makes a
schema-owning module a *unit* in a host's `uber-go/fx` graph — though two of the
three things a module contributes there are already fully determined by the
declaration. Measured in a real 38-module consumer
([#171](https://github.com/mind-vm/sqlb/issues/171)): 78 byte-identical
migration providers, 183 operation-set literals, 209 `fx.Module(...)`
declarations. `codegen.Options` grows `WiringMigrations` and `WiringOperations`,
each a `WiringSet{Type, Group, Name, EmbedDir}` naming the host's own types as
`"import/path.TypeName"` — a wrong name is a compile error in the generated
file, not a runtime surprise. What is emitted is one `fx.Option` value,
`FxModule`, rather than a wrapped `fx.Module`, so the hand-written module
composes it and the generated file never carries a hand edit. This was tried
once as a runtime library and rejected, and the rejection is recorded with the
design (["Fx wiring is generated, not a runtime
library"](architecture.md#fx-wiring-is-generated-not-a-runtime-library)).
`example/fxapp`'s store module is the proof: two hand-written providers became
`var Module = fx.Module("store", FxModule)`.

**Claiming a locked batch is one statement now.** The Postgres queue-claim idiom
is `WITH claimed AS (SELECT ... FOR UPDATE SKIP LOCKED) UPDATE ... FROM claimed
... RETURNING ...`. sqlb had both halves and no way to join them, so the only
spelling was two statements inside an explicit transaction — and a caller who
forgot `WithTx` silently reintroduced the double-claim race `SKIP LOCKED` exists
to prevent ([#174](https://github.com/mind-vm/sqlb/issues/174)).
`Update[T].From(name, query)` renders the CTE form. `query` is the existing
`Subquery` interface, so it shares the statement's compiler and bind numbering,
and it gets the same resolution discipline: an unresolved query over a hooked
model is refused rather than compiled with its scope missing.

### Also in the engine and the generators

- **`sqlb.Match(expr)`** gives a predicate spanning more than one column a typed
  entry point — `(on_hand - reserved) >= $1` — where `RawPred` was the only way
  in, and a rename in a raw string is a runtime break in exactly the statement
  most likely to be under contention
  ([#221](https://github.com/mind-vm/sqlb/pull/221)).
- **`Field.SharedAs(name)`** makes two columns declaring the same enum one Go
  type instead of two nominally incompatible ones. Opt-in on purpose: matching
  value sets today is not evidence of shared meaning, and `Registry.Validate`
  refuses two declarations under one name whose values or order disagree
  ([#197](https://github.com/mind-vm/sqlb/issues/197)).
- **`sqlb check` reads back the header it writes.** A generated migration
  somebody has hand-edited to add a `CREATE TRIGGER` was invisible to both
  existing gates. `check` now fails on a header-bearing file containing DDL
  sqlb's own emitters never write, no database needed. A file with no header is
  not sqlb's to police ([#178](https://github.com/mind-vm/sqlb/issues/178)).
- **A generated client that would not compile is a generator error.** Two tables
  can want the same name — `board_columns` singularises onto what `boards` calls
  its column type — and the failure used to arrive from `tsc` as two lines
  naming neither table. Both the TypeScript and the Dart emitter now read back
  what they are about to write and refuse a duplicate declaration, naming the
  identifier, both tables and what each contributed
  ([#261](https://github.com/mind-vm/sqlb/issues/261)).
- **`schema.Lint` is wired into `check`.** Twelve advisory rules existed and
  nothing called them. Page-size fields with no `OpList` are dead config and now
  say so; a required `Text` column with no default is an info-level note before
  it is a `422`; `unindexed-ref` carries the advice the dropped index inference
  used to ([#201](https://github.com/mind-vm/sqlb/issues/201),
  [#223](https://github.com/mind-vm/sqlb/pull/223)).
- **A write's generated response type stops lying.** `v0.11.0` made a
  create/update omit a `Needs`-carrying computed column from its JSON response;
  the clients were never told, so `post.myAcknowledged` typechecked as `boolean`
  while being `undefined` ([#188](https://github.com/mind-vm/sqlb/issues/188)).
- **The seq-scan diagnostic gates on cost, not rows.** A scan's `Plan Rows` is
  what its filter *kept*, so the rule got quieter exactly as the query became
  the textbook missing-index case
  ([#176](https://github.com/mind-vm/sqlb/issues/176)).
- **The typed facade names what `Hidden` omitted, and the word that brings one
  back.** A consumer building an `api_tokens` table concluded the facade could
  not express the query that table exists for and went to `sqlb.F(...)`, meeting
  `LookupKey` afterwards ([#256](https://github.com/mind-vm/sqlb/issues/256)).
- **`sqlb init` writes `sqlb.md`**, so a scaffolded project's next steps, command
  cheat-sheet, capability vocabulary and REST query grammar survive the terminal
  that printed them ([#244](https://github.com/mind-vm/sqlb/issues/244)); and
  `generate` nudges for a second `go mod tidy` when its output grew an import
  ([#204](https://github.com/mind-vm/sqlb/issues/204)). A template's own
  `//go:generate` line is no longer a directive at this repository's root
  ([#200](https://github.com/mind-vm/sqlb/issues/200)).
- **studio mounts on someone else's mux.** `NewServer(m, apiBase, basePath)` —
  every link, asset and redirect used to be root-absolute, so `StripPrefix`
  loaded the index page once and 404'd everything on it
  ([#225](https://github.com/mind-vm/sqlb/pull/225)).

### Docs

**The 58 architecture decisions are one document.** `docs/adr/` was a directory
of individually numbered files, each with its own hand-maintained revisions log
that nothing read. They are now `### ` sections of
[architecture.md](architecture.md)'s Decisions, folded one commit at a time so
that *changing* a decision's reasoning is a commit arguing for the change — the
way every other commit here already has to be. `git log -G'### <heading>' --
docs/architecture.md` finds every commit that touched one. The `/adr/` route and
the `adr-check` gate that guarded the directory are both retired.

**Six hard cases, built.** `docs/special-cases.md` named six things real
Postgres applications write that sqlb had no worked example for. All six are now
lean standalone modules under `example/` — `tasks-evolved` (a non-additive schema
change), `meter` (arithmetic upsert, composite key, `date_trunc` rollup),
`outbox` (competing consumers, retry, dead-letter), `rooms` (`EXCLUDE`
constraints), `vault` (a payload only Go may write, polymorphic owner) and
`catalog` (self-referencing tree, search escalation) — and building them found
three of that page's own verdicts already stale.

**Four new pages and a skill.** `rest.Serve`, `sqlb init` and the admin mount
had no user-facing page; a webhook endpoint is not a table and so is not a
resource; [comparisons](comparisons.md) answers the Supabase and PocketBase row
an evaluator asks for first. And `skills/sqlb-authoring` is the writing-direction
sibling of the generated skill — what the DSL can express at all, rather than
what one project's schema exposes ([#203](https://github.com/mind-vm/sqlb/issues/203)).

## v0.14.0

2026-08-14 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.14.0)

No breaking changes. Three pieces of work, all additive.

**`sqlb docs` writes facts instead of asking for them, and gains a section for
routes it can't see.** Follow-up to `v0.13.0`'s first cut, based on feedback
after that PR landed. Filterable/sortable/searchable/expandable columns on a
list, required/optional columns on a create, writable columns on a patch, a
declared verb's `Body`/`Writes`/`Touches` — all schema-derivable, now rendered
above the notes block so an agent's writing time goes to what only it can
supply. The open-ended notes prompt is now a fixed template —
`Source:`/`Request:`/`Response:`/`Invariants:` — because a diff on an
`Invariants:` line is meaningful and a diff on a paragraph of prose is not.
A note's key is `"<table> <kind>"` instead of `"METHOD /path"`, so renaming a
resource's REST path carries its notes with it instead of archiving them
under a new key. And a route mounted by hand with `huma.Register` — the one
with no `Describe()` to fall back on, and so the one that most needed a
written note — now gets a section too, via `Project.HandwrittenOps`. Closes
[#211](https://github.com/mind-vm/sqlb/issues/211).

**studio's README now says what it doesn't do, not just what it does.**
A real-consumer review of `v0.13.0`'s studio confirmed the built scope works —
hidden columns excluded, row-scoping held, bearer-token login as designed —
and then named what "an uncurated data/schema/action browser" was leaving
unsaid next to Django's admin: no curation layer, no inline/nested editing, no
bulk actions, no history/audit trail, no permission-configuration screen.
None of that was ever promised by [ADR-0053](architecture.md#the-manifest-describes-what-cannot-be-guessed),
and the README now says so directly instead of by omission.

**A worked cross-tenant admin surface, in `example/tasks`.** Answers "how do I
see all data from all tenants as an admin?" with the mechanism that already
exists — [ADR-0054](architecture.md#a-named-scope-is-releasable-at-the-mount)'s
named-scope release — written out as a copy-pasteable pattern. The boundary is
two independent halves: row visibility (naming the workspace-confining hook so
a mount may release it, while the soft-delete hook stays permanently
unreleased) and route access (`RequireAdmin("/admin/")`, gated on a
`PlatformAdmin` claim no public flow can set). Neither half is the boundary
alone. Where releasing a model's only confining hook would otherwise leave
`rest.Resource` unable to mount ([ADR-0030](architecture.md#declared-scope-is-required)
doing its job), a permanent, unnamed no-op hook — `rest/scope.go`'s own
sanctioned answer — takes its place on `/admin/*`.

## v0.13.1

2026-08-14 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.13.1)

One bug, found the same day `v0.13.0` shipped it: the `CREATE EXTENSION`
statement `migrate.Diff` generates had no trailing semicolon. goose splits a
migration file's ungrouped statements on `;`, so the statement ran together
with whatever the file printed next rather than failing loudly — a real port
had to hand-edit the generated file before it would apply.

Predates `v0.13.0`'s own `btree_gist` detection
([#194](https://github.com/mind-vm/sqlb/issues/194)): the `vector` extension
change had the identical gap, undetected because both tests asserting on it
used `strings.Contains`, which a missing trailing character is invisible to.
Both are now exact-equality checks, confirmed to fail without the fix before
being restored ([ADR-0016](architecture.md#guards-proven-both-ways)).

No API change. A schema already on `v0.13.0` that declares `AddExclude` with
a scalar `=` inside a gist index, or a `Vector` column, regenerates a correct
migration on `sqlb migrate`; nothing else in a generated file changes.

## v0.13.0

2026-08-14 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.13.0)

Two separate pieces of work, not one shape. A first real external port —
sqlbcoach, `dcoach` ported onto `v0.12.0` — found four gaps and confirmed one
of last tag's own open questions; all four are closed here. Two commands land
alongside it, both additive: `sqlb docs` and a browsable `studio` module.

**One break.** It is *not* listed under [*Will
move*](compatibility.md#will-move), and that is a miss rather than a plan —
recorded [where the announcement should have
been](compatibility.md#four-that-broke-without-being-listed-here-first).

**BREAKING: `schema.Mutation` is gone, folded back into `Action`'s item
form.** `v0.12.0` shipped `Mutation` beside `Action`'s existing item-form
envelope for the same row-scoped write, on the argument that naming the shape
at the declaration was worth a second type — and named its own doubt in the
same breath: *"deliberately not deprecated, since its form may still
change… left open until there is evidence rather than a guess."* The
evidence arrived the same day, in an independent port: swapping
`AddMutation` for `AddAction` on a live table changed the generated code by
exactly one identifier and nothing about behavior, route, or response shape.
Convex's query/mutation/action split — where `Mutation`'s name was borrowed
from — names differences that exist in a query-*language* RPC surface;
sqlb's row-scoped write is already one REST verb with one envelope, so there
was never a second shape for a second name to describe.

The mechanical edit: `AddMutation(schema.Mutation{...})` becomes
`AddAction(schema.Action{...})` — the two structs share every field, `Name`
through `Description` — `rest.Mutation[T,In]` becomes `rest.Action[T,In]`,
and `rest.MutationSpec` becomes `rest.ActionSpec`. `Query` is unaffected:
its split from `Action` is a genuine shape difference (`Do`'s return type
vs. a row), and the argument for it still holds.

### Three more from the same port, none breaking

**CRUD reads as complete and is not.** `schema.CRUD`/`rest.CRUD` is
create+read+update+delete, and the constant's own doc comment already said
to combine it with `OpList` *"for a fully exposed collection"* — nothing
enforced it, so a table declared with bare `CRUD` mounted a working
create/read/update/delete and a silent `405` on the collection route.
`rest.Options.validate()` now refuses to mount a resource whose `Ops` is
exactly `CRUD`.

**`AddExclude`'s `btree_gist` requirement now arrives with it.** A scalar
`=` inside a gist `EXCLUDE` — `coach_id WITH =, tstzrange(...) WITH &&` —
needs the extension, and nothing generated it: the only worked pattern in
the tree for an extension the diff engine does handle (pgvector) is
actively misleading here, since a hand-written `CREATE EXTENSION` after the
migration containing the inline `EXCLUDE` fails outright on a fresh
database. `migrate.Diff` now emits `btree_gist` in the same migration as
whatever needs it.

**A column can be `WriteOnly`.** `Hidden` conflated "never read" with
"never written" — a column like `QuizOption.is_correct`, set by the coach
who authored the quiz and never served to the student taking it, had no
generated create or update at all, and the only way out was hand-writing
the whole route. `WriteOnly` is `Hidden`'s converse: omitted from every
response the same way, but still settable through the generated body and
still present in the typed column facade, so trusted Go code — a grading
hook — can read it back.

### `sqlb docs` writes a feature checklist that survives schema evolution

Every other generated artefact is fully owned by sqlb — a rerun overwrites
it and `check` gates on that — which cannot be how a file meant for a coding
agent or a teammate to fill in with what an endpoint actually does works.
`sqlb docs <package>` writes `FEATURES.md`, keeping whatever notes sit
inside a still-existing endpoint's key on rerun, and archiving the rest
instead of discarding it.

### `studio` is an uncurated data/schema/action browser

Over `sqlb.json`, its own module off the engine's release cadence.
[ADR-0053](architecture.md#the-manifest-describes-what-cannot-be-guessed)
declined to carry any UI, on the argument that a curated admin needs
guesses — a row label, a field order — the manifest cannot answer; that
argument holds and stays. It does not hold for a raw grid over
primary-keyed rows, Convex's dashboard shape rather than Django's, and the
manifest was already sufficient. Every data fetch goes through the
generated REST API with the operator's own bearer token, never the database
directly, so the browser sees exactly what that token could already fetch.
Schema, data grid, edit/create, and action invocation; no delete, no bulk
edit, no service credential, no logs.

### Deliberately not done

**No retroactive edit to `v0.12.0`'s own release notes** to say the
`Mutation` doubt was already there — a tag message is immutable and the
page quotes it, and editing the prose around a quote to imply it said more
than it did is the one repair worse than the gap.

**No new lint rule for the `CRUD`-without-`OpList` mistake**, though
`schema.Lint()` — unwired from the CLI entirely — was already positioned to
catch it earlier and advisory-style; filed as
[#201](https://github.com/mind-vm/sqlb/issues/201), not built here.

### What to expect on upgrade

- **The one break.** `AddMutation`, `schema.Mutation`, `rest.Mutation` and
  `rest.MutationSpec` no longer exist. Replace with `AddAction`,
  `schema.Action`, `rest.Action` and `rest.ActionSpec` — same fields, same
  envelope.
- A bare `schema.CRUD` with no `OpList` now refuses to mount rather than
  serving a silent `405` on the collection route. If that describes yours,
  add `OpList`.
- Nothing else in this tag changes behavior for a schema that does not
  reach for `WriteOnly`, `AddExclude`, `sqlb docs`, or `studio`.

Three issues from the same port: [#193](https://github.com/mind-vm/sqlb/issues/193),
[#194](https://github.com/mind-vm/sqlb/issues/194),
[#195](https://github.com/mind-vm/sqlb/issues/195). `sqlb docs` and
`studio` are new. ADR-0057 revised in place; ADR-0053 revised in place.

## v0.12.0

2026-08-14 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.12.0)

Three separate pieces of work, not one shape. The change feed's durable half
got built. Four gaps a comparison against bun found got closed. And a read got
a name of its own — a declared `Query`, alongside a row-scoped write as a
declared `Mutation` — with the boilerplate every server's `main.go` repeated by
hand collapsed into one call. No breaking changes in this tag.

### The change feed is durable

[ADR-0012](architecture.md#change-feed-outbox) has been *Exploring* since
2026-07-27, the largest unbuilt item in the vision: what shipped instead was
only the half downstream of it ([ADR-0045](architecture.md#the-stream-is-a-seam))
— the SSE endpoint, the wire format, `rest.Broker` — which holds events in
memory and is therefore at-most-once and correct on exactly one replica. Both
limits produce the same failure, a client that never learns a row changed and
displays it forever, invisible from the outside.

The `outbox` package is the other half: a table written in the same
transaction as the change, and a dispatcher that tails it. The swap is one
constructor call — `rest.TxPublisher` is a new optional interface, so an
existing `rest.PublishChanges` records into the writing transaction when it
can and announces after commit when it cannot. Nothing else in an application
changes: not the registration, not the endpoint, not the wire, not either
generated client. The visible difference is the failure — an `Outbox` that
cannot record an event rolls the mutation back, because a row that exists
while every subscriber believes it does not is exactly what this feed exists
to prevent.

Ordering needed an answer this record could not have had before there was an
implementation: a `bigserial` does not promise rows become visible in id
order, so `Record` takes a transaction-scoped advisory lock before it appends,
held to the commit. Gating the tail on `pg_snapshot_xmin` instead was rejected
— it has no write-path cost and is wrong in a way that took a while to see,
since a transaction can hold an earlier id and a later xid. What the lock
costs is stated rather than discovered: writes to published models serialise
from the outbox insert to the commit, and ADR-0012 carries the trigger to
revisit it — nothing here has measured what the ceiling actually is, which is
this record's stated low confidence.

### Four things worth taking from a comparison against bun

**A batch too wide for one statement is refused in rows, not in parameters.**
`InsertRows` checked nothing before this, so 12,000 rows of a six-column model
silently compiled to 72,000 bind parameters and failed with a driver error
naming a unit nobody wrote. The refusal is now sqlb's own and actionable: the
row count, the column count, the ceiling, and the largest batch that would
work. Chunking the batch into several statements was rejected — it stops being
atomic outside a transaction, and how to divide the work is the caller's
decision.

**A query nests inside another.** `Exists`, `NotExists`, `Field.InQuery` and
`Field.NotInQuery` compile a nested query into the surrounding statement's own
compiler. The obstacle was never the SQL — hooks apply when a query runs, so a
model confined by a `BeforeQuery` scope could contribute its rows to somebody
else's `WHERE` clause with the confinement silently absent. A nested query
whose model would run confined is refused unless it has been `Resolved`
first; auto-resolving it during compile was rejected, since resolution
produces a new value and a rewriter that misses a node type fails open exactly
where this guard exists to prevent that.
[ADR-0055](architecture.md#a-nested-query-runs-nobodys-hooks).

**A caller may narrow an expanded row.** `ExpandOnly("author", "id", "name")`
takes columns off an expanded row and can put none on — `Hidden` stays hidden,
a computed column stays absent, both refused by name rather than silently
skipped. Opt-in, per query, narrowing only.

**A junction is a table, stated in one place instead of three.** sqlb has no
many-to-many keyword and none is planned: a junction is an ordinary table with
two references, reached by querying it directly, because a junction is almost
never empty and a declared traversal would hide the row that holds
`added_at`, `role`, `position`. [ADR-0056](architecture.md#a-junction-is-a-table)
says so; the position existed already, split across
[ADR-0034](architecture.md#one-column-addresses-a-row), `best-practices.md` and
`schema/references.md`, none of which answered the question somebody arriving
from bun actually asks first.

### A read is a declared Query, a row-scoped write is a declared Mutation

Both new, both still *Exploring* in the ways that matter — only one example
application, and the naming and the typing of `Mutation.Writes` both changed
shape while building this.
[ADR-0057](architecture.md#a-read-is-a-query-and-a-row-scoped-write-is-a-mutation).

`Query` is a `GET` with no fetch, no lock and no obligation check; `Do` runs
against whatever `Executor` it is handed and inherits its hooks. `Mutation` is
`Action`'s item-form envelope under its own name.
[ADR-0043](architecture.md#declared-actions)'s `Action` is unchanged and keeps its
item form — deliberately not deprecated, since a table can declare the same
shape two ways today and nothing refuses the redundant one, left open until
there is evidence rather than a guess.

`codegen` wires both: `Register` grows `mutations` and `queries` parameters
alongside `actions`, each present only if a table declares one. A generated
`Query`'s result is fixed at `[]T`, every row of the table it reads, filtered
— a query wanting a different result stays hand-mounted.

`rest.Serve(ctx, ServeConfig, mount)`
([ADR-0058](architecture.md#serve-owns-the-boilerplate-mount-is-the-seam))
collapses the pool-open, ping, migrate, listen and graceful-shutdown
boilerplate every server's `main.go` wrote identically into one call, leaving
`mount(*Server, sqlb.Executor) error` as the one seam that cannot be generic.
`sqlb init -module <path>` scaffolds a new project on it — `go.mod`, a
one-table schema, `sqlb.go`, `cmd/server/main.go` — verified against the real
published module on the proxy: init, tidy, generate, migrate, run produces a
working CRUD API from an empty directory in five commands.

## v0.11.0

2026-08-09 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.11.0)

The last tag named two things it would not do, and this one does both. A
tenant-keyed singleton was *"a release of its own, not the ninth item on a
list"*; [ADR-0050](architecture.md#reachability-is-a-property-of-the-mount) scoped its column
split to columns and said in as many words that a second surface differing in
rows was the alternative it did not cover. Both came out of the same adoption
and both needed a *shape* rather than a setting, which is why neither fitted in
the release that found them.

The other half of this tag is smaller and less flattering: four conventions that
existed only as prose became checks, after each of them drifted in the space of
a week.

**One break.** It is *not* listed under [*Will
move*](compatibility.md#will-move), and that is a miss rather than a plan —
recorded [where the announcement should have
been](compatibility.md#four-that-broke-without-being-listed-here-first).

**BREAKING: a write evaluates no computed column unless asked.** `Insert`,
`Update` and `Delete` grow `WithComputed`, defaulting to none, and
`rest.Options.Computed` narrows a write's `RETURNING` as it already narrowed the
read's projection. Reads took this opt-in in `v0.6.0` and nothing revisited
writes, so [ADR-0041](architecture.md#computed-fields)'s clause keeping computed
columns in `RETURNING` *"so a POST response carries the derived fields without a
second read"* outlived the default it was written against.

An adopting application found all three consequences. A per-write tax on
aggregates nobody read. A create whose subquery counted rows the same
transaction had not written yet — so the returned value was *always* wrong and
the second read the clause exists to delete had to come back anyway. And a
subquery naming another module's table riding into every insert, which made the
table unwritable unless that module's tables were present, and failed the
module's own isolation boot test.

The mechanical edit is `WithComputed(names…)` on the statement, or `Computed` on
the resource. It is [#147](https://github.com/mind-vm/sqlb/issues/147)'s
direction and the same argument: the failure of the new default is a missing
field that the first test which looks catches, where the failure of the old one
is a cost nothing reports and a value that can be silently wrong. A column
carrying `Needs` is refused rather than accepted-and-skipped — a write has
nowhere to take a bind from, so naming one asks for something no statement here
can produce.

### A tenant-keyed singleton is a shape

A table keyed by the column that scopes it has one row per caller, and neither
exposed shape fitted: `OpList` answered a one-element `{items:[…]}` envelope for
a resource that is definitionally singular, and `OpRead` put the caller's own
tenant id in the URL — a value the server already holds and the `BeforeQuery`
hook already enforces, so the segment is redundant when it matches and a lie
when it does not.

```go
Expose(schema.REST{
    Path: "/billing-subscription",
    Ops:  schema.OpSingleton | schema.OpUpdate,
})
```

`OpSingleton` changes the resource rather than adding a route beside it: the
item path loses its `{id}`, so `OpUpdate` is `PATCH /billing-subscription` and
`OpDelete` is `DELETE /billing-subscription`. It needs no primary key, which is
what lets a table keyed only by its tenant column be a resource at all.

**The refusal is the design.** There is no key in the path and no key predicate
in the statement, so the read compiles to `SELECT … FROM billing_subscriptions`
and the hook appends `WHERE org_id = $1`. Read cold those statements are
under-constrained, and without the hook they are: on an unconfined table the
read answers an arbitrary row and the `PATCH` reaches every row there is. So the
chain is startup refusals rather than convention — `OpSingleton` requires a
`Scoped` column, a `Scoped` column requires the hook — checked in `sqlb generate`
and again at mount, because `rest` is usable without the DSL. Two rows is a 500
rather than a pick, for the same reason. `OpList` and `OpRead` are refused
alongside it, named separately because the fix differs.

A singleton reports *no* filterable, sortable or searchable columns and emits no
filter vocabulary in the clients even where the columns declare those
capabilities: its one `GET` rejects every query parameter but `?expand`, so
publishing them would document requests that answer 400.
[ADR-0052](architecture.md#a-singleton-is-an-op-that-removes-the-id).

### A named scope is releasable at the mount, and only a named one

Row visibility is a `BeforeQuery` hook keyed by the model's Go type, reaching
every statement whose subject is that type — so a rule registered to confine a
storefront confined the admin panel that exists to see the drafts. ADR-0050
closed the columns half of that in `v0.10.0` and said the rows half needed a
second model over the same table, which gives up all four derivations.

```go
sqlb.On[Product](reg).Scope("storefront").BeforeQuery(publishedOnly)
admin := db.WithoutScope("storefront")
```

`rest.Options.Unscoped` selects it at a mount. Only `BeforeQuery`,
`BeforeUpdate` and `BeforeDelete` are nameable — `BeforeCreate` stamps a row
rather than confining a set, so there is nothing to be released from.

Three properties are the design, and the second is what made it safe to add at
all.

**An unnamed registration cannot be released by anybody.** `Hooks.BeforeQuery`
is unchanged and absolute, so every existing registration stays unreleasable and
the short spelling stays the safe one: the author of a rule decides whether it
is negotiable, next to the rule rather than at the mount that would like to be
out of it.

**The obligation check runs after the release.** `rest.Resource` derives the
released handle first and checks against the handle it will actually serve from,
so a `Scoped` model whose every confining rule a resource released does not
mount. [ADR-0030](architecture.md#declared-scope-is-required) declined an escape
hatch on the grounds that *"an unused escape hatch is the thing most likely to
be reached for reflexively"*; a hatch that still refuses the case the check
exists for is not that hatch.

**A scope name belongs to the registry, not to a type.** "A shopper sees the
published catalog" is one rule over four tables, so a handle releases it once
and the release reaches all four — including models a request arrives at through
`?expand`, whose hooks run requalified onto the join alias.
[ADR-0054](architecture.md#a-named-scope-is-releasable-at-the-mount).

### Five more from the same set of reports

**A write's response no longer states a definite false.** The mount serialised
its whole read projection, so a `Needs`-computed column came back at its Go zero
value on the one path nobody looked at — the permanently-zero JSON key ADR-0041
exists to prevent. The key is now absent rather than zero. The reporting
consumer had ported a module to sqlb *because* its hand-written `PATCH` lacked
the `EXISTS` predicate its `GET` had; the declaration moved the bug into the
runtime rather than deleting it, and their tests passed because they asserted on
the `GET`.

**A resource can declare what its list is.** `schema.REST` bounded every
dimension of a list request except the one deciding what the list *is*.
`DefaultSort` lands on `schema.REST`, `rest.Options` and `filter.Options`,
checked by `sqlb generate` and again at mount against the sortable columns — a
default naming an unsortable column would otherwise answer 400 to a client that
sent nothing at all. `?sort` replaces it and the primary-key tiebreak is
appended either way, so cursors are unchanged. It travels the manifest, the
generated mount, the OpenAPI description, the generated skill and the ejected
handlers, which is the point of declaring it rather than keeping it in an SDK
facade where no other client and no agent reading the spec ever sees it.

**A hook can reach past its own model, and nothing showed it.** "Write the
consequence" is the operation [the hooks page](queries/hooks.md) holds up as the
reason hooks exist, and every `TxFrom` example in the repo stayed inside the
hook's own model. The obvious way to write it is wrong twice over: `TxFrom`
hands back the handle the *request* is running on, so a checkout's stock
decrement inherits the buyer's scope and updates nothing, and `Exec` reports
that as an empty slice and a nil error — so the order commits having reserved
nothing, and both mistakes are invisible in the Go code and in review. The
discriminator is not "is this hook writing to another model" but "is the
request's scope a true statement about the row I am about to touch": same axis,
`tx`; different axes, `tx.WithHooks(system)`. A comment's `AfterCreate` bumping
its task's `comment_count` is the same-axis case already in this repository, and
a reader escalating there would drop a confinement whose absence shows up as
another tenant's counter moving.

**Two errors and a comment that pointed somewhere else.** `qualifyExpr`'s
refusal recommended writing the scope "with `F()` and the comparison operators",
which cannot express the commonest raw scope there is — a child table confined
through its parent needs `IN (SELECT …)` and the predicate vocabulary has no
subquery node, so the reader searches for an API that is not there and then
denormalises the scope column onto the child. `FromSQL` accepted a subquery into
another module's tables with no comment, where `ExternalRef` refuses the
identical coupling with a stated reason; sqlb cannot check it, so the doc
comment is the only lever and it was silent. And "a hook is registered per
model; there is no receiver for all of them" is now a stated gap in
[domain logic](concepts/domain-logic.md) with its counter-argument attached.

### `impact` sees a `WireCase` flip

[Compatibility](compatibility.md#frozen) has said since 2026-08-02 that
"changing a deployment's `WireCase` is a breaking change for that deployment,
exactly as renaming a column is". That sentence reached no code: `restcompat`
recorded each field by its *column* name and never recorded the registry's case,
so flipping a schema from `Verbatim` to `Camel` respelled every field of every
resource and `sqlb impact -error` stayed green. Both snapshots held the same
column names, and a comparison matching columns by column name is blind to the
one edit that renames all of them at once.

`Snapshot.WireCase` is new and one breaking finding reports a change to it,
naming both spellings and one column that actually moves — `author_id is now
authorId` — because "the wire case changed" is not actionable without them.
**One finding, not one per column:** every field did change, so N lines would be
accurate and would also report the consequence and lose the cause, which is a
single-line edit.

**Absent means `Verbatim`, deliberately.** The field is `omitempty`, so a
`Verbatim` schema's snapshot is byte-identical to one recorded before the field
existed and no committed `restcontract.json` needs re-recording. A schema that
already declared `Camel` sees one spurious break the first time it is checked
and clears it with `-write`. That is the safe direction and the alternative is
not: reading absence as "not recorded" would suppress the finding against
exactly the baselines that predate the check.
[ADR-0039](architecture.md#a-schema-edit-is-an-api-edit).

### The TypeScript writes reach the layer the reads were already at

`queryOptions` and `infiniteQueryOptions` have shipped since `v0.2.0` and were
being missed because the docs named the file and never showed a `useQuery` call.
The write side genuinely stopped at the fetcher, so turning `createTask` into
something `useMutation` takes was four lines every consumer wrote for
themselves. `mutationOptions` is now emitted per write, declared verbs included
— an action is a write whose route the schema knows, and leaving it out would
make it the one mutation still hand-written next to three that were not.

What it deliberately does **not** emit is the half that matters. There is no
`onSuccess`: what a write invalidates depends on which views an application
keeps, and a computed view is not a table, so its key cannot be derived at all.
A generated `onSuccess` would be a guess, and a guess in generated code is
precisely what gets copied out and edited. `keysByTable` is the mechanical half;
choosing what to invalidate stays with the caller. No `mutationKey` either, on
the narrower ground that a key shape is expensive once anything consumes it and
nothing here needs one. [ADR-0028](architecture.md#typescript-client).

### A verb the resource already generates is refused

A table exposing `OpCreate` and declaring `Action{Name: "create"}` validated
clean and then failed everywhere downstream: two `createTask` declarations in
`client.gen.ts` so it does not compile, two `create:` properties in
`queries.gen.ts`, and Huma panicking at startup with `duplicate operation ID:
create-post`. The refusal goes in schema validation rather than in the emitters
— five surfaces would each need their own version of it, they would disagree
about the wording, and four of them are downstream of a declaration that is
already wrong.

The taken verbs are the *generated* ones rather than `Op.String()`'s: `OpRead`
is `get` in the clients, on the command line and in the document, so `get` is
refused beside it and `read` is not.

The DSL is optional ([ADR-0010](architecture.md#codegen-is-optional)), so
`rest.Action` and `rest.CollectionAction` return an error too, naming the id,
the method and path already holding it, and the two ways out. The scan is Huma's
own rather than a table of the verbs each `Op` generates: `rest` does not import
`schema`, so a table here would be that table's second copy with nothing keeping
them honest, and it answers a narrower question — the scan also catches two
resources sharing a `Name`, and an action colliding with one mounted elsewhere.
Known limit, in the doc comment rather than papered over: an action mounted
*before* its resource still panics from inside `Resource`.

### Four rules that were prose are now gates

Each of them drifted first, which is the only reason they are here.

**Both workflows green is a gate.** `CLAUDE.md` has said since `v0.7.0` that a
release needs a commit where *both* `ci` and `pages` are green, and gave the
reason `gh pr checks` cannot answer it. Saying it did not make it happen:
`v0.7.0` was tagged with `pages` red and `v0.8.0` nearly was.
`.claude/hooks/verify-release-gate.sh` is a `PreToolUse` gate on `git tag`, the
tag push and `gh release create`. Infrastructure failure asks rather than denies
— being unable to see the runs is not the same as the runs being red, and a gate
that blocks when the network is down is a gate that gets switched off. `/release`
is the procedure around it and writes down the ordering that makes the gate
satisfiable: **tag the releases-page commit, not the feature commit.**

**The ADR status vocabulary is four words.**
[ADR-0040](architecture.md#the-driver-is-a-dependency) carried `Accepted` for ten
days, in [a directory](architecture.md#decisions) whose README says in so many words that
there is no *Accepted* and no *Final*. A person caught it by eye. `adr-check`
reads `docs/adr` and refuses a status outside the vocabulary, a record with no
index row, a row whose link no longer names the record, an orphan row, an index
cell contradicting the record, and a missing or unparseable `Confidence` or
`Last reviewed` line. Compound statuses are real, so each slash-separated part
must *lead* with a vocabulary word rather than match one.

**The tagline says what the project is, in all four copies.** The first sentence
anyone read described "a schema-first data layer … typed composable queries, a
validated REST filter grammar, and domain hooks", which is the query layer and
the seam beside it — a fraction of what a declaration is the source for. The gap
matters more here than it would elsewhere, because the name does not close it:
`sqlb` reads as "SQL builder", so the one place the declarative claim could be
made was the one place it was missing. All four copies now open with
**Declarative Postgres for Go** and name what is derived, and `tagline-check`
gates the *claim* rather than the wording — the lede, then six terms naming the
derived surfaces within 400 characters of it.

Renaming was considered and declined. `sqld` was the candidate; it is the libSQL
server binary and one letter from the name it would replace, so every existing
mention becomes ambiguous forever. A rename touches 596 import lines and 161
non-Go files to buy branding, and pre-1.0 a subtitle buys the same thing for
nothing.

And **`tagline-check` itself was wrong on its first working day**, which is the
most useful item in this list. The lede test asked whether the phrase appears
anywhere: lowercased, `declarative postgres for golang` contains `declarative
postgres for go`, so a copy that drifted to *Golang* while the other three said
*Go* passed the gate — the exact drift the check exists to catch, in the one
phrase it is built around. The both-ways proof missed it because both mutations
were too large. A guard proven only against total absence is proven against the
easy half ([ADR-0016](architecture.md#guards-proven-both-ways)).

### Deliberately not done

**No admin emitter**, and no row-label declaration to feed one. A UI is HTML,
CSS, a component vocabulary, a theme and a browser support matrix, none of which
exist here and all of which would tie a release cadence to frontend churn rather
than to the schema.
[ADR-0053](architecture.md#the-manifest-describes-what-cannot-be-guessed) states the
rule the manifest had been following without saying so: it carries what a
competent author cannot guess and would get wrong silently, and nothing an
author can decide and the compiler will check.

**The manifest does not carry what a mount releases**, so `restcompat` cannot
see a surface change what it is exempt from between versions. That is ADR-0054's
own open question and the likeliest next step.

**No refusal of an unregistered scope name in `WithoutScope`**, because a
registry may gain registrations after a handle is built; `rest` refuses it,
where the whole registry is known. And nothing changed in the hand-written
`rest.ActionSpec` path beyond the error above.

### What to expect on upgrade

- **The one break.** A `POST` or `PATCH` response no longer carries derived
  fields unless the mount declares `Computed`; the key is absent rather than
  zero. Everything else in this tag is additive — a schema setting none of the
  new fields generates what it generated before.

- A hand-written statement that relied on `RETURNING` evaluating computed
  columns needs `WithComputed`. A statement naming a `Needs` column there is
  refused rather than skipped.

- A schema that already declares `WireCase(Camel)` sees one spurious
  `restcompat` break the first time `sqlb impact` runs, cleared with `-write`. A
  `Verbatim` schema's committed `restcontract.json` is byte-identical and needs
  nothing.

- Regenerating a TypeScript client emits `mutationOptions` per write into the
  file that already takes `@tanstack/react-query`, and the TanStack import is now
  computed from the body rather than fixed — so a read-only or write-only schema
  imports only what it uses under `noUnusedLocals`.

- A schema declaring an action named after a verb its own exposed operations
  generate now fails `sqlb generate` rather than emitting code that does not
  compile and a server that does not start. If that describes yours, it was
  already broken downstream.

- Nothing about hook registration changed. Named scopes are opt-in and an
  unnamed registration cannot be released.

Ten issues: [#158](https://github.com/mind-vm/sqlb/issues/158),
[#159](https://github.com/mind-vm/sqlb/issues/159),
[#160](https://github.com/mind-vm/sqlb/issues/160),
[#161](https://github.com/mind-vm/sqlb/issues/161),
[#163](https://github.com/mind-vm/sqlb/issues/163),
[#164](https://github.com/mind-vm/sqlb/issues/164),
[#165](https://github.com/mind-vm/sqlb/issues/165),
[#166](https://github.com/mind-vm/sqlb/issues/166),
[#167](https://github.com/mind-vm/sqlb/issues/167),
[#177](https://github.com/mind-vm/sqlb/issues/177). ADR-0052, ADR-0053 and
ADR-0054 are new; 0028, 0030, 0036, 0039, 0040, 0041 and 0044 carry revisions.

## v0.10.0

2026-08-05 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.10.0)

The release where the reports started rhyming. Twelve issues, every one of them
from somebody putting sqlb on a real schema — a sixteen-registry adoption, a
headless shop, a product catalog — and the last four turned out to be one shape
rather than four small gaps. A layer below the declaration could say something
the declaration could not, and nothing reported the gap. Naming that is the most
useful thing in this tag.

**One break**, and it was listed under [*Will move*](compatibility.md#will-move)
before it landed.

**BREAKING: a computed column is nullable unless it says otherwise.**
`schema.Computed` generates a pointer field now, and `NotNull()` is the opt-in
for an expression that cannot produce a `NULL` — a `count(*)`, an `EXISTS`, a
comparison already guarded against its own nulls. The old default was the one an
expression cannot satisfy: a correlated subquery matching nothing is `NULL`,
arithmetic over a nullable column is `NULL`, and a comparison against one is
`NULL`. The failure was a 500 at scan time saying `cannot scan NULL into
*string`, naming the generated model rather than the `Computed` call that
produced it, on data a fixture is unlikely to contain — and both gates were
green, because `generate` had no opinion and `Diff` correctly ignores a column
that is not in the database.

The mechanical edit is `NotNull()` on every computed column whose expression
genuinely cannot produce a `NULL`. Leaving it off is the safe direction: a
pointer scans a non-null value fine and the reverse is the 500. Stored columns
are untouched, as is the structs-first path, where the Go field's own type has
always carried this. Inferring nullability from the expression was the
alternative and its own objection stands — an incomplete inference is wrong in
the unsafe direction, which is the direction the change is about.

### A gap below the declaration is reported, not silent

Four issues filed as minor, each with a workaround already in hand, and the
missing spelling was the cheap half in every one. The expensive half is that a
tool reporting *no difference* is making a claim, and a tool that cannot see a
property makes that claim about it whether or not a difference exists. So: close
the gap where that is cheap, and where it is not, make the gap visible — a
refusal at the boundary, a report from the tool that reads the database, or a
sentence where the reader is standing. What that rules out is the fourth option
all four had: correct behaviour, an available workaround, and the two facts
documented in different files from each other
([ADR-0051](architecture.md#a-gap-in-the-declaration-is-reported)).

**Two cost ceilings the mount could express and the schema could not.**
`schema.REST` gains `MaxSortTerms` and `MaxOffset`, so all five per-request
bounds are declarable beside the table. `MaxOffset` is the one that matters: its
default of 100,000 is right *as a default* precisely because it has to be safe
for a table nobody described, which puts it two to four orders of magnitude
above what any particular resource wants. A catalog with ten thousand products
has no legitimate offset past ten thousand, and every one above it is a
guaranteed empty page that still costs a scan to the end. Two surfaces were
dropping the pair on the way out: the ejected exit emitted a literal
`MaxSortTerms: 0` and had no `MaxOffset` at all — so the handlers that replace
the API served `?page=50000000` while the API refused it, with no `?cursor` to
redirect to since keyset paging does not come out — and the generated skill
stated the filter budget and neither of the others.

**The inspection points now show the statement that runs.** `SQL()` renders what
the caller built, which on a model confined by a `BeforeQuery` hook is a
statement with the confinement missing. `Explain` is the sharp half, because its
documentation claims otherwise: `WHERE status = $1` and
`WHERE status = $1 AND org_id = $2` have different plans, the second is the one
with the composite index behind it, and a plan-regression test written on the
first stays green through exactly the change that makes the real query seq-scan.

```go
q, err := sqlb.Query[Post]().Where(…).Resolved(ctx, db)
sql, args, err := q.SQL()   // … AND "org_id" = $2
```

`Builder.Resolved` applies the hooks and the expansion scopes and hands back a
copy; `Update` and `Delete` have the same for theirs; `Explain` and
`ExplainAnalyze` compile through it, which on `ExplainAnalyze` is a correctness
property rather than a reporting one, since it executes. The exec paths were
rewritten onto `Resolved` rather than keeping their own copy of
clone-then-run-hooks: the failure being fixed is two paths disagreeing about one
statement. `Insert` is deliberately not resolvable — `BeforeCreate` rewrites the
rows, so resolving one would mutate the caller's data as a side effect of
inspecting it.

**A constraint's deferrability is declared, read back and diffed.** The missing
spelling was the small half. The interesting half is that the round trip was a
fixpoint *because both sides were blind to the same property* — the introspector
did not read `condeferrable`, the differ had no field to compare, and a
hand-altered constraint passed `sqlb check` green. `Unique.Deferrable` and
`Field.Deferred()` declare it; every constraint kind is read; and the kinds it
cannot be declared on are reported as skips with their definition attached
rather than dropped in silence. The proof is the break-on-purpose: with the
mapping reverted the rebuilt database no longer matches the original and names
the constraint that lost its clause, while the fixpoint test *passes* — both
registries having dropped the same thing, which is
[ADR-0016](architecture.md#guards-proven-both-ways)'s failure mode stated about a
field rather than about an object.

**A hidden column can say it is the key it is looked up by.** `Hidden` names one
property — the value must never leave the process — and the generated facade
asserted a second by omitting the typed column. For a password hash they
coincide. For session tokens, API keys, reset and verification tokens, webhook
secrets and idempotency keys they do not: the presented secret is hashed and the
hash *is* the lookup key, so `Hidden` took away the operation the column exists
for.

```go
schema.Text("token_hash").Hidden().LookupKey()
```

`LookupKey` keeps the facade entry and moves nothing else. It adds no capability
and no struct-tag token, and `?token_hash=eq.…` is still a 400 naming what would
have been accepted — a client that can probe a credential column by equality has
an oracle, and that refusal is what capabilities are for.

### One table can serve two surfaces

A headless shop reads `products` from a public storefront and from an admin
panel, and the admin surface exists precisely to serve `cost_price_minor` and
`internal_notes`. Neither lever reached it: `Hidden` is a property of the model
and there is one model per table, and `Expose` assigns rather than appends, so a
second call replaces the first.

`rest.Options.Columns` narrows a mount, and `filter.Options` carries it into the
parser *and* into `Apply`'s default projection — both, because a resource whose
parser refused a column while its projection selected it anyway would read the
value out of Postgres on every request and drop it on the way out. A column not
listed is unreachable: not projected, not filterable, not sortable, not
searched, not nameable in `?select`, cleared off any row a body produced, and
absent from the list a rejection offers back — that last one because a surface
narrowed to conceal something must not confirm the column exists.
[ADR-0050](architecture.md#reachability-is-a-property-of-the-mount) records what it
costs and what the stronger schema-side answer would need.

### A delete can hand its rows to a hook

`AfterUpdate` received the rows and `AfterDelete` received a count, and that
asymmetry stopped a port: a module publishing a domain event per mutation could
say how many posts were deleted and not *which*, and an event carrying no id is
worse than no event. `AfterDeleteRows` sits beside the count form rather than
replacing it, so nothing is added to the statement unless a hook of that kind is
registered and a program that only wants "did anything change" pays what it
always paid.

`rest.PublishChanges` moves to it, which is the half that was not asked for and
the reason to pay the cost: `Event.Scope` is read off the changed row, so a
keyless delete was also a *scopeless* one and every tenant's subscribers woke on
every other tenant's delete. [ADR-0045](architecture.md#the-stream-is-a-seam) had
listed keyless deletes as a what-would-change-our-mind gated on a measured
refetch cost; that named the wrong axis and the record says so.

### An action declares what it Touches

`Writes` is what the envelope persists — columns, on one row — and the same page
of docs hands the verb a transaction it can write anything through. Three tools
reported `Writes` as complete with no signal that a verb can exceed it, and the
CLI case is the sharp one, since
[ADR-0029](architecture.md#go-cli)'s argument for the CLI
is that `--help` answers a caller with no compile step. A declared write set of
two columns invites the inference that the route is confined to one row, and
that inference can be wrong by ten tables.

`Touches` names tables beside `Writes`'s columns and travels with it — the
manifest, the contract snapshot, the OpenAPI description, the generated doc
comment and `--help` — unenforced, deliberately, and a verb that declares
nothing now gets a sentence saying so rather than silence.

### One after OnConflictDoNothing is refused

"Give me exactly one row" and "do not produce a row on conflict" cannot both
hold, and the way it used to resolve was the worst reading available: the
conflict came back as `ErrNotFound`, through the same `if err != nil` as
everything else, from a call whose job was to make the row exist. The failure
inverts with state, so a test inserting into a clean database passes and only
the second call fails.

The refusal names both routes out, because which one is right depends on what
the caller wanted: `Exec`, whose empty slice and nil error are what "it was
already there" looks like, or `OnConflictUpdate` with the target as its own
update column, since a write that changes nothing is still a written row and a
written row is a returned one.

### The emitted skill was wrong about the wire, and two registries wrote one file

Both from adopting `Options.SkillDir` across eighty modules and 184 tables.

A camelCase registry got a capability table listing `org_id` and a closing
sentence asserting that those names are the JSON field names. An agent doing
exactly what the file said wrote `?org_id=eq.…` and got a 400 — worse than
guessing, since guessing from the camelCase model would have been right.
`generate-check` was green throughout, because the file was a faithful render of
a manifest that was itself wrong: `BuildManifest` reported the column's own name
in a section that describes the wire. It is fixed there rather than in the
emitter, so anything else reading the manifest for a contract is fixed with it,
and `Manifest.WireCase` and `ColumnManifest.Wire` are new so a consumer holding
only the document can tell the two spellings apart. `Wire` is absent where they
are equal, so a `Verbatim` schema's `sqlb.json` is byte-identical.

Worth stating next to [ADR-0049](architecture.md#the-skill-is-generated)'s claim
that gating is what makes writing instructions into a repository safe: this is
the first bug in that emitter the gate structurally could not have found. Being
gated proves the file matches the schema, not that it is *right* about it.

And `SkillDir` had no per-registry component, so every registry pointing at one
directory wrote the same `SKILL.md` and the last writer won — with the doc
comment recommending the placement that does it. `Options.SkillName` is the fix,
defaulting to `sqlb-schema`.

Also: `Immutable`'s doc comment now names its boundary the way `ReadOnly` does.
It is enforced at the REST layer and application code writing through the query
engine is trusted, which is the right design and was not what the sentence said.

### What to expect on upgrade

- The one break above. Everything else in this tag is additive: a schema setting
  none of the new fields generates what it generated before.
- An adoption whose database defers a foreign key, primary key, check or
  exclusion now gets report entries where it got none, and `sqlb introspect`
  exits non-zero on them. That is the change working — the entry is what turns
  an invisible divergence into one a person can decide about — and it is still a
  new obligation for a schema that had been quietly fine.
- A camelCase or snake_case schema's `sqlb.json` changes: the REST section's
  capability lists carry the wire spelling now, and columns carry both. A
  `Verbatim` schema's is byte-identical.
- A delete on a model with an `AfterDeleteRows` hook — including any model wired
  to `rest.PublishChanges` — runs `DELETE … RETURNING` and scans every row it
  matched. That is real on a bulk delete, and it is why the count form stayed.
- An ejected package regenerated from a schema declaring `MaxOffset` now refuses
  deep offset paging. It did not before, against a README saying it refuses what
  the API refused.

Twelve issues: [#142](https://github.com/mind-vm/sqlb/issues/142),
[#143](https://github.com/mind-vm/sqlb/issues/143),
[#144](https://github.com/mind-vm/sqlb/issues/144),
[#146](https://github.com/mind-vm/sqlb/issues/146),
[#147](https://github.com/mind-vm/sqlb/issues/147),
[#148](https://github.com/mind-vm/sqlb/issues/148),
[#149](https://github.com/mind-vm/sqlb/issues/149),
[#150](https://github.com/mind-vm/sqlb/issues/150),
[#151](https://github.com/mind-vm/sqlb/issues/151),
[#153](https://github.com/mind-vm/sqlb/issues/153),
[#154](https://github.com/mind-vm/sqlb/issues/154),
[#155](https://github.com/mind-vm/sqlb/issues/155). Two new records,
ADR-0050 and ADR-0051, and one revised, ADR-0045.

## v0.9.0

2026-08-03 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.9.0)

The release that measured the agent-facing claim rather than asserting it. Two
hand-written skills and one generated, gated the way every other emitted file
is — and then sixty A/B runs which said the honest case for it is latency and
not correctness, so the record says that instead of what it was built on. Beside
it, three constructs and one false alarm, all found the way v0.8.0's were: a
real database refusing to be described, or described wrongly.

**Nothing breaks.** Everything here is additive, opt-in, or a build-step tool,
and no name a program compiles against has moved.

**The schema an agent reads is generated**, because a written-down one cannot be
gated. `Options.SkillDir` emits `<SkillDir>/sqlb-schema/SKILL.md`: the mounted
path and operations per resource, the four capability lists with an undeclared
one named as *none* rather than omitted, the declared verbs and what they write,
the inverse relations `?expand` knows, and the tables whose declaration obliges
a hook.

```go
codegen.Options{ …, SkillDir: ".claude/skills" }
```

Capabilities are opt-in, so "can I filter on this column" has a different answer
in every project and no static document can carry it. `example/tasks` commits a
generated skill, so `generate-check` gates it in CI rather than a test asserting
the property in the abstract — and it earned that on the first run, catching a
real drift when the emitter changed after the file was written
([ADR-0049](architecture.md#the-skill-is-generated)).

What the document does not carry is prose. `introspect` reads `col_description`
off a live database and calls `Field.Comment`, so a comment is not necessarily
first-party text. Every other emitter passes those through safely because DDL
and OpenAPI are read as data; a skill is read as instructions. This one carries
names, types and capability flags and nothing else, guarded by a test that
injects an instruction-shaped comment as both a table and a column comment and
requires it absent.

**Then the claim was measured, and it shrank.** Twenty runs per arm across three
rounds, control given the schema declaration and treatment the same plus the
skill. Both arms answered ten direct questions at 50/50. Both caught every trap
in the final round — 80 trap-instances scored, zero misses on either side — and
a 2-of-5 silent failure seen at n=5 did not replicate at n=20, having been an
artefact of the prompt. What survived is cost: 3.5 tool calls and 47s against
1.1 and 19s, with the control's real figure understated because two of its runs
spawned research subagents that do not show up in the parent's counters. So the
emitter is worth ~290 bytes per resource for round-trips, not for correctness,
and ADR-0049 now says so and says the premise should not be restated without new
evidence.

**The two hand-written skills are the half no check can reach.** `sqlb-queries`
carries four traps that compile, pass their tests and answer the request: an
aggregate over an empty set scanning NULL into an `int64`, one bind parameter
numbered twice across a projection and a `GROUP BY`, a day filter against
`timestamptz` matching zero rows with no error, and `OnConflictDoNothing`
turning a retried write into `ErrNotFound`. Every sample was compiled and
rendered rather than written from memory, which caught three wrong idioms and
one claim about this API that had rotted in four days — which is the argument
for generating the other half, observed rather than reasoned. The second skill
is the adoption census, and its load-bearing content turned out to be not the
procedure but the two conditions that end an evaluation cheaply.

**An auto-incrementing integer key is declarable**, in both of Postgres's
spellings:

```go
schema.BigSerial("id").PrimaryKey()          // bigserial
schema.BigInt("id").Identity().PrimaryKey()  // identity, by default
schema.Int("attempt").IdentityAlways()       // identity, always
```

Neither was expressible before, so no auto-incrementing key was at all. Across
the eleven applications that reported it — 80 modules, 184 tables — exactly one
table was not describable and this construct is what it had; a drift gate is per
registry, so that one column took its module out of the gate. The substitution
was not the cheap one it looks like: all three tables using it used the serial
as the tiebreak that makes `ORDER BY occurred_at DESC, id DESC` a *total* order,
and the id is in a public interface, so widening it is an API change and sixteen
bytes a row on the highest-volume tables in the system.

Auto-ness is a property of the column and not a `Type`. A `bigserial` column
*is* a `bigint`: that is what the catalog reports, what an `ALTER COLUMN TYPE`
has to name, and what comes back when you read it. A type constant would have
given `int64` two spellings and split the filter grammar from the sort
machinery; the evidence the cut is right is that `scalarSQLType` did not change.
The older spelling is not deprecated, either — every report came from a database
that already has a serial, and a DSL that could only declare the modern one
would propose rewriting the column on its first diff
([ADR-0048](architecture.md#auto-incrementing-keys)).

**An enum value is data**, so a dotted one names a Go constant instead of
failing to parse. `task.assigned` produced `NotificationTypeTask.assigned` and
generation refused its own output, which was right about the output and wrong
about the value set, which had no declaration at all. Every run of characters
that cannot appear in an identifier is a word boundary now, which is what `_`
already was, so the value stays verbatim and the initialism table still reaches
`api.key`. Dart derived its members the same broken way and is fixed in the same
place; TypeScript was already correct, because a union is the raw strings. A
collision is refused with both values and the column named rather than emitted
as a duplicate const that fails in the consumer's package. This also reopens a
door: a table whose `CHECK` introspects back as an enum column is adoptable.

**Phase C stopped reporting a residual that was not one.** Postgres stores a
`CHECK` over a `varchar` as a cast of the array on first application and as a
cast of each element when fed that back, so the round trip is a fixpoint at two
iterations and the probe compared after one — 26 of these across eleven
applications, every one that shape, on schemas whose every table was clean. It
iterates now, bounded at three rounds, and the verdict carries the round count,
so a residual of 0 reached after two still says something was rewritten on the
way in. The reading it invited was wrong in the expensive direction: *this
schema will never be stable under sqlb*, about a schema that is stable, in the
one phase an adopter trusts precisely when Phase B looked too good.

**And the gate moved off the laptop.** Six packages across three modules each
started a Postgres of their own through testcontainers, so one `mise run ci`
brought up six servers and six worktrees testing at once brought up forty-two.
They read a DSN now and start nothing; `compose.yaml` defines the three servers
and `mise run pg-up` starts them, CI gets the same three as service containers,
and no reaper removes another worktree's containers by label any more. `pgtest`'s
`go.mod` goes from 50 modules to 6, `test-pg` from 49s to 12s, and 130 of its
135 tests take `t.Parallel()` because each already created a database of its
own. What runs before a push is `mise run preflight` — 17s, no containers —
since CI is the gate and running it twice only starved the machine.

Also, for anyone reading rather than importing: every library package's
introduction is in `doc.go` now rather than in whichever file sorted first,
moved verbatim, and `CLAUDE.md` is the map plus the four traps that are not
visible from the code.

**What it cost:**

- The database-backed suites require `SQLB_TEST_POSTGRES`,
  `SQLB_TEST_PGVECTOR` and `SQLB_TEST_PGBOUNCER`. There is no skip-when-absent
  path and no fallback that starts a container: an unset variable is a fatal
  error naming the task that fixes it, and `mise run pg-up` supplies all three.
- A project that sets `SkillDir` gets a file that wants committing, and this is
  the one emitter that writes into a directory sqlb does not own. Its path and
  the document's shape are under [*Will move*](compatibility.md#will-move),
  since the `SKILL.md` convention belongs to the agent tooling rather than to
  sqlb. The asymmetry is worth stating in advance: if this emitter is ever
  removed, the verb has to *delete* the file rather than stop writing it,
  because a stale skill still loads.
- That document is linear in exposed resources and uncapped. Measured over
  twelve real applications: 12KB at 29 resources, 37KB at 127. Past about 40KB
  the answer is an index with per-resource detail on demand rather than another
  round of trimming.
- Dropping the per-column table, which was 44-49% of it, cost the ability to
  name a column an agent should *not* filter on. Two of 25 runs invented the
  identifier while correctly saying no such filter exists. Probably still the
  right trade, recorded as a real cost.
- A column that becomes a serial on a table that already has rows starts its
  sequence at 1. The change says so in its hazard and names the `setval` to run
  first; it is not generated, because the row count is not in the schema and
  getting it wrong is a duplicate key.
- `sqlb survey`'s Phase C verdict names the round its fixpoint was reached on,
  so anything reading that line reads one more field.

**What is still owed is the trigger.** Inlining a skill assumes its description
already caused it to load, and nothing here tested that — a skills directory
that did not exist when a session started is not watched, so the emitted skill
could not be invoked from the session that wrote it. ADR-0049 lists that as the
live unknown rather than as a footnote, because if a schema skill is only ever
read when someone names it, the frontmatter is doing nothing and this design
reduces to a document with a pointer.

## v0.8.0

2026-08-02 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.8.0)

The release that stopped refusing tables. Every version before this one was
argued from the library outward. This one was argued inward, from two corpora
of real databases and one 312-route application, and what they said is that the
declaration language itself had become the thing blocking adoption — one
construct at a time, and never the same one twice.

**One break, and it is a word.** `sqlb-survey` is now `sqlb survey`, a second
binary folded into the one command tree: needing no schema package is a fact
about one verb's arguments, not a reason a user has to hear about a separate
command — and the one it made separate was the adoption probe, the first thing
somebody deciding whether to adopt sqlb would run, and the only thing `sqlb
help` did not mention ([ADR-0032](architecture.md#sqlb-command)).

```
go run ./cmd/sqlb-survey …   →   go run ./cmd/sqlb survey …
```

It fails loudly rather than quietly, which is the whole of the risk.

**Five constructs the database has and the DSL could not declare.** Each was
found the same way — a survey refusing a table, then a person deciding whether
to change the schema or give up — and each is here because the answer to that
question kept being wrong. The gate is per registry and all-or-nothing, so one
unmodelable table takes its whole module out.

```go
t.PrimaryKeyColumns("provider", "model_id")
t.Unique("tenant_kind", "tenant_id", "name")
t.AddExclude(schema.Exclusion{Using: "gist", Elements: …})
schema.SmallInt("pos_x")        // smallint, int16
schema.Real("confidence")       // real,     float32
```

The workarounds these replace are the reason they are worth the surface. A
surrogate UUID beside a composite unique index is a schema change forced by the
declaration language: 16 bytes and an index per row identifying something
nothing points at, plus a data migration on any deployed database. Widening
`smallint` to `integer` is four columns × two bytes × every row, forever, for
nothing. Both fail the rule an adopter actually applies, which is that a schema
change must be defensible if sqlb vanished tomorrow.

The exclusion constraint is the one where dropping the construct loses a
*correctness* property rather than performance or ergonomics. Its alternatives
were enforcing the overlap in Go, where two concurrent requests interleave
between the check and the insert — precisely the drift surface sqlb exists to
remove — or holding a permanent known-difference exception in the gate. One app
in ten, and the only skip in either corpus that cost correctness.

**Two things that were reported wrong rather than not reported.** A `serial`
column imported as an ordinary `bigint` whose default named a sequence, so the
table read clean and the DDL it produced did not run; it is refused with a
reason now, and the schema that found it goes from two apply failures and a
residual of one to a fixpoint. And an extension was invisible on both sides — a
clean `Report` and a clean `Diff` both claimed everything was represented about
a schema that could not be created, which surfaced as 228 identical `function
uuid_generate_v4() does not exist` errors naming a function when the missing
thing was an extension. `introspect` reads `pg_extension` now and prints the
statements to run, ahead of the skips, because that list is useless as trivia
and load-bearing as the step before a bootstrap.

Measured on the ten schemas that ranked composite `UNIQUE` first: clean tables
174 to 214 of 233, partial 59 to 19.

**The probe that found all of it is now a command**, rather than a throwaway
program written twice per adoption. `sqlb survey` reports the whole database in
three phases — the schema as a gate would see it, every table alone so a blocked
one is named rather than mixed into a list of skips, and a render into a scratch
database to separate a construct that survives import from one that survives the
round trip. `-modules` groups the verdict the way a modular monolith is
deployed, `-exclude` takes SQL wildcards so a project on another migration
runner needs no patch, and an unmatched-table set above 25% now says which two
explanations to check rather than reading as a shared core. `sqlb introspect` is
the single-shot half, and takes a `-dsn` rather than a package.
[Surveying an existing codebase](surveying-a-codebase.md) is the other half of
that census — the routes and queries in front of the database.

**Additive, and new:**

- **`WireCase`.** `schema.NewModule("app").WireCase(schema.Camel)` makes
  `created_at` read `createdAt` in the body, the filter, the sort, the OpenAPI
  document and both clients, and leave it `created_at` in the database, in every
  hand-written query and in `pg_dump`. One spelling per deployment, derived from
  the column, no mapping layer and no per-field override —
  [ADR-0036](architecture.md#the-wire-is-the-column-name) amended rather than
  reversed. It exists because six applications were blocked on the same rename
  and the escape the record offered was to rename 615 columns into quoted
  camelCase identifiers, which is not reversible and should not have been in the
  record. `Verbatim` is the default and regenerates byte-for-byte identically.
- **`rest.Reads`** — `OpRead | OpList`, named. The most common mount in an
  adoption was the one with no name, and with only `CRUD` named it read as a
  resource with two thirds switched off rather than as an app that already has
  its writes and has four different reasons a generated one would be wrong.
- **`?not=(…)`** joins `?and=` and `?or=`, so a negated group no longer has to
  be De Morgan'd by the caller — which got silently wrong rows rather than an
  error. Both grammars spell the same set, and a test pins that rather than
  leaving it to two parsers happening to agree.
- **The TypeScript and Dart clients emit their runtime once**, beside the
  client. A second module used to ship a second copy of `Page`, `Problem` and
  `Transport` and ask the application to wire a second transport; in Dart,
  nominal typing made those two *unrelated* classes, so no shared pager could
  accept both.

**And three that only show up in a profile.** A page of rows is one buffer with
its keys rendered at registration — 1,776 allocations to 279 on a fifty-row
page, 173µs to 120µs, byte-identical output. A timestamp is appended straight
into that buffer rather than marshalled, which is the fifty allocations
`time.Time.MarshalJSON` cannot avoid because the `Marshaler` interface makes the
value answer in bytes it owns. An identifier is quoted into the compiler's
buffer rather than into a string first, 25% off parse-apply-compile. None of the
three changes a byte of any statement or any response.

**What it cost:**

- Two files appear on the next regeneration, `runtime.gen.ts` and
  `runtime.gen.dart`, and want committing with it. A project with one module
  need not notice otherwise: both clients re-export the runtime, so an existing
  import keeps compiling.
- A composite-key table is declarable so that it can be *gated*, and is refused
  by name for REST exposure, as the target of a `Ref`, and for a non-collection
  `Action`. One column is what addresses a row in a URL, a cursor and a cache
  key, and each of those is a wire format.
- Changing a deployment's `WireCase` after it ships is a breaking change for
  that deployment, exactly as renaming a column is.
  [compatibility.md](compatibility.md) says so where it freezes the wire
  spelling.
- `real` widens to `double precision` and *not* to `numeric`, and a diff that
  would make that cast renders destructive: it swaps an approximate binary float
  for an exact decimal, so what comes back is the rounded expansion of the
  stored approximation rather than what anyone wrote.
- `CREATE EXTENSION` is printed for a person, never emitted into a migration and
  never dropped by one. Creating an extension usually needs a superuser a
  production runner deliberately does not have. Worth revisiting with a decision
  attached; not worth deciding as a side effect of fixing the diagnosis.

**One fix worth naming on its own**, because the failure it prevents is silent.
`Describe` checked its in-use flag when the `Description` was constructed and
wrote in the chained calls after it, so a query starting in that window raced
the writes to the fields the request path reads to decide what a caller may see
— a torn read there is a hidden column reaching a response, not a crash. Every
mutator now clones the model, writes the clone and publishes it, so a published
`*Model` is never written again and a statement in flight keeps a consistent
snapshot. [ADR-0010](architecture.md#codegen-is-optional)'s no-locks-on-the-read-path
constraint holds because the cost moved to the writer, where it is one copy at
startup. The two new concurrency tests are the only ones in the suite that run
two requests at once, and two of them fail against the old code — one without
the race detector, since copy-on-write is a property an ordinary CI run can
check.

## v0.7.0

2026-08-01 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.7.0)

One break, and it is the one this library most needed to make before anyone
depended on it: **there is no default hook registry**
([ADR-0047](architecture.md#no-default-hook-registry)).

Hooks are the rules that confine what a query may see, so they were also the
one surface where ambient state could decide a tenant boundary. `On[T]()`
registered into a package-level default, `New(exec)` handed every handle that
same default, and `OnIn[T](r)` — the form that says where the rules land —
carried the longer name. [compatibility.md](compatibility.md) had listed the
hazard under *Will move* for two releases, in the words it turned out to
deserve: which registry a statement uses is decided by the dynamic type of the
executor passed to it.

What made it a release rather than a note is that it cost an adopter a tenant
boundary. Moving an application onto a per-application registry left one module
still calling `On[T]()`, so that module's rules were no longer on the handle it
queried through — and it still compiled, still mounted, and still answered,
with every tenant's rows in the response. Both spellings were valid, and the
wrong one was shorter.

So the default is gone. `sqlb.New` gives each handle an empty registry of its
own, `On[T](r)` is the only registration form, `rest.PublishChanges[T](r, p)`
takes the registry too, and an `Executor` that is not a `*sqlb.DB` resolves to
a registry nothing can register into — a statement against a bare pool is
unconfined, and says so.

**The mechanical edit**, and every one of them is a compile error rather than a
behaviour change, which is the point: the failure this prevents is silent, so
its migration must not be.

```
On[T]()               →  On[T](reg)
OnIn[T](reg)          →  On[T](reg)
PublishChangesIn[T]   →  PublishChanges[T]
sqlb.New(pool)        →  sqlb.New(pool).WithHooks(reg)
```

There is no shim, because a shim is the ambient registry under a new name.

What goes with the default is its bookkeeping. `Hooks.Reset` survives with its
reason rewritten — a test gets isolation from `NewRegistry`, which cannot be
forgotten in a teardown — and the `sync.Once` guards, `t.Cleanup(Reset)` pairs
and "has anything registered yet?" checks that existed to manage a registry
nobody asked for are deleted. The examples had all left already, which is the
tell: `example/tasks` built its own registry and said why, and the fx kit never
used the default at all.

**What this does not fix**, named in the record rather than discovered later: a
registry nothing attaches. `On[T](reg)` compiles whether or not any handle
carries `reg`, so hooks can still be registered where nothing runs them. That
is strictly narrower — the registry and the handle are usually adjacent
expressions rather than action at a distance — and the case that matters is
still caught at the mount, because a model declaring `Scoped` is refused when
the handle's registry has no hook for it
([ADR-0030](architecture.md#declared-scope-is-required)).

## v0.6.0

2026-08-01 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.6.0)

The release an issue tracker wrote. Twenty issues were filed against the request
path on 31 July from an external review and an adoption port; this closes the
last of them, plus six more raised the day after by an adoption that got far
enough to find sharper ones. The theme is not a feature — it is that most of
what a real consumer hit was a default that was right for the schema and wrong
for the reader.

Three breaks, and the first is the one to read.

**A computed column is opt-in.** It is declared on the model, and the model is
shared, so projecting every declared one charged every reader for the most
expensive one — three correlated subqueries attached to an existence check by
id — and a column declaring `Needs` made that check *fail*, demanding a `viewer`
bind from a query with no business supplying one. Nothing projects a computed
column now unless it asks:

```go
sqlb.Query[Project]().WithComputed("total_tasks", "is_starred")
rest.Options{Computed: []string{"total_tasks", "is_starred"}}
```

The mechanical edit is `WithComputed` on a hand-written query and `Computed` on
a hand-written mount. A *generated* resource opts into its table's own computed
columns, so generated endpoints answer exactly as they did; what changes is
everything else reading the same model, which is where the bug was. For a
resource it is a boundary rather than a projection setting — a column the
resource does not select is not filterable, sortable or nameable in `?select`
there either, because a filter on a correlated subquery costs what the
projection would have. The obligation moved with it: `rest.Resource` used to
refuse any mount whose *model* declared a `Needs` column, and now asks only of
the resources that render one.

**The generated Go client is its own package.** A program that wanted the typed
client took [spf13/cobra](https://github.com/spf13/cobra) and a whole command
tree with it, so a sync job made one HTTP request at the cost of a command-line
framework. `cli/client/client_gen.go` now carries `Client`, `Request`,
`Transport`, `Do` and `Run` against the standard library and nothing else, and
`cli/cli_gen.go` is the cobra tree importing it. Regenerate, then the edit is in
the four-line main: `&cli.Client{…}` becomes `&client.Client{…}` from the new
package. `ClientDir` emits the client with no command tree at all, which is the
server-to-server case; `CLIDir` emits both and defaults the client into a
`client/` subdirectory, so a project that set only `CLIDir` keeps working.

**A nil member of `OneOf` widens the set.** `IN (NULL)` is never true, so a set
assembled from nullable values came back quietly narrower than the caller wrote;
the nil member now contributes `IS NULL` instead. A set with no nil in it
renders byte-identical, and generated endpoints never reach it — this is
hand-written Go. `NotOneOf` is deliberately unchanged, and now says why on
itself.

One thing that is not a source break and will still stop a build: every
generated struct tag gained the column's logical type, so `sqlb generate` has to
run before `sqlb check` passes. That tag is what fixed the expansion bug below.

Worth stating rather than leaving to be noticed: none of the three was listed
under *Will move*. [compatibility.md](compatibility.md) says a minor bump may
break a surface listed there and that each break is described here with its
mechanical edit — half of that promise is kept above and half is not, because
all three came out of consumer reports rather than a plan. The document now
records them where the announcement should have been, which is the correction
available after the fact.

What landed, beyond the breaks:

- **A change feed, as a transport.** `rest.Events` mounts an SSE stream through
  huma's sse package, so it lands in the OpenAPI document typed rather than as
  untyped text; `rest.Broker` is the in-process source behind a `rest.Source`
  seam the outbox implements later. A subscriber receives `{table, key, op}` and
  refetches, because a payload built outside the subscriber's context would skip
  the resource's `BeforeQuery` scope and hand one tenant's rows to another.
  Correct on one replica and quietly wrong on two, which is the first thing its
  doc comment says. [ADR-0045](architecture.md#the-stream-is-a-seam).
- **The filter tree gained `not`, and containment gained its negation.** `nhas`,
  `nhasany`, `nhasall` and `nhasdoc` exist because the URL grammar conjoins by
  design and has nowhere to put a `not` — shipping only the tree would have left
  the two frontends compiling different vocabularies, which is the one thing
  [ADR-0003](architecture.md#one-ast-two-producers) claims they do not. A negation
  is not a complement: each compiles to `NOT (…)`, so a NULL column matches
  neither `has` nor `nhas`, exactly as `nin` already behaved.
- **`ON CONFLICT DO UPDATE` assigns an expression.** An upsert could only copy
  the proposed row, so `updated_at = now()` had to come from the application
  clock and a counter could not be written at all. `OnConflictSet` takes any
  expression, and a column reference inside one has to say which row it means —
  `Excluded` or `Current` — because `count = count + 1` reads like an
  accumulation whichever side SQL silently picks.
- **Where NULLs sort is declared on the column.** Postgres's default is not one
  placement but two, `NULLS LAST` ascending and `NULLS FIRST` descending, so a
  feed ordered by a column that is NULL until a row is published lifted every
  draft to the top. `Sortable(schema.NullsLast)` fixes it once, in both
  directions, for every caller including the generated clients — which need no
  new syntax for it. The cursor carries the declared placement, so cursors
  issued before this release still decode and one issued under a since-changed
  declaration is refused rather than mispaged.
- **An expanded row is the same shape as a direct one.** Expanding a relation
  whose target had a `date` column answered 500: `json_build_object` serialises
  a date as `"2026-07-01"` and the Go field is a `time.Time`, which parses
  strictly as RFC 3339. Cast to UTC midnight now — `::timestamp AT TIME ZONE
  'UTC'`, not `::timestamptz`, which resolves through the session zone and loses
  a day east of UTC.
- **`?search` can reach past the row.** A `Searchable` computed column of text
  type is now legal, so a chat named in the UI by its participants — a direct
  message has no name of its own — is findable by a participant's name. The
  refusal that blocked it gave a reason about type that the "Searchable requires
  a text column" rule already made; what it actually cost was the only way to
  search across a relation. The cost objection is answered by the opt-in above.
- **An adoption's declarations.** `numeric(p, s)`, index column ordering, a
  foreign-key cycle broken with an `ExternalRef` instead of dropped, and a
  self-referential FK that no longer reads as permanent drift — the four things
  that made a drift gate against a live database un-buildable.
- **The request path's bounds.** `?page=`/`?offset=` are capped and no longer
  overflow; a repeated single-valued parameter is refused rather than silently
  dropped; `POST` and `PATCH` reject unknown query parameters like every other
  operation; a multi-row insert decides default-omission per row rather than per
  statement.
- **`sqlbfx`**, an fx module over the same handles, and a principal seam so a
  core-style app takes `Handles()` only.

What it cost. `FromGo` is **cut** rather than pending:
[ADR-0041](architecture.md#computed-fields) wrote the condition — "if the first two
applications express everything in SQL" — and both did, so the record says so
and closes [#17](https://github.com/mind-vm/sqlb/issues/17) with the evidence
rather than leaving a fourth tier in the tracker. The change feed is correct on
one replica and loses a publication if the process dies between `COMMIT` and the
fan-out; both are stated where a reader meets them rather than in a footnote.
And a `time` column has the same expansion defect a `date` column had, unfixed
on purpose: nothing round-trips one, so casting it would have been a guess.

## v0.5.0

2026-07-31 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.5.0)

Three things the adoption review ranked, built: a computed column, a declared
action, and the exit. Between them they answer the two objections that were not
about missing features — that one derived value pushed an entity off the
generated path entirely, and that sqlb owns too much to be reversible.

One break, and it is a rename. `schema.Action` is no longer the foreign-key
referential type; that noun went to the domain verb below, and the type is
`schema.RefAction`. The constants every call site actually writes —
`schema.Cascade`, `schema.SetNull` and the rest — are unchanged, so a schema
breaks only if it named the type, and the mechanical edit is `schema.Action` →
`schema.RefAction` in a foreign-key position. [compatibility.md](compatibility.md)
announced it under *Will move* and now records that it landed.

**A computed column is an expression.**
[ADR-0041](architecture.md#computed-fields), three of its four tiers:

```go
schema.Computed("is_overdue", schema.TypeBool,
    schema.FromSQL("due_date < current_date AND open_tasks > 0")).
    Filterable()
```

One interception point, as the record's trace predicted: every consumer already
resolves through a `*ColumnInfo` and renders through the compiler's column, so
substituting the expression there puts the value in the projection, the `WHERE`
and the `ORDER BY` at once. The parameterised tier takes
[ADR-0030](architecture.md#declared-scope-is-required)'s shape — `Needs("viewer")`
declares the bind, a `BeforeQuery` hook supplies it through `Builder.Bind`, and
`rest.Resource` refuses to mount when nothing does. Without that refusal an
unbound expression renders `member_id = NULL`, returns false for every row
forever, and looks exactly like a feature that works. No DDL in either
direction, so converting a stored column into a computed one proposes the drop.
`FromGo` is **cut**, not pending — ADR-0041 set the condition "if the first two
applications express everything in SQL", and both did. Nothing in the tree
reaches for it and nothing outside it did either.

**A declared action generates the envelope, and the verb stays plain Go.**
[ADR-0043](architecture.md#declared-actions), against the 26 item verbs and ~20
collection verbs the evaluated application had, and the ~30 lines of identical
envelope written four times over before any domain logic:

```go
Task.Action(schema.Action{
    Name:   "complete",
    Body:   schema.Body(schema.Text("note").Nullable()),
    Writes: []string{"status", "completed_at"},
})
```

serves `POST /tasks/{id}/complete` and asks `Register` for one func. The id, the
scoped fetch, the 404, the body, the transaction, the row lock, the write set
and the response are generated; the transition is not. `Writes` is enforced
rather than documented — exactly those columns, off the row the verb mutated —
and it is what makes the fetch take `FOR UPDATE`, since every one of these is a
read-modify-write across a round trip. The verb reaches the TypeScript, Dart and
CLI emitters, `sqlb.json` and the `sqlb impact` diff, where removing one is
breaking and adding one is additive. There is no `Method` field: every legal
value was `POST`. And the hole is named rather than papered over — a collection
action fetches nothing, so it obliges no hook, and that is two in five of the
measured verbs.

**`sqlb eject` writes [the way out](eject.md).**
[ADR-0042](architecture.md#the-exit-is-generated), and the answer to the objection a
pre-1.0 library with no consumers cannot answer with a promise: sqlc and chi are
cheap to reverse because they own almost nothing, while sqlb owns the schema,
the migrations, the wire format, the client and the CLI. `sqlb eject ./schema`
generates a package that imports pgx and the standard library and nothing else —
the DDL, the row structs without their `sqlb` tags, one function per statement
with the SQL written out, `net/http` handlers, and a README saying what came out
and what did not. Deleting sqlb from `go.mod` afterwards is a supported end
state.

The fidelity line is between the surface and the engine. Out whole: CRUD and
list at the same paths with the same status codes and the same envelope, every
filter operator that is one SQL fragment, `?sort`, `?search`, `?page`,
`?per_page`, `?count=exact`, the declared ceilings and the RFC 9457 error shape.
Not out, and refused with a 400 that says so rather than ignored: keyset
cursors, `?select`, `?expand`, the JSON filter tree, and the array and document
operators — reproducing those would mean emitting a copy of sqlb, which is a
fork with a different import path rather than an exit. Two properties survive
the loss of the machinery they were implemented in: capabilities stay opt-in, so
a column that never declared `Filterable` is not filterable in the exit and a
`Hidden` one has no spelling at all, and
[ADR-0030](architecture.md#declared-scope-is-required)'s obligation stays compulsory.
The load-bearing half is `pgtest/eject_test.go`, which stands the committed exit
beside the generated resources it came from, points both at one database, sends
both the same requests and compares the bodies byte for byte.

**Adopting an existing database** is where the rest of the work went. Each of
these made a schema-vs-database gate propose migrations nobody asked for, which
is the failure that teaches people to stop reading the gate:

- `IndexNamed` and `UniqueIndexNamed` declare an index under the name the
  database already gave it. The name is not inert — Postgres reports a violated
  constraint by name, so renaming a unique index turns a handled collision into
  an unhandled 500 without touching the code that handled it. The generated
  migration says so now.
- `ExternalRef(...).Enforced()` emits a real `FOREIGN KEY` against a table this
  schema has not declared, which is the thing an incremental adoption always has
  to say and had no spelling for. What it gives up is what
  [ADR-0015](architecture.md#module-isolation) bought by refusing the constraint: two
  modules joined this way can no longer be migrated independently, so it is
  opt-in and unenforced stays the default. `introspect` imports foreign keys this
  way, which is what stops a gate proposing `DROP CONSTRAINT` forever.
- A `jsonb` default is compared as a document, so `'{"a":1,"b":2}'::jsonb` and
  `'{"b": 2, "a": 1}'` are one default — which is what Postgres thinks too, since
  `jsonb` stores a parsed value rather than the text it arrived as. Only for
  `jsonb`; on a text column those are two strings, and the test says so.

**The round trip is a fixpoint**, asserted rather than assumed. `introspect`,
`RenderSchema` and `Diff` were each well tested and nothing checked that they
agreed with one another about one schema, which is why none of their own tests
could see the three disagreements that fell out. `RenderSchema` could not write a
vector column at all, so a 69-table database could not be turned into 69
declarations to review on account of one column; an index lost its operator class
and storage parameters, which for pgvector Postgres rejects outright, since the
class selects the distance function and there is no default; and an enum's
`CHECK` lost its name, so every later diff proposed dropping and re-adding it.
The gate applies an awkward schema, reads it, renders it back to source that must
compile, rebuilds a second database from what was read, and compares the two
through `pg_catalog` — databases rather than registries, because two registries
agree about everything they both dropped.

**A family of codegen import bugs**, in both directions and all with one cause:
`format.Source` parses without type-checking, so an import that is named but
missing, or present but unused, is valid Go source that fails only at the
consumer's compiler. Three were `jsonb`-shaped and the rest were found by
auditing the whole set — a read-only resource importing `time`, a table whose
patchable columns are all nullable importing `errors`, a schema whose only uuid
column is a primary key importing `google/uuid`, a hidden timestamp named by the
typed update with nothing importing `time`, and a nullable vector matched against
a hand-maintained list of type spellings that was one short. Beside them, a
nullable `jsonb` create body assigning a pointer into a non-pointer field. The
general guard is `TestGeneratedGoCompiles`: eight schema shapes generated into a
scratch package and handed to one `go build`, so the compiler decides rather than
a substring assertion naming the mistake in advance.

## v0.4.0

2026-07-30 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.4.0)

The release [ADR-0040](architecture.md#the-driver-is-a-dependency) was announced for.
`v0.3.0` said the driver question had been decided and that nothing of it was
built; this is it built. sqlb depends on pgx v5, `database/sql` is not the
contract, and `Executor` — Frozen in [compatibility.md](compatibility.md) — broke
on purpose, before the tag that would have made the same work a major version and
a hand migration for everybody.

The mechanical edit is at the seam: pass a `*pgxpool.Pool` where a `*sql.DB` used
to go. `*pgx.Conn` and `pgx.Tx` satisfy `Executor` as they stand, and the last of
those is what this was for — sqlb writes now join a transaction the application
opened itself, which two handles over one pool could never do. `sqlb.New(tx)`
knows it is inside one and deliberately does not take the boundary over, so
`AfterCommit` refuses there rather than queueing callbacks behind a commit sqlb
will never perform. Something that still wants a `*sql.DB` — goose, sqlc — gets
one from `stdlib.OpenDBFromPool` over the same pool, and the examples are written
that way because that is the shape a real adopter lands on.

Two surfaces disappear with it. `sqlb.EncodeArray` is gone with nothing in its
place, and the 449-line array-literal codec behind it: a `[]string` binds as
`text[]` and scans back from one because pgx does that. `SetErrorClassifier`
stays, but the case it was written for is now the default — `ConstraintError`
carries the constraint name, table, column and detail read off `*pgconn.PgError`,
with nothing registered.

The second break is smaller and shows up on the next regeneration. A nullable
`jsonb` column's model field is `*json.RawMessage`: it was the one column whose
generated type did not say it could be NULL. It was also unreadable through
`database/sql`, which is how it was found in a real port — that half no longer
reproduces, because taking pgx replaced the executor that had the gap.
Regenerate, and the compiler names the call sites.

Additive, and new:

- A `jsonb` column is filterable. `?metadata=hasdoc.{"lang":"de"}` compiles to
  `@>`, subset containment rather than equality, so a document carrying more keys
  than the filter named still matches. Not spelled `contains`, for the third time
  and for the same reason: that name is the text substring operator, and one name
  dispatched on column type is the ambiguity the generated clients exist to
  remove. A document column takes `hasdoc`, `isnull` and `notnull` and nothing
  else — there is no bare-value shorthand, and the ordering and pattern operators
  would answer rather than refuse, which is worse.
- A vector column. `schema.Vector("embedding", dim)` stores a pgvector embedding,
  `sqlb.Near` yields the score, the ordering and an `AtLeast` threshold from one
  call rather than three that must agree, and `RegisterVectorType` puts the binary
  codec on the connection — a pgx API with no `database/sql` spelling, and one of
  ADR-0040's arguments. The column is `Hidden` and not optionally so. There is no
  index kind and no REST search operation: a similarity search is an exact scan
  over the rows a filter already selected.
  [ADR-0026](architecture.md#vectors-declare-their-index) stages the index as a second
  decision and stays *Exploring*.

Fixed, most of them found by adopting sqlb over something that already existed. A
`VARCHAR(n)` default round-tripped as an expression, so `Diff` proposed the same
`ALTER` on every run and the drift gate stayed red for a reason that was not real.
A schema package under `internal/` could not be read by the generator.
`attgenerated` was misread after the flip, so every column of every imported
database looked generated. A rejected write arrived as "none of the result columns
map to T". And `sqlb generate`'s scratch directory survived an interrupted
compile, into somebody's `git add -A`.

The rest is evidence rather than surface. ADR-0026's physical claims about
pgvector are measured now instead of read out of documentation, and a fourth was
added: the planner may decline the ANN index, which makes the silent under-return
conditional on statistics nobody watches.
[ADR-0041](architecture.md#computed-fields) decides computed fields, including the
per-viewer tier a static SQL string cannot express, and builds none of it.
`example/recipes` is 86 Go example functions, one point each, whose printed output
is compared on every `go test` — so a recipe describing an API that changed fails
the build instead of misleading the next reader.

See [the driver](compatibility.md#the-driver), which says what the break bought
and what it cost, in both directions.

## v0.3.0

2026-07-30 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.3.0)

No API change. What this release carries is a decision, the seam that makes it
buildable, and the test coverage that was holding it up.

[ADR-0040](architecture.md#the-driver-is-a-dependency) decides that the engine will
depend on pgx and that `database/sql` stops being the contract — a break to
`Executor` that lands before 1.0 or not at all. Nothing of it is built yet. Read
[the driver](compatibility.md#the-driver) before pinning: the interface every
terminal call takes is going to change, and this is the release that says so in
advance rather than the one that does it.

The enabling refactor is here: the scanners read an internal `rowSource`
interface instead of `*sql.Rows`, which is correct under either answer and turns
the eventual migration into an adapter rather than a rewrite of scan and mutate.

`pgtype` values — `pgtype.Date`, `pgtype.Timestamptz`, `pgtype.UUID` — are now
covered in both directions including NULLs, with compile-time assertions that
fail the build if a pgx release ever drops `sql.Scanner` or `driver.Valuer`. That
path is load-bearing for adopting sqlb over existing sqlc structs and was
previously tested only with `sql.NullTime`.

The `go` directive drops from 1.25.7 to 1.25.0. It was patch-pinned by `go mod
init` rather than by any requirement, and pinning it forced every consumer onto
that exact toolchain.

Also: a documentation pass that closed the open ends across the ADRs and the six
review reports, and a nested `rest` module that was built and reverted within the
day — huma stays the default HTTP path, in the same module.
[ADR-0007](architecture.md#generated-rest-handlers) records why.

## v0.2.0

2026-07-30 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.2.0)

The first release with a transaction handle, and the first that a consuming
application can depend on without a local `replace`.

`v0.1.0` predates `db.go` entirely: `sqlb.DB` and `sqlb.New` — the handle every
data layer takes — landed after it, along with array columns, codegen type
overrides, a JSON filter tree, schema-impact diffing, and a fix for an expansion
carrying its target's scope onto the join.

Cut for the studio-apps port, which could not compile against `v0.1.0`.

## v0.1.0

2026-07-27 · [tag](https://github.com/mind-vm/sqlb/releases/tag/v0.1.0)

The first tagged release.

Pre-1.0. Semantic versioning applies from `v1.0.0`; until then a minor bump may
break a surface listed as moving, and the release notes carry the mechanical edit
that fixes it.

Frozen, because other systems couple to them:

- `Executor` — `QueryContext` and `ExecContext`, nothing more
- The filter grammar — the URL syntax and its operator names
- Already-applied migrations are never reinterpreted

Expected to move, named in advance:

- Hook registration, when the transaction-scoped handle scopes the registry that
  `sqlb.On[T]()` reaches today
- Terminal call signatures, when Go 1.27 allows methods to declare type
  parameters — additive, the functions stay
- `?expand`, which is refused at startup until the joins land

See [compatibility.md](compatibility.md).
