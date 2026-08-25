# example/recipes — one file per aspect

The other directories under `example/` are whole applications. They answer
"what does this look like assembled", which is the right question once and the
wrong one when you already know what you are building and need to know how one
piece is spelled.

This directory answers the second question. Each file is one aspect, each
function is one point, and each point ends in output that was produced by
running the code rather than by typing it.

```bash
go test ./example/recipes            # no Docker, no Postgres, ~0.3s
go doc github.com/mind-vm/sqlb/example/recipes
```

**None of it can drift.** Every recipe is a Go example function, so the printed
output is compared against the comment on every run. A recipe describing an API
that changed fails the build instead of misleading its next reader, which is the
whole reason they are examples rather than a page of prose. They are part of
`mise run test`, so no separate gate exists to forget.

## Finding one

Grep is the intended entry point — the file names and the function names are
both the index:

```bash
rg -l cursor example/recipes             # which files are about keyset paging
rg '^func Example_' example/recipes      # every recipe, one per line
```

## The index

Queries

| | |
|---|---|
| [`query_test.go`](query_test.go) | A query is a value; `SQL()` renders it without running it. Terminal methods, `Clone`, projecting into another type. |
| [`predicates_test.go`](predicates_test.go) | The case static generators cannot express: a predicate added on a branch. `If`, `Or`, `And`, `Not`, predicates as functions and as slices. |
| [`operators_test.go`](operators_test.go) | The operator vocabulary by column type — comparison, text, `IN`, null tests, `BETWEEN`, column-to-column. |
| [`arrays_test.go`](arrays_test.go) | A `text[]` column is a plain Go slice. `Has`, `HasAny`, `HasAll`, and what each does with an empty value set. |
| [`json_test.go`](json_test.go) | A `jsonb` column is `json.RawMessage`, and `@>` is the operator a GIN index serves. |
| [`aggregate_test.go`](aggregate_test.go) | `GROUP BY`, `HAVING`, and `Collect` into the struct a grouped query actually returns. |
| [`join_test.go`](join_test.go) | `Join`, `LeftJoin`, self-joins, and `EqField` — which is the only column-to-column comparison there is. |
| [`expand_test.go`](expand_test.go) | Relations resolved inline, and why the joined row's own capabilities travel with it. |
| [`paging_test.go`](paging_test.go) | Offset paging, `Stable`, and the cursor loop: run, `CursorFor`, `After`. |
| [`raw_test.go`](raw_test.go) | The escape hatches, and the discipline they keep: raw structure, never raw values. |
| [`typedcolumns_test.go`](typedcolumns_test.go) | The generated column facade: `Col`, `TextCol`, `ArrayCol`, and why a hidden column has no entry at all. |

Writes

| | |
|---|---|
| [`mutate_test.go`](mutate_test.go) | Insert with database defaults written back, upserts, the unscoped-statement refusal, `SetExpr`, delete. |
| [`transaction_test.go`](transaction_test.go) | `WithTx`, rollback, `AfterCommit` for anything the outside world can see, and why nesting joins rather than nests. |
| [`hooks_test.go`](hooks_test.go) | The domain seam. Tenant scoping every read, normalising on write, amending a statement, a registry of your own, and `TxFrom`. |
| [`errors_test.go`](errors_test.go) | The sentinels, constraint classification, and the `SetErrorClassifier` seam that fills in the constraint name without sqlb naming your driver. |

The HTTP layer

| | |
|---|---|
| [`filter_test.go`](filter_test.go) | The URL grammar, `Options` as the resource's limits, and the whole HTTP-to-SQL layer for a list endpoint in one handler. |
| [`filtertree_test.go`](filtertree_test.go) | The JSON expression tree: the same compiler, the same gate, arbitrary nesting. |

Design time

| | |
|---|---|
| [`schema_test.go`](schema_test.go) | Declaring a table, `Expose`, `Lint` versus `Validate`, and module prefixes. |
| [`migrate_test.go`](migrate_test.go) | `Diff` returns changes as values. The first migration, a later one, and what a destructive change renders as. |
| [`describe_test.go`](describe_test.go) | Using sqlb over structs you did not generate and would rather not edit. |
| [`explain_test.go`](explain_test.go) | Planning against the live schema without running it, and plan diagnostics as a test assertion. |

Wiring

| | |
|---|---|
| [`executor_test.go`](executor_test.go) | `Executor` is two methods, which is why sqlb ships no tracing API. |

Support, not a recipe: [`models.go`](models.go) holds the three models every
file queries, and [`helpers_test.go`](helpers_test.go) holds the print helpers
and the recording executor.

## Adding one

Keep it to one point. A recipe that shows three things is three recipes, and
the reader searching for the second will not find it inside the first.

Name it `Example_<topic><WhatItShows>` — the topic prefix is what makes the
grep above useful, and the lower-case first letter after the underscore is what
`go vet` requires of a package-level example.

Print the clause the recipe is about rather than the whole statement;
`showWhere` does that. Then say in the comment *why* the API is shaped this way,
not only what it does. The comment is the recipe, and the code is the proof that
the comment is still true.

Most recipes need no database: compiling the statement is the honest way to show
a query builder, and it is why this suite runs in under a second. Reach for
`recordingDB` only when the thing being shown is execution itself — a hook fires
on execution, and a transaction is not a statement. Since [ADR-0040](../../docs/architecture.md#the-driver-is-a-dependency)
an `Executor` is pgx-shaped, so the canned result set behind it comes from
`internal/pgfake` rather than from a `database/sql` driver this package
registers.
