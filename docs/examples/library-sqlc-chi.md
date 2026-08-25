# library on sqlc + chi

> **Where the source is.** This example lives outside the sqlb repository and is not published yet, so there is nothing to link to. The code quoted below is real; the paths are given so they can be found once it is.

Same domain as [library](library.md), built the other way round:
there, a schema DSL generates the queries and the REST layer; here the SQL is
written by hand, `sqlc` types it, and `chi` routes it.

This exists so the comparison is a real one. The interesting part of both builds
is the same single invariant, and reading them side by side is more honest than
any feature table.

## The invariant, in both builds

`books.available_copies` must equal `total_copies` minus the loans that are
still open. None of it reads a value and then decides in Go:

```sql
-- TakeCopy: the whole concurrency story for borrowing, in one statement
UPDATE books
SET available_copies = available_copies - 1
WHERE id = $1 AND deleted_at IS NULL AND available_copies > 0
RETURNING *;
```

That is the same shape sqlb's version produces from a hook. Postgres evaluates
the predicate under a row lock, so twenty simultaneous requests for the last
copy produce one loan and nineteen refusals — asserted by
`TestTheLastCopyIsLentExactlyOnce`, which is the test the design exists to pass.

Three more places carry the same weight:

- **Borrowing is one transaction.** The copy comes off the shelf, then the loan
  row is inserted. Inserting second means the partial unique index
  (`loans_one_open_per_book_per_borrower`) gets the last word on borrowing a
  book you already hold — and because it is the same transaction, that refusal
  rolls the decrement back with it. A refused loan costs the library nothing.
- **`CloseLoan` says `WHERE returned_at IS NULL`.** That is what makes the
  Return button safe to double-click: the second press matches nothing and is
  told the loan is already closed, rather than stamping a new date and crediting
  a copy that was never lent.
- **The database backs both up.** `copies_are_accounted_for` refuses a count
  outside `[0, total_copies]`.

One rule genuinely cannot live in Go, and it is a trigger: when the library
acquires a second copy of a title, something has to make it borrowable. Only a
trigger sees the old row and the new one at once, so only a trigger knows by how
much `total_copies` moved. That is true in both builds.

## What sqlc asks of you

The trade is visible in `SearchBooks`. Every filter the catalogue offers is
written out with a `parameter IS NULL OR …` guard, so the SQL is static, sqlc
can type it, and the plan is the same shape every time:

```sql
WHERE b.deleted_at IS NULL
  AND (sqlc.narg('genre')::text IS NULL OR b.genre = sqlc.narg('genre')::text)
  AND (NOT sqlc.arg('available_only')::bool OR b.available_copies > 0)
```

**What you give up is composition**: a filter this file does not name cannot be
added by a handler. For a catalogue with a fixed set of facets that is the right
side of the trade — and it comes with a property worth having, which is that
sorting from a query parameter is expressed as
`CASE WHEN @sort = 'title' THEN b.title END` rather than as a column name spliced
into `ORDER BY`. The injection site that survives every other precaution is not
reachable, because sqlc will not generate a query it cannot parse.
`TestSortIsAnAllowList` fires `?sort=title; DROP TABLE books --` at it.

**The honest cost is on the next query down.** `CountBooks` repeats
`SearchBooks`'s `WHERE` clause, and the two have to be edited together. Every
search test asserts that the count agrees with the list, which is what catches
it when they are not.

## Where the soft delete lives

sqlc has no hooks, so a `deleted_at IS NULL` written per query would be one
chance per query to forget it — and forgetting is quiet, a withdrawn book
reappearing in one view and not the others. So it is in the schema instead:

```sql
CREATE VIEW live_books AS
    SELECT * FROM books WHERE deleted_at IS NULL
    WITH CHECK OPTION;
```

Every query but one reads and writes `live_books`. It is a plain single-table
view, so Postgres makes it auto-updatable and the planner rewrites rather than
materialising anything.

`WITH CHECK OPTION` is what makes it a boundary rather than a convenience: a
write through the view may not produce a row the view cannot see. So withdrawing
a book — the one operation whose purpose is exactly that — must name the
underlying table, which means there is precisely one statement in the codebase
that can soft-delete, and it says so. `TestTheCatalogueViewIsABoundary` tries to
withdraw through the view and asserts Postgres refuses.

Two things it costs. `CREATE VIEW … SELECT *` expands the star at creation time,
so a column added to `books` later needs the view recreated. And sqlc names an
embedded struct after its relation, so rows arrive as `LiveBook` with the tag
`live_book`; one mapping type exists so that a schema decision does not end up
naming a JSON field.

**This is the honest comparison with sqlb's hook.** A view is a stronger
enforcement point than a `BeforeQuery` registration — the database refuses,
rather than the application remembering to ask. What it cannot do is take the
tenant out of a request context, which is what a hook is for. Neither mechanism
subsumes the other.

## Layout

```
db/migrations/     the schema (tables, triggers, the live_books view), applied by
                   goose and read by sqlc — one description, not two
db/query/          the hand-written SQL
internal/store/    sqlc's output, plus a pool, a transaction helper, error predicates
internal/library/  the rules: Borrow, Return, Search, Registry, AddBook, Withdraw
internal/web/      chi router, server-rendered pages, JSON API
cmd/server/        main
```

`internal/library` is where the two front doors meet. The pages do not fetch the
API over HTTP and the API renders nothing; both call the same methods, so
borrowing from the form and borrowing with curl run the same code and produce
the same refusals. `TestBorrowingFromTheFormAndFromTheAPIAreTheSameOperation`
checks that they even say the same sentence.

Errors are sentinels in `library` (`ErrNoCopies`, `ErrAlreadyBorrowed`, …)
mapped to status codes by exactly one function, `web.statusFor`. The domain
package does not know it is behind HTTP.

## The API, for comparison

```
GET    /api/books                 ?q= &genre= &shelf= &available=1 &sort= &page= &per_page=
POST   /api/books                 donate a title
GET    /api/books/{id}            the book, who is holding the copies, recent returns
PATCH  /api/books/{id}            {"total_copies": n} — the only field a request may move
DELETE /api/books/{id}            withdraw; refused while copies are out
POST   /api/books/{id}/borrow     {"name": …, "email": …}
GET    /api/loans                 ?status=out|overdue|returned|all &email= &book_id=
POST   /api/loans/{id}/return
```

A fixed set of named facets, versus sqlb's
`?search=…&available_copies=gt.0&sort=-published_year&expand=author`. That is
the trade in one line: this API is exactly as expressive as somebody wrote it to
be, and no more, which is both the cost and the point.

`available_copies` is absent from every request body on purpose: there is no
request that can set it, which is what makes it unable to disagree with the open
loans.

## Two decisions worth arguing with

**The registry's email filter is an exact match, not a substring search.** Exact
match answers "find my own loans", which is what the box exists for. A substring
search over an address column on a page anyone can load is an address-harvesting
endpoint — `@gmail.com` would return the list. Addresses are stored lower-cased
so that exact match still recognises somebody who capitalised theirs
differently.

**Withdrawing a book is a soft delete, and it is refused while copies are out.**
The loans reference the book with `ON DELETE RESTRICT`, because a loan is the
record that a physical object left the building; deleting the book would either
fail on the foreign key or destroy the history.

## Next

- [library](library.md) — the same brief on sqlb
- [library on sqlc + gin](library-sqlc-gin.md) — the third build
- [Using sqlb with sqlc](../with-sqlc.md) — how to run both in one
  project, sharing one transaction
- [How sqlb compares](../comparisons.md) — the same argument without
  the code
