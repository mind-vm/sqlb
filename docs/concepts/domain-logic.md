# Where domain logic goes

Generated CRUD is only useful if the rules that are not CRUD have somewhere to
live. In sqlb that place is **hooks**: registrations keyed by model, run by
every path that touches it — your own code and the generated REST handlers
alike.

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

One registration, and every read of the model is constrained. Multi-tenancy and
soft deletes stop being something each call site has to remember.

## Why the seam is here and not in a handler

The alternative is a hand-written endpoint that applies the rule, and the
problem with it is not that it is more code. It is that **the generated door
stays open beside it**. If `POST /orders/place` reserves funds and
`POST /orders` still inserts a row, the next person to write against orders will
find the second one.

Putting the rule on the model closes both doors with one registration, because
there is only one path to the model. A generated create *is* the placement,
because the hook runs inside it.

Returning an error is how "no tenant in this context" becomes impossible to
forget rather than merely documented: no statement runs at all.

### The seam is under the statement, not around the endpoint

Worth saying once, because it is the difference from every framework this one
gets compared to. PocketBase's rules and a Django permission class attach to the
API boundary and leave direct data access open beneath them; a sqlb hook attaches
to the statement, so the generated handler, the background job, the CLI and the
admin script all pass through it. One registration covers them because there is
no way around it.

The same property has a cost, and it is the one that surprises people: **a hook's
own statements go through the rules too.** From below there is nothing to
distinguish "this UPDATE is the request" from "this UPDATE is a consequence the
request triggered", so a hook writing to another model inherits the request's
confinement unless it says otherwise. That is a real trap with a quiet failure —
[writing the consequence](../queries/hooks.md#writing-the-consequence) is the
worked version — and it is the price of the reach, not a defect in it.

## The four places a rule can live

This is the judgement the examples spend most of their prose on, so it is worth
stating as a set. They are not alternatives — a serious invariant uses several.

**A database constraint** is a guarantee. `CHECK (available_copies >= 0)` cannot
be bypassed by code that has not been written yet. It is the floor under
everything else, and it is the only layer that survives a bug upstream.

**A single conditional statement** is how a contended resource is decided.
`UPDATE books SET available_copies = available_copies - 1 WHERE id = $1 AND
available_copies >= 1` takes a row lock for its duration, so twenty concurrent
requests for the last copy are serialised by the database: one matches, nineteen
match nothing and their transactions roll back. Reading the row first and
deciding in Go is wrong, and would pass every test that runs one request at a
time.

**A hook** is a convention, and it is where the rule is *stated*. It runs inside
the caller's transaction, so returning an error rolls the write back. It is the
right place for scoping, for stamping an owner, for normalising a value, and for
turning an unclassifiable Postgres error into a 409 that says what went wrong.

**A trigger** is for what only the database can see. A rule that depends on what
a column is *becoming* — the old row and the new one at once — cannot be written
as a `BeforeUpdate` hook, because that hook receives the statement rather than
the assignments.

The ordering is deliberate: a hook is a convention, a constraint is a guarantee,
and having both is what makes a lost race a rolled-back transaction rather than
a library that believes it owns −1 copies.

## Which hook, and when it runs

| Hook | Receives | Use for |
|---|---|---|
| `BeforeQuery` | `*Builder[T]` — the query itself | Scoping every read. The load-bearing one |
| `BeforeCreate` | `*T`, and any [declared create input](../rest/README.md#a-body-that-carries-more-than-the-row) through the context | Normalising, deriving, stamping an owner, hashing a secret the request carried |
| `AfterCreate` | `*T`, defaults populated | Validation, and turning an insert into a transition |
| `BeforeUpdate` | `*Update[T]` | Forcing a column, narrowing affected rows |
| `BeforeDelete` | `*Delete[T]` | Narrowing, or refusing |
| `AfterCommit` | — | Anything the outside world can observe |

The write hooks run **inside** the caller's transaction. That is right for
validation — an error rolls the write back — and wrong for anything observable
outside it, which is what `AfterCommit` is for. `AfterCreate` publishing an event
means the transaction can still abort after the hook has already told the world
it succeeded.

`rest.Resource` wraps every generated write in a transaction, so `AfterCommit` is
reachable from a generated handler and a hook can read its own writes through
`sqlb.TxFrom(ctx)`.

## What hooks do not cover

Four gaps, all deliberate and all documented where they bite:

- **`BeforeUpdate` cannot read the assignments it was handed**, so a rule
  depending on what a column is becoming belongs in a `BEFORE` trigger
  ([ADR-0021](../architecture.md#hooks-receive-an-event)).
- **Hooks follow an `?expand` join, with one restriction.** The target's
  `BeforeQuery` predicates are requalified onto the join alias and added to the
  join condition. A predicate that cannot be requalified — `RawPred`, or a
  column from a table the expansion did not join — fails the query rather than
  being dropped silently ([ADR-0030](../architecture.md#declared-scope-is-required)).
- **`AfterCommit` is in-process and at-most-once.** A callback that never ran
  because the process died leaves no trace. That is what a transactional outbox
  is for, and it is not built ([ADR-0012](../architecture.md#change-feed-outbox)).
- **A hook is registered per model; there is no receiver for all of them.**
  `On[T](reg)` is the only way in, so a cross-cutting concern — an audit log, a
  write counter — is one registration per model, written by hand, and a table
  added later is simply absent from it with nothing failing. Django's `signals`
  has the register-for-everything form and this does not.

  The reason is that the concern most people reach for it with is a change feed,
  and there selectivity *is* the design: `example/tasks` publishes three of its
  six models and leaves Users, Workspaces and Memberships out on purpose, each
  with a stated reason, because every model added is a fan-out every subscriber
  pays for. A register-for-everything form would make the cheap thing the default
  and the considered thing the opt-out, which is backwards for a broker.

  That argument does not transfer to an audit log or a metric, where the
  per-model registration carries no per-model decision, so this gap is smaller
  than "we decided against it" and larger than "nobody asked"
  ([#161](https://github.com/mind-vm/sqlb/issues/161)). Until it closes, write
  the loop over an explicit list and let a test assert the list covers every
  exposed table — a failing check beats a remembered convention, which is the
  same answer this codebase gives everywhere else.

## Read next

A hook is where a rule on CRUD is *stated*. Two related seams sit beside it,
for logic a hook does not fit:

- [Actions](../rest/actions.md) — a transition beyond CRUD, with its own route
  and a generated envelope around a plain Go func
- [Mounting resources](../rest/README.md) — `rest.Reads` and hand-written
  endpoints, for writes that stay outside the generated surface entirely
- [Hooks](../queries/hooks.md) — registration, scoping, `AfterCommit`, testing
- [ADR-0008](../architecture.md#hooks-as-domain-seam) — the decision record
- [ADR-0030](../architecture.md#declared-scope-is-required) — why a schema can oblige
  a hook to exist
