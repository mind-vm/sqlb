# External review — library, ecosystem, and readiness (2026-07-31)

An outside review of sqlb at `5365f75` (v0.4.0+): four independent passes — a
core-engine code review, a review of the REST/codegen/migrate layers, a
web-researched comparison against the ecosystem as of July 2026, and a
production-readiness audit including a simulated week-1 adoption. Every defect
reported below was verified against the source; the ones marked *reproduced*
were confirmed with throwaway tests before being written down. Fifteen issues
were filed: [#65](https://github.com/mind-vm/sqlb/issues/65)–[#79](https://github.com/mind-vm/sqlb/issues/79).

## Verdict

**The design is real, the discipline is exceptional, and the niche is genuinely
unoccupied in Go — but the library is four days old, and no amount of internal
rigor substitutes for elapsed time under someone else's traffic.** The first
commit is 2026-07-27; HEAD is 2026-07-31; in between sit 136 commits, ~62k
lines of non-example Go, 45 ADRs, 96 test files, four tagged releases, two
simulated adoption ports, and three prior review documents whose findings were
closed the same day they were filed. That velocity is the project's most
striking fact in both directions: the self-correction loop demonstrably works,
and none of the correction has come from a stranger's production workload.

The new defects found by this review cluster exactly where the July 28 review's
findings did not reach: the two newest surfaces (declared actions, eject), the
representation boundaries (JSON float64 vs int64, typed nil vs untyped nil,
qualified vs bare column names), and the writer side of the compat gate. The
mature paths — bind discipline, capability enforcement, cursor correctness on
the documented path, transaction handling, error sanitization — came through
clean, for the second review in a row.

## What holds up

Stated to calibrate the findings, and consistent with the repo's own July 28
review:

- **No injection vector found, again.** Values only ever reach SQL as bind
  parameters; identifiers resolve against the model before quoting; hidden
  columns are probe-resistant (same 400 as a nonexistent column); the one
  string-literal splice is escaped and developer-controlled.
- **Capability enforcement is airtight on both frontends.** URL grammar and
  JSON tree terminate in one compiler with shared budgets; mass-assignment is
  triple-guarded (generated bodies, `clearReadOnly`, `rejectUnwritable`).
- **The transaction story is complete**: panic-safe rollback, nested joins with
  isolation-narrowing refused, startup refusal when the executor cannot begin a
  transaction, `AfterCommit` with honest failure semantics.
- **CI is stronger than most production libraries**: real Postgres 18 +
  PgBouncer in transaction pooling, generated-code drift gates, a wire-contract
  break gate (`impact-check`), `tsc`/dart-analyze over emitted clients,
  per-commit bisect, dependency allowlist with positive controls.
- **Error handling is a model**: SQLSTATE class-23 classification to typed
  `ConstraintError`, RFC 9457 problems with allow-lists, sanitized 500s with
  server-side logging, hook errors carrying their own status passed through.

## Findings and issues filed

Ordered by severity. "Reproduced" means a throwaway test confirmed the
behavior; none are committed.

| # | Finding | Severity |
|---|---|---|
| [#66](https://github.com/mind-vm/sqlb/issues/66) | JSON filter tree decodes numbers as float64: an int64 filter above 2^53 binds a **different value** than the identical URL filter — silent wrong rows from an untrusted request, violating ADR-0003's one-compiler promise. Reproduced. | major |
| [#67](https://github.com/mind-vm/sqlb/issues/67) | A declared action with a Hidden column in `Writes` fetches without it and persists a zero-derived value over the stored one — under the `FOR UPDATE` lock meant to make read-modify-write safe. Reproduced. | major |
| [#68](https://github.com/mind-vm/sqlb/issues/68) | `restcompat` never diffs `ReadOnly`/`Immutable`, so writer-side contract breaks pass `sqlb impact -error`; `FacetPatch` is declared and never emitted. Three false negatives reproduced. | major |
| [#69](https://github.com/mind-vm/sqlb/issues/69) | The ejected filter parser enforces neither `MaxListValues` nor `MaxValueLength` (100k-member `in.` list reproduced), and ejected search leaves LIKE wildcards live — the exit does not hold the API's own line, while claiming to. | major |
| [#65](https://github.com/mind-vm/sqlb/issues/65) | The quickstart's Step 3 still opens with `sql.Open` — the documented first path has not compiled since the ADR-0040 pgx flip. | major (adoption) |
| [#70](https://github.com/mind-vm/sqlb/issues/70) | Ejected handlers answer constraint violations 500 where the API answered 409/422, and the eject README's refusals list does not name the loss. | minor |
| [#71](https://github.com/mind-vm/sqlb/issues/71) | Typed-nil pointer comparand bypasses nil→`IS NULL`: `Eq(row.DeletedAt)` with a nil `*time.Time` matches nothing, silently. Reproduced. | minor |
| [#72](https://github.com/mind-vm/sqlb/issues/72) | Cursor machinery matches ordering columns by bare name: a qualified join column sharing the PK's name suppresses `Stable` and encodes the wrong boundary. Reproduced. | minor |
| [#73](https://github.com/mind-vm/sqlb/issues/73) | Multi-row insert decides default-zero omission per statement: a zero row in a mixed batch writes `0` instead of its default. Reproduced. | minor |
| [#74](https://github.com/mind-vm/sqlb/issues/74) | A second `?sort=`/`?filter=`/`?search=` is silently dropped, and POST/PATCH accept unknown query parameters — against the "refused, never ignored" stance. | minor |
| [#75](https://github.com/mind-vm/sqlb/issues/75) | `?page=`/`?offset=` are the one unbudgeted input dimension: deep offsets are a cheap scan lever, and `(page-1)*size` can overflow. | minor |
| [#76](https://github.com/mind-vm/sqlb/issues/76) | A hook predicate on a computed column, carried across an expansion join, fails at request time with a bare 42703 — against fail-at-build. | minor |
| [#77](https://github.com/mind-vm/sqlb/issues/77) | Consumers cannot test without Docker: `pgfake` is internal, and an `Executor` stub means hand-implementing `pgx.Rows`. The single biggest week-1 friction. | enhancement |
| [#78](https://github.com/mind-vm/sqlb/issues/78) | `cmd/sqlb` scratch cleanup discards the `os.RemoveAll` error; the leftover-policing test is racy against the shared module root (one observed flake). | minor |
| [#79](https://github.com/mind-vm/sqlb/issues/79) | `docs/comparisons.md` staleness: entoas does not generate handlers, Atlas lint went paid (v0.38), and the nearest competitors (GORM, jet, bob, prest, the Huma+hand-rolled-filters incumbent) are missing rows. | docs |

**Test-coverage gaps behind the findings**: no test combines action `Writes`
with Hidden/ReadOnly columns; no `restcompat` test flips a body-only
capability; the ejected package has CRUD parity tests but no adversarial-input
parity tests; and no rest-over-real-Postgres suite exercises constraint
mapping, expand+scope, or cursor paging end-to-end through HTTP.

## How sqlb compares (researched July 2026)

Full detail in [#79](https://github.com/mind-vm/sqlb/issues/79)'s context; the
table evaluators actually build:

| Alternative | State (July 2026) | Their edge | sqlb's edge |
|---|---|---|---|
| **sqlc** (18.1k★, v1.31.1) | Active | Compile-time typing against real SQL | Dynamic predicates — sqlc's #1 open request since 2020. Complementary, and the tested coexistence is sqlb's cheapest adoption wedge |
| **ent + entrest** (17.2k★ / 41★) | Active / one-person extension | The graph: M2M, traversal, depth; maturity; corporate backing | One tool, one vocabulary, layers over structs it didn't generate. entrest is the only real whole-story competitor and it splits across two projects |
| **GORM** (39.9k★, ~87k importers) | Active; generics API bumpy | Ubiquity | Everything above the builder; fail-loud vs silent-failure culture |
| **sqlx** (17.7k★) | **Abandoned** (no commits since Aug 2024) | — | Not a live alternative |
| **pgx** (14.1k★, v5.10.0) | The de-facto driver | Zero abstraction | sqlb is built on it; `eject` emits the plain-pgx version of your app |
| **jet** (3.8k★) / **bob** (1.8k★, ~69 importers) | Active | Compile-time column safety from live-DB introspection; bob's test factories | The entire HTTP-and-above story; migrations from declaration. Closest query-layer competitors; bob's adoption is barely ahead of zero |
| **squirrel** (8k★) | Maintenance mode, dormant | — | The predicate-AST idea with nothing above it |
| **PostgREST** (27.5k★, v14.16) / **Supabase** (~$10.5B, >10M databases) | Industrial | Zero app code, hosted platform, years of hardening | An in-process home for Go domain logic; capability opt-in instead of policy-guarded full exposure. Supabase absorbs greenfield demand from one end |
| **Hasura** (32.1k★) | Pivoted to PromptQL (Jun 2025) | — | Receding for new Go builds |
| **Prisma** (47.4k★, v7) | Go client **archived Jul 2025** | — | The Prisma-shaped experience is simply unavailable in Go |
| **The incumbent**: sqlc/jet + Huma/oapi-codegen + hand-rolled filters + hey-api | What Go teams actually assemble | Each piece proven | The hand-rolled filter layer is the exact boilerplate — and leak habitat — sqlb generates |

**Positioning.** The niche — a validated, capability-scoped filter grammar as
an embeddable Go library — is real (PostgREST/Supabase growth, .NET OData,
Spring JPA Specifications, sift's 2.76M weekly downloads prove the pattern is
institutionalized everywhere else) and currently unoccupied in Go: prest is a
standalone binary with a 2025 injection advisory, not a library. sqlb's
structural criticism of OpenAPI-derived clients (a filter param documents as
`array<string>`, so only schema-derived generation can encode
operator-per-column typing) checked out. The risks are not feature gaps:
trust (one author, zero consumers, in a category that prices longevity
heavily — mitigated unusually well by the generated eject path), the graph
ceiling (one-level expand, no M2M — real apps hit it fast), and Supabase
absorbing greenfield demand.

## What is missing before an MVP

Bar: a small team ships a working multi-tenant CRUD app in week 1 on the
documented path. **Verdict: usable now** — via `example/tasks` as the
template — with these concrete costs:

1. **The quickstart doesn't compile** ([#65](https://github.com/mind-vm/sqlb/issues/65)). Day-1 hours lost unless the team cribs from `example/tasks/cmd/server`. The cheapest fix on this list and the one with the worst funnel effect.
2. **No test double** ([#77](https://github.com/mind-vm/sqlb/issues/77)). First handler test means Docker in the unit loop or hand-rolling a `pgx.Rows` fake. Exporting `pgfake` closes it cheaply.
3. **Auth is BYO.** `rest.Options.Security` documents; enforcement is your middleware, with `example/tasks/auth` (JWT) the only reference. Fine for a team that has wired chi middleware before; a wall for one that hasn't.
4. **snake_case wire names, by design** (ADR-0036). A JS front end either adopts snake_case or transforms client-side; the generated TS client softens this considerably.
5. **Money and big integers need care**: `numeric → float64` by default (override via `codegen.Options.Types`, but no decimal guidance page), and `bigint` serializes as a JSON number (2^53 loss in JS clients — known, documented-at-minimum).

None of these block; all of them tax the first week. The multi-tenancy story
itself is the strongest documented path — `Scoped` models refuse to mount
without a confining hook, and the `?expand` cross-tenant hole was found and
closed before this review.

## What is missing before production

The library's own bar — "six months of someone other than the author running
it against production traffic" ([release-1.0.md](release-1.0.md)) — is honest
and unmet, and cannot be built. Beyond elapsed time:

1. **The major findings above** ([#66](https://github.com/mind-vm/sqlb/issues/66)–[#69](https://github.com/mind-vm/sqlb/issues/69)): silent wrong rows from a well-formed JSON filter, action write-back corruption, a compat gate that passes breaking deploys, and an eject path that drops the API's DoS caps. All small fixes; all the kind that spend adopter trust faster than time earns it.
2. **Observability is a seam, not a feature.** The sanctioned pattern (wrap the 2-method `Executor`; pgx's `QueryTracer` for OTel) lives in a test file, not a docs page. No metrics, no slow-query hook, no request-ID propagation guidance. All buildable; all DIY; none documented.
3. **Load-shaping gaps**: no rate-limiting story; no warning that `Filterable` on an unindexed column is a per-request seq scan; `Searchable` is `ILIKE '%x%'` with no trigram-index guidance; unbounded `?page=` ([#75](https://github.com/mind-vm/sqlb/issues/75)).
4. **Operational migration guidance**: lock-aware DDL and `Unblock` are genuinely good, but concurrent-deploy safety (two replicas applying at boot) is wholly the runner's problem and undocumented; `shadow` never validates Down migrations.
5. **Bus factor 1.** One author, no org, no SECURITY.md. The ADR density and the eject path are the only mitigations — the eject path being a real one: the exit produces working code with no sqlb import, which is more de-risking than most pre-1.0 libraries offer.
6. **The deferred traps owed doc notes before the tag** (release-1.0.md stream G): `NotOneOf` silently dropping NULL rows, `RawPred`'s `$N` repetition 500.

## Bottom line

For a greenfield Go+Postgres app whose core surface is filterable lists —
admin panels, dashboards, multi-tenant SaaS CRUD — sqlb is **adoptable for an
MVP today by a team that reads `example/tasks` first**, and the eject path
caps the downside in a way nothing else in this category does. For
production, the code is closer to ready than its age has any right to be, but
the honest gate is the one the project set for itself: someone other than the
author, running it, for a while. The highest-leverage week of work: fix
[#65](https://github.com/mind-vm/sqlb/issues/65)–[#69](https://github.com/mind-vm/sqlb/issues/69),
export the test fake ([#77](https://github.com/mind-vm/sqlb/issues/77)), and
write the observability and load-shaping pages — at which point the remaining
gap is not something code review can close.
