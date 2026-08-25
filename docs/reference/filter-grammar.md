# Filter grammar reference

Checked against `filter/filter.go`. The guide page is
[Filtering and search](../rest/filtering.md).

## Shape of a condition

```
?column=operator.value          eq.published
?column=value                   published          — equality shorthand
?column=op.value&column=op.value                   — repeats conjoin
```

The shorthand is why a dotted value is not read as an operator: `?date=2024-01-02`
and `?email=a@b.com` mean equality, because the leading segment is not a known
operator name.

## Operators

| Operator | Meaning | Value form | Accepts |
|---|---|---|---|
| `eq` | `=` | one value | any type |
| `ne`, `neq` | `<>` | one value | any type |
| `gt`, `gte` | `>`, `>=` | one value | any type |
| `lt`, `lte` | `<`, `<=` | one value | any type |
| `in` | `IN (…)` | comma list | any type |
| `nin` | `NOT IN (…)` | comma list | any type |
| `between` | `BETWEEN a AND b` | exactly two, comma-separated | any type |
| `isnull` | `IS NULL` | none | any type |
| `notnull` | `IS NOT NULL` | none | any type |
| `like` | `LIKE`, pattern as written | one value | **text columns only** |
| `ilike` | `ILIKE`, pattern as written | one value | **text columns only** |
| `contains` | `ILIKE %v%`, metacharacters escaped | one value | **text columns only** |
| `startswith` | `ILIKE v%`, metacharacters escaped | one value | **text columns only** |
| `endswith` | `ILIKE %v`, metacharacters escaped | one value | **text columns only** |
| `has` | `$1 = ANY(col)` | one **element** | **array columns only** |
| `hasany` | `col && $1` | comma list | **array columns only** |
| `hasall` | `col @> $1` | comma list | **array columns only** |
| `nhas` | `NOT ($1 = ANY(col))` | one **element** | **array columns only** |
| `nhasany` | `NOT (col && $1)` | comma list | **array columns only** |
| `nhasall` | `NOT (col @> $1)` | comma list | **array columns only** |
| `hasdoc` | `col @> $1::jsonb` | a JSON document | **jsonb columns only** |
| `nhasdoc` | `NOT (col @> $1::jsonb)` | a JSON document | **jsonb columns only** |
| `day` | `col >= $1::date AND col < $2::date + 1` | a calendar date, `2026-09-01` | **date and timestamp columns only** |

"Text column" means the Go field is a string kind, pointer or not. A pattern
operator on any other column is rejected with
`operator "contains" needs a text column, but views is int64`.

### A whole day

`?starts_at=day.2026-09-01` matches every row whose timestamp falls on that
calendar date, in the database session's time zone — the same rows
`starts_at::date = '2026-09-01'::date` selects, by a half-open range an index on
the column can serve.

It exists because equality cannot ask the question. A bare date is midnight, and
a stored timestamp is almost never exactly midnight, so `?starts_at=eq.2026-09-01`
returned zero rows, a 200, and nothing to notice. That spelling is now refused:

```
?starts_at=eq.2026-09-01
400: starts_at is a timestamptz, so "2026-09-01" compares against midnight on
     that date and matches almost nothing; ask for the whole day with
     starts_at=day.2026-09-01, or give a full timestamp such as
     starts_at=eq.2026-09-01T09:00:00Z
```

The refusal covers `eq`, `ne`, `in` and `nin`, which are the comparisons a date
makes silently wrong. The ordering operators are untouched:
`?starts_at=gte.2026-09-01` means "from midnight onwards", which is what it says.
A `date` column is untouched too — there, equality against a date is exact — and
so is a column whose type the model does not declare, since a hand-written model
need not say what its columns are and an unknown type is not evidence of a
mistake.

### Array columns

An array column — one declared `.Array()`, whose Go field is a slice — accepts a
disjoint set from a scalar one:

| | |
|---|---|
| Accepts | `has`, `hasany`, `hasall`, `nhas`, `nhasany`, `nhasall`, `eq`, `ne`/`neq`, `isnull`, `notnull` |
| Refuses | `gt`, `gte`, `lt`, `lte`, `between`, `in`, `nin`, and every pattern operator |

`has` binds one **element**, so `?labels=has.urgent` is coerced to the element's
Go type and compared as a scalar. `hasany`, `hasall`, `eq` and `ne` take the
same comma-separated, quotable list `in` does, and bind it as a whole array.

The `n`-prefixed forms are the negations, taking the same operands as the
operators they negate. They follow `nin`'s spelling, and they exist because a
query string cannot nest a `not`: repeated parameters conjoin, so negation has
to live in the operator. A JSON filter tree can spell either form.

A negation is **not** a complement. Each compiles to `NOT (…)`, evaluated by
SQL's three-valued logic, so a row whose column is NULL matches neither `has`
nor `nhas` — as with `nin`. Reaching those rows is a second condition:
`?or=(labels.nhas.urgent,labels.isnull)`.

An empty list keeps the meaning its positive has: `nhasany` of nothing excludes
nothing, and `nhasall` of nothing excludes every row, since every array contains
the empty one.

`contains` is **not** array containment. It is the text substring operator, and
overloading it by column type would put an ambiguity into the one vocabulary
whose whole purpose is that there is none. A request that spells it that way is
rejected with the operators that would have worked:

```
operator "contains" does not apply to the array column labels
  (allowed: eq, has, hasall, hasany, isnull, ne, neq, nhas, nhasall, nhasany,
   notnull)
```

`isnull` keeps meaning something here, because a NULL column and an empty array
are different values. An array column can never be `Sortable` or `Searchable`,
so it appears in neither `?sort` nor the `?search` fan-out.

`isnull` and `notnull` are accepted on **any** filterable column at the server;
nothing checks that the column is nullable. The generated
[TypeScript client](../typescript/README.md) and [CLI](../cli/README.md) are narrower — they
offer a null test only where the schema says the column is nullable — so the
looser server behaviour is only reachable by hand-written requests.

An unknown operator is rejected with the full list of operator names.

### Value lists and quoting

```
?tag=in.a,b,c
?tag=in."a,b",c        a value containing a comma
?views=between.10,20
```

Splitting ignores separators inside parentheses and double quotes, so a grouped
or quoted value survives. `between` with anything other than two values is
rejected by count.

### Value coercion

Values are coerced to the model's Go type before they reach the compiler, so
nothing downstream sees a string:

| Go type | Accepted |
|---|---|
| `string` kinds | as written |
| `bool` | anything `strconv.ParseBool` accepts |
| signed integer kinds | base 10, parsed at 64 bits |
| unsigned integer kinds | base 10, non-negative, parsed at 64 bits |
| float kinds | parsed at 64 bits |
| `time.Time` | `RFC3339Nano`, `RFC3339`, `2006-01-02T15:04:05`, or `2006-01-02` |
| anything implementing `encoding.TextUnmarshaler` | delegated to it — this is what covers `uuid.UUID` and similar wrappers |

Anything else — a `[]byte` or a `json.RawMessage` column, say — is rejected with
`values of type … cannot be used in a filter`, so a `jsonb` column is queryable
from Go and not from a URL.

`time.Time` is checked before the `TextUnmarshaler` branch on purpose: its own
`UnmarshalText` accepts RFC 3339 only, which would reject the plain dates a date
range is usually written with.

## Grouping

```
?or=(status.eq.draft,age.lt.18)
?and=(status.eq.draft,or(a.eq.1,b.eq.2))
```

Conditions inside a group are written `column.operator.value`. Groups nest, and
`or(…)` and `and(…)` may appear as items inside one. Repeated `?or=` parameters
conjoin with each other and with the plain conditions.

Nesting deeper than **3** levels is rejected.

There is **no `not(…)`** in the query string. Negation here is spelled by the
operator — `ne`, `nin`, `notnull`, and the `n`-prefixed containment forms — which
is why those operators exist. A request needing to negate a whole group sends a
JSON filter tree instead.

### The JSON filter tree

`?filter=` carries a filter as JSON, which is the only form that can nest a
negation. A node is a group (`and`, `or`, `not`) or a leaf condition:

```json
{"op":"not","children":[
  {"op":"or","children":[
    {"op":"eq","field":"status","value":"draft"},
    {"op":"isnull","field":"published_at"}
  ]}
]}
```

`and` and `or` take one or more children; **`not` takes exactly one**. Several
conditions are negated by wrapping them in an `and` or `or` group first, which
keeps the tree from having to be read for a convention about whether
`not` over a list means `NOT (a AND b)` or `NOT a AND NOT b`.

The operator vocabulary is the one above, so the spellings are `ne`, `nin` and
`isnull`. A tree and the equivalent query string compile to the identical
statement. The tree is bounded at **4** levels of nesting and **64** nodes, and
its conditions charge the same `MaxFilters` budget the query string does — a
request cannot exceed it by splitting conditions across the two forms.

The same filter may also be sent on its own, in a `POST` body, via
`filter.ParseFilterTree`.

## Reserved parameters

These never name a column:

| Parameter | Meaning |
|---|---|
| `select` | Projection. The primary key is always kept, even if omitted |
| `sort`, `order` | Ordering, most significant first |
| `search` | Substring fan-out over every `Searchable` column |
| `expand` | Relations to resolve inline. `Apply` performs the join, so a parsed `?expand` is never silently dropped — a name that is not expandable is a 400 listing the ones that are |
| `page`, `per_page` | Offset paging |
| `limit`, `offset` | Offset paging, the explicit form |
| `cursor` | Keyset paging |
| `count` | `count=exact` adds `total`, at the cost of a second query |
| `or`, `and` | Explicit groups |
| `filter` | A JSON filter tree, the one form that can nest a `not` |

Any other parameter that does not name a filterable column is rejected as an
unknown parameter rather than ignored.

### Sort

```
?sort=-created_at,title
?sort=created_at.desc,title.asc
```

`-` prefix and `.desc`/`.asc` suffix both work. A direction that is neither is
rejected naming both. Only `Sortable` columns are accepted; the primary key is
appended automatically unless the sort already contains it, so every list is
totally ordered.

### Search

Fans out over every `Searchable` column as a disjunction, with LIKE
metacharacters in the input escaped. A resource with no searchable column
rejects `?search=` with `no column of this resource is searchable` rather than
silently matching nothing.

## Limits

| Limit | Default | Bounds |
|---|---|---|
| `DefaultPageSize` | 25 | Page size when the request does not ask |
| `MaxPageSize` | 200 | Page size a client may request |
| `MaxFilters` | 24 | **Leaf conditions**, counting the ones inside `or=`/`and=` groups |
| `MaxSortTerms` | 4 | Terms in `?sort` |
| `MaxListValues` | 100 | Values in one `in`/`nin` list |
| `MaxValueLength` | 256 | Bytes in one filter value or search term |
| `MaxGroupDepth` | 3 | Nesting of `or=`/`and=` groups |
| `MaxOffset` | 100 000 | Rows `?page=`/`?offset=` may skip |

The first three are per-resource in `schema.REST`; the rest are package
constants, overridable on `filter.Options`.

Four of them exist because the obvious budget is the wrong one:

- **`MaxFilters` counts leaf conditions, not parameters.** Counting top-level
  parameters would leave the budget open to a single group holding as many
  conditions as the client cared to write.
- **`MaxListValues`** — a list is one condition against `MaxFilters` however
  long it is, so without a separate bound `?id=in.1,2,3,…` is one parameter, one
  predicate, and a bind parameter per member until the driver's 65535 run out.
- **`MaxValueLength`** — the pattern operators pass their operand through
  unescaped on purpose, so a value is a lever on how much work a scan does, and
  a long one is a cheap way to pull it.
- **`MaxOffset`** — every other dimension of a request costs what it says, but
  an offset costs the rows it skips: `?page=50000000` asks Postgres to produce
  and discard ten billion rows for a page of twenty-five. Cursor paging has no
  such cost, but it is opt-in per request, so it is not a bound.

A page size above the maximum is **capped rather than rejected** — a client
asking for too much gets the maximum. The others *are* rejections, naming the
count and the limit: `%d filters requested, the limit is %d`,
`operator %q was given %d values, the limit is %d`,
`value is %d bytes, the limit is %d`.

`page` below 1 and a negative `offset` are both rejected, as is one past
`MaxOffset` — that rejection names the budget and points at `?cursor=`, which
reads deeper for free.

A reserved parameter that means one thing per request — `sort`, `search`,
`select`, `filter`, `cursor`, `page`, `limit` and the rest — is rejected when
sent twice rather than reading the first and dropping the rest. `or=` and `and=`
are the exceptions: several of either is a request with several groups.

## Cursors

`?cursor=` cannot be combined with `?page=` or `?offset=`, and the rejection
says which to drop. A cursor is valid only for the ordering it was issued under;
using one against a different `?sort=` is a 400 naming both orderings.

See [Pagination](../rest/pagination.md) and
[ADR-0027](../architecture.md#keyset-pagination).
