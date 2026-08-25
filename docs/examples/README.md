# Examples

Seven worked applications. Each one exists to settle a different question, and
each says out loud what it deliberately is *not*.

Three of them live in this repository and can be read line by line. The other
four are separate projects; their pages here describe what they do and quote the
parts worth stealing, but there is no source to link to yet.

## Start with these

### blog — the smallest whole thing

A worked schema, everything codegen emits from it, the two hand-written
pieces generated code cannot produce, and an assembled server. It is a real
test suite, so it cannot drift from the code.

[Read it →](blog.md)

### tasks — the whole surface

A multi-tenant task manager: six tables, JWT auth, a workspace boundary held
entirely by hooks, a migration history, a generated TypeScript client and
CLI, and a runnable server tested against real Postgres.

[Read it →](tasks.md)

## The harder questions

### fxapp — the wiring

The same pieces assembled by uber-go/fx: the hook registry built from a
value group, "the migrations have run" expressed as a type, and a resource
that refuses to mount when nobody registered the hooks its declared scope
needs — asserted by booting the broken composition.

[Read it →](fxapp.md)

### library — a finite resource

A public lending library. No authentication at all; the interesting rule is
that the copies on the shelf must never drift from the loans that are open.
Twenty concurrent borrows of the last copy decide whether the design is
right.

[Read it →](library.md)

### exchange — money, both ways

A stock exchange with prices that move on a clock. The invariant is
two-sided, the commitment outlives the request, and the price is not an
input. Every balance change in the system is in one file.

[Read it →](exchange.md)

## The same domain, built the other way

Two builds of the library above using hand-written SQL, `sqlc` and a router —
one with chi, one with gin. They are here because the honest comparison is not a
feature table; it is the same brief solved twice, with the costs of each visible.

### library on sqlc + chi

Static SQL with `parameter IS NULL OR …` guards, a `live_books` view with
`WITH CHECK OPTION` doing what a hook would have done, and the count query
that has to be edited alongside the search.

[Read it →](library-sqlc-chi.md)

### library on sqlc + gin

The same again, plus what gin asks for: per-page template namespaces, a flat
route table, and a pointer-typed PATCH field so that "absent" and "zero" stay
different.

[Read it →](library-sqlc-gin.md)

## What they prove, side by side

| | Auth | The hard part | Where the rule lives |
|---|---|---|---|
| [blog](blog.md) | none | A soft delete needs two halves | A hook, plus a hand-written endpoint |
| [tasks](tasks.md) | JWT | A tenant boundary across 25 endpoints | One hooks file, and a composite foreign key where hooks cannot reach |
| [fxapp](fxapp.md) | a key per tenant | Registering the hooks before anything is mounted | A value group, so the handle cannot exist until every module has contributed |
| [library](library.md) | none | The last copy is lent exactly once | One conditional `UPDATE`, under a check constraint |
| [exchange](exchange.md) | none | Cash and shares move together, or not at all | An `AfterCreate` hook that turns an insert into a placement |
| [library-sqlc-chi](library-sqlc-chi.md) | none | Composition, given up on purpose | Hand-written SQL, and a view with `WITH CHECK OPTION` |
| [library-sqlc-gin](library-sqlc-gin.md) | none | The same, with the framework's own traps | The same, plus `render.HTMLRender` |

All but `blog` are tested against a real Postgres in a container, with no
skip-when-Docker-is-absent path — because a suite that passes silently when it
cannot reach a database reports coverage it does not have, and most of what
they claim is only checkable by a database: that the last copy is lent once
under contention, that a refused write leaves no row, that a trigger fires.

`blog` is the exception and deliberately so. It runs against an in-memory
`database/sql` driver, which keeps it part of the fast inner loop — it is
proving what codegen emits and how the pieces assemble, not what Postgres does
under concurrency.
