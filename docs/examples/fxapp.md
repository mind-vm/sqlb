# fxapp — sqlb under a dependency-injection container

**In this repository:** [`example/fxapp/`](https://github.com/mind-vm/sqlb/tree/main/example/fxapp)

The example about *wiring*. [blog](blog.md) is the shortest path
from a schema to a server and [tasks](tasks.md) is that machinery
at application scale; this one answers the question people building on
[uber-go/fx](https://github.com/uber-go/fx) ask first — where do the sqlb pieces
go when the application is assembled by a container rather than by a `main` that
news everything up in order?

The schema is two tables, one of them a tenant, because the schema is not the
subject. It has exactly one property that matters: the tenant column is
`Scoped`, so the generated resources refuse to mount unless the hooks that
confine them are registered.

```bash
docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18

export FXAPP_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
export FXAPP_SPACE_KEYS="acme=$(head -c 24 /dev/urandom | base64)"
go run ./cmd/server
```

The body of `main` is one call. Migrations apply at startup and the configured
spaces are created, so an empty database is enough.

## What it proves

**That the hook registry belongs in a value group.** A module contributes its
rules with a `group:"hooks"` result tag, and the handle everything queries
through cannot be constructed until every contributor has run:

```go
func NewScoped(unscoped *sqlb.DB, sets []HookSet, log *slog.Logger) (*sqlb.DB, error) {
    reg := sqlb.NewRegistry()
    for _, set := range sets {
        if err := set.Register(reg); err != nil {
            return nil, fmt.Errorf("fxkit: registering %s hooks: %w", set.Module, err)
        }
    }
    return unscoped.WithHooks(reg), nil
}
```

In a hand-written `main`, "register the hooks before mounting the resources" is
a matter of writing the lines in the right order. Here it is a dependency edge,
which is the version that still holds for a module written next year by someone
who never read the file.

**That a refused mount can be a boot failure.** The generated `Register` returns
the error sqlb raises when a resource declares a scope no hook backs
([ADR-0030](../architecture.md#declared-scope-is-required)), the operation set
carries it out, and fx reports it instead of listening. Remove the notes module
from the list and the server does not start:

```
rest: /notes exposes create|read|update|delete|list, and nothing confines store.Note
  create: BeforeCreate is not registered (space_id is Scoped)
  list and read: BeforeQuery is not registered (space_id is Scoped)
```

That is asserted rather than described. `TestResourcesRefuseToMountWithoutHooks`
boots the reduced module list and requires the failure, because a guard nobody
has watched refuse is a claim rather than a check
([ADR-0016](../architecture.md#guards-proven-both-ways)).

**That ordering is better expressed as a type than as a list.** `fxkit.Migrated`
is an empty struct meaning "every registered migration set has been applied".
The sqlb handle takes one, so every query in the process is downstream of the
migrations by construction — no module list can be written in an order that
queries a table that does not exist yet.

**That the unscoped handle is a named value, not a flag.** Two jobs cannot be
scoped, because they run before there is anything to scope by: provisioning the
configured tenants at boot, and resolving a key to the tenant the hooks then
filter on. Both ask for `fxkit.Unscoped`, a distinct type, so `grep` lists every
consumer — a "skip the hooks" boolean would instead be something any caller
could pass, and which callers may pass it is the whole question. A type also
cannot be misspelled the way a `name:"unscoped"` string tag can, which is what
it replaced.

## The shape of it

```
logs ──── fxkit ── Migrated ─┬─ unscoped handle ── spaces.Directory ─┐
                            │                                       │
                            └───────────── scoped handle ◀── hooks ─┘
                                                 │
                              huma.API ◀── operations ── store (generated)
                                                       └── notes  (hand-written)
```

Two platform modules that would be the same in any application — `logs` and
`fxkit`, which is the pool, the migration runner, the handles and the HTTP
surface in one package — and four that are this one: `store`, `access`,
`spaces`, `notes`. That split is what a platform repository makes into two
packages: a standard stack every product composes, and the product.

`fxkit` is glue to copy rather than a library to import. It was briefly a
published module and is not one now
([ADR-0044](../architecture.md#the-container-is-an-adapter) has the reversal):
nearly all of it is opinion — chi, humachi, goose, `log/slog` — and opinions
that load-bearing are better adapted in a file you own. Its `doc.go` states the
four obligations a copy has to preserve, of which the boot refusal above is the
first.

## What it is not

**It is not an authentication system.** A tenant presents a shared secret from
the configuration; there are no users, no sessions and no revocation.
[tasks](tasks.md) is where authentication lives. The shared secret
is still a boundary rather than a convention, which a plain `X-Space` header
would not be — with that, any caller could name any tenant and every hook here
would be decorative.

**It is not a claim that sqlb needs fx.** Nothing in the engine knows what a
container is, and `tasks` assembles the same pieces with a function and an
argument. What the container buys is that ordering constraints become edges the
boot enforces; the cost is a layer of indirection between a constructor and its
caller. Which is worth it depends on how many modules the application has.

## Testing

```bash
mise run test-fx
```

Against a real Postgres in a container, one database per test, handed to the
application *empty* — so every boot in the suite is also a run of the checked-in
migration history. The container starts on first use rather than in `TestMain`,
so the one test that constructs nothing runs without Docker:

```go
func TestGraphIsValid(t *testing.T) {
    if err := fx.ValidateApp(fxapp.Modules()); err != nil {
        t.Fatalf("the module graph does not resolve: %v", err)
    }
}
```

That is the half worth copying into any fx application. `fx.ValidateApp`
resolves every dependency and builds nothing, which catches the mistakes a
container introduces and a compiler cannot see — a misspelled group tag, a
missing provider, a cycle.
