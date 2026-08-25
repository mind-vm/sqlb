# One grammar, two producers

There is one predicate AST. Hand-written Go produces it, and so does the URL
filter grammar. Escaping, identifier validation, capability checks and hook
application each happen exactly once, on the shared side of that seam.

```
   Go code ────────▶┌─────────────────────────────┐
                    │      predicate AST          │──▶ compiler ──▶ SQL + args
   HTTP query ─────▶│   (sqlb.Pred, sqlb.Expr)    │
     (filter)       └─────────────────────────────┘
                                  ▲
                                  │
                            BeforeQuery hooks
```

The two sides are the same query, written twice:

```go
sqlb.Query[Article]().
    Where(sqlb.F("status").Eq("published"), sqlb.F("views").Gte(100)).
    OrderBy(sqlb.F("views").Desc()).
    Limit(10)
```

```
?status=eq.published&views=gte.100&sort=-views&per_page=10
```

Both compile to:

```sql
SELECT "id", "title", "body", "status", "views", … FROM "articles"
WHERE ("status" = $1) AND ("views" >= $2) ORDER BY "views" DESC LIMIT 10 OFFSET 0
-- args: published 100
```

## Why this is the interesting claim

The obvious alternatives each give up something here.

**Hand-writing the HTTP layer** means every resource re-implements parsing,
validation, allow-listing and pagination. It is high-volume, low-novelty code,
and a good place for security bugs to hide — the one resource where somebody
interpolated a sort column is not visible in a diff of the other twelve.

**Making the database the API** — PostgREST, prest — removes the boilerplate by
removing the Go layer, and with it the place domain logic would have lived. The
whole schema then sits one policy mistake away from being public.

**Generating a handler per resource** would work, and it is not what happens
here. `rest.Resource[T, C, U]` is *one generic function* serving every resource;
what is generated per resource is the OpenAPI document, built from each column's
capabilities. So there is no per-resource handler to review, and no chance of
twelve handlers that agree and a thirteenth that does not.

## What it buys, concretely

**One escaping discipline.** Values are always bind parameters, on both paths.
Identifiers are validated against the model before they reach the compiler and
quoted when they get there. LIKE metacharacters in user input are escaped, so
`?search=50%` searches for the literal string — and so does
`sqlb.F("title").Contains("50%")`, because it is the same code.

**One set of hooks.** A `BeforeQuery` registration constrains a URL-driven read
exactly as it constrains a hand-written one. Tenant scoping does not have to be
re-argued for the generated half.

**One vocabulary, generated outward.** Because the capabilities are data, the
same vocabulary can be emitted as a
[TypeScript client](../typescript/README.md) whose `where` admits only
filterable columns, and as a [cobra command tree](../cli/README.md) whose
`--help` lists a column's operators before a request is sent. Neither is a
second definition of the grammar; both are renderings of the one manifest.

## Where the seam actually is

`filter.Parse` turns a query string into a `filter.Query` — validated against
the model, with values already coerced to Go types. `filter.Apply` writes that
onto a `*sqlb.Builder[T]`. After that call there is no distinction left: the
builder does not know whether its predicates came from a URL or from a Go
function.

That is also the extension point. A hand-written handler can call `Parse` and
`Apply` itself and then add whatever it likes, and a generated resource is
exactly that with nothing added.

## What it costs

The grammar is a fixed vocabulary. It covers comparison, list membership, null
tests, ranges, pattern matching, explicit disjunction, sorting, projection,
search and both kinds of paging — and it does not cover joins the schema did not
declare, arbitrary expressions, or aggregation. Those are Go, or `Raw`.

`?expand` resolves one level: a relation expands to its row, and that row's own
relations do not expand in turn. Nesting is where a depth limit and a cost model
have to be argued for, and neither has been.

## Read next

- [Filtering and search](../rest/filtering.md) — the grammar in full
- [Filter grammar reference](../reference/filter-grammar.md)
  — the operator matrix
- [ADR-0003](../architecture.md#one-ast-two-producers) — the decision record
