# Generated CLI reference

Checked against `codegen/gocli.go` and `codegen/gocli_runtime.go`. The guide
page is [Go CLI](../cli/README.md).

Names below use `taskctl` as the binary name; yours is whatever `CLIName` says,
and the environment prefix is that name upper-cased.

## Commands

One command per exposed table, one subcommand per operation it declares:

```
taskctl tasks list
taskctl tasks get <id>
taskctl tasks create
taskctl tasks update <id>
taskctl tasks delete <id>
```

An operation the resource does not expose **has no subcommand** — not a command
that returns 405.

## Global flags

| Flag | Environment | Default |
|---|---|---|
| `--base-url` | `TASKCTL_BASE_URL` | `http://localhost:8080` |
| `--token` | `TASKCTL_TOKEN` | *none* |
| `--compact` | — | off — pretty-printed JSON |
| `--verbose`, `-v` | — | off |

Precedence is flag, then the field set on `cli.Client`, then the environment,
then the built-in default. The token is sent as an `Authorization: Bearer`
header and never reaches the query string.

## `list`

| Flag | Type | From |
|---|---|---|
| `--<column>` | repeatable string | one per **filterable** column. Takes the wire spelling of a condition, `operator.value`, or a bare value for equality. Repeating conjoins |
| `--sort` | string list | the sortable columns, `-` prefix for descending |
| `--select` | string list | the non-hidden columns |
| `--expand` | string list | the expandable relations. Absent if the resource declares none |
| `--search` | string | present only if some column is searchable |
| `--page` | int, default 1 | |
| `--per-page` | int | the resource's default when unset |
| `--count` | bool | asks for `total` |
| `--cursor` | string | resume after a position |
| `--all` | bool | follow `next_cursor` until exhausted, writing everything as one page |

`--all` cannot be combined with `--page` or `--cursor`; the error says so. It
pages by cursor rather than by page number, so a concurrent insert cannot make
the walk read a row twice — the failure a hand-written loop over `?page=` has
and does not report.

A column that never declared `.Filterable()` has **no flag**, so the request the
server would reject has no spelling. Flags are kebab-case as cobra expects, and
the snake_case spelling works too, so a column name copied out of `sqlb.json` or
an error message can be typed verbatim.

### What `--help` narrows

The operator set in each flag's usage is narrowed by column type, exactly as the
[filter grammar](filter-grammar.md) narrows it:

```
      --status stringArray    Filter on status, written operator.value, or a bare value for
                              equality. Repeat the flag to conjoin conditions. Operators: eq,
                              ne, gt, gte, lt, lte, in, nin, between, like, ilike, contains,
                              startswith, endswith.
                              Values: todo, in_progress, blocked, done.
      --completed-at stringArray
                              Filter on completed_at... Operators: eq, ne, gt, gte, lt, lte,
                              in, nin, between, isnull, notnull.
```

Null tests appear only on a nullable column; pattern operators only on text; an
enum names its values. Hidden columns have no flag anywhere — not filterable,
not selectable, not settable.

## `get`

Takes the id as a positional argument. `--expand` is its only flag, and it is
absent when the resource declares no relation.

## `create` and `update`

`create` takes one flag per settable column, and marks required the ones the
database has no answer for. A read-only column has no flag: `workspace_id` is
supplied by a `BeforeCreate` hook, so there is nothing for a caller to send.

`update` sends **only the flags you passed**. That distinction is load-bearing:
`--title ''` sends an empty string; leaving `--title` out sends nothing at all.

| Flag | Meaning |
|---|---|
| `--set-null <column>` | Sets a column back to NULL — the case no value flag can express. Repeatable, and checked against this resource's nullable columns before the request goes anywhere |

## Output and exit codes

Output is the server's JSON, written through unchanged, so it pipes:

```bash
taskctl tasks list --status eq.todo --compact | jq -r '.items[].title'
```

Errors go to stderr and set a non-zero exit code, so `set -e` and a caller
reading stdout both behave. A 400 arrives as the problem document with its
allow-list intact:

```
$ taskctl tasks list --sort -nonexistent
Error: the request could not be understood (HTTP 400)
  query.sort: column is not sortable
    allowed: title, status, priority, due_at, completed_at, position, comment_count
```

## The seam

Everything the schema cannot derive lives on `Client`, and `Transport` replaces
the built-in HTTP implementation entirely — for a test that must not open a
socket, or for auth that is a signature rather than a bearer token:

```go
cli.New(&cli.Client{
    BaseURL: "https://api.example.com",
    Transport: func(ctx context.Context, req cli.Request) (json.RawMessage, error) {
        // sign, retry, refresh — whatever this application does
    },
})
```

## Not generated

Interactive prompts, a config file, output formatting beyond `--compact`,
credential storage, and any command for an endpoint you wrote by hand. A login
endpoint is not a table; the generated tree covers generated CRUD and stops
there ([ADR-0029](../architecture.md#go-cli)).
