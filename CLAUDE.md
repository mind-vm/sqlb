# Working in this repository

sqlb is a composable SQL builder and schema DSL for Postgres. A schema is
ordinary Go, and everything else — migrations, models, REST handlers, the
OpenAPI document, four clients — is derived from it.

This file is the map and the traps. It does not restate the docs; it says where
they are and what is not inferable from reading code.

## Orientation

Read in this order, and stop as soon as you have what you need:

1. **The package's own `doc.go`.** Every library package has one and it is the
   real introduction — what the package is for, the argument behind its shape,
   and a worked example. `schema/doc.go` and `rest/doc.go` are the two that
   most repay reading before touching anything. A command documents itself the
   Go way instead, as `// Command sqlb …` at the head of `cmd/sqlb/main.go`.
2. **[docs/architecture.md](docs/architecture.md)** for how the pieces fit and
   why the seams are where they are — including its
   **[Decisions](docs/architecture.md#decisions)** section, 66 of them, and
   they are *load-bearing rather than historical*. A decision here is usually
   the answer to "why is this not simpler", and reversing one without reading
   it is the most common way to spend an afternoon rediscovering a rejected
   alternative. Most end with a revisit trigger.

   These used to be a separate `docs/adr/` directory of 58 individually
   numbered files, each editable in place with its own revisions log. They
   were folded into architecture.md one at a time, each as its own commit, so
   that *changing* a decision's reasoning is a commit with a message that
   argues for the change — the way every other commit in this repo already
   has to — rather than a hand-maintained log entry nothing else reads. To see
   why a decision reads the way it does, `git log -G'### <heading>' --
   docs/architecture.md` finds every commit that touched it.

   Cite a decision by its heading, not by an ADR number. The headings carry
   no numbers, so a number is a label with no registry to allocate it from —
   two branches once both reached for 0059. The numbers still in older
   citations are held consistent by `adr-check` for as long as they last.

Everything under `docs/` is prose for humans, readable straight from a
checkout or on GitHub. It is also the source of the published site at
https://mind-vm.github.io/sqlb — `site/scripts/sync-docs.mjs` derives the
Starlight pages from it on every build and commits nothing, so `docs/` stays
the one copy to edit. `mise run site-check` says whether it can be published
as it stands. The API reference is the Go doc comments.

## Layout

Fifteen Go modules. The seven worth knowing before you start are below, and
`go test ./...` at the root covers **only the first** of them:

| | |
|---|---|
| `.` | the engine — builder, compiler, hooks, model cache. 23 files at the root, which is the package |
| `pgtest/` | round-trip tests against a real Postgres. Its own module so the engine's suite stays database-free. It takes a DSN and starts nothing — `mise run pg-up` provides one, and `sqlbtest.Fresh` makes a database per test on it |
| `example/tasks/`, `example/fxapp/` | worked applications, each with its own gate |
| `example/auth-workos/` | a `sqlb.Verifier[T]` adapter for WorkOS AuthKit — its own module so the WorkOS SDK and JWT/JWKS dependencies never reach sqlb core's `go.mod` |
| `example/auth-supabase/` | the same seam for Supabase Auth, and the authentication half of [docs/supabase.md](docs/supabase.md). Separate module for the same reason. It refuses a project's `anon` and `service_role` keys, which are JWTs the project itself signed |
| `example/attachments/` | presigned direct-to-S3 uploads: the row-before-bytes ordering, and a stdlib SigV4 presigner cross-checked against `aws-sdk-go-v2`. Its own module for the same reason, though it ended up needing no dependency at all |

Packages: `schema` (the DSL), `codegen` (emitters), `rest` (Huma mount),
`filter` (URL → predicate), `migrate` (diff → DDL), `introspect` (database →
declaration), `shadow` (replay a history), `restcompat` (contract diffing),
`sqlbtest`, `cmd/sqlb`.

## The gate

```bash
mise run heal   # everything the tooling can fix on its own
mise run ci     # the gate: never rewrites, only fails
```

There is no server-side CI for Go — `mise run preflight` and `mise run ci`
are the gate, full stop, run by hand before pushing and before tagging a
release. The one workflow that does run is `site`, which builds the docs and
deploys them to Pages; it is deliberately not a Go gate, so a red site build
never claims the library is broken.
`mise run preflight` is the push path: heal, build, database-free tests, about
fifteen seconds. `mise run ci` is the full fourteen-stage set — everything
`preflight` skips, plus the database-backed and multi-toolchain suites — and
is what to run before a tag, since nothing runs it for you any more. The
database-backed suites read a DSN and start nothing; `mise run pg-up` provides
it from `compose.yaml`, and the tasks that need it depend on that. Individual
steps — `vet`, `lint`, `generate-check`, `impact-check`, `eject-check`,
`tagline-check`, `column-check`, `lint-check`, `adr-check`, `map-check`,
`test-race`, `test-pg`, `test-ts`, `test-dart`, `dart-sdk-check`, `test-cli` — run on their own and
`mise tasks` describes all 41.

## Traps

Two things that are not visible from where you would look for them.

**Docs mirror source by hand.** `docs/typescript/README.md` and
`docs/dart/README.md` restate what `codegen.Options` says about the files each
emitter writes. Change an emitter's output and three places need the edit;
nothing links them yet. The column vocabulary used to have the same problem and
no longer does: the guide names the constructors in prose and points at the
reference page for the table, and `column-check` fails if that page and
`schema/field.go` disagree about what exists.

**`docs/howto/` is an index, not a section.** One page, and every recipe it
names lives in the section that owns its subject. A new how-to belongs in that
section with a row added here — not as a second page under `howto/`.

## Conventions

**Commit messages argue.** The subject is a claim in the repo's voice —
`fix(model): a description publishes a copy, so a model handed out is never
written` — and the body says what was wrong, what was decided, and what was
deliberately not done. `git log` is the design record now that architecture.md's
Decisions section replaced a directory of individually revised files, so a
body that only restates the diff loses the reasoning nothing else captures.

**A guard is not trusted until it has failed on purpose**
([docs/architecture.md, "Guards proven both ways"](docs/architecture.md#guards-proven-both-ways)).
When you add a check, break the thing it checks and record that it caught it.
The v0.8.0 exclusion work is the model: with the import silently dropping
exclusions, one test failed and named the constraint while the fixpoint test
*passed*, because both registries had dropped the same thing.

**Tooling operates on tracked files only**
([docs/architecture.md, "Tooling scoped to tracked files"](docs/architecture.md#tooling-scoped-to-tracked-files)).

**Prefer a failing check to a written-down rule.** `generate-check`,
`eject-check`, `impact-check`, `deps-check`, `column-check`, `lint-check`,
`adr-check`, `map-check` and `bisect-check` all exist
because a convention that is only documented is a convention that drifts. If
you are about to add a paragraph telling someone to remember something,
consider whether it can fail as a `mise run ci` step instead. `deps-check` is
a plain case: the engine's dependency list is a decision, not a memory, and it
is checked by diffing `go.mod` against a pinned allow-list rather than by
asking a reviewer to notice a new import.

**Releases.** A release is an annotated tag whose message *is* the notes, plus
a GitHub release carrying the same text — run `mise run ci` on the commit
being tagged first, since there is no server-side check behind it any more.
[docs/compatibility.md](docs/compatibility.md) says what is frozen and what is
expected to move; a pre-1.0 minor may break a surface listed under *Will
move*, and the notes carry the mechanical edit that fixes it.

## Things that are deliberate

Worth knowing before you propose removing them:

- **Postgres only** ([docs/architecture.md, "Postgres only"](docs/architecture.md#postgres-only)),
  and **pgx is the driver, not `database/sql`**
  ([docs/architecture.md, "The driver is a dependency"](docs/architecture.md#the-driver-is-a-dependency)).
  `Executor` is `Query` and `Exec` over pgx types and grows by optional
  interfaces that are type-asserted for, never by adding methods.
- **The schema DSL is optional.** The engine reflects over struct tags, so
  every feature must be reachable without codegen
  ([docs/architecture.md, "Codegen is optional"](docs/architecture.md#codegen-is-optional)).
- **Nothing on the read path locks.** Describing a model is copy-on-write: a
  published `*Model` is never written again, so the cost sits with the writer.
- **A column has one wire spelling**, derived from its name by the schema's
  `WireCase` ([docs/architecture.md, "The wire is the column name"](docs/architecture.md#the-wire-is-the-column-name)).
  There is no mapping layer and no per-field override, in either direction.
- **Capabilities are opt-in.** Nothing is filterable, sortable or selectable
  unless the column declares it, and a rejection names what would have been
  accepted ([docs/architecture.md, "Actionable errors"](docs/architecture.md#actionable-errors)).
