# Mutations and transactions

```go
post := Post{Title: "Hello", Status: "draft"}
stored, err := sqlb.InsertRows(&post).One(ctx, db)
```

Rows are passed as pointers so hooks and returned database values can be written
back into them. Columns carrying a database default are omitted when their Go
value is the zero value, so a generated uuid comes from the database rather than
being overwritten with `""`. Every statement returns the stored rows, so
generated values land back in your structs without a follow-up read.

That rule reads a zero as "let the database decide", which is right whenever a
column's default agrees with its zero — a generated id, a `created_at`, a
defaulted string. It goes the other way on the one common declaration where the
two disagree:

```go
schema.Bool("active").Default(schema.Value(true))
```

`false` is both "not set" and the interesting state, so an insert leaving it
false wrote `true`. `Explicit` says the zero is meant:

```go
sqlb.InsertRows(&offering).Explicit("active").One(ctx, db)
```

It is `Only`'s effect on that rule without `Only`'s restriction: the columns it
does not name keep the ordinary behaviour, so a `BeforeCreate` hook can still
fill in a column nobody named. Generated creates do this for you — the request
body knows which fields it carried, and it passes them through.

`OnConflictDoNothing(target...)` and `OnConflictUpdate(target, update...)` cover
upserts. A row skipped by do-nothing is simply absent from the result, so the
terminal is `Exec`: an empty slice and a nil error are what "it was already
there" looks like. `One` after `OnConflictDoNothing` is **refused**, because the
only answer it could give a conflict is `ErrNotFound` — a failure reported on
the exact path an idempotent insert exists to serve. If you want the row back
whether or not this call created it, update the conflict target to itself:

```go
sqlb.InsertRows(&p).
    OnConflictUpdate([]string{"idem_key"}, "idem_key").
    One(ctx, db)
```

A write that changes nothing is still a written row, and a written row is a
returned one.

When any row is skipped, **no** caller struct is written back — the returned
slice is shorter than the rows that went in, so position no longer identifies
them, and writing back by position would hand one row's generated id to another.
The returned slice is the account of what was written.

## An upsert that assigns more than the proposed row

Naming a column in `OnConflictUpdate` is shorthand for `col = EXCLUDED.col`.
When that is not the assignment you want, `OnConflictSet` takes an expression:

```go
sqlb.InsertRows(&s).
    OnConflictUpdate([]string{"key"}, "payload").
    OnConflictSet("updated_at", sqlb.Now()).
    OnConflictSet("hits", sqlb.Add(sqlb.Current("hits"), sqlb.Val(1))).
    OnConflictSet("note", sqlb.Coalesce(sqlb.Excluded("note"), sqlb.Current("note")).Expr())
```

```sql
ON CONFLICT ("key") DO UPDATE SET
  "payload"    = EXCLUDED."payload",
  "updated_at" = now(),
  "hits"       = "secrets"."hits" + $4,
  "note"       = coalesce(EXCLUDED."note", "secrets"."note")
```

`sqlb.Now()` is the one that matters most in practice. Computing the timestamp
in Go moves its source from the database clock to the application clock, and
forces the column into the INSERT list so `EXCLUDED` can echo it back — so on a
row whose other timestamps come from Postgres, one column ends up on a different
clock, and under skew they disagree with nothing to report it.

**A column reference has to say which row it means.** Both are in scope inside
`DO UPDATE`: the row being inserted and the row already stored. `Excluded(col)`
is the first, `Current(col)` the second, and a bare `F(col)` is refused rather
than resolved — `count = count + 1` reads like an accumulation whichever one it
picks, so guessing would be choosing silently. Names are checked against the
model on both sides, so a typo is an error from sqlb naming the column rather
than a `42703` from Postgres at request time.

Assignment values are ordinary bind parameters, numbered in the same sequence as
the `VALUES` list.

## When the database refuses a write

A unique index, a foreign key or a check constraint refusing a write is usually
the caller's mistake rather than an outage, so it arrives as a value you can
branch on:

```go
if _, err := sqlb.InsertRows(&loan).One(ctx, tx); err != nil {
    var c *sqlb.ConstraintError
    if errors.As(err, &c) && c.Kind == sqlb.ConstraintUnique {
        return "you already have a copy of that book out"
    }
    return err
}
```

`errors.Is(err, sqlb.ErrConstraint)` is the cheap test for the class.
`ConstraintError.Kind` is always set — `ConstraintUnique`, `ConstraintForeignKey`,
`ConstraintCheck`, `ConstraintNotNull`, `ConstraintExclusion`. The generated REST
handlers use exactly this to answer 409 for a conflict and 422 for the rest,
instead of the 500 an unrecognised error would otherwise become.

`Constraint` — the name of the index that refused — is filled in too, along with
`Table`, `Column` and `Detail`. So is:

```go
var c *sqlb.ConstraintError
if errors.As(err, &c) && c.Constraint == "loans_one_open_per_book_per_borrower" {
    return "you already have a copy of that book out"
}
```

a comparison against a value rather than a `strings.Contains` on a message —
which is what the same code looks like without it, and what no rename survives.

This used to require registering a `SetErrorClassifier` at startup, because the
constraint name is a struct field on the driver's error and sqlb depended on the
standard library alone. Since [ADR-0040](../architecture.md#the-driver-is-a-dependency)
sqlb reads `*pgconn.PgError` itself. If you registered a classifier for that
reason, you can delete it; `SetErrorClassifier` remains for errors that reach
sqlb wrapped past `errors.As`, and for an application that wants its own
mapping.

This is the mechanism behind [where domain logic
goes](../concepts/domain-logic.md): the check constraint is the guarantee, and
the classifier is what turns its refusal into a sentence a caller can act on.

```go
_, err := sqlb.UpdateRows[Post]().
    Set("status", "archived").
    Where(sqlb.F("published_at").Lt(cutoff)).
    Exec(ctx, db)

n, err := sqlb.DeleteRows[Post]().Where(sqlb.F("id").Eq(id)).Exec(ctx, db)
```

An update or delete with no `Where` is **rejected rather than run**:

```
sqlb: statement would affect every row; add a Where clause or call Everything to confirm
```

`Everything()` confirms it when that is genuinely what you meant. `Set` checks
the column name against the model, and the generated `UpdatePost()` wrapper
checks the value types too — worth using, since `Set(string, any)` checks
neither.

## Compute in the database, not in Go

`SetExpr` writes an expression rather than a value, which is how a counter moves
without a read-modify-write:

```go
u.Stmt().SetExpr("view_count", sqlb.Raw{SQL: "view_count + ?", Args: []any{n}})
```

The difference matters under concurrency. `available_copies = available_copies -
1 WHERE id = $1 AND available_copies >= 1` is evaluated by Postgres under a row
lock, so twenty simultaneous requests for the last copy produce one success and
nineteen refusals whose transactions roll back. Reading the row first and
deciding in Go is wrong, and passes every test that runs one request at a time.

## Transactions

`sqlb.New(pool)` returns a `*sqlb.DB`: a handle carrying an executor and the
hook registry its queries resolve against. It satisfies `Executor` itself, so it
goes wherever the pool went.

```go
db := sqlb.New(pool)

err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
    order, err := sqlb.InsertRows(&o).One(ctx, tx)
    if err != nil {
        return err
    }
    _, err = sqlb.UpdateRows[Stock]().
        Set("reserved", true).
        Where(sqlb.F("sku").Eq(order.SKU)).
        Exec(ctx, tx)
    return err
})
```

Commits if `fn` returns nil, rolls back otherwise. A panic rolls back and is
re-raised, so a transaction is never left open by one.

`fn` receives a context carrying the transaction — pass **that** ctx onward, not
the enclosing one, or `TxFrom` will not find it inside your hooks.

Nesting **joins** rather than nests: `WithTx` on a handle already in a
transaction runs `fn` on that same transaction and leaves the commit to the
outermost call, so a function that opens a transaction stays callable from
inside one. Savepoints are the alternative and are a larger promise; nothing
needs them yet.

`WithTxOptions` takes an isolation level. Asking for stricter isolation than an
enclosing transaction already has is an error rather than a silent downgrade.

### Row locking

A transaction on its own does not stop two requests reading the same row,
deciding the same thing and both writing. `ForUpdate` is what does:

```go
stock, err := sqlb.Query[Stock]().
    Where(sqlb.F("sku").Eq(sku)).
    ForUpdate().
    One(ctx, tx)
```

The lock is held until the transaction ends. `ForShare` takes the weaker form,
and `SkipLocked` — valid only with one of the two — steps over rows another
transaction already holds, which is what makes a queue consumer work.

Two rules make the difference between code that passes its tests and code that
survives load.

**Take locks in a consistent order.** Two transactions locking the same rows in
opposite orders deadlock, and the test that would have found it is the one
nobody writes.

**In a hook, take the lock in `BeforeCreate`, not `AfterCreate`.** See
[Locking order](hooks.md#locking-order) — it is invisible in the Go code and
scales with concurrency.

## Sharing the transaction with another library

`Executor` is deliberately two methods, which is what keeps every wrapper and
pool adapter valid — but it means code wanting more than that cannot be handed a
`*sqlb.DB`. `CopyFrom`, `SendBatch` and sqlc's generated `DBTX` all want more.
`DB.Tx()` reaches the underlying `pgx.Tx` so both sides land on one unit of work:

```go
err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
    post, err := sqlb.InsertRows(&p).One(ctx, tx)
    if err != nil {
        return err
    }
    pgTx, ok := tx.Tx()
    if !ok {
        return errors.New("expected a transaction")
    }
    return sqlcgen.New(pgTx).RecordPublication(ctx, post.ID)
})
```

It reports false when the executor is a pool, or a wrapper that does not expose
the transaction it holds.

**Do not commit or roll back the returned `pgx.Tx` yourself.** `WithTx` owns
that boundary, and taking it over leaves the after-commit callbacks unrun — the
one failure mode that looks like success.

### When your code opened the transaction

The other direction needs no `WithTx` at all: a `pgx.Tx` is an `Executor`, so
hand it to `sqlb.New` and sqlb writes join the unit of work you already have.

```go
tx, err := pool.Begin(ctx)
if err != nil {
    return err
}
defer tx.Rollback(ctx)

if err := legacy.DebitAccount(ctx, tx, id); err != nil {
    return err
}
if _, err := sqlb.InsertRows(&entry).Exec(ctx, sqlb.New(tx)); err != nil {
    return err
}
return tx.Commit(ctx)
```

That handle knows it is inside a transaction — `InTx()` reports true and a
`WithTx` on it joins rather than opening a second one — and deliberately does not
take the boundary over. You commit, so `AfterCommit` refuses there rather than
queueing a callback behind a commit sqlb will never perform. Use `WithTx` when
you want that guarantee.

[Using sqlb with sqlc](../with-sqlc.md) covers the pairing in full: who owns the
schema, and which queries go where.

## Next

- [Hooks](hooks.md) — the registrations these statements run
- [Inspecting and tracing](inspecting.md) — seeing the SQL before it runs
- [Migrations](../migrations/README.md) — changing the schema underneath all this
