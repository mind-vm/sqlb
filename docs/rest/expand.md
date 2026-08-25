# Expanding relations

`schema.Ref("list", List).Expandable()` makes a reference reachable inline, on
the collection and on a single row alike:

```
GET /tasks?expand=list
GET /tasks/{id}?expand=list
```

```json
{
  "id": "01937...",
  "list_id": "01936...",
  "list": { "id": "01936...", "name": "Backlog", "color": "#6b7280" }
}
```

The key stays where it was — expansion adds the row, it does not replace the
reference — and the relation is named `list`, not `list_id`: the parameter names
the relation, the column keeps its own name.

It is one statement, a `LEFT JOIN` and a `json_build_object` over the target's
columns. Not two queries: the batched `WHERE id IN (…)` alternative runs at a
later snapshot, so a row can vanish between the two and a caller gets a null
expansion for a reference the database still holds.

`Hidden` survives the join. The target's columns are listed explicitly rather
than taken with `row_to_json`, so a hidden column of the expanded table is as
absent from an expansion as it is from the table's own responses — otherwise
`?expand` would be a way to read a column the resource refuses to serve.

Codegen wires all of it: the relation field on the model, and the resource's
`Expandable` list. Nothing here is hand-written.

## The other direction

A reference can also be expanded backwards — a list and the tasks that point at
it — and that is declared on the same line, because the referencing table is the
one that already owns the column:

```go
schema.Ref("list", List).Filterable().Expandable().
    Inverse("tasks").
    InverseExpandable(schema.ExpandOrder("position"), schema.ExpandLimit(20))
```

Read as: a task has a list; a list has tasks; both may be expanded. The name has
to be declared rather than derived, because two references to one table — an
author's posts and the posts an author reviewed — would derive the same name for
different sets. Absent `Inverse` there is no reverse relation, which is normal.

```
GET /lists?expand=tasks
```

```json
{
  "id": "01936...",
  "name": "Backlog",
  "tasks": {
    "items": [{ "id": "01937...", "title": "Write the migration" }],
    "has_more": false
  }
}
```

The value is an envelope, not a bare array, and the reason is `has_more`. A
collection is capped — 20 above, 50 by default — because an uncapped one makes
one response's size a function of data nobody bounded, and an array that was
silently truncated is a wrong answer rather than a short one. Past the cap the
caller follows the child's own endpoint, filtered by the same key:

```
GET /tasks?list_id=eq.{id}&sort=position&page=2
```

which is why that column wants to be `Filterable` — `schema.Lint` says so when
it is not, along with reporting an unindexed foreign key, which matters more
here than in the forward direction.

Under the covers this is one correlated subquery per relation rather than a
join: joining a collection would multiply the base rows, so the page's row count
would depend on the data, and two expanded collections would multiply each
other. It is still one statement, so the snapshot argument above is unchanged,
and `Hidden` holds over the collected rows exactly as it holds over a joined one.

Ordering is declared and always total — the child's primary key is appended as a
tiebreaker — because under a cap the order does not merely arrange the result,
it decides which children the caller never sees.

## What is refused, and what is free

A relation the schema did not mark expandable is refused with the list of the
ones that would have worked, and an unexpanded request pays for no join at all.
Both endpoints produce the same rejection, because both go through the same
parser rather than through two hand-written checks.

`?expand` is the item endpoint's only query parameter, and it is absent on a
resource that declares no relation — asking for it there is an unknown
parameter, not a silently ignored one. `POST` and `PATCH` return the row they
wrote without expansions; fetch the relation with a `GET` if you need it.

Expansion resolves **one level**. A relation expands to its row; that row's own
relations do not expand in turn, and there is no `?expand=list.workspace`. One
level is a join per relation and a bounded statement; nesting is where a depth
limit and a cost model have to be argued for, and neither has been.

An expanded row carries every column of the target that is not `Hidden`, and a
request cannot ask for fewer. That is deliberate: the wire shape of an expansion
is derived from the schema, and a client choosing which keys come back would
make one endpoint answer with rows of varying shape
([ADR-0039](../architecture.md#a-schema-edit-is-an-api-edit)).

In Go, where the caller is the application rather than a client, `ExpandOnly`
narrows it:

```go
sqlb.Query[Task]().ExpandOnly("list", "name")   // {"name": …} rather than the whole list
```

It only ever removes columns. `Hidden` stays hidden and a computed column stays
absent — both are refused by name rather than skipped — and a collection's cap
and ordering stay where the schema declared them, since those are what stop a
response's size being a function of data nobody bounded.

## An embedded row is the same shape as a direct one

An expanded row is built by Postgres, with `json_build_object`, rather than
marshalled by Go — which is what keeps `Hidden` honest across the join, and
which is also the one place the two paths could drift apart. They must not: a
client holding a generated type for the target decodes it the same way wherever
it arrived from.

Date columns are where that took work. Postgres serialises a `date` as
`"2026-07-01"`, and the Go field for it is a `time.Time`, which parses strictly
as RFC 3339 — so an expansion over a date column used to fail the decode
outright ([#84](https://github.com/mind-vm/sqlb/issues/84)). The value is cast
to UTC midnight on the way out, so it arrives in the same RFC 3339 form a direct
read produces and both generated clients already document receiving.

The cast is spelled `::timestamp AT TIME ZONE 'UTC'` rather than `::timestamptz`
on purpose: `::timestamptz` resolves through the session's `TimeZone`, so under
`Europe/Berlin` the date `2026-07-01` would come back as `2026-06-30T22:00:00Z`
and the column would lose a day.

## Hooks follow the join

A `BeforeQuery` hook registered on the target model **does** run for an
expansion. Its predicates are rewritten onto the join alias and added to the
join condition, so a tenant scope or a soft-delete filter registered against
`List` confines the `list` an expanded task carries, exactly as it confines
`GET /lists`:

```sql
LEFT JOIN "lists" AS "__ex_list"
       ON "__ex_list"."id" = "tasks"."list_id"
      AND "__ex_list"."workspace_id" = $1      -- List's own BeforeQuery hook
```

The rewrite is what makes this safe rather than merely present. A hook writes
`sqlb.F("workspace_id")` — a bare column, because the query it was written for
has one table — and a bare name inside a join resolves to the *parent* table. So
each predicate is requalified before it is spliced in, and one that cannot be
requalified with certainty fails the query rather than being dropped:

- **`RawPred`** is opaque text this package never parsed, so its bare names
  cannot be rewritten.
- **A column qualified with another table** names something the expansion did
  not join — a table the hook reached with its own `Join`.

Both produce an error naming the relation and what to do instead. A dropped
scope predicate would be a silent leak, which is worse than a loud refusal.

Two things the expansion does not take from the hook: its **ordering and its
limit** — the hook runs against a throwaway builder, and a collection's order
and cap belong to the schema — and anything it does at **build time**, since the
predicates are resolved when the query runs. `SQL()` renders the builder as it
stands, which is the contract it has always had: the parent's own hooks do not
run at build time either.

### The composite key is still worth having

`example/tasks` keeps a task and its list in the same workspace with a composite
foreign key, and that is still the arrangement to reach for. It makes a
cross-tenant reference **unrepresentable in the data** rather than merely
unreachable through this query path — a stronger property, and one that survives
a statement someone writes by hand
([ADR-0030](../architecture.md#declared-scope-is-required)).

[ADR-0025](../architecture.md#expansion-is-one-statement) records why it is one
statement, why the columns are listed rather than taken wholesale, and what
would make either worth revisiting.

## Next

- [Rejections](errors.md) — what a non-expandable relation says
- [References and relations](../schema/references.md) — declaring both directions
