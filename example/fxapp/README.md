# example/fxapp — sqlb under a dependency-injection container

The example about *wiring*. `example/blog` is the shortest path from a schema
to a server and `example/tasks` is what that machinery looks like at
application scale; this one answers a different question, which people building
on [uber-go/fx](https://github.com/uber-go/fx) ask first: where do the sqlb
pieces go when the application is assembled by a container rather than by a
`main` that news everything up in order?

The answer is [`fxkit`](fxkit/): the pool, the migration runner, the hook
registry, the two handles and the HTTP surface are the kit's, and what is left
is what is genuinely this application's — a schema, its hooks, an auth module,
and the configuration boundary in [`platform.go`](platform.go). This example
used to hand-write that glue in three packages (`dbbase`, `sqlbkit`, `httpkit`);
`fxkit` is those three, collected.

**It is glue to copy, not a library to import.** It was briefly published as a
module, `github.com/mind-vm/sqlb/sqlbfx`, and
[ADR-0044](../../docs/architecture.md#the-container-is-an-adapter) records the
reversal: nearly all of it is opinion — chi, humachi, goose, `log/slog` — and
opinions that load-bearing are better adapted in a file you own than taken
whole from a dependency. [`fxkit/doc.go`](fxkit/doc.go) states the four
obligations a copy has to preserve, which is the part that is not opinion.

The schema is deliberately small — two tables, one of them a tenant — because
it is not the subject. It has exactly one property that matters: `notes.space_id`
and `spaces.id` are `Scoped`, so the generated resources refuse to mount unless
the hooks that confine them are registered ([ADR-0030](../../docs/architecture.md#declared-scope-is-required)).
That refusal is what makes the container's ordering guarantee worth stating,
and it is what [`app_test.go`](app_test.go) asserts by taking a module away.

It is a module of its own, like `pgtest` and `example/tasks`, so fx costs the
engine nothing. `mise run deps-check` still reports **standard library only**.

## Running it

```bash
docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18
```

```bash
export FXAPP_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
export FXAPP_SPACE_KEYS="acme=$(head -c 24 /dev/urandom | base64)"
go run ./cmd/server
```

Then <http://localhost:8080/docs>. The migrations apply at startup and the
configured spaces are created, so an empty database is enough.

```bash
export KEY='...the acme key...'
curl -X POST localhost:8080/notes -H "authorization: Bearer $KEY" \
  -H 'content-type: application/json' \
  -d '{"title":"Quarterly plan","body":"Ship the thing.","status":"published"}'

curl "localhost:8080/notes?status=eq.published&sort=-created_at" -H "authorization: Bearer $KEY"
curl localhost:8080/insights/notes -H "authorization: Bearer $KEY"
```

**Postgres 18 or newer**, for the reason `example/tasks` gives: the migration
uses the built-in `uuidv7()` so the DDL applies to a stock install with no
extension.

## What to read, and in what order

| | |
|---|---|
| [`cmd/server/main.go`](cmd/server/main.go) | The whole binary. One call. |
| [`app.go`](app.go) | The module list, and why its order does not matter. |
| [`platform.go`](platform.go) | The configuration boundary: this app's env, turned into the kit's two config structs. |
| [`store/module.go`](store/module.go) | The generated resources and the migration history, contributed to the kit's groups. Three lines and two arguments. |
| [`notes/hooks.go`](notes/hooks.go) | The space boundary: one registration per statement kind, and no handler that knows about spaces. |
| [`access/middleware.go`](access/middleware.go) | The auth module: verify a key, store the principal through sqlb’s seam. |
| [`app_test.go`](app_test.go) | Every claim here, asserted against a real Postgres — including the composition that must fail. |

## The shape of it

```
logs ──── fxkit glue ── Migrated ─┬─ Unscoped handle ── spaces.Directory ─┐
platform.go (configs) ─┘          │                                       │
                                  └───────────── scoped handle ◀── hooks ─┘
                                                       │
                                  huma.API ◀── operations ── store (generated)
                                                          └── notes  (hand-written)
```

Four things in that picture are the argument.

**The hook registry is a value group.** A module contributes its rules with
`fxkit.ProvideHooks`, and the handle everything queries through cannot be
constructed until every contributor has run. In a hand-written `main` that
ordering is a matter of writing the lines in the right order; here it is a
dependency edge, which is the version that still holds for a module written
next year by someone who never read this file.

**A refused mount is a boot failure.** `store.Register` returns the error sqlb
raises when a resource declares a scope no hook backs, `fxkit.OperationSet`
carries it out, and fx reports it instead of listening. Delete `notes.Module`
from the list and the server does not start:

```
rest: /notes exposes create|read|update|delete|list, and nothing confines store.Note
  create: BeforeCreate is not registered (space_id is Scoped)
  list and read: BeforeQuery is not registered (space_id is Scoped)
  ...
```

That is asserted, in `TestResourcesRefuseToMountWithoutHooks`. A guard nobody
has watched refuse is a claim rather than a check
([ADR-0016](../../docs/architecture.md#guards-proven-both-ways)).

**Ordering is a type, not a position.** `fxkit.Migrated` is an empty struct
that means "every registered migration set has been applied". The handles take
one, so every query in the process is downstream of the migrations by
construction — no module list can be written in an order that queries a table
that does not exist yet. The same trick puts the operations after the API and
the API after the handle.

**Two handles, one connection.** The kit provides the scoped handle everything
uses and `fxkit.Unscoped` for the two jobs that cannot be scoped because
they run before there is anything to scope by: provisioning the configured
spaces at boot, and resolving a slug to the id the hooks then filter on. It is
a distinct type rather than a "skip the hooks" flag, because a flag is
something a caller passes and the set of callers allowed to pass it is the
whole point. `grep -r 'fxkit.Unscoped'` lists every consumer.

## The modules

The platform half is one call; the rest is this application. That split is
what a platform repository makes into two packages — an `appbase.Standard()`
every product composes, and the product.

| | |
|---|---|
| `Platform()` ([`platform.go`](platform.go)) | The process logger, and the fxkit glue fed by this app's env: pool, migrations, handles, chi + Huma, server lifetime. |
| `store` | The generated package. `wiring_gen.go`'s `FxModule` contributes its migrations and its resources; `module.go` is one line composing it into `fx.Module("store", …)`. |
| `access` | Which space a request speaks for: a bearer key per space, verified in constant time, stored through the principal seam. |
| `spaces` | The tenant — provisioning, the slug-to-id directory the hooks resolve against, and the rule that confines the table itself. |
| `notes` | The feature: the space boundary for notes, and the one endpoint the generator does not write. |

## What this example is not

**It is not an authentication system.** A space presents a shared secret from
the configuration. There are no users, no sessions and no revocation, and a
leaked key is a leaked tenant until the configuration changes. That is the
smallest thing that is still a boundary rather than a convention — the
alternative an example is tempted by, a plain `X-Space` header, would let any
caller name any tenant and make every hook here decorative.
[`example/tasks`](../tasks/) is where authentication lives: registration, login,
PBKDF2, and an HS256 verifier with the three checks that make one safe. Copy
that for the identity half and this one for the wiring. What the `access`
module does show is the shape any replacement takes: verify however you
verify, then `sqlb.WithPrincipal` — the hooks read the principal back
through one seam and never learn the mechanism, which is what makes the auth
module swappable ([ADR-0044](../../docs/architecture.md#the-container-is-an-adapter)).

**It is not a claim that sqlb needs fx.** Nothing in the engine knows what a
container is, and `example/tasks` assembles the same pieces with a function and
an argument. `fxkit` is an adapter, and one this example owns rather than
imports; what the container buys is that the ordering constraints become edges
the compiler and the boot enforce, and the cost is a layer of indirection
between a constructor and its caller. Both are real; which one is worth it depends on how many modules the
application has.

**Migrations at startup suit a demo and a single-instance service.** They do
not suit a rolling deploy, where several new instances race to apply the same
migration and the old code briefly runs against the new schema. The kit is
where that changes: take `fxkit.Pool()` and `fxkit.Handles()` without
`fxkit.Migrations()`, and supply the `Migrated` fact from whatever your
deployment pipeline asserts.

## Testing

```bash
mise run test-fx               # starts the test databases; graph validation needs none
```

Against a real Postgres in a container, one database per test, given to the
application empty — so every boot in the suite is also a run of the checked-in
migration history. The container starts on first use rather than in `TestMain`,
so `go test -run TestGraphIsValid .` needs no Docker; that is not a skip, and a
test that needs Postgres and cannot have it still fails.

`TestGraphIsValid` is the cheap half worth copying into any fx application:
`fx.ValidateApp` resolves every dependency and constructs nothing, which catches
the mistakes a container introduces and a compiler cannot see — a misspelled
group tag, a missing provider, a cycle.

## Regenerating

```bash
sqlb generate ./noteschema     # models, typed columns, REST bodies, manifest
go run ./cmd/migrate -force    # migrations, from the schema
```

`go generate ./...` runs the first line. The second is a baseline plus one
hand-written trigger — `updated_at` is otherwise set by its column default and
by nothing afterwards — and `migrate.Write` refuses to overwrite, because a
migration already applied somewhere must not change under the runner's feet.
`-force` is the development escape, and it deletes only the files it is about
to write.
