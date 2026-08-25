# library — a finite resource

> **Where the source is.** This example lives outside the sqlb repository and is not published yet, so there is nothing to link to. The code quoted below is real; the paths are given so they can be found once it is.

A bookstore library anyone can borrow from. Four tables, a catalogue you can
search, a registry of who has what, and one hard invariant: the number of copies
on the shelf must never drift from the loans that are actually open.

[tasks](tasks.md) is about a boundary between tenants; this is the
mirror image — there is **no authentication at all**, and the interesting rules
are about a finite resource instead.

| | |
|---|---|
| `/` | the library — search, filter, browse |
| `/loans` | the registry — who has what, and what is overdue |
| `/docs` | the API, generated from the same schema |

## What it proves

**That a contended resource is decided by one statement, not by Go.**
`books.available_copies` is `ReadOnly` in the schema, so no request can set it.
It moves in exactly two places — `AfterCreate` on `Loan` takes one, the return
endpoint gives one back — and both do it with `available_copies ± 1` computed by
the database.

Nowhere is there a check-then-act. `AfterCreate` issues one conditional
`UPDATE`:

```sql
UPDATE books SET available_copies = available_copies - 1
WHERE id = $1 AND available_copies >= 1
```

Postgres takes a row lock for the duration, so twenty requests for the last copy
are serialised by the database: one matches a row, nineteen match none and their
whole transactions roll back — taking the loan rows they had already inserted
with them. Reading the book first and deciding in Go would be wrong, and would
pass every test that runs one request at a time.

`TestTheLastCopyIsLentExactlyOnce` fires twenty concurrent borrows at a one-copy
book and asserts exactly one 201, nineteen 409s, a count of zero, and exactly
one loan row.

**That a hook is a convention and a constraint is a guarantee.** Behind that
sits `CHECK (available_copies >= 0 AND available_copies <= total_copies)`.
Having both is what makes a lost race a rolled-back transaction rather than a
library that believes it owns −1 copies.

**That both front doors run the same rules.** The HTML pages do not call the API
over HTTP. They build queries with the same handle the generated resources were
mounted on, so borrowing from the book page and borrowing with `curl` run the
identical hooks.

```
HTML pages ─┐
            ├─▶ one *sqlb.DB, hooks attached ─▶ BeforeCreate / AfterCreate ─▶ Postgres
REST API  ──┘
```

## The API

The filter grammar is the reason to use it rather than the pages:

```bash
curl 'localhost:8080/api/books?search=le%20guin&available_copies=gt.0&sort=-published_year&expand=author'
curl 'localhost:8080/api/loans?returned_at=isnull&sort=due_at&expand=book,borrower'   # who has what
curl 'localhost:8080/api/loans?due_at=lt.2026-07-28&returned_at=isnull'                # overdue
curl -X POST localhost:8080/api/loans -H 'content-type: application/json' \
     -d '{"book_id":"…","borrower_id":"…"}'                                            # borrow
curl -X POST localhost:8080/api/loans/{id}/return                                      # return
```

A column that does not declare a capability cannot be reached through it, and
the refusal names what *would* have been accepted:

```bash
curl 'localhost:8080/api/books?sort=description'
# 400 — "column is not sortable", allowed: [title publisher published_year genre shelf]
```

## Two things worth copying

**Capabilities are a privacy decision, not just an API one.** `borrowers.email`
is `Filterable` and deliberately *not* `Searchable`. Exact match is what "find
my own record" needs; a substring match on a public table anyone can read is an
address-harvesting endpoint. `?search=example.com` therefore matches nothing —
not because it is refused, but because the search never sees the column.

**A partial unique index is expressible, and a two-column foreign key is not.**
`UNIQUE (book_id, borrower_id) WHERE returned_at IS NULL` via
`schema.Index{Where: …}` makes borrowing a book you already have out impossible,
and borrowing it again next year ordinary. The `BeforeCreate` hook checks the
same thing first, which is a check-then-act **on purpose**: the index is the
guarantee, and the read only turns an unclassifiable Postgres error into a 409
that says what went wrong. The comment in the hooks file is careful about why
that is acceptable there and was not acceptable for availability.

## And one worth not copying

**`schema.SoftDelete` adds a column and stops.** Nothing writes `deleted_at`,
nothing filters it out of reads, and the generated `DELETE` issues a real
`DELETE`. So `books` does not expose `OpDelete`: the `BeforeQuery` and
`BeforeUpdate` hooks add the predicate, and a hand-written endpoint serves the
removal as an `UPDATE` that also refuses while copies are still out. Both halves
are a few lines and both are visible — but it is two pieces of your own, not one
declaration.

## What it is not

There is no authentication, which is a property of the example rather than an
omission to be embarrassed about — but it means anyone can add a book, borrow on
anyone's behalf, and read the whole registry. Three things would change first if
it needed one: `borrowers` would stop being publicly listable and "my loans"
would come from a token rather than a form field; the return endpoint would
check who is returning; and `POST /api/books` would need a librarian role, which
is a `BeforeCreate` hook reading claims out of the context — the same shape as
[tasks](tasks.md).

Nothing is publicly destructible today, which is the compensating control: the
only delete is the soft withdrawal above, loans are
`OpCreate | OpRead | OpList`, and `ON DELETE RESTRICT` on both of a loan's
references means the registry cannot be erased by removing what it points at.

## Next

- [exchange](exchange.md) — the same question asked of money, which
  is harder in three ways
- [library on sqlc + chi](library-sqlc-chi.md) — the same brief,
  built the other way round
- [Where domain logic goes](../concepts/domain-logic.md) — the four places a
  rule can live
