# tasks — a multi-tenant task manager

**In this repository:** [`example/tasks/`](https://github.com/mind-vm/sqlb/tree/main/example/tasks)

The larger worked example. [blog](blog.md) shows the shortest path
from a schema to a server; this shows what the same machinery looks like once an
application has a real shape — six tables, twenty-five endpoints, and a boundary
that has to hold.

It is a module of its own, so its dependencies — a Postgres driver, goose,
testcontainers — cost the engine nothing. `mise run deps-check` still reports
**standard library only**, because a nested module is invisible to the root
module's package list by construction rather than by exemption.

```bash
docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18

export TASKS_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
export TASKS_JWT_SECRET="$(head -c 32 /dev/urandom | base64)"
go run ./cmd/server
```

Then [localhost:8080/docs](http://localhost:8080/docs). Migrations apply at startup, so an empty
database is enough. **Postgres 18 or newer** — `cmd/migrate` passes
`migrate.MinPostgres(18)`, so UUIDv7 keys use the built-in `uuidv7()` and the
DDL applies with no extension.

## What it proves

**That a tenant boundary can be held in one file.** `rest.Resource` mounts six
tables in one call, and none of those handlers mentions a workspace, a token or
a role. They call `sqlb.Query[T]`, and the hooks add the predicate.

```
register / login  ──▶  auth.Middleware  ──▶  claims in the context
                                                    │
                          generated handlers        │  BeforeQuery / BeforeCreate
                          hand-written handlers ────┴──▶  every query, scoped
```

The middleware and the hooks are **two independent checks**. The middleware
rejects an unauthenticated request; the hooks refuse to build a query without
claims. The second exists because the interesting failures are the ones where
the first is bypassed — a background job, a test, a future gRPC surface. Neither
ever falls back to "no restriction", which is the shape most tenancy bugs take.

**That the schema reaches the browser and the shell without a second
declaration.** The same `go generate` run emits the models, the REST bodies, a
TypeScript client and a cobra command tree.

```ts
const page = await listTasks(request, {
  where: { status: { in: ['todo', 'in_progress'] }, due_at: { notnull: true } },
  sort: ['-priority', 'position'],
  select: ['title', 'status'],
  expand: ['list'],
});
```

`status` admits the enum's four values and nothing else. `due_at` offers a null
test because the column is nullable. `select` narrows the response type, so
reading `page.items[0].description` after that call does not compile — and
neither does `sort: 'description'`, since that column is searchable but never
asked to be sortable. `web/src/refusals.ts` asserts each refusal with
`@ts-expect-error`, so a generator that widened a type fails the build.

```bash
go run ./cmd/taskctl tasks list --status eq.todo --sort -priority --expand list
go run ./cmd/taskctl tasks list --status eq.done --all | jq -r '.items[].title'
go run ./cmd/taskctl tasks update 019... --set-null assignee_id
```

`password_hash` has no flag on any command, because it is hidden.

**That an endpoint can become a hook.** `POST /tasks/{id}/comments` used to be
hand-written, because inserting a comment and incrementing `tasks.comment_count`
have to land together and a generated create under autocommit could not promise
that. `rest` now wraps a generated write in a transaction, so the rule moved to
`BeforeCreate`/`AfterCreate` on the model. That is a **stronger** guarantee than
the endpoint gave, not merely a shorter one: the invariant now holds for every
path that creates a comment, including one written later by someone who never
read the file.

## The two workarounds, which are the part worth reading

Both are worth knowing before copying the schema.

**`schema.SoftDelete` adds a column and stops.** The tables that declare one
therefore do not expose `OpDelete`; the read hooks add `deleted_at IS NULL`, and
`app/deletes.go` serves `DELETE` as an `UPDATE`. What the declaration *does* do
is oblige the hook to exist — the resource does not mount until the
registrations its operations need are on the registry.

**Composite foreign keys are not expressible in the DSL.** The hooks stop a
request naming a list in another workspace; they cannot stop a migration, a
repair script or an endpoint written next year — and they do not run for an
`?expand` join at all. So `cmd/migrate` adds
`tasks (workspace_id, list_id) → lists (workspace_id, id)` as a hand-written
`migrate.Change`, which makes the wrong reference *unrepresentable* rather than
merely unreachable. The unique index it needs is expressible, so half of it
lives in the schema with a comment pointing at the other half.

The same file adds two triggers, for the same reason: `updated_at` is otherwise
only ever set by its column default, and `completed_at` has to be reconciled
with `status` by something that can see the new row — which a `BEFORE` trigger
can and a `BeforeUpdate` hook cannot.

## Authentication

Self-issued HS256, written out rather than imported, so the path from an
`Authorization:` header to a `WHERE` clause is readable without leaving the
module. About a hundred lines of standard library.

It is **not** a general-purpose JWT library — a service verifying tokens it did
not mint needs JWKS and RS256. What is not simplified is the verify path,
because that is where a JWT implementation becomes an authentication bypass: the
algorithm is pinned *before* the signature is used, the comparison is
`hmac.Equal`, and expiry is required rather than merely checked when present.
`auth/jwt_test.go` builds each of those forgeries and asserts it is refused.

Two things a real deployment needs and this does not have: token revocation, and
a refresh endpoint. Both are noted where they bite.

## Read it for

| | |
|---|---|
| [`taskschema/schema.go`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/taskschema/schema.go) | Six tables, with a comment on every decision that is not obvious |
| [`app/hooks.go`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/app/hooks.go) | The whole boundary: one generic scoping function used four times, and reads scoped separately from writes |
| [`app/app.go`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/app/app.go) | Six generated resources and six hand-written endpoints, one router |
| [`auth/jwt.go`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/auth/jwt.go) | The three checks that make a verifier safe rather than merely working |
| [`cmd/migrate/main.go`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/cmd/migrate/main.go) | A generated baseline, plus the three things the DSL cannot express |
| [`web/src/api/http.ts`](https://github.com/mind-vm/sqlb/blob/main/example/tasks/web/src/api/http.ts) | The transport that is deliberately not generated |

## Next

- [Hooks](../queries/hooks.md) — the mechanism this example is an argument for
- [library](library.md) — the mirror image: no authentication at
  all, and the hard part is a finite resource
