# Go client and CLI

The REST layer's filter grammar is compositional and capability-gated, which is
what makes it safe and also what makes it invisible from the outside.
`curl 'localhost:8080/tasks?priority=gte.2'` either works or comes back with a
400 naming the filterable columns, and the only way to find out is to send it.

`codegen` emits a [cobra](https://github.com/spf13/cobra) command tree that puts
the same vocabulary in `--help`, where it can be read before the request rather
than after it. That matters most for the caller this exists for: an agent
working in a shell, for which a round trip is a turn.

## Turning it on

Set `CLIDir` on the generator you already have:

```go
codegen.Must(codegen.Generate(codegen.Options{
    Registry: schema.DefaultRegistry(),
    Dir:      ".",
    Package:  "tasks",

    // Relative to Dir. One file lands here; nothing is emitted without it.
    CLIDir:  "cli",
    CLIName: "taskctl",  // the binary's name, and the env prefix
}))
```

That writes **two** packages:

| File | Package | Depends on |
|---|---|---|
| `cli/client/client_gen.go` | the typed client — `Client`, `Request`, `Transport`, `Do`, `Run`, the problem document | the standard library, and nothing else |
| `cli/cli_gen.go` | the cobra command tree | the client above, plus cobra |

Neither imports sqlb or the generated models, so the binary holds no database
credential and needs no build tag to keep one out.

## Just the client

If what you want is the typed client — a sync job, a server-to-server caller, an
admin tool that already has a command tree of its own — set `ClientDir` and
leave `CLIDir` empty:

```go
ClientDir: "apiclient",   // relative to Dir
```

That emits the client and no command tree. It is the same package the CLI
imports, so a project can start with the CLI and add a Go consumer later without
generating anything twice.

```go
c := &client.Client{BaseURL: "https://api.example.com", Token: tok}
raw, err := c.Do(ctx, client.Request{Method: http.MethodGet, Path: "/tasks"})
```

The two were one package until
[#97](https://github.com/mind-vm/sqlb/issues/97), which meant a program that
wanted to make one typed HTTP request took a command-line framework to do it.
This is the split the TypeScript emitter already makes between `client.gen.ts`
and `queries.gen.ts`, for the same reason: a consumer that does not want the
framework should not pay for it.

Then a `main` that is four lines, and is the whole of what is not generated:

```go
package main

import (
    "os"

    "myapp/cli"
)

func main() {
    if err := cli.New(nil).Execute(); err != nil {
        os.Exit(1)
    }
}
```

`codegen.Check` covers both emitted files, so the usual staleness gate catches a
schema change that was never regenerated. A schema that exposes no resource
emits neither, rather than a file that imports cobra for an empty tree.

Add the dependency with `go get github.com/spf13/cobra`.

## What the commands are

One command per exposed table, one subcommand per operation it declares:

```
taskctl tasks list
taskctl tasks get <id>
taskctl tasks create
taskctl tasks update <id>
taskctl tasks delete <id>
```

An operation the resource does not expose has no subcommand. `lists` declares
`SoftDelete` and therefore no `OpDelete`, so `taskctl lists delete` is not a
405 — it is not a command.

## What `--help` knows

Everything a column declared, and nothing it did not:

```
$ taskctl tasks list --help
...
      --status stringArray    Filter on status, written operator.value, or a bare value for
                              equality. Repeat the flag to conjoin conditions. Operators: eq,
                              ne, gt, gte, lt, lte, in, nin, between, like, ilike, contains,
                              startswith, endswith.
                              Values: todo, in_progress, blocked, done.
      --completed-at stringArray
                              Filter on completed_at... Operators: eq, ne, gt, gte, lt, lte,
                              in, nin, between, isnull, notnull.
      --labels stringArray    Free-form labels. Filter on labels, an array column: written
                              operator.value, or a bare comma-separated list for whole-array
                              equality. Repeat the flag to conjoin conditions. Operators: eq,
                              ne, has, hasany, hasall, nhas, nhasany, nhasall.
      --sort strings          Ordering, most significant first. Prefix a column with - for
                              descending. Columns: title, status, priority, due_at,
                              completed_at, position, comment_count, created_at, updated_at.
```

- **One flag per filterable column**, taking the wire spelling of a condition.
  A column that never declared `.Filterable()` has no flag, so there is no way
  to spell the request the server would reject.
- **The operator set is narrowed by column type.** The null tests appear only on
  a nullable column, the pattern operators only on text, and the containment
  ones — `has`, `hasany`, `hasall` and their `n`-prefixed negations — only on an
  array, with `hasdoc`/`nhasdoc` the pair a jsonb column takes instead. An enum
  names its values.
  This is what the guarantee has to look like for a caller with no compile step:
  an agent reading `--help` is told what the resource accepts without sending a
  request to find out.
- **`--sort`, `--select` and `--expand`** list the columns and relations that
  opted in, and complete from them in a shell with completions installed.
- **Hidden columns have no flag anywhere.** `users.password_hash` is not
  filterable, not selectable, not settable.

Repeating a flag conjoins its conditions, because repeating a query parameter is
what conjoins them:

```bash
taskctl tasks list --priority gte.2 --priority lt.5 --status eq.todo
```

Flags are kebab-case, as cobra expects, and the snake_case spelling works too —
so a column name copied out of `sqlb.json` or out of an error message can be
typed verbatim.

## Reads

```bash
# Filtered, sorted, projected, and expanded — one request
taskctl tasks list --status eq.todo --sort -due_at --select id,title --expand list

# One row, with its list embedded
taskctl tasks get 019... --expand list

# Every matching row, walked by cursor
taskctl tasks list --status eq.done --all
```

`--all` follows `next_cursor` until the collection is exhausted and writes
everything as one page. It pages by [cursor rather than by page
number](../architecture.md#keyset-pagination), so a concurrent insert cannot make
the walk read a row twice — which is the failure a hand-written `while` loop
over `?page=` has and does not report.

Output is the server's JSON, written through unchanged, so it pipes:

```bash
taskctl tasks list --status eq.todo --compact | jq -r '.items[].title'
```

## Writes

A create takes one flag per settable column and marks the ones the database has
no answer for as required:

```bash
taskctl tasks create --list-id 019... --title 'Ship the CLI' --description '...'
```

Read-only columns have no flag: `workspace_id` is supplied by a `BeforeCreate`
hook, so there is nothing for a caller to send. A column with a default is
optional, and leaving it out means the database supplies the value rather than
the zero value overwriting it.

An update sends only the flags you passed:

```bash
taskctl tasks update 019... --status done
```

That distinction is load-bearing. `--title ''` sends an empty string; leaving
`--title` out sends nothing at all. The case no value flag can express — setting
a column back to NULL — is `--set-null`, and its argument is checked against
this resource's nullable columns before the request goes anywhere:

```bash
taskctl tasks update 019... --set-null assignee_id
```

## Configuration, and the seam

The root command reads two environment variables, named after the binary:

```bash
export TASKCTL_BASE_URL=http://localhost:8080
export TASKCTL_TOKEN="$(curl -s -X POST "$TASKCTL_BASE_URL/auth/login" \
    -H 'content-type: application/json' \
    -d '{"email":"you@example.com","password":"..."}' | jq -r .token)"
```

`--base-url` and `--token` override them. The token is sent as an
`Authorization: Bearer` header and never reaches the query string.

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

Precedence is what a reader would expect: flag, then the field you set, then the
environment, then the built-in default.

A header the schema has no way to name — tenant selection, an idempotency key,
a trace id — is a field on `Request` rather than a reason to replace
`Transport`: `Request.Header` reaches the wire after `Accept`, `Content-Type`
and the derived `Authorization`, so it replaces any of them rather than sitting
beside them ([#254](https://github.com/mind-vm/sqlb/issues/254)). Nothing
generates it; a hand-written command built on `Client.Run` sets it directly:

```go
client.Run(ctx, cmd.OutOrStdout(), cli.Request{
    Method: http.MethodGet,
    Path:   "/tasks",
    Header: http.Header{"X-Workspace-Id": []string{workspaceID}},
}, false)
```

`--timeout` binds a `context.Context` deadline around the request, not only
`http.Client.Timeout`, so setting `Client.HTTP` to reach for a custom
`RoundTripper` no longer costs the flag along with it.

## Rejections

A 400 arrives as the problem document with its allow-list intact, which is the
[actionable errors](../architecture.md#actionable-errors) guarantee reaching the
last consumer in the chain:

```
$ taskctl tasks list --sort -nonexistent
Error: the request could not be understood (HTTP 400)
  query.sort: column is not sortable
    allowed: title, status, priority, due_at, completed_at, position, comment_count
```

Errors go to stderr and set a non-zero exit code, so `set -e` and a caller
reading stdout both behave.

## What is not generated

Interactive prompts, a config file, output formatting beyond `--compact`,
credential storage, and any command for an endpoint you wrote by hand.
`example/tasks` has `POST /auth/login` on the same router, and nothing generic
can emit a command for it — the endpoint mints the credential every other
command sends, which is not a shape a schema describes.
[ADR-0029](../architecture.md#go-cli) records why, and what would change it.

## Adding a command of your own

The boundary above is not a dead end, and the seam it leaves is deliberate.
`Client.Run` takes a `context.Context` and an `io.Writer` rather than a
`*cobra.Command`, precisely so a caller outside the generated tree can have the
conventions the generated commands have: the response written to stdout as
JSON, `--compact` honoured, a rejection rendered to stderr with its allow-list
intact, and a non-zero exit. `New` returns the root, so `AddCommand` is right
there.

```go
func NewLoginCommand(c *client.Client) *cobra.Command {
    var email, password string
    cmd := &cobra.Command{
        Use:   "login",
        Short: "Exchange an email and password for a bearer token",
        Args:  cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            return c.Run(cmd.Context(), cmd.OutOrStdout(), client.Request{
                Method: http.MethodPost,
                Path:   "/auth/login",
                Body:   map[string]any{"email": email, "password": password},
            }, false)
        },
    }
    cmd.Flags().StringVar(&email, "email", "", "The account's email address. Required.")
    _ = cmd.MarkFlagRequired("email")
    cmd.Flags().StringVar(&password, "password", "", "The account's password. Required.")
    _ = cmd.MarkFlagRequired("password")
    return cmd
}
```

Two arguments carry the whole convention. `cmd.Context()` is what `--timeout`
and a Ctrl-C cancellation travel on — a `context.Background()` there compiles
and silently un-bounds the request. `cmd.OutOrStdout()` is what a test captures,
and what makes the command's output redirectable the way the generated ones are.

**Then the part the four-line `main` above hides.** `cli.New(nil)` builds the
`client.Client` internally and never hands it back, so the client your command
holds has to be constructed in `main` and passed in — otherwise the hand-written
command has a second configuration, and the root's `--base-url`, `--token` and
`--timeout` reach everything except it:

```go
func main() {
    c := &client.Client{}          // one client for the whole tree
    root := cli.New(c)             // the root's persistent flags bind to it
    root.AddCommand(cli.NewLoginCommand(c))

    if err := root.Execute(); err != nil {
        os.Exit(1)
    }
}
```

That failure has no compile error and no visible symptom until the first flag,
which is why it is worth the paragraph. `example/tasks` is this, worked:
[`cli/login.go`](../../example/tasks/cli/login.go) beside the generated
`cli_gen.go`, assembled in
[`cmd/taskctl/main.go`](../../example/tasks/cmd/taskctl/main.go), and pinned by
two tests in
[`cli/cli_test.go`](../../example/tasks/cli/cli_test.go) — one asserting that
the root's `--base-url` and `--token` reach the hand-written command, one that a
401 arrives as the server's problem document rather than a decoding error.

A hand-written file lives in the generated package on purpose: the emitter names
its own helper `runRequest` rather than `run` so that the name a consumer's file
is likely to want stays free.

## Worked example

[`example/tasks`](../../example/tasks/) generates `cli/` from its schema and
runs it from [`cmd/taskctl`](../../example/tasks/cmd/taskctl/). Its
[`cli/cli_test.go`](../../example/tasks/cli/cli_test.go) drives the emitted
commands against an `httptest` server and asserts what reaches the wire, which
is where to look for the exact encoding of anything above.
