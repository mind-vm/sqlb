# example/tasks — a multi-tenant task manager

The larger worked example: six tables, a workspace boundary that has to hold,
and JWT authentication. `example/blog` shows the shortest path from a schema to
a server; this one shows what the same machinery looks like once an application
has a real shape.

It is a module of its own, like `pgtest`, so its dependencies — a Postgres
driver and goose — cost the engine nothing. `mise run deps-check`
still reports **standard library only**, because a nested module is invisible to
the root module's package list by construction rather than by exemption.

## Running it

```bash
docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18
```

```bash
export TASKS_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
export TASKS_JWT_SECRET="$(head -c 32 /dev/urandom | base64)"
go run ./cmd/server
```

Then <http://localhost:8080/docs>. The migrations apply at startup, so an empty
database is enough.

```bash
curl -X POST localhost:8080/auth/register -H 'content-type: application/json' \
  -d '{"name":"Ada","email":"ada@example.com","password":"correct-horse-battery-staple","workspace":"Acme"}'
```

That returns a bearer token. Everything else needs it:

```bash
curl "localhost:8080/tasks?priority=in.high,urgent&sort=-due_at" -H "authorization: Bearer $TOKEN"
```

**Postgres 18 or newer.** `cmd/migrate` passes `migrate.MinPostgres(18)`, so
UUIDv7 keys use the built-in `uuidv7()` and the DDL applies to a stock install
with no extension. On 17 or older, change that call to `schema.GenUUIDv4` in the
schema or install [`pg_uuidv7`](https://github.com/fboulnois/pg_uuidv7).

## What to read, and in what order

| | |
|---|---|
| [`taskschema/schema.go`](taskschema/schema.go) | The source of truth. Six tables, and a comment on every decision that is not obvious. |
| [`taskschema/sqlb.go`](taskschema/sqlb.go) | What `sqlb generate` emits and where, and the scratch database `sqlb migrate` replays into. One function, and it replaced the `cmd/gen` this example used to carry. |
| [`app/hooks.go`](app/hooks.go) | The workspace boundary. One registration per model, and no handler that knows about tenants. |
| [`auth/jwt.go`](auth/jwt.go) | HS256 in the standard library, with the three checks that make a verifier safe rather than merely working. |
| [`app/auth_routes.go`](app/auth_routes.go) | Register and login: the endpoints that establish the identity everything else is scoped by. |
| [`app/hooks.go`](app/hooks.go) | Also where the comment invariant lives: two writes in one transaction, and `AfterCommit` for the side effect. |
| [`app/events.go`](app/events.go) | The change feed. The one path a `BeforeQuery` hook does not reach, and the filter that closes it. |
| [`app/admin.go`](app/admin.go) | A worked cross-tenant admin surface: `/admin/*`, hand-mounted per [ADR-0050](../../docs/architecture.md#reachability-is-a-property-of-the-mount), over a handle that releases the `"tenant"` scope [ADR-0054](../../docs/architecture.md#a-named-scope-is-releasable-at-the-mount) names hooks.go's workspace predicate under. `auth.RequireAdmin` is the route half of the boundary; `cmd/mint-admin` is how the token gets minted. Not schema-declared — see the file's doc comment for why `studio` can't browse it yet. |
| [`cmd/migrate/main.go`](cmd/migrate/main.go) | The generated baseline, plus three things the DSL cannot express. |
| [`web/`](web/) | The generated TypeScript client, and the hand-written transport it takes. The schema reaches the browser without a second declaration. |
| [`mobile/`](mobile/) | The generated Dart client, plus the cursor pager an infinite-scrolling list needs. The same schema reaches a phone. |
| [`cli/`](cli/) | The generated cobra command tree, and [`cmd/taskctl`](cmd/taskctl/) — four lines — that runs it. The same schema reaches a shell. |
| [`app/server_test.go`](app/server_test.go) | Every claim above, asserted against a real Postgres. |

## The shape of it

```
register / login  ──▶  auth.Middleware  ──▶  claims in the context
                                                    │
                          generated handlers        │  BeforeQuery / BeforeCreate
                          hand-written handlers ────┴──▶  every query, scoped
```

`rest.Resource` mounts six tables in one call. None of those handlers mentions a
workspace, a token or a role — they call `sqlb.Query[T]`, and the hooks in
`app/hooks.go` add the predicate. That is the whole argument for hooks being the
domain seam: the scoping is written once and cannot be forgotten by a call site
that has not been written yet.

The middleware and the hooks are two independent checks. The middleware rejects
an unauthenticated request; the hooks refuse to build a query without claims. The
second exists because the interesting failures are the ones where the first is
bypassed — a background job, a test, a future gRPC surface. Neither ever falls
back to "no restriction", which is the shape most tenancy bugs take.

### The one path the diagram does not cover

`GET /events` is a Server-Sent Events stream that tells a client which rows
changed, so it can refetch. It is the one route where `BeforeQuery` does *not*
do the work, and that is not an oversight in the wiring — an invalidation is
published by a write rather than read through a query, so no read hook is on the
path at all. Without a filter, every subscriber would receive every workspace's
row ids and their timing.

[`app/events.go`](app/events.go) closes it with three lines of comparison, and
the thing being compared comes from the schema: `workspace_id` is `.Scoped()`, so
each event carries the workspace of the row that changed. The same declaration
that obliges the read hooks to exist is what makes the feed filterable — which is
the argument for declaring an obligation rather than writing a predicate.

The events are addresses, never rows. A client is told `tasks/{id} changed` and
refetches through `GET /tasks/{id}`, where every predicate above applies as
usual. Deletes are in the feed with their key because a delete here is a soft
delete, and a soft delete is an `UPDATE`.

The source is in-process, so this demo is single-replica and at-most-once. See
[`docs/rest/events.md`](../../docs/rest/events.md).

## Where the generated layer stops

Six endpoints are hand-written, in two groups, and each group marks a real
boundary rather than a gap in the generator:

- **`POST /auth/register`, `POST /auth/login`** — they establish the identity
  everything else is scoped by, so they run on a handle with no hooks attached.
  One deliberate exception, created in one place.
- **`DELETE /tasks/{id}`, `/lists/{id}`, `/comments/{id}`** — see below.

One used to be here and is not any more, which is the more interesting entry.
`POST /tasks/{id}/comments` existed because inserting a comment and
incrementing `tasks.comment_count` have to land together, and a generated
create under autocommit could not promise that. `rest` now wraps a generated
write in a transaction, so a hook receives a context carrying it — and the rule
moved to `BeforeCreate` and `AfterCreate` on the model. That is a stronger
guarantee than the endpoint gave, not merely a shorter one: the invariant now
holds for *every* path that creates a comment, including one written later by
someone who never read this file.

## The client is generated too

[`web/`](web/) holds a TypeScript client emitted from the same schema, in the
same run as `models_gen.go`. It is the same argument as the server's generated
layer, made in the other language: the capabilities a column declares become
types, so the compiler refuses what the server would answer 400 to.

```ts
const page = await listTasks(request, {
  where: {
    status: { in: ['todo', 'in_progress'] },
    due_at: { notnull: true },
    labels: { has: 'urgent' },
  },
  sort: ['-priority', 'position'],
  select: ['title', 'status'],
  expand: ['list'],
});
```

`status` admits the enum's four values and nothing else. `due_at` offers a null
test because the column is nullable, and `title` offers `contains` because it is
text. `labels` is a `text[]`, so it offers `has`, `hasany` and `hasall` — and
*not* `contains`, which stays the text substring operator rather than acquiring a
second meaning on array columns. `select` narrows the response type, so reading `page.items[0].description`
after that call does not compile — and neither does `sort: 'description'`, since
that column is searchable but never asked to be sortable.
[`web/src/refusals.ts`](web/src/refusals.ts) asserts each of those refusals with
`@ts-expect-error`, so a generator that widened a type fails the build.

What is *not* generated is the interesting half. [`web/src/api/http.ts`](web/src/api/http.ts)
is hand-written: the base URL, the bearer token from `POST /auth/login`, what a
401 does. The generated functions take that request function as an argument
rather than building one, for the reason `rest` mounts onto a `huma.API` you
built rather than handing you a router — and `POST /auth/login` is called from
the same file, because it is not a table and no schema generator will ever
produce it.

Run `mise run test-ts` to typecheck it. [ADR-0028](../../docs/architecture.md#typescript-client)
is the record.

## And so is the Dart client

[`mobile/`](mobile/) holds the same vocabulary for a Flutter app, emitted in the
same run. Three things could not carry over from TypeScript, and each is a
language fact rather than a preference: members are camelCase because
snake_case fails Dart's lowerCamelCase lint everywhere it is read; a projection
throws `MissingColumn` when the dropped column is read rather than failing to
compile, because Dart has no `Pick<T, K>`; and the fourth layer is a cursor
pager rather than a query-key factory, because Dart has no keyed query cache
for keys to key.

```dart
final feed = taskPager(
  transport,
  params: TaskListParams(
    where: TaskWhere(
      status: Cond(isIn: [TaskStatus.todo, TaskStatus.inProgress]),
      dueAt: NullableCond(notNull: true),
    ),
    sort: [TaskSort.priority.desc],
    perPage: 100,
  ),
);
await feed.loadMore();
```

That last part is the piece a phone actually needs. Every list response carries
`next_cursor`, so an infinite-scrolling list needs no page arithmetic — and the
arithmetic is what hand-written mobile clients reimplement out of `has_more` and
an offset counter, once per screen.

[`mobile/lib/refusals.dart`](mobile/lib/refusals.dart) asserts twenty-one
refusals the way `refusals.ts` asserts seventeen, using the `unnecessary_ignore` lint as
Dart's `@ts-expect-error`. Run `mise run test-dart`;
[ADR-0031](../../docs/architecture.md#dart-client) is the record.

## And so is the CLI

[`cli/`](cli/) holds a cobra command tree emitted in the same run, and
[`cmd/taskctl`](cmd/taskctl/) is the four-line `main` that runs it. Same
argument again, for a consumer that has no compile step to refuse anything at:
the capabilities a column declares become flags, so `--help` states exactly what
the resource accepts, without a request.

```bash
export TASKCTL_BASE_URL=http://localhost:8080
export TASKCTL_TOKEN="$(curl -s -X POST "$TASKCTL_BASE_URL/auth/login" \
    -H 'content-type: application/json' \
    -d '{"email":"you@example.com","password":"..."}' | jq -r .token)"

go run ./cmd/taskctl tasks list --status eq.todo --sort -priority --expand list
go run ./cmd/taskctl tasks list --status eq.done --all | jq -r '.items[].title'
go run ./cmd/taskctl tasks update 019... --set-null assignee_id
```

`--status` names the enum's four values in its usage; `--completed-at` offers
`isnull` because the column is nullable and no pattern operators because it is a
timestamp; `password_hash` has no flag on any command because it is hidden.
`--all` walks the collection with `?cursor=` rather than counting pages, so it
cannot read a row twice while the table is written to underneath it.

The token comes from `POST /auth/login` — hand-written, not a table — which the
CLI has no command for, the same asymmetry `web/src/api/http.ts` has.

Run `mise run test-cli` to build it and exercise the wire format against an
`httptest` server; no Docker.
[ADR-0029](../../docs/architecture.md#go-cli) is the record.

## Two things sqlb does not do that this example works around

Both are worth knowing before copying the schema.

**`schema.SoftDelete` adds a column and stops.** Nothing writes `deleted_at`,
nothing filters it out of reads, and the generated `DELETE` handler issues a real
`DELETE`. That is what its doc comment says, and `rest` has a test that fails if
the runtime ever starts reading the column instead — so this is settled
behaviour to build on, not a gap waiting to be closed. The tables that declare a
soft delete therefore do not expose `OpDelete`; the read hooks add
`deleted_at IS NULL`, and [`app/deletes.go`](app/deletes.go) serves `DELETE` as an
`UPDATE`. Both halves are a few lines and both are visible.

What the declaration does do is oblige the hook to exist: a resource over a
table declaring a soft delete, or a `Scoped` column, does not mount until the
registrations its operations need are on the registry
([ADR-0030](../../docs/architecture.md#declared-scope-is-required)). The hooks in
this example predate that check and satisfy it unchanged, which is the reason
the check is shaped the way it is.

**Composite foreign keys are not expressible in the DSL.** The hooks stop a
request naming a list in another workspace; they cannot stop a migration, a
repair script or an endpoint written next year. `cmd/migrate` adds
`tasks (workspace_id, list_id) → lists (workspace_id, id)` as a hand-written
`migrate.Change`, which makes the wrong reference unrepresentable rather than
merely unreachable. The unique index it needs *is* expressible, so half of it
lives in the schema with a comment pointing at the other half.

The same file adds two triggers, for the same reason: `updated_at` is otherwise
only ever set by its column default, and `tasks.completed_at` has to be
reconciled with `tasks.status` by something that can see the new row — which a
`BEFORE` trigger can and a `BeforeUpdate` hook cannot, since a hook is handed the
statement rather than the result.

## Authentication

Self-issued HS256, written out rather than imported, so the path from
`Authorization:` header to `WHERE` clause is readable without leaving the module.
[`auth/jwt.go`](auth/jwt.go) is about a hundred lines of standard library.

It is not a general-purpose JWT library. A service verifying tokens it did not
mint — Auth0, Keycloak, Cognito, Clerk — needs JWKS and RS256, and should use
`github.com/golang-jwt/jwt` rather than extending this. What is *not* simplified
is the verify path, because that is where a JWT implementation becomes an
authentication bypass:

- the algorithm is pinned **before** the signature is used, so `alg: none` and
  RS256-confusion are refused without either being attempted;
- the comparison is `hmac.Equal`, not `bytes.Equal`;
- expiry is required, not merely checked when present — a missing `exp`
  unmarshals to zero, and treating zero as "no expiry wanted" is an immortal
  token.

`auth/jwt_test.go` builds each of those forgeries and asserts it is refused.

Passwords are PBKDF2-HMAC-SHA256 at 600,000 iterations, with a per-password salt
and a constant-time comparison. PBKDF2 only because `crypto/pbkdf2` is in the
standard library as of Go 1.24; argon2id is the better choice and lives in
`golang.org/x/crypto`.

Two things a real deployment needs and this does not have: token revocation (the
TTL is the only bound on a logout or a removed membership taking effect) and a
refresh endpoint. Both are noted where they bite.

## Regenerating

```bash
sqlb generate ./taskschema     # models, typed columns, REST bodies, manifest, TS client, Dart client, CLI
go run ./cmd/migrate -force    # migrations, from the schema
```

`go generate ./...` runs the first line, because that is the directive in
[`taskschema/sqlb.go`](taskschema/sqlb.go) — which is also where this example
declares what it emits and where it lands ([ADR-0032](../../docs/architecture.md#sqlb-command)).
There is no `cmd/gen` any more; the whole of it was that declaration wrapped in
flag parsing.

`cmd/migrate` stays, and is not an oversight. Its second migration is three
things the DSL cannot express — two triggers and a pair of composite foreign
keys — written as `migrate.Change` values by hand, and no diff will ever produce
them. What `sqlb migrate` adds is the other direction: whether the history still
builds the schema after someone edits `schema.go`.

```bash
docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=x postgres:18-alpine
export SQLB_SHADOW_DSN='postgres://postgres:x@localhost:5433/postgres?sslmode=disable'
sqlb migrate -check ./taskschema
```

It reports one thing here and it is worth seeing, because it is an honest answer
rather than a failure of the example: the composite foreign keys come back as
constructs the DSL cannot express, so `current` is an incomplete picture and the
command says so before showing any diff. Then it reports no changes, which is
the claim — the checked-in history still builds what `schema.go` declares.

That it says nothing about `done_tasks_have_a_completion_time` is the fix for
[#24](https://github.com/mind-vm/sqlb/issues/24). Postgres stores a `Check` as
a parse tree and hands back its own spelling, so a declared check and an
introspected one never matched as strings and every run proposed dropping and
re-adding it. The declared expression is now put through the same normalisation
by asking the shadow database, which is open anyway at that point.

**That check is a CI gate, and it does not need you to run it.**
[`migrations/drift_test.go`](migrations/drift_test.go) is the same three steps —
replay, normalise, diff — with the shell taken off, and `mise run test-demo`
runs it. Editing `schema.go` without adding a migration passes `lint`, `vet` and
`generate-check` and fails there, with the `ALTER TABLE` it wanted printed in
the failure. It is also the gate that notices if the history ever grows a
construct the DSL cannot describe, because a *new* entry in the introspection
report means every diff computed from then on is working from a partial picture.

`mise run generate-check` fails if the committed output has drifted from the
schema. The migrations are *not* checked that way: `migrate.Write` refuses to
overwrite, because a migration already applied somewhere must not change under
the runner's feet. `-force` is the development escape, and it deletes only the
files it is about to write.

## Testing

```bash
mise run test-demo             # starts the test databases if they are not up
```

```bash
mise run test-ts               # needs no Docker
```

The first is the Go suite; the second typechecks the generated client and runs
its encoder tests. Both are gates in `mise run ci`.

Against a real Postgres in a container, one database per test. There is no
skip-when-Docker-is-absent path, for the reason `pgtest/doc.go` gives: a suite
that passes silently when it cannot reach a database reports coverage it does not
have. Several claims here can only be checked this way — that the composite
foreign keys reject a cross-workspace reference, that the `completed_at` trigger
fires, that a failed comment leaves the counter untouched.
