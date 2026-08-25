# Contributing

Thanks for looking. This is a pre-1.0 project with one author, so the most
useful contributions are reports of what broke and what was confusing, in that
order.

## Before you start

For anything more than a typo, **open an issue first**. The design has a
written record — see [architecture.md's Decisions section](docs/architecture.md#decisions) — and a change that contradicts
one of those records is not necessarily wrong, but it needs to argue with the
record rather than around it. That conversation is cheaper before the code than
after.

## The loop

```bash
mise run test
```

No Docker and no Postgres: the engine's tests run against an in-memory
`Executor` — see `internal/pgfake` — which is what keeps this fast enough to run
on every save.

```bash
mise run ci
```

The full fourteen-stage gate. There is no server-side CI to run it for you —
this is what to run before tagging a release, not before every push. Before
pushing, run `mise run preflight` instead: heal, build, and the database-free
tests, in about fifteen seconds.

```bash
mise run preflight
```

The database-backed suites need Postgres, pgvector and PgBouncer. They no longer
start their own — `compose.yaml` defines all three, `mise run pg-up` starts them,
and every task that needs them depends on it, so `mise run test-pg` just works.
`mise run pg-down` stops them again.

```bash
mise run heal
```

Applies every fix the tooling can make on its own — formatting, lint autofixes,
`go mod tidy`, regenerated code — and reports what it changed. `ci` gates and
never rewrites; `heal` rewrites and never gates. Nothing does both, on purpose.

`mise tasks` lists the rest.

## What a change is expected to carry

**A test that fails without it.** For a bug fix, the most useful thing you can
put in the pull request is the assertion in its failing form: the value that
came out wrong, not a description of it.

**A guard proven in both directions.** If the change adds a check, show that it
fires. [ADR-0016](docs/architecture.md#guards-proven-both-ways) exists because this
repository has already shipped three guards that reported success while
verifying nothing. A test that cannot fail is worse than no test, because it
is also a claim.

**Comments that say why, not what.** The code in this repository explains the
reasoning that is not recoverable from reading it — why a barrier is where it
is, what broke the first time, what the alternative cost. It does not narrate
the next line.

**Regenerated output, committed.** Generated code is checked in, so
`mise run generate` is part of the change rather than something CI does. The
`generate-check` gate fails otherwise.

## Decision records

An ADR is a living document, not a minute of a meeting. If you find one that
the code has since outgrown, correcting it *is* a contribution — say what
changed and why, and update the Status and Confidence lines.
[architecture.md's Decisions section](docs/architecture.md#decisions) has the convention.

## Commit messages

The subject line says what changed in the terms of the problem, and the body
says why it was worth changing and what it cost. `git log` here is meant to be
readable as an account of the reasoning, so a body of three sentences is
normal and a body of "fix bug" is not.

## Reporting a security issue

Please do not open a public issue. Email the address on the commits in this
repository with what you found and how to reproduce it, and give it a few days
before disclosing.

## Scope

The things this project has decided *not* to do are recorded rather than left
implicit: multi-dialect support ([ADR-0001](docs/architecture.md#postgres-only)),
a query DSL that reaches every Postgres construct
([ADR-0005](docs/architecture.md#runtime-query-engine)), and an annotation slot on
the schema DSL ([ADR-0024](docs/architecture.md#no-annotation-slot)) are the three
that come up most. Each says what would change its mind.
