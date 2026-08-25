# library on sqlc + gin

> **Where the source is.** This example lives outside the sqlb repository and is not published yet, so there is nothing to link to. The code quoted below is real; the paths are given so they can be found once it is.

The same brief as [library](library.md) and
[library on sqlc + chi](library-sqlc-chi.md), built a third time.
The SQL, the view and the invariant are the chi build's; what differs is the
router, and the three things gin asks for.

Read the [chi build](library-sqlc-chi.md) first — this page covers
only what is different.

## What the constraint is actually worth

The chi page claims that the check constraint backs up the guard. This build
measured it. Deleting the `available_copies > 0` guard from `TakeCopy` and
running the race test gives **one 201 and nineteen 500s** — the check constraint
catching what the guard no longer does.

So: the guard is what makes the nineteen a civil 409; the constraint is what
makes them safe either way. That is the clearest statement of "a hook is a
convention, a constraint is a guarantee" anywhere in these examples, because it
is the one that was run rather than asserted.

## A curiosity about the injection test

`TestSortIsAnAllowList` fires `?sort=title; DROP TABLE books --` at the
catalogue and gets a 400 naming the five sorts that exist. Checking that by
hand turned up something worth knowing: sent with a **raw** semicolon, the
parameter never arrives at all — Go's `url.Query()` rejects `;` as a separator
and drops the pair, so the catalogue sorts by its default and answers 200. It
takes `%3B` to get the string as far as the allow-list.

Both roads are safe. Only one of them is safe for a reason this code chose,
which is the distinction worth keeping.

## What sqlc asks for, beyond the chi build

**The occasional cast.** `is_overdue` is
`l.returned_at IS NULL AND l.due_at < now()`, which cannot be null and which
sqlc types as `*bool` anyway, because nothing in the column types tells it
otherwise. `coalesce(…, false)::bool` is what turns that back into a `bool` the
templates can use without a nil check.

**One struct, not two.** Because `WithdrawBook` is the only statement that names
the underlying table and it is `:execrows`, `omit_unused_structs` leaves the
`Book` struct unemitted entirely. Every book-shaped value in the application is
a `store.LiveBook`, and there is no second type to reach for by autocomplete.

**A count that agrees with its list.** `TestTheCountAgreesWithTheList` pages
through eight different filter combinations two rows at a time and asserts that
what arrives equals what the count claimed. That is the guard against the real
cost of the static-SQL approach: `CountBooks` repeats `SearchBooks`'s `WHERE`
clause and the two have to be edited together.

## What gin is doing here

Not much, which is the point — but two things were worth writing down.

**Templates are parsed one set per page.** `LoadHTMLGlob` parses every file into
a single namespace, which works right up until two pages both define `content`,
and then the last one parsed silently wins for all of them.
`internal/web/render.go` implements gin's `render.HTMLRender` in about fifteen
lines, parsing the layout separately with each page, so every page gets its own
namespace and a new page cannot break an existing one.

**The route table is flat and static-vs-parameter conflicts are designed out.**
Donating is `/donate` rather than `/books/new`, so nothing sits beside
`/books/:id` in the same position of gin's tree.

## Two API details worth stealing

**`available_copies` is absent from every request body on purpose.**
`TestAvailableCopiesCannotBeSetByARequest` sends it anyway, twice, and asserts
it is ignored and then refused. This is the hand-written equivalent of marking
the column `ReadOnly` in a sqlb schema — the same guarantee, restated per
endpoint.

**`PATCH` takes `total_copies` as a pointer**, so that a body which does not
mention it changes nothing and a body which says `0` is refused. Binding into an
`int32` would silently turn the first into the second. That is exactly the
problem sqlb's generated `PostPatch` solves by making every field a pointer and
reporting the change set — here it is solved once, by hand, for one field.

**Errors have two audiences.** `web.statusFor` maps domain sentinels to status
codes; its sibling `web.message` decides what a caller is allowed to *read*.
Everything `statusFor` recognises is a rule this application chose to state, so
its text is the answer; everything else is a 500, and a 500's text is a wrapped
Postgres error with table and constraint names in it, which goes to the log
instead.

## What this is not

There is no authentication, which is a property of the example rather than an
omission — but it means anyone can add a book, borrow on anyone's behalf, read
the whole registry, and withdraw a title nobody is holding. Three things would
change first: "my loans" would come from a token rather than a form field; the
return endpoint would check who is returning; and the write endpoints would need
a librarian role, which in this shape is middleware on the `/api` group plus a
claim read in `internal/library`.

`TestWithdrawIsRefusedWhileCopiesAreOut` checks both halves of the soft delete:
that the book disappears from the catalogue, the book page and the borrow
endpoint, and that the registry still remembers it.

## Next

- [library](library.md) — the same brief on sqlb
- [library on sqlc + chi](library-sqlc-chi.md) — the second build
- [How sqlb compares](../comparisons.md) — where each approach is the
  right one, including when not to use sqlb
