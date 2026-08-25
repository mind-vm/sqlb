# Filtering and search

```
?status=eq.published          operator form
?email=alice@example.com      shorthand (a dotted value is not read as an operator)
?age=gte.18&age=lt.65         repeated parameters conjoin
?tag=in.a,b,c                 value lists, quotable: in."a,b",c
?labels=has.urgent            an array column contains this element
?labels=hasany.a,b            overlaps these; hasall.a,b contains all of them
?labels=nhas.urgent           the n-prefixed forms negate: nhasany, nhasall too
?metadata=hasdoc.{"lang":"de"}  a jsonb document contains this one; nhasdoc negates
?starts_at=day.2026-09-01     a whole calendar day of a timestamp column
?deleted_at=isnull            null tests
?views=between.10,20          ranges
?or=(status.eq.draft,age.lt.18)   explicit disjunction, nestable
?and=(…) ?not=(…)             the other two groups; not=(a,b) is NOT (a AND b)
?sort=-created_at,title       "-" for descending; created_at.desc also works
                              (where NULLs go is declared on the column)
?select=id,title              projection (the primary key is always kept)
?search=ada                   fan-out over searchable columns
?page=2&per_page=50           offset pagination, capped by the schema
?cursor=eyJrIjpb…            keyset pagination: resume after a position
```

Values are always bind parameters. Identifiers are validated against the model
before they reach the compiler, and quoted when they get there. LIKE
metacharacters in user input are escaped, so a search for `50%` searches for the
literal string.

Every operator, and which column types accept it, is in the
[filter grammar reference](../reference/filter-grammar.md).

## Array columns take containment, and nothing else

A column declared `.Array()` accepts a vocabulary of its own:

| Request | Means |
|---|---|
| `?labels=has.urgent` | the array contains that one element |
| `?labels=hasany.a,b` | the array overlaps the list |
| `?labels=hasall.a,b` | the array contains every member of the list |
| `?labels=nhas.urgent` | the array does *not* contain that element |
| `?labels=nhasany.a,b` | the array overlaps none of the list |
| `?labels=nhasall.a,b` | the array is missing at least one of the list |
| `?labels=eq.a,b` | the whole array, compared element by element |
| `?labels=isnull` | the column is NULL — which is *not* the same as empty |

The ordering operators, `in` and `between` are refused: Postgres will order
arrays, but that is not an ordering an API should offer, and a list of arrays
has no spelling in this grammar.

The `n`-prefixed forms follow `nin`'s convention, and they exist because a
*per-column* parameter has nowhere to put a `not`: those parameters conjoin, so
negation has to live in the operator. The JSON tree can spell either, and the
two compile to the same statement.

The group parameters are the exception that proves the rule — `?or=(…)` is a
single node carried by one parameter — and `?not=(…)` is one of them. Several
`?not=` on a request conjoin like any group parameter, so two of them are
`NOT A AND NOT B`.

A group is variadic by syntax, so `?not=(a,b)` has to mean something: it reads
as `NOT (a AND b)`, which keeps the conjunction a group already carries and
makes `?not=(…)` the exact inverse of `?and=(…)`. The JSON tree refuses a
second child under `not` instead of choosing, and the difference is deliberate:
there an explicit `and` wrapper costs one node, while here it would cost the
terseness this grammar exists for. The two spell the same set — `?not=(a,b)` is
the tree's `not` over an `and` — so no filter is expressible in one and not the
other.

**A negation is not a complement.** `nhas` is `NOT (…)`, evaluated by SQL's
three-valued logic, so a row whose column is NULL matches neither `has` nor
`nhas` — the same way `nin` already behaves. A caller who wants those rows asks
for them:

```
?or=(labels.nhas.urgent,labels.isnull)
```

Why it works this way, and why the obvious repair — making the group `not`
null-inclusive while the leaf complements stay three-valued — is the one option
that cannot work, is
[ADR-0046](https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#a-negation-is-sqls).
Read it before proposing a change here.

`contains` is refused too, and that one is deliberate rather than incidental.
It is the case-insensitive substring operator for text, and giving it a second
meaning on array columns would put an ambiguity into the one vocabulary whose
whole purpose is that there is none. The refusal names `has` instead, since that
is what a request spelling it that way meant:

```
GET /tasks?labels=contains.urgent
400 — operator "contains" does not apply to the array column labels
      (allowed: eq, has, hasall, hasany, isnull, ne, neq, nhas, nhasall,
       nhasany, notnull)
```

An array column cannot be `Sortable` or `Searchable`, and a `Filterable` one has
to carry a GIN index — `schema.Validate` reports all three. The index is not a
suggestion: an array filter without one still returns the right rows, by
scanning the table for them, so nothing would ever report it
([ADR-0033](https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#array-columns)).

## Document columns take containment, and nothing else

A `jsonb` column is the one place where the useful filter cannot be declared in
advance, because the keys a caller attaches are the point of having it. So it
gets one operator:

| Request | Means |
|---|---|
| `?metadata=hasdoc.{"lang":"de"}` | the document contains that key and value, whatever else it holds |
| `?metadata=nhasdoc.{"lang":"de"}` | it does not |
| `?metadata=isnull` | the column is NULL — not the same as `{}` |

`hasdoc` compiles to Postgres's `@>`, which is subset containment rather than
equality, and it is the operator a GIN index over the column serves. `nhasdoc`
is that under `NOT`, with the same three-valued caveat the array negations
carry — and one of its own worth stating, since containment is not equality:
it excludes a document holding every key given and keeps one holding only some
of them.

It is spelled `hasdoc` rather than `contains` for the reason ADR-0033 gives
about arrays: `contains` is already the case-insensitive substring operator for
text, and a third meaning dispatched on column type is exactly the ambiguity the
generated clients exist to remove. `hasdoc` joins the `has` family, which is how
containment is already spelled here.

There is no bare-value shorthand — `?metadata={"lang":"de"}` is refused, because
the `eq` it would infer is not an operator the column takes — and the ordering
and pattern operators are refused too. Comparing documents by Postgres's
ordering rule means something almost nobody intends, and a pattern would match
against a serialisation whose key order and whitespace are Postgres's to choose;
both would answer, which is worse than refusing:

```
GET /docs?metadata=startswith.x
400 — operator "startswith" does not apply to the JSON document column metadata
      (allowed: hasdoc, isnull, nhasdoc, notnull)
```

The same filter can arrive through the JSON tree, where the document is a value
rather than text — `{"field":"metadata","op":"hasdoc","value":{"lang":"de"}}` —
and binds the identical parameter.

## It is the same builder

Here is that end to end, from `filter/example_test.go`:

```go
values, _ := url.ParseQuery("status=eq.published&views=gte.100&sort=-views&per_page=10")
q, _ := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article]()})
sql, args, _ := filter.Apply(sqlb.Query[Article](), q).SQL()
```

```sql
SELECT "id", "title", "body", "status", "views", … FROM "articles"
WHERE ("status" = $1) AND ("views" >= $2) ORDER BY "views" DESC LIMIT 10 OFFSET 0
-- args: published 100
```

That is the same builder your Go code uses, so a `BeforeQuery` hook applies to
it. Tenant scoping is a startup registration, not something each handler
remembers — and a table that declared `Scoped` does not mount at all until the
registration exists, so it is not something the *schema* has to remember
either. See [Hooks](../queries/hooks.md).

Those two functions are also the extension point. A hand-written handler can
call `Parse` and `Apply` itself and then add whatever it likes; a generated
resource is exactly that with nothing added.

## Search

`?search=` fans out over every `Searchable` column as a disjunction, with the
input escaped:

```sql
WHERE ("title" ILIKE $1) OR ("body" ILIKE $2)
-- args: %50\%% %50\%%
```

A `Searchable` computed column joins the fan-out as an expression, which is how
a search reaches past the row: a chat named in the UI by whoever is in it has no
`name` of its own, so a computed text column rendering the participants' names
is what makes it findable. The resource pays for that subquery only if it
selected the column — see `rest.Options.Computed`.

Which columns join that fan-out is a privacy decision as much as an API one — an
address column that is filterable but not searchable answers "find my own
record" and refuses to answer "who here uses example.com". See
[Capabilities](../schema/capabilities.md#choosing-between-them).

**This is a substring match, not full-text search**, and the distinction is
worth knowing before relying on it. A user who types `ada` matches `Nowlada`; a
user who types `running` does *not* match `run`, because nothing stems, drops
stop words or ranks. That is the right default for a filter box over identifiers
and the wrong one for a search box over prose, and sqlb has the first
([ADR-0037](https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#search-is-ilike-until-it-cannot-be),
which also says why `tsvector` is not in 1.0 and what a schema carrying one does
today).

## Projections cannot leak

`filter.Apply` owns the projection. Given `?select` it uses those columns;
otherwise it projects every non-hidden column — deliberately *not* falling back
to the builder's default of "all mapped columns", because that would put a
`Hidden` column into a response any time a handler forgot to project.

```go
q, _ := filter.Parse(url.Values{}, filter.Options{Model: sqlb.ModelOf[Article]()})
sql, _, _ := filter.Apply(sqlb.Query[Article](), q).SQL()
// SELECT "id", "title", "body", "status", "views", … — internal_note is absent
```

The primary key is always kept, even when `?select` drops it, because an item
that cannot be addressed is not much use to a caller. That is also what lets the
[TypeScript client](../typescript/README.md) narrow a response type on `select`
and still promise the key is there.

If you want a custom projection, apply `Where`, `Order` and the limits from the
`Query` fields yourself rather than calling `Apply`.

## Sorting, and where NULLs go

`?sort=` takes a comma-separated list, `-` for descending, and `created_at.desc`
as the PostgREST-familiar alternative spelling. Only columns declared `Sortable`
are accepted; anything else is a 400 listing the ones that are.

### What a request with no `?sort` gets

`DefaultSort` on `schema.REST` answers it:

```go
Expose(schema.REST{
    Path:        "/posts",
    Ops:         schema.Reads,
    DefaultSort: []string{"-pinned", "-published_at", "-created_at"},
})
```

Without it the answer is primary-key order, which is an implementation detail
rather than something the resource decided. That matters more than it looks: a
list is well-formed in *any* order, so a client that does not restate the real
ordering gets a 200 and the wrong product, with nothing to notice. For a feed —
pinned first, then newest — the ordering is what the collection is, not a client
preference, and putting it in the declaration is what carries it to the OpenAPI
description, the manifest, the generated skill and the ejected handlers at once.

Terms are column names in schema spelling, most significant first, and each must
declare `Sortable` — checked by `sqlb generate` and again at mount, because a
default naming an unsortable column would answer 400 to a client that sent
nothing at all. A request that names `?sort` replaces the default outright rather
than adding to it, and the primary-key tiebreak is appended in both cases, so
cursors work exactly as before.

Where NULLs sit is **declared on the column, not asked for by the request**:

```go
schema.Timestamp("published_at").Nullable().Sortable(schema.NullsLast)
```

The reason it is not a request parameter is that Postgres's default is not one
placement but two — `NULLS LAST` ascending, `NULLS FIRST` descending — so it
flips underneath a column the moment the direction flips. For a column whose
NULLs are incidental that is fine. For one whose NULLs *mean* something it is
not: a NULL `published_at` means "not published", and those rows belong at the
bottom of the feed in either direction. Left to the default, `?sort=-published_at`
lifts every unpublished draft to the top.

So the placement is a property of what the column means rather than of what a
caller wants, and declaring it once is what makes every request get it right —
including the ones a generated TypeScript or Dart client builds, which need no
new syntax for it. A hand-written query says the same thing with
`sqlb.F("published_at").Desc().NullsLast()`; `Description.SortNullsLast` is the
spelling for a hand-written model.

Two consequences worth knowing:

- **it applies in both directions.** `Sortable(schema.NullsLast)` means NULLs
  sort last ascending *and* descending. A placement that only bit on one
  direction would be a rule with an invisible exception;
- **it is part of the ordering a cursor was issued for.** Adding or changing a
  placement invalidates outstanding cursors, which are then refused with the
  ordinary "drop the cursor when the sort changes" error rather than silently
  paging under an ordering they were not built for. `sqlb impact` reports the
  change as breaking for exactly this reason.

## Limits

`MaxPageSize` is a hard ceiling, not a hint — a client asking for more gets the
maximum. `MaxFilters` bounds how many predicates one request may carry, which
bounds the cost of a single query. Both default to the `filter` package's values
when left zero, and both are worth setting per resource in the schema's
`Expose`.

Nesting in a group parameter is bounded too — `?or=(…)`, `?and=(…)` and
`?not=(…)` share the one limit — so a deeply nested group is a rejection rather
than a stack the parser walks.

## Next

- [Pagination](pagination.md) — where `?page` and `?cursor` differ
- [Rejections](errors.md) — what happens when a column has not opted in
- [One grammar, two producers](../concepts/one-grammar.md) — why this is the
  same code as the Go path
