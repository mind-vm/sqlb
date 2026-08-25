# Hooks

Hooks are where domain logic lives. Register once at startup, typically from
`init` or `main`; they run in registration order, and one returning an error
aborts the operation with the error reaching the caller unwrapped.

[Where domain logic goes](../concepts/domain-logic.md) argues why the seam is
here. This is how to use it.

## BeforeQuery is the load-bearing one

It receives the query itself, so **one registration constrains every read of the
model** — including the reads the generated REST handlers issue.

```go
sqlb.On[Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Post]) error {
    org, ok := auth.OrgFrom(ctx)
    if !ok {
        return auth.ErrNoTenant
    }
    q.Where(sqlb.F("org_id").Eq(org), sqlb.F("deleted_at").IsNull())
    return nil
})
```

Multi-tenancy and soft deletes stop being something each call site has to
remember. This is also why REST registration is generic over the model rather
than reflective: hooks are keyed by type, and a reflective dispatcher could not
run them.

Returning an error is how "no tenant in this context" becomes impossible to
forget rather than merely documented — no statement runs at all.

The hook amends a clone, on the exec path, so `q.SQL()` on the builder you built
does not show what it added. `q.Resolved(ctx, db)` does — reach for it when the
predicate has to be read as *text*, for a raw statement that must count the same
rows or for a test asserting the scope is in force. See
[Inspecting](inspecting.md#resolved-which-renders-the-statement-that-runs).

### A rule cannot reach through a subquery

Sooner or later a confining rule needs a column the table does not carry — a
transcript turn scoped by the session's owner, where only the session knows who
that is. The obvious shape is a nested query, and sqlb refuses it:

```go
sqlb.On[TutorMessage](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[TutorMessage]) error {
    // refused: a nested SELECT does not run TutorSession's own hooks
    q.Where(sqlb.F("session_id").InQuery(
        sqlb.Query[TutorSession]().Select(sqlb.F("id")).Where(...),
    ))
    return nil
})
```

The refusal is the same one any nested query over a confined model gets, and for
the same reason: a nested SELECT is compiled, not run, so the inner query would
be unconfined exactly where its absence is invisible. Elsewhere the fix is to
resolve the inner query first with `Resolved(ctx, db)`. Inside a hook that is not
available — a hook is handed the query and no executor — so the refusal names the
two fixes that are:

- **Denormalise the column onto the confined table** and make the rule a plain
  predicate. Usually the better schema anyway: it is the same argument that put
  the tenant column there in the first place.
- **Register the rule on the other model**, so its reads are confined where they
  are issued rather than where they are referenced.

### When one surface is the exception

A `BeforeQuery` confines every reader of the model, which is the point of it and
occasionally the problem. A storefront and an admin panel read the same
`products`, and the admin panel exists to see the drafts the storefront's rule
hides.

Name the rule, and one handle can be released from it:

```go
sqlb.On[Product](reg).Scope("storefront").BeforeQuery(publishedOnly)

storefront := sqlb.New(pool).WithHooks(reg)
admin      := storefront.WithoutScope("storefront")
```

An ordinary `BeforeQuery` has no name, and `WithoutScope` cannot reach it
whatever it passes. Naming a rule is what makes it negotiable, so the decision
sits with whoever wrote the rule rather than with whoever would like to be out
of it — and a release does not get a resource past the obligation check below,
which is asked of the handle that will actually serve the requests. See
[capabilities](../schema/capabilities.md#the-other-half-rows) for the mount
side, `rest.Options.Unscoped`.

## Say it in the schema, so the missing hook is the one that is caught

The hook above cannot be forgotten at a call site. It can be forgotten
entirely, and an unscoped model serves every tenant's rows with a `200` next to
them. So the table declares what it expects:

```go
schema.Ref("org", Org).Filterable().ReadOnly().Scoped()
```

`Scoped` writes no predicate — it is inert in exactly the way `SoftDelete`'s
column is. What it does is oblige the resource: `rest.Resource` refuses to
mount a model whose declarations no hook satisfies, and names every missing
registration at once. The obligation follows the operations, because a
`BeforeQuery` hook says nothing about what a request can overwrite by id — an
exposed update needs `BeforeUpdate`, a delete needs `BeforeDelete`, and a
create needs `BeforeCreate` to supply the tenant column that `ReadOnly` kept
out of the request body.

The check proves a hook exists, not that it is right. That is worth knowing
before relying on it, and it catches the case that actually happens: the table
somebody added last week. [ADR-0030](../architecture.md#declared-scope-is-required)
has the reasoning, including why the predicate is not generated for you.

[`example/tasks/app/hooks.go`](../../example/tasks/app/hooks.go) is this taken
as far as it goes: one file, a little over two hundred lines, confining six
models across twenty-five endpoints. Two details there are worth stealing. The
scoping is one generic function used four times rather than four near-copies,
which is only possible because every table in that schema names the column
`workspace_id` — a convention kept deliberately so the boundary can be written
once. And reads and writes are scoped by *separate* registrations, because a
`BeforeQuery` predicate constrains what a request can see and says nothing about
what it can overwrite by id.

## The rest

| Hook | Receives | Use for |
|---|---|---|
| `BeforeCreate` | `*T` | Normalising an email, deriving a slug, stamping an owner |
| `AfterCreate` | `*T`, with defaults populated | Validation |
| `BeforeUpdate` | `*Update[T]` | Forcing a column, narrowing affected rows |
| `AfterUpdate` | `[]T` | Validation |
| `BeforeDelete` | `*Delete[T]` | Narrowing, or refusing |
| `AfterDelete` | `int64` | Validation |
| `AfterDeleteRows` | `[]T`, as they were | Anything needing the row's identity |

`AfterDelete` and `AfterDeleteRows` are two hooks rather than one because the
rows are not free. A `Delete` is write-only for predicates, so a `BeforeDelete`
cannot ask what a statement addresses and the rows have to come back from the
statement itself — which means `DELETE … RETURNING` and a scan of everything it
matched. sqlb adds that clause only when an `AfterDeleteRows` hook is registered
for the model, so a delete whose rows nobody reads still costs one command tag.

Register the rows form when a count is not enough, which in practice means
publishing anything: an event that says *how many* posts were deleted and not
*which* is worse than no event, because the subscriber invalidating a cache
keyed on the row has nothing to key on and the feed looks wired up.
[`rest.PublishChanges`](../rest/events.md) uses it for exactly that.

All of these run **inside** the caller's transaction. That is right for
validation — an error rolls the write back — and wrong for anything the outside
world can observe.

The write hooks are narrower than `BeforeQuery`: they receive the row or the
statement rather than a handle. They can still reach the database, but only
where a transaction is — `rest` wraps every generated write in one, so
`sqlb.TxFrom(ctx)` finds it and a hook can query, as
[Reading your own writes](#reading-your-own-writes) below shows. On a read, or
under `Options.DisableTransactions`, there is nothing to find.

### What fires when, and inside which transaction

Four questions decide whether a domain invariant holds, and none of them is
answerable from a signature. Stated here so they need not be answered by
reading the source.

**Every write path fires the hooks, not just the generated ones.**
`sqlb.InsertRows(&a, &b).Exec(ctx, tx)` runs `BeforeCreate` on each row and
`AfterCreate` on each stored row, exactly as `POST /posts` does. A hand-written
HTML form handler and a generated REST handler enforce the same rules without
either knowing the other exists, and that is the property the whole arrangement
is for.

**`AfterCreate` receives a pointer into the returned rows, so it can change
the response.** Mutating `*T` there changes what the caller gets back and what
`rest` writes to the wire — which is how a generated `POST /orders` can answer
with the fill it just executed rather than with the order as submitted. This
makes `AfterCreate` a good deal more than the "validation" its row in the table
above suggests.

**A defaulted column holding its zero value is omitted from the insert.** So
the database supplies it rather than a zero overwriting it, and a `BeforeCreate`
that copies one column into another falls out correctly in the zero case
without a special case. This is why "has a default" and "is optional in the
create body" are the same question.

**Hooks reach the transaction.** `WithTx` hands `fn` a `*sqlb.DB` carrying the
same registry, so hooks fire on statements issued inside it, and `TxFrom(ctx)`
resolves *within* a hook — a `BeforeCreate` can read what earlier statements in
the same unit of work have written but not yet committed.

The gap that remains is narrower and deliberate: `BeforeUpdate` cannot read the
assignments it was handed, so a rule that depends on what a column is *becoming*
belongs in a `BEFORE` trigger.
[ADR-0021](../architecture.md#hooks-receive-an-event) records why the event types
that would have closed it are not being built.

What has landed from that record is the transaction: `rest.Resource` wraps every
generated create, update and delete in one, so `AfterCommit` is reachable from a
generated write. Set `Options.DisableTransactions` to opt out, and read the next
section before you do.

## An insert can mean something

`AfterCreate` running inside the write's transaction is what lets a generated
`POST` be a domain operation rather than an insert. The handler decodes a body,
validates it and inserts a row, and knows nothing about the rule; the hook turns
that insert into a placement — reserve, match, write the consequence — in the
same transaction, so a refusal rolls the row back with it.

That is why a schema modelled this way has no "rejected" status: an operation
that could not be performed is not a row, it is a 422.

The alternative is a hand-written `/orders/place`, which would work and would be
a **second door**: the generated create would still exist, and the next person
to write against the model would insert rows that reserved nothing. Closing both
doors with one registration is the argument for hooks in a sentence.

### Writing the consequence

"Write the consequence" is the step the signature does not show. A hook receives
its own model and nothing else — no `*DB`, no `Executor`, no application handle —
so read `AfterCreate(func(context.Context, *Order) error)` cold and the
reasonable conclusion is that a hook can amend the statement it was given and
nothing more. It is not, and the door out is `sqlb.TxFrom(ctx)`: the `ctx` that
looks like plumbing is carrying the transaction the write is running in.

Here is the checkout in full. Placing an order decrements the shop's stock, in
the same transaction, so a refusal rolls the order back with it:

```go
sqlb.On[Order](reg).AfterCreate(func(ctx context.Context, o *Order) error {
    tx, ok := sqlb.TxFrom(ctx)
    if !ok {
        return errors.New("orders must be placed in a transaction")
    }
    // system, not tx. See below.
    updated, err := sqlb.UpdateRows[Stock]().
        SetExpr("count", sqlb.Raw{SQL: `"count" - ?`, Args: []any{o.Qty}}).
        Where(sqlb.F("sku").Eq(o.SKU)).
        One(ctx, tx.WithHooks(system))
    if err != nil {
        return err   // ErrNotFound rolls the order back: nothing to sell.
    }
    if updated.Count < 0 {
        return fmt.Errorf("%w: %s is oversold", ErrConflict, o.SKU)
    }
    return nil
})
```

Two things in that block are the whole of it.

**`tx.WithHooks(system)`, not `tx`.** `TxFrom` hands back the handle the *request*
is running on, and that handle carries the request's rules. `Stock` is
`Scoped`, so `rest.Resource` obliged a `BeforeUpdate` hook confining stock rows
to their owner — and running this statement through the request's registry
appends the *buyer's* scope to the *shop's* inventory write:

```sql
UPDATE "stocks" SET "count" = "count" - $1 WHERE ("sku" = $2) AND ("shop_id" = $3)
--                                                              ^ the buyer
```

Against a real database that matches nothing. The rule written to confine a
request has silently confined the domain logic that was supposed to act past it.
So the escalated write runs on a handle carrying a *different* registry — the
second one from [Scoping and tests](#scoping-and-tests) — which is what makes the
statement unscoped and says so at the call site. `tx.Tx()` gives the raw `pgx.Tx`
and resolves to no registry at all, which is the same thing spelled without a
registry to name.

But `system` is not the answer for every cross-model write, and reaching for it
by reflex removes confinement that was doing work. What makes the checkout need
it is that the two models are scoped on *different axes*: an order is the buyer's,
a stock row is the shop's, and no predicate about one says anything true about
the other. Where both sides share an axis the inherited scope is the point.

[`example/tasks`](../../example/tasks/app/hooks.go) is that case. A comment's
`AfterCreate` bumps its task's `comment_count` through plain `tx`, and
`scopeWrites[tasks.Task]` appends `workspace_id = <the caller's>` to that update —
which is exactly right, because a comment and its task are in one workspace, and
a bump that could reach outside it would be a bug. Escalating there would drop a
confinement nobody would notice was gone until the counter on another tenant's
task moved.

So the question to ask is not "is this hook writing to another model" but "is the
request's scope a true statement about the row I am about to touch". Same axis:
`tx`. Different axes: `tx.WithHooks(system)`, and the reason belongs in a comment
because the next reader cannot recover it from the types.

**`One`, not `Exec`.** `Exec` is the natural spelling for a decrement — you want
the effect, not the row — and it is the one that goes quiet: zero rows is
`([]T{}, nil)`, the hook returns nil, and the transaction commits with an order
that reserved nothing. `One` answers `ErrNotFound` on zero rows, which rolls the
write back. Prefer it in a hook whenever "this changed nothing" is a reason to
refuse (#159).

Both mistakes are invisible in the Go code and in review, which is why this
section exists rather than a sentence about `TxFrom`.

**Why a hook's own statements are subject to the same rules at all.** Hooks here
run at the *statement* layer, not at the API layer. That is what makes one
registration cover the generated handler, the background job and the admin script
alike — and it is the same property that puts a hook's own writes inside the
rules, because from below there is nothing to distinguish them. Frameworks that
confine at the API boundary (PocketBase's rules, a Django permission class) leave
DAO access open, so a hook there reaches the database unconfined by default and a
background job reaches it unconfined too. The trade is real in both directions;
this is the side sqlb takes, and `tx.WithHooks(system)` is the escape hatch it
costs.

## AfterCommit, for side effects

Publishing an event, enqueuing a job, invalidating a cache: none of these may
happen if the write does not. `AfterCreate` running inside the transaction means
the transaction can still abort after the hook has already told the world it
succeeded.

```go
sqlb.On[Order](reg).AfterCreate(func(ctx context.Context, o *Order) error {
    id := o.ID
    return sqlb.AfterCommit(ctx, func(ctx context.Context) error {
        return events.Publish(ctx, OrderPlaced{ID: id})
    })
})
```

Callbacks run in registration order once `Commit` returns nil, and not at all if
it rolls back. The context they receive carries no transaction — there is
nothing left to join, and handing back a committed one would be a trap.

A failing callback does not stop the others; the failures are joined under
`ErrAfterCommit`. That sentinel matters, because the two cases need opposite
responses:

```go
if err := db.WithTx(ctx, placeOrder); err != nil {
    if errors.Is(err, sqlb.ErrAfterCommit) {
        // The order exists. Something downstream of it did not fire.
        log.Error("order placed, notification failed", "err", err)
    } else {
        return err // The order does not exist.
    }
}
```

Outside a transaction, `AfterCommit` is an error rather than an immediate call:
under autocommit sqlb cannot say when the commit happened, so the callback would
fire before the insert or after it depending on which hook called it.

**From a generated handler there is always a transaction**, because
`rest.Resource` opens one per write. The two ways to end up without one are a
write you issue yourself outside `WithTx`, and a resource that set
`Options.DisableTransactions`. The second is worth stating plainly: turning it on
does not disable `AfterCommit`, it makes every registration fail at request time.
That is loud rather than silent, which is the point — but it means the option is
a decision about the resource's hooks, not only about its latency.

This is in-process and at-most-once. A callback that never ran because the
process died leaves no trace. For the change feed that is what the transactional
outbox is for — the event is written by the same transaction as the row, so a
process that dies between the commit and the fan-out has already recorded it
([`outbox`](../rest/events.md#the-outbox),
[ADR-0012](../architecture.md#change-feed-outbox)). For anything else the same
shape is yours to build: record the intent as a row in the writing transaction,
and dispatch it from a worker that tails the table.

Deleting a file when its row goes is this seam's other common use, and the
ordering it forces is worth reading before writing one:
[`example/attachments`](../../example/attachments) removes the object *after*
the commit, because object storage is not in the transaction and a roll-back
would otherwise leave a row pointing at bytes that are gone.

## Reading your own writes

A hook that needs to see rows written earlier in the same transaction must read
through the transaction handle. Reading through the pool would miss them,
because they are not committed yet:

```go
sqlb.On[Post](reg).BeforeCreate(func(ctx context.Context, p *Post) error {
    tx, ok := sqlb.TxFrom(ctx)
    if !ok {
        return errors.New("posts must be created inside a transaction")
    }
    n, err := sqlb.Query[Post]().Where(sqlb.F("slug").Eq(p.Slug)).Count(ctx, tx)
    …
})
```

A check-then-act like that is only sound when something else has the last word.
Where the guarantee is a unique index, the read exists to turn an unclassifiable
Postgres error into a 409 that names the problem — which is a good reason. Where
there is no constraint underneath, two concurrent requests will both pass the
check; see [where domain logic goes](../concepts/domain-logic.md).

### And it narrows every write that is not a generated one

The hook above works for free behind every generated handler, because
`rest.Resource` wraps each create, update and delete in a transaction. It does
not work behind a plain `sqlb.InsertRows(&p).Exec(ctx, db)` from a worker, a
cron job or a hand-written endpoint: there is no transaction on that context,
`TxFrom` returns `false`, and the hook refuses.

**The refusal is data-dependent**, which is what makes this worth a section.
A hook that only reaches for `TxFrom` on the branch that validates a reference
lets every row through until the first row that carries one — so the write path
is green in tests, green in staging, and fails on a Tuesday against production
data. Two consumers have now written that sentence down in a comment beside a
helper they had to invent
([#275](https://github.com/mind-vm/sqlb/issues/275)).

Nothing states the requirement at the call site, the registration site, or at
compile time — a hook is free to need something the handle does not provide,
and the need lives in the hook body. So there are two halves to keeping it
legible, and both are on you today:

**Open the transaction at the call site.** A direct write against a model whose
hooks read is a unit of work, and `WithTx` is one line:

```go
err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
    return sqlb.InsertRows(&p).Exec(ctx, tx)   // ctx carries tx, so TxFrom resolves
})
```

Note the `ctx` the closure receives, not the outer one: that is the one carrying
the transaction. Passing the outer `ctx` compiles and leaves `TxFrom` right back
where it started.

**Make the refusal name the hook**, so the failure is one grep rather than one
afternoon:

```go
tx, ok := sqlb.TxFrom(ctx)
if !ok {
    return huma.Error500InternalServerError(
        "Post.BeforeCreate validates list_id and needs a transaction; " +
            "wrap this write in db.WithTx")
}
```

What is deliberately *not* offered is a `TxFrom` that falls back to the bare
executor when there is no transaction. That would turn a validating read into
an unconfined one — the hook would answer from outside the tenant's rules, and
quietly — which is worse than the error it replaced.

## Locking order

A hook is also where a lock is taken deliberately, and **it has to be taken in
`BeforeCreate`, not `AfterCreate`**. This one is invisible in the Go code.

Inserting a row takes a `FOR KEY SHARE` lock on every row its foreign keys
reference — Postgres checking the reference, not anything you wrote. Key-share
locks are shared, so two concurrent inserts both get them; if each then tries to
upgrade the same referenced row to `FOR UPDATE` inside `AfterCreate`, each waits
for the other's share lock. That is a guaranteed deadlock, it scales with
concurrency, and it surfaces as a 500 naming a statement you did not write:

```
ERROR: deadlock detected (SQLSTATE 40P01)
  on: SELECT ... FROM "stocks" WHERE "id" = $1 LIMIT 2 FOR UPDATE
```

Taking the exclusive lock in `BeforeCreate` — before the row exists, so before
the key-share lock is taken — fixes it, and costs one line of ordering.

The other rule is to **take locks in a consistent order**: two transactions
locking the same rows in opposite orders deadlock, and the test that would have
found it is the one nobody writes.

Both are easy to state and impossible to see, which is why they are worth being
a named function with the explanation attached rather than a line inside a
handler. `ForUpdate`, `ForShare` and `SkipLocked` are on
[Mutations and transactions](mutations.md#row-locking).

## Scoping and tests

`On[T](r)` registers into the registry you hand it, and `db.WithHooks(r)` is
how a handle acquires it. There is no process-wide registry to fall back on
([ADR-0047](../architecture.md#no-default-hook-registry)) — a handle built by
`sqlb.New` starts with an empty one of its own, so the rules in force are a
property of how the handle was assembled rather than of what ran first.

A test therefore gets isolation for free: build a registry, attach it, and
there is nothing to tear down. Two tenants' worth of differing domain rules
coexist in one process the same way.

One consequence worth knowing: an `Executor` that is not a `*sqlb.DB` — a raw
pool, a borrowed `pgx.Tx` — carries no registry, so a statement issued against
one runs unconfined. That is why models whose rows must not be read unscoped
declare `Scoped`, which refuses the mount rather than trusting the call site
([ADR-0030](../architecture.md#declared-scope-is-required)).

### The second registry is not only for tests

It is a normal part of assembling an application, and this repo has arrived at it
twice: `example/tasks` builds `sys` beside its hooked handle, and `example/fxapp`
provides `fxkit.Unscoped` as a distinct type so `grep -r 'fxkit.Unscoped'` lists
every consumer. Three reasons to want one, in the order an application meets
them:

1. **Reads that happen before there is a tenant.** Sign-in has to find the user
   before it knows which workspace to scope to. `example/tasks` uses `sys` for
   exactly two endpoints, and for nothing else.
2. **A hook writing past the scope of the request that triggered it.** The
   checkout above: the buyer's rules must not narrow the shop's inventory write.
   This is the one an application hits on day one and the one with the quietest
   failure, which is why it has [its own
   section](#writing-the-consequence).
3. **Isolation between suites and between tenants**, which is where a test starts
   and is the cheapest of the three.

Two values, one of which never leaves its own file, is harder to misuse than one
handle and a "skip the hooks" flag — a flag is something a caller can pass, and
the set of callers allowed to pass it is the whole question. That is why there is
no `db.Unscoped()`: the answer to "who may escalate" should be readable from the
wiring, not from every call site that happens to hold a handle.

### A background worker releases one rule, not all of them

The second registry above is the blunt instrument, and it is the right one for
the three cases it names. There is a fourth case that looks like them and is
not, and reaching for `sys` there costs more than it should.

An agent, a queue consumer or a scheduled job usually acts as a **synthetic
principal** — `Claims{Subject: "system", Workspace: msg.WorkspaceID, Role:
RoleOwner}` — precisely so its writes go through the same hooks a request's do.
That is the right instinct. It breaks on the one hook whose rule is
*identity-shaped* rather than tenant-shaped: a read scoped by "is this subject a
member" has nothing to ask about `"system"`, and the failure is a 500 from
whatever the subquery does with a subject that is not a user id.

The cheap answer is to route that one call through an unhooked handle. It is
also the wrong one: the boundary is being bypassed by hand to escape a single
rule, and the bypass drops *all* the rules — a tenant-scoped read now issued
with nothing confining it. One consumer arrived at four structs each carrying a
`{Sys, Hooks}` pair, where which handle a call site holds is convention,
invisible to the type system and to review
([#276](https://github.com/mind-vm/sqlb/issues/276)).

**Name the rule at registration instead, and release that one at the handle.**
A scope name is why `Scope` exists:

```go
// Registration: the identity-shaped rule gets a name; the tenant-shaped one
// does not, because nothing should ever be released from it.
sqlb.On[Workspace](reg).Scope("membership").BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Workspace]) error {
    claims, ok := sqlb.PrincipalFrom[Claims](ctx)
    if !ok {
        return errors.New("no principal on this context")   // fail closed
    }
    q.Where(sqlb.F("id").InQuery(
        sqlb.Query[Membership]().
            Select(sqlb.F("workspace_id")).
            Where(sqlb.F("user_id").Eq(claims.Subject)),
    ))
    return nil
})
sqlb.On[Card](reg).BeforeQuery(scopeToWorkspace)   // unnamed: absolute

// Wiring, next to the registry so the release is visible in one place.
requests := db.WithHooks(reg)
worker   := db.WithHooks(reg).WithoutScope("membership")
```

The worker handle still runs every workspace rule, every soft-delete rule and
every audit stamp. What it does not run is the one question that has no meaning
for a principal that is not a user. And the release is a property of how the
handle was assembled — `grep -rn WithoutScope` lists every rule anyone is
allowed to escape, which is the readable-from-the-wiring property the section
above is arguing for.

Two things this does not get you. A model declared `Scoped` whose *every*
`BeforeQuery` has been released still refuses to mount
([ADR-0030](../architecture.md#declared-scope-is-required)) — releasing is not a
way past the obligation check, it is a way to satisfy it with the other rules.
And `WithoutScope` alone is not a route guard; [a cross-tenant admin
surface](../rest/admin.md) is the version with the guard attached.

Reach for the second registry when there is no principal at all — sign-in, a
migration, a test fixture. Reach for a named scope when there *is* one and a
single rule cannot answer for it.

### The create side is not releasable, and what to write instead

`Scope` covers `BeforeQuery`, `BeforeUpdate` and `BeforeDelete` — the three that
narrow which rows a statement addresses. `BeforeCreate` is deliberately absent:
it stamps a row on the way in rather than confining a set, so a create that
skipped it would write a row with *no* tenant rather than see more of them. A
released read fails visibly; a released stamp fails silently, and the row is
still there tomorrow.

The consequence is that every create goes through a hook that wants the
request's claims — including the creates that have no request. A fixture, a
seed, an import, a job that materialises a row
([#289](https://github.com/mind-vm/sqlb/issues/289)) all arrive at the same
place:

```
tenant_test.go:114: seeding a child: auth: no family in context
```

The pattern that works is a fallback in the hook itself, and the important half
is that it fails closed:

```go
sqlb.On[Child](reg).BeforeCreate(func(ctx context.Context, row *Child) error {
    familyID, err := auth.FamilyID(ctx)
    if err != nil {
        // No claims: a fixture, a seed, an import, a job. Trust the row only
        // if it named a tenant itself.
        if row.FamilyID != "" {
            return nil
        }
        return err   // no claims and no tenant is still refused
    }
    if !isParent(ctx) {
        return errParentOnly
    }
    row.FamilyID = familyID   // stamp; never trust the body when there is a caller
    return nil
})
```

Read the two branches as one rule: *the tenant comes from the claims when there
are claims, and from the row only when there are none, and never from nowhere.*
Returning `nil` unconditionally in the no-claims branch is the mistake this
shape exists to avoid — it writes rows with no tenant, which is precisely what
keeping `BeforeCreate` out of `Scope` is protecting.

A trusted caller then needs no special handle at all: it supplies the tenant on
the row, and the same registry serves it. That is a smaller escalation than a
second handle, because the thing being trusted is one field of one row rather
than every rule at once.

## Next

- [Inspecting and tracing](inspecting.md) — seeing what a hook did
- [Mounting resources](../rest/README.md) — the handlers these hooks reach
- [Capabilities](../schema/capabilities.md) — `ReadOnly` plus a hook, and
  `Scoped`
- [A cross-tenant admin surface](../rest/admin.md) — a released scope, plus
  the route guard `WithoutScope` alone does not give you
