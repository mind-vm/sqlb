# subject-mono → sqlb port — pilot & feedback  (2026-07-30)

> **Status: all six findings are triaged**, in
> [release-1.0.md stream G](release-1.0.md#g-port-findings-triaged--two-land-four-are-written-down).
> Finding 1 is the driver, and it is decided —
> [ADR-0040](architecture.md#the-driver-is-a-dependency) takes option (b) of this
> report's *Recommended next steps*: the platform moves, rather than sqlb growing
> a second path. Finding 2 lands before 1.0. Finding 3 is answered by
> [ADR-0034](architecture.md#one-column-addresses-a-row), which this report's evidence
> moved off the 1.0 blocker list. Finding 4 is answered by
> [ADR-0007](architecture.md#generated-rest-handlers); 6 is scheduled for 1.1; 5 is
> the platform's.

**The subject is anonymised.** It is the same `subject-mono` as
[the multi-app evaluation](review-adoption-multi-app.md) — the same `core/`
platform layer, `dbbase`, fx wiring, pgx-native pool and testcontainers — here
*ported* rather than assessed. The product apps it touches are referred to
generically as "the consuming apps"; their specific identities are not
load-bearing and are elided (and deliberately **not** mapped onto that
evaluation's `app-a`…`app-i` labels, since the mapping isn't established here).
Internal platform paths (`core/waitlist`, `core/secrets`, `core/dbbase`, …) are
kept as written, exactly as that evaluation keeps `core/rag` and `dbbase`. The
working branch is given without its SHA. Nothing else technical was removed:
every count, every layer and every finding is as written.

Read it as a snapshot of one port, not as a verdict. Where a finding names a file
or a behaviour, it was checked against the code or against a running Postgres
(testcontainers); where it is a judgement call, it says so.

Branch: `claude/subject-mono-fx-sqlb-port` (SHA elided)
sqlb: `github.com/mind-vm/sqlb` (local `replace` → a working checkout, currently at `v0.1.0`+)

**Status: PAUSED for review.** 3 of ~10 persistent `core` modules ported
(`waitlist`, `secrets`, `llmcatalog`), all green. Nothing committed yet; the
local `replace` must come out before this branch can land (finding 5). The open
decision is scope — see [Recommended next steps](#recommended-next-steps).

## Findings at a glance

| # | Finding | Severity | Who fixes it | Blocks the port? |
|---|---|---|---|---|
| 1 | `database/sql` vs pgx-native pool → second handle, no shared tx | Architectural | platform (scope decision) | Only non-leaf / pgvector modules |
| 2 | `ON CONFLICT DO UPDATE` can't assign expressions, only `EXCLUDED.<col>` | High | **sqlb** | No (Go-side workaround) |
| 3 | No composite `PrimaryKey` (single-column only) | Medium | **sqlb** | No (explicit conflict target) |
| 4 | Stdlib-only import surface still MVS-bumps huma/chi | Low | **sqlb** (deps-free submodule) | No |
| 5 | Local `replace` is machine-specific, added per-app | Low (landing blocker) | platform (pin `@v0.1.0`) | Yes, for merge |
| 6 | `Describe` panics after first use — init-ordering hazard | Low | **sqlb** (doc/idempotency) | No |

## What was done

Ported three disjoint/leaf `core` modules end-to-end, removing sqlc from each:

| Module | Shape exercised | Result |
|---|---|---|
| **`core/waitlist`** | insert w/ defaults, JSONB, conditional-`Where` list, guarded update, count | 9/9 tests green |
| **`core/secrets`** | **upsert** (ON CONFLICT), BYTEA columns, tenant-scoped reads, **metadata-only projection**, delete | 10/10 tests green |
| **`core/llmcatalog`** | **upsert on a composite key**, multi-column order, JSONB, count | 2/2 tests green |

Shared plumbing (added once):

| Change | File |
|---|---|
| `*sqlb.DB` provider (own `database/sql` handle over the DSN, pgx stdlib driver) | `core/dbbase/sqlb.go` |
| Wired into the fx graph | `core/dbbase/module.go` (`fx.Provide(NewSQLB)`) |
| Per module: sqlb model (struct tags carry `pk`/`default`) + service rewrite; `{db,queries,sqlc.yaml}` removed | `core/{waitlist,secrets,llmcatalog}/model.go`, `service.go` |
| Dep (require + local replace) added to core and every consuming app | `core/go.mod`, `apps/<consuming apps>/app/go.mod` |

### Verification (all green)

- `go test ./core/{waitlist,secrets,llmcatalog}/...` — 21/21 pass against real Postgres (testcontainers).
- `go build ./...` in `core` — OK.
- All four consuming apps `go build` **and** `go vet ./...` (compiles their `*_test.go`) — OK.
- Platform guards: `mise run module-check` + `layers-check` OK. `sqlc-check` naturally skips the ported modules.

### What worked cleanly (no friction)

- **JSONB and BYTEA both map to `[]byte`** and round-trip through pgx's
  `database/sql` driver with no codec setup — jsonb (`waitlist.metadata`,
  `llmcatalog.{capabilities,metadata,regions}`) and bytea (`secrets` envelope
  columns) alike. sqlb correctly does **not** treat `[]byte` as a Postgres array.
- **Column-subset projection** — `secrets.List` selects 6 of 10 columns to keep
  the encrypted blobs in Postgres; sqlb's partial scan fills the matching struct
  fields and leaves the rest zero, exactly as wanted.
- **Default-column omission** — `sqlb:"default"` on `id`/`created_at`/etc. made
  `InsertRows` omit zero values so Postgres filled `gen_random_uuid()`/`NOW()`.
- **Error mapping** (`pgconn.PgError` unique-violation) and **`ErrNotFound` /
  multi-row semantics** on `One`/`Update.One` mapped straight onto existing
  sentinel-error logic.
- **Conditional / multi-column ordering** (`llmcatalog.List`) and the
  conditional `Where` (`waitlist.List`) read better than the sqlc `sqlc.arg()=''`
  workarounds they replaced.

## Issues with sqlb (ranked)

### 1. `database/sql` requirement vs. the platform's pgx-native pool — architectural

The platform standard is a native `*pgxpool.Pool`; sqlc is generated for pgx/v5.
sqlb's `Executor` is `database/sql`, which a pgxpool does **not** satisfy, so a
ported module needs a **second connection handle** — I open a `sql.DB` over the
same DSN via the pgx stdlib driver (`core/dbbase/sqlb.go`).

Consequences (all matching sqlb's own [`with-sqlc.md`](with-sqlc.md) "disjoint tables" note):
- Two pools per process.
- **No shared transaction** between a sqlb module and a pgx/sqlc module. Fine for
  these three self-contained modules; blocks any module whose writes must be
  atomic with a pgx-native module's in one unit of work.
- pgvector's `AfterConnect` codec (registered on the pgxpool) is **not** on the
  sqlb `sql.DB`, so `rag`/`memory` can't port this way without extra
  stdlib-driver type registration.

This determines how far the port scales: leaf/disjoint modules are cheap; a
module sharing a transaction with pgx-native code needs the whole platform on
`database/sql` first.

### 2. `ON CONFLICT DO UPDATE` can only assign `EXCLUDED.<col>` — no expressions

`OnConflictUpdate(target, cols...)` renders every update column as
`col = EXCLUDED.col`. There is no way to express `SET updated_at = NOW()` or any
literal/expression in the conflict clause. **Both** upsert ports hit this:
`secrets` (`updated_at = NOW()`) and `llmcatalog` (`synced_at = now()`).

Workaround used: compute the timestamp in Go, put it on the proposed row, and
list the column in `OnConflictUpdate` so it lands as `EXCLUDED.updated_at`. It
works (tests green), but it has two real costs:
- the timestamp's source moves from the **DB clock to the app clock**, and
- the column is **forced into the INSERT column list** (you can't leave it to its
  DB default on the insert path anymore).

This is a very common upsert shape (`updated_at = now()`, `count = count + 1`,
`SET x = COALESCE(EXCLUDED.x, table.x)`). A first-class API — e.g.
`OnConflictSet("updated_at", sqlb.Raw{SQL: "now()"})` or an expression variant of
the update clause — would remove the workaround. (`sqlb.Raw` is today a struct
literal, not a `Raw(...)` constructor; the function-form raw primitives are
`RawPred`/`RawSel`.) **Highest-value sqlb change surfaced by this port.**

### 3. No composite primary key

`Describe.PrimaryKey` / `sqlb:"pk"` take a single column. `llmcatalog`'s PK is
`(provider, model_id)`. It didn't block me — the upsert names the composite
conflict target explicitly (`OnConflictUpdate([]string{"provider","model_id"}, …)`,
which *does* accept multiple columns) — but the table can't declare its real key,
so any PK-derived affordance (REST single-row addressing, a future update-by-PK
helper) is unavailable for composite-key tables. Worth a doc note now, and a
`PrimaryKey(cols...)` later.

### 4. Stdlib-only *import surface* still bumps huma/chi at the *module* level

`go list -deps github.com/mind-vm/sqlb` → zero non-stdlib packages ✓. But
`go mod tidy` selects versions per **module**, and sqlb's `go.mod` requires
`huma/v2 v2.39.0` + `chi/v5 v5.3.1` (its `rest` subpackage, never imported), so
adding it bumped, in core and every consuming app:

| dep | before | after |
|---|---|---|
| danielgtaylor/huma/v2 | v2.38.0 | v2.39.0 |
| go-chi/chi/v5 | v5.3.0 | v5.3.1 |
| golang.org/x/crypto | v0.51.0 | v0.52.0 |
| golang.org/x/net | v0.53.0 | v0.55.0 |
| golang.org/x/sys | v0.44.0 | v0.45.0 |

Harmless here (all still build), but the "inherits nothing" claim is easy to
over-read: importing only the engine still drags the REST adapter's requirements
into every consumer's version selection. A deps-free `sqlb/engine` submodule
would make the claim true where it's felt. Doc caveat at minimum.

### 5. Distribution: local `replace` is machine-specific, added per-app

The port uses `replace …/sqlb => <working checkout>` in **every** consuming
module's `go.mod` (core + 4 apps so far) — there's no root `go.work`, so it's not
a one-liner. Can't be committed to CI. Landing = drop the replace and
`go get github.com/mind-vm/sqlb@v0.1.0` (repo is public + tagged).

### 6. `Describe` panics after first use — init-ordering hazard for the pure adoption path

Sidestepped here by owning the struct and using `sqlb:` tags (no `Describe`
call). But the advertised *sqlc-adoption* path (`Describe` over untaggable
generated structs) must call `Describe` **before the first statement is built
against the type** — once the model is in use a further `Describe` panics
(`InUse`) — and nothing enforces the ordering. A "describe in `init()`" doc note
and/or an idempotent re-describe would de-risk it.

## Recommended next steps

1. **Decide scope.** These three prove the disjoint-leaf path cheaply. Choose
   between (a) continue porting only self-contained leaf modules over the second
   `sql.DB`, or (b) commit to moving the platform onto `database/sql` (large;
   unblocks shared transactions and pgvector modules).
2. **Next leaf candidates** (self-contained, no cross-module tx): `tenants`,
   `superadmin`. Both are bigger but same shape.
3. **Before merge:** drop the local `replace`, pin `@v0.1.0`+.
4. **Upstream sqlb (by leverage):** (2) expression assignments in
   `ON CONFLICT DO UPDATE`; (3) composite `PrimaryKey`; (4) deps-free `engine`
   submodule; (6) `Describe` init-ordering note.

---

*Companion reports: the multi-app **evaluation** of this same subject is
[review-adoption-multi-app.md](review-adoption-multi-app.md); a hands-on port of a
different subject (a single existing app) is
[review-adoption-port.md](review-adoption-port.md).*
