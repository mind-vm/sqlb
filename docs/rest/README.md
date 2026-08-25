# Mounting resources

The premise: the HTTP-to-SQL layer of a filter/sort/search page is mostly
boilerplate, and it is boilerplate you should not have to write. What makes that
safe rather than reckless is that the URL grammar compiles into the *same*
predicate AST your Go code produces — so one compiler, one bind-parameter
discipline, and one set of hooks cover both.

## Mounting

The batteries-included path is one call. `rest.NewServer` builds a huma API on
`net/http` — no third-party router — and has huma serve the OpenAPI document and
its docs page for you:

```go
srv := rest.NewServer(rest.Config{Title: "Blog", Version: "1.0.0"})
if err := blog.Register(srv.API, db); err != nil {   // generated from the schema
    return err
}
http.ListenAndServe(":8080", srv.Handler)   // wrap srv.Handler with your middleware
```

That is the default because importing sqlb should cost nothing: the engine and
the REST adapter reach a consumer's module graph as the standard library plus
huma, and no router beyond `net/http`.

### Bringing your own router

`rest` mounts on a `huma.API`, not a router it builds, so chi, gin, echo — and
all of that router's middleware — stay a first-class choice. Build the API with
the matching huma adapter and hand it to the same generated `Register`:

```go
router := chi.NewRouter()
router.Use(middleware.RequestID, middleware.Recoverer, yourAuth)

api := humachi.New(router, huma.DefaultConfig("Blog", "1.0.0"))
if err := blog.Register(api, db); err != nil {
    return err
}
http.ListenAndServe(":8080", router)
```

`rest.NewServer` is a convenience over this seam, not a replacement: whatever you
pass `blog.Register` — the server's `srv.API` or one you built yourself — is a
plain `huma.API`. The two examples show both paths:
[`example/blog`](../../example/blog) mounts on the `NewServer` default, and
[`example/tasks`](../../example/tasks) brings a chi router for its middleware.

`blog.Register` is one `rest.Resource` call per exposed table. Written out, one
of them looks like this:

```go
rest.Must(rest.Resource[blog.Post, blog.PostCreate, blog.PostPatch](api, db, rest.Options{
    Path: "/posts",
    Ops:  rest.CRUD | rest.OpList,
}))
```

`T` is the row type, `C` the create body and `U` the update body. A resource
exposing neither create nor update passes `rest.None[T]` for both. Registration
is the startup path, so failures are returned rather than panicked — a mistake
should name the resource that caused it.

`rest.CRUD | rest.OpList` is the fully exposed collection. The other named
shape is **`rest.Reads`** — `OpRead | OpList`, generated reads with the writes
left hand-written:

```go
rest.Must(rest.Resource[blog.Post, rest.None[blog.Post], rest.None[blog.Post]](api, db, rest.Options{
    Path: "/posts",
    Ops:  rest.Reads,
}))
```

That is the mount an application adopting sqlb into an existing REST surface
reaches for, and it is deliberate rather than unfinished: the app already has
its writes, and the reasons they stay hand-written are domain reasons that do
not expire — a create that writes bytes to object storage before the row
([`example/attachments`](../../example/attachments) is that one, worked through), a row
born in one domain verb and closed in another, a column whose transition *is*
the publish that notifies the org, per-field authorization a hook can constrain
but not express. Reaching for `rest.CRUD` and switching two thirds of it off
describes the same mount as a shortfall; this one names it (issue #101).

The handlers are **not** generated: `rest.Resource[T, C, U]` is one generic
function serving every resource. What is per-resource is the OpenAPI document,
built from each column's capabilities.

[`example/tasks/app/app.go`](../../example/tasks/app/app.go) is this assembled
for real: authentication middleware, six generated resources mounted in one
call, and six hand-written endpoints on the same router and in the same
OpenAPI document. The thing to notice is what the generated half does *not*
contain — no mention of tenants, tokens or roles anywhere in it, because the
hooks cover those for every read the handlers issue.

## Serve: the whole server

`rest.NewServer` gets you the HTTP layer. Everything around it — opening the
pool, pinging it, running migrations, listening, shutting down gracefully on
`SIGINT`/`SIGTERM` — is boilerplate every sqlb server writes identically, and
`rest.Serve` owns that part too:

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

err := rest.Serve(ctx, rest.ServeConfig{
    DSN:     os.Getenv("DATABASE_URL"),
    Migrate: migrations.Apply,
}, func(srv *rest.Server, db *sqlb.DB) error {
    return blog.Register(srv.API, db)   // generated, or hand-written — whatever mounts
})
```

`mount` — the function's last argument — is the seam for which resources
mount, whether one needs a `huma.Group`, and what that group's middleware
does. None of it is inferable from a schema value alone, and `Serve` does not
try to guess it; everything before `mount` runs the same way in every
application.

Middleware for the whole server — establishing a principal, request logging,
panic recovery — is `ServeConfig.Middleware`, applied once `mount` returns
rather than by assigning `srv.Handler` from inside `mount` and relying on
`Serve` reading it back afterward:

```go
err := rest.Serve(ctx, rest.ServeConfig{
    DSN:        os.Getenv("DATABASE_URL"),
    Middleware: authn.Middleware,
}, func(srv *rest.Server, db *sqlb.DB) error {
    return blog.Register(srv.API, db)
})
```

`mount` receives a `*sqlb.DB`, not an `sqlb.Executor`. `Serve` opens the pool
and wraps it a frame up, so it knows the concrete type — and the first thing a
real mount usually does is attach its hook registry, which lives on `*sqlb.DB`:

```go
}, func(srv *rest.Server, db *sqlb.DB) error {
    reg := sqlb.NewRegistry()
    blog.RegisterHooks(reg)                 // your own, wherever they live
    return blog.Register(srv.API, db.WithHooks(reg))
})
```

That is the same handle a [`Scoped`](../schema/capabilities.md) model's
mount-time obligation check runs against, so the seam hands you the value at
the type the check needs. Passing `db` straight through still works wherever
an `Executor` is wanted.

| `ServeConfig` field | Default | |
|---|---|---|
| `DSN` | — | required |
| `Addr` | `:8080` | |
| `Server` | zero `Config` | passed through to `NewServer` |
| `Middleware` | `nil` | `func(http.Handler) http.Handler`, applied to the handler `mount` left once `mount` returns |
| `Migrate` | `nil` | `func(ctx, *pgxpool.Pool) error`, runs before `mount`; nil means no migration step |
| `ShutdownTimeout` | 5s | how long `Serve` waits for in-flight requests after `ctx` is cancelled |
| `Log` | `slog.Default()` | receives startup and shutdown messages |

`Migrate` stays a plain callback rather than `rest` shipping a migration
runner, on purpose: goose, atlas, a hand-rolled one, or nothing because your
deploy migrates as a separate step — that choice is exactly the kind `Serve`
does not make for you. `Options.DisableTransactions` and the rest of a
resource's own options are unaffected; `Serve` only wraps what sits outside
any one mount.

**What it does not fit.** `Serve` is one pool, one `*Server`, one
`http.Server` on one address. An application that wants two independent
`huma.API`s in one process — a consumer surface and an admin one, each with
its own OpenAPI document — opens the pool and wires both servers by hand, the
way every application did before `Serve` existed.

`sqlb init` scaffolds a `cmd/server/main.go` built on `Serve`, so the fastest
way to see it running is [scaffolding a
project](../start/quickstart.md#or-scaffold-it). This is younger than
`NewServer` — measured byte-identical against a hand-rolled `main` in
`example/tasks2`, but not yet adopted by a second application — so expect
`ServeConfig` to grow fields sooner than it changes shape under you. See
[ADR-0058](../architecture.md#serve-owns-the-boilerplate-mount-is-the-seam).

## What each operation gives you

| Operation | Endpoint | Notes |
|---|---|---|
| `OpList` | `GET /posts` | Filtering, sorting, search, pagination, `?expand` |
| `OpRead` | `GET /posts/{id}` | `?expand` is its only query parameter |
| `OpCreate` | `POST /posts` | Body is `C`; returns the stored row |
| `OpUpdate` | `PATCH /posts/{id}` | Body is `U`; reports its own change set |
| `OpDelete` | `DELETE /posts/{id}` | A real `DELETE` |
| `OpSingleton` | `GET /settings` | The caller's one row, as a bare object |

An operation the schema does not expose has no endpoint — not a 405. That is
also true of the generated [TypeScript client](../typescript/README.md) and
[CLI](../cli/README.md): the function and the subcommand do not exist.

`OpDelete` issuing a real `DELETE` is why a soft-deleting table usually leaves
it out and serves the removal as an update instead; the pair is written out in
[Your first app](../start/first-app.md).

**A collection has one path, and a parent relationship is a filter.** The tasks
of a list are `GET /tasks?list_id=eq.<id>`, not `GET /lists/{id}/tasks` — so
sorting, projection, paging and `?expand` all work on it unchanged, and it is the
same request a capped `?expand` tells a caller to follow for the rest of the
children. The one real cost is that a parent which does not exist yields an empty
page rather than a 404
([ADR-0038](https://github.com/jryannel/sqlb/blob/main/docs/architecture.md#collections-are-flat)).

### A table with one row per caller

Some tables are keyed by the tenant that owns them: a subscription per org, a
settings row per workspace, a profile per user. Both collection shapes are wrong
for those. `OpList` answers a one-element `{items:[…]}` envelope that every
client unwraps forever, and `OpRead` puts the caller's own tenant id in the URL —
a value the server already holds and the hook already enforces, so the segment is
either redundant or a lie and a mismatch is a 404 meaning *you typed your own name
wrong*. Until `OpSingleton` the answer was a permanent hand-written handler beside
an otherwise fully declared module ([#166]).

```go
r.Table("billing_subscriptions",
    schema.UUIDv7("org_id").PrimaryKey().ReadOnly().Scoped(),
    schema.Enum("plan", "free", "pro", "team").Default(schema.Value("free")),
).Expose(schema.REST{
    Path: "/billing-subscription",
    Ops:  schema.OpSingleton | schema.OpUpdate,
})
```

`GET /billing-subscription` answers the caller's row as a bare object, and 404
when they have no row yet. `OpSingleton` removes the `{id}` segment from the whole
resource rather than adding a route beside it, so `OpUpdate` is `PATCH
/billing-subscription` and `OpDelete` is `DELETE /billing-subscription`.
`OpCreate` is `POST /billing-subscription` as it always was.

The row every one of those addresses is **the row the scope hook leaves**: there
is no key in the path and no key predicate in the statement, so a singleton read
compiles to `SELECT … FROM billing_subscriptions` and the hook appends `WHERE
org_id = $1`. That is why the shape is refused on a table with no `Scoped` column
— without the hook the read would answer an arbitrary row and the `PATCH` would
reach every row in the table — and it is why the obligation check treats this read
as its strongest case. A singleton that matches two rows is a 500 rather than a
choice between them.

`OpList` and `OpRead` are refused alongside it: the first is the same route, and
the second is the question the shape exists to delete. Nothing else changes — the
same hooks, the same computed columns, the same `?expand`, and no filter, sort,
page or `?select`, since there is one row and the caller does not choose it.

A singleton needs no primary key at all, which is what lets a table keyed only by
its tenant column be a resource.

[#166]: https://github.com/jryannel/sqlb/issues/166

### Documenting the auth scheme

`Options.Security` puts an OpenAPI security requirement on every operation of a
resource:

```go
rest.Options{Path: "/posts", Ops: rest.CRUD | rest.OpList,
    Security: []map[string][]string{{"bearerAuth": {}}}}
```

It **documents**; it does not enforce — authentication is middleware on your
router and runs whether or not this is set. Leaving it empty produces operations
that are protected and do not say so, which is what every reader of the document
then has to guess about. Declare the scheme itself once on the API:

```go
api.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
    "bearerAuth": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
}
```

The generated clients do not read this, and that is not an oversight: they are
built from the schema rather than from the document, and they take the credential
from the transport your project supplies.

## Request bodies

Codegen emits `PostCreate` and `PostPatch` because two problems need types
rather than reflection.

`PostCreate` omits read-only columns — the database or a `BeforeCreate` hook
owns those — and makes defaulted columns optional, so leaving one out means the
database supplies the value rather than a zero overwriting it. Its `Row()`
method builds the row to insert; returning an error there is a 422, which is
where cross-field validation belongs.

Both halves of "the database or a hook" are live: the handler clears every
read-only field before inserting, so a hand-written `Row()` cannot set one, and
a hook still can. That is what makes a tenant id expressible as a column no
request may name — see [`example/tasks`](../../example/tasks/app/hooks.go),
where four models are stamped that way.

`PostPatch` has every field as a pointer and reports which ones the request
actually carried. A typed struct cannot tell "absent" from "zero", which is the
whole difficulty of PATCH, so the body reports its change set explicitly. An
empty change set is a 400 rather than a no-op update, because it almost always
means the client sent the wrong shape. Immutable columns are absent entirely.

### A body that carries more than the row

Some creates take something that is not a column. A signup carries a password
and the table stores a digest; an invite carries a token that is hashed and
never persisted; a broadcast carries the ids of the companies it goes to, which
become rows of another table. `REST.CreateInput` declares those properties:

```go
Children.Expose(schema.REST{
    Ops: schema.OpCreate | schema.Reads,
    CreateInput: schema.Body(
        schema.Varchar("pin", 4).Comment("Four digits. Hashed on the way in; never stored as sent."),
    ),
})
```

The body is still the columns; this is the part of it that is not one. It is
declared in the same vocabulary an [action's body](actions.md) uses, so it
reaches the OpenAPI document, both generated clients and the CLI — and it
reaches the hook, which is where the derivation belongs:

```go
sqlb.On[Child](reg).BeforeCreate(func(ctx context.Context, c *models.Child) error {
    in, ok := sqlb.CreateInputFrom[models.CreateChildInput](ctx)
    if !ok {
        return errors.New("children: a child is created with a PIN")
    }
    hash, err := bcrypt.GenerateFromPassword([]byte(in.Pin), bcrypt.DefaultCost)
    if err != nil {
        return err
    }
    c.PinHash = string(hash)
    return nil
})
```

Three things about that `!ok`. It is not defensive: `BeforeCreate` runs for
**every** insert of the model, including one from a seeding command or a
background job that never saw a request, and only a request carries a body. It
must fail closed — a hook that shrugs writes the row with an empty digest in the
column that authenticates. And a caller outside the REST path that wants the
same derivation supplies the input itself, with `sqlb.WithCreateInput`.

The property is an input and nothing else: `Row()` does not write it, no
response carries it, and no read can name it. It is spelled on the wire exactly
as declared — `WireCase` is a function of a *column* name, and this is not a
column — and the schema refuses a property that takes a column's name in either
spelling, since the two would be one key in one JSON object.

## Writes run in a transaction

`rest.Resource` wraps every generated create, update and delete in one, which is
what gives a hook a commit to be after and what lets a hook read its own writes
through `sqlb.TxFrom(ctx)`. Reads are left alone, since one `SELECT` is atomic
already.

`Options.DisableTransactions` opts out, and does not disable `AfterCommit` — it
makes every registration fail at request time. Loud rather than silent, which is
the point, but it makes the option a decision about the resource's hooks and not
only about its latency. See [Hooks](../queries/hooks.md#aftercommit-for-side-effects).

## A resource can refuse to mount

A model whose schema declares `Scoped`, or that carries a `SoftDelete` column,
does not mount until a hook confines it. `Register` returns an error naming
every missing registration and the declaration that asked for it.

Serving it instead would answer 200 with another tenant's rows, which is the
quietest wrong answer in the system. See
[Capabilities](../schema/capabilities.md#scoped-so-the-missing-hook-is-caught).

## Next

- [Filtering and search](filtering.md) — the grammar and what it compiles to
- [Pagination](pagination.md) — offset, cursors, and `total`
- [Expanding relations](expand.md) — `?expand`, both directions
- [Actions](actions.md) — a domain verb with a generated envelope
- [Authenticating a request](auth.md) — the identity seam, and the second stage it does not cover
- [A cross-tenant admin surface](admin.md) — releasing a scope, and guarding the route it needs
- [Change events](events.md) — an SSE stream of invalidations, and what it does not promise
- [Webhooks and HTTP callbacks](webhooks.md) — receiving Stripe/Clerk-style callbacks, and sending your own
- [Rejections](errors.md) — what a refusal says and why
- [API compatibility](compatibility.md) — what a schema edit does to the contract
