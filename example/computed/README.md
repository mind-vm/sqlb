# Derived values: five techniques, and which one to reach for

A project has a due date and two task counters. A list page wants six things the
row does not literally contain: `totalTasks`, `completedTasks`, `openTasks`,
`isOverdue`, `progress %`, and `isStarred`.

Those six are not one problem. They are four kinds of problem, and picking the
wrong technique for one of them is the difference between an index scan and a
subquery per row.

Run it:

```bash
go test ./example/computed/
```

Nothing here needs a database — every test asserts the compiled SQL, which is
what `SQL()` exists for.

## Pick by where the value comes from

| The value depends on | Technique | Filterable | Sortable | Indexable |
|---|---|---|---|---|
| the same row, immutably | `GENERATED ALWAYS AS … STORED` | yes | yes | **yes** |
| other rows | a trigger-maintained counter | yes | yes | **yes** |
| the same row + `now()` | `schema.Computed` | yes | no¹ | no |
| the same row, stably | `schema.Computed` | yes | yes | no |
| **who is asking** | `schema.Computed` + `Needs` | yes | yes | no |

¹ a keyset cursor pages on the sort column, so an expression that reads
`now()` cannot be one — the declaration is refused rather than the request.
Technique 3 is the same SQL written per call site, which is still what a
one-query expression wants; §5 is the declared form and what it buys.

The first two produce ordinary columns. sqlb needs to be told nothing about
them: they are `Filterable`, `Sortable`, and reachable from the REST filter
grammar, the TypeScript client and the CLI exactly like a column somebody typed
in by hand. **If a value can be one of these, make it one of these.** The rest of
this example is about the two rows that cannot.

## 1 · Postgres computes it on write

```sql
open_tasks int GENERATED ALWAYS AS (total_tasks - completed_tasks) STORED
```

`openTasks` is arithmetic over two columns of the same row, so Postgres can keep
it. It is then a real column: `CREATE INDEX ... (org_id, open_tasks)` works, and
`ORDER BY open_tasks DESC` over a million rows is an index scan rather than a
sort of the whole table.

The restriction is the reason `isOverdue` cannot join it: a generated column's
expression must be `IMMUTABLE`, and `isOverdue` reads `current_date`.

There is no `schema.Generated()` yet, so this is a hand-written migration and the
column is declared to sqlb as an ordinary one — which is all it is, from above.

## 2 · A trigger keeps a counter

`totalTasks` and `completedTasks` count rows in another table. Recomputing them
per read is a correlated subquery on every request; a trigger on `tasks` pays the
cost once per write instead, and the columns are again ordinary columns.

This is the technique most often skipped in favour of something cleverer, and it
is usually the right answer. The trade is explicit: writes to `tasks` get slower
and can drift if the trigger has a bug — which is not hypothetical, and the
counter needs a backfill when it happens.

## 3 · Project an expression

```go
sqlb.RawSel("(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)").
    As("is_overdue")

sqlb.RawSel("EXISTS (SELECT 1 FROM project_stars s "+
    "WHERE s.project_id = projects.id AND s.member_id = ?)", viewer).
    As("is_starred")
```

`isOverdue` and `progress` are volatile or cheap enough to evaluate per read.
`isStarred` has no other option at all: whether a project is starred depends on
who is asking, so no column and no view can hold it — there is no *the* answer.

**The `?` is the point.** `RawSel`'s placeholders are renumbered into `$N` along
with every other bind in the statement, so the viewer arrives as a parameter and
cannot become SQL. It also means the fragment composes: the projection's bind,
a predicate's bind and a `?search` term are written at three call sites that
cannot see each other, and only the compiler knows what position each lands in.

`ProjectView` embeds `Project` and adds the three fields, and `sqlb.Collect`
scans into it *exactly* — a field no result column filled is an error naming the
field, not a silent zero.

## 4 · A view

Not shown in code, because it needs no code: `CREATE VIEW` the whole thing, and
`Describe[T]().Table("project_rows")`. The entire generated read path — filter
grammar, cursor paging, TypeScript, CLI — works over it, because sqlb never
asked whether the relation was a table.

Reach for it when the derived set is large enough that the projection stops being
readable. The cost is that writes go somewhere else, so `rest` exposes reads
only, and the view becomes a second place a schema change has to land.

## The ceiling, stated plainly

Here is what the example actually compiles for a starred-and-overdue list:

```sql
SELECT "id", …, "open_tasks",
       (due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0) AS "is_overdue",
       (completed_tasks * 100 / NULLIF(total_tasks, 0)) AS "progress",
       EXISTS (SELECT 1 FROM project_stars s
               WHERE s.project_id = projects.id AND s.member_id = $1) AS "is_starred"
FROM "projects"
WHERE EXISTS (SELECT 1 FROM project_stars s
              WHERE s.project_id = projects.id AND s.member_id = $2)
ORDER BY (due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0) DESC,
         "due_date" ASC
```

Correct, parameterised, and one round trip. Two things are wrong with it anyway:

**The expression is written three times** — once to project it, once to filter,
once to sort — and the viewer is bound twice, as `$1` and `$2`. Nothing keeps the
three copies agreeing.

sqlb can already bind one value once across all three positions: that is what
`sqlb.Near` does for a query vector, which is twenty kilobytes and would
otherwise be sent three times per search. The `sharedValue` behind it is
unexported and reachable only through `Nearness`, so `RawSel` cannot ask for it.

**Nothing above Go knows the field exists.** `is_overdue` is not a declared
column, so it is not in the filter grammar, not in the generated TypeScript
type, not in the CLI's flags, and not in the OpenAPI document. A REST client
cannot ask for `?filter=isOverdue.eq.true` no matter what the SQL can do. That is
not a gap in what sqlb can *express* — the SQL above is proof it can — it is a
gap in what sqlb can *declare*, and the declaration is what the emitters read.

Both of those are what [ADR-0041](../../docs/architecture.md#computed-fields) and
[issue #17](https://github.com/mind-vm/sqlb/issues/17) argued for, and both are
closed below.

## 5 · Declare it

```go
schema.Computed("is_overdue", schema.TypeBool,
    schema.FromSQL("(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)")).
    Filterable()

schema.Computed("is_starred", schema.TypeBool,
    schema.FromSQL("EXISTS (SELECT 1 FROM project_stars s "+
        "WHERE s.project_id = projects.id AND s.member_id = ?)")).
    Needs("viewer").Filterable()
```

`declared.go` is the same three values written this way, and its tests assert
what the declaration buys over technique 3:

- **The expression is written once.** The compiler substitutes it wherever the
  column is named, so `?filter=is_overdue.eq.true`, `?sort=-progress` and the
  projection all render from one string.
- **The viewer binds once.** `Bind("viewer", …)` is `$1` in the projection *and*
  in the predicate — the facility `sqlb.Near` proved worth having for a query
  vector, now reachable by any declared column.
- **Every emitter sees the field.** It is in the row type, the JSON, the
  TypeScript and Dart types, the CLI's column set and the OpenAPI document,
  because to all of them it is a column.
- **An unsupplied bind fails loudly.** `rest.Resource` refuses to mount a
  resource whose `Needs` no `BeforeQuery` hook satisfies, so a per-viewer field
  nobody wired up is a startup error rather than `false` for every row forever.

And what it costs, stated as plainly:

- **No index, ever.** A declared expression is evaluated per candidate row. That
  is why techniques 1 and 2 come first in this file and are not deprecated by
  this one — `Lint` says so too, once per filterable computed column.
- **`Sortable` is refused on a volatile expression.** `is_overdue` reads
  `current_date`, so a keyset cursor paging on it would compare this page's
  boundary against next page's value. `progress` is arithmetic over two stored
  columns and may be sorted.
- **The SQL is unchecked until a query runs.** It is raw text in the schema, so
  a typo surfaces at the database rather than at generate time.
- **An expansion does not carry one.** `?expand=project` joins the target under
  an alias, and a raw fragment cannot be requalified onto it with certainty — the
  same refusal `qualify.go` already makes for `RawPred`. An expanded row carries
  the target's stored columns; its derived ones come from its own endpoint.

Techniques 1, 2 and 4 survive unchanged. Technique 3 remains the answer for an
expression only one query wants, since a declaration is a property of the table.
