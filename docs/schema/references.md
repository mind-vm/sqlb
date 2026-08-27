# References and relations

```go
schema.Ref("author", Author).OnDelete(schema.Restrict).Expandable()
```

`Ref` produces a column named `author_id` and a relation named `author`, typed
to match the target's primary key. The actions are `NoAction`, `Restrict`,
`Cascade`, `SetNull` and `SetDefault`.

The column and the relation are two different names on purpose: the column keeps
its own name, and `?expand=author` names the relation. Expansion adds the row,
it does not replace the reference.

## Choosing the delete action

`Restrict` is the right default for a reference that is a *record that something
happened* — a loan is the record that a physical object left the building, so
deleting the book would either fail on the foreign key or destroy the history.
`Cascade` is right for a reference that is ownership: a tenant's rows go when the
tenant does.

`ON DELETE RESTRICT` on both sides of a join table is also a compensating
control in an application with no authentication: the registry cannot be erased
by removing what it points at.

## Both directions of one reference

`Inverse` names the relation as the *target* knows it, and it is declared on the
referencing side because that is where the column and the constraint already
are:

```go
schema.Ref("list", List).Filterable().Expandable().
    Inverse("tasks").
    InverseExpandable(schema.ExpandOrder("position"), schema.ExpandLimit(20))
```

Read as: a task has a list; a list has tasks; both may be expanded.

The two exposures are separate decisions about two endpoints — `?expand=list` on
a task and `?expand=tasks` on a list — and neither implies the other. Absent
`Inverse` there is no reverse relation, which is normal.

**The name has to be declared rather than derived**, because two references from
one table to another — an author's posts and the posts an author reviewed —
would derive the same name for different sets of rows.

The target's generated struct gains a field for the collected rows; its
declaration is untouched.

Two things follow, and both are covered in
[Expanding relations](../rest/expand.md): a reverse expansion is **capped** and
returns an envelope with `has_more` rather than a bare array, and the ordering
is **declared and always total**, because under a cap the order does not merely
arrange the result — it decides which children the caller never sees.

Past the cap, the caller follows the child's own endpoint filtered by the same
key:

```
GET /tasks?list_id=eq.{id}&sort=position&page=2
```

which is why that column wants to be `Filterable`. `schema.Lint` says so when it
is not, along with reporting an unindexed foreign key, which matters more here
than in the forward direction.

## Across a module boundary

`ExternalRef` emits the column but **no foreign key**:

```go
// in the billing module, with no import of the tenants module
schema.ExternalRef("tenant", "tenants.id").Filterable().Indexed()
```

`.Indexed()` is the index to join on, and it is not implied. It used to be — a
soft foreign key exists to be joined on, so the column carried one on the
strength of its own shape. That rule was invisible to a registry read back out
of a database, which is how a self-referencing key came to be imported as an
`ExternalRef`, acquire an index the database did not have, and make every
subsequent migration propose dropping it
([#259](https://github.com/mind-vm/sqlb/issues/259)). `schema.Lint` reports the
missing index instead, under `unindexed-ref`.

The two modules stay independently deployable and independently migratable, and
either can move to its own database without dropping a constraint. Referential
integrity becomes the application's job — the trade a module architecture is
already making everywhere else. The target string is free text and is not
resolved, because resolving it would require exactly the dependency this avoids.

An external reference cannot be `Expandable`, and cannot declare an `Inverse`
either: expanding it in either direction would reach a table this module does
not own, and nothing about the other side is resolvable.

## A real foreign key to a table you have not declared

`Ref` needs the target's `*TableDef` and `ExternalRef` emits no constraint,
which leaves the case an incremental adoption always reaches: the database has a
live, enforced foreign key, and the table it points at has not been declared yet.

```go
schema.ExternalRef("org", "organizations.id").Enforced().Filterable()
```

`Enforced` emits the `FOREIGN KEY` against the *name*, without resolving it.
`"organizations.id"` names the table and the column; a bare `"organizations"`
means its `id`. A module-qualified target is refused, because a constraint has to
name a table in this database and being unable to is the whole of what a module
boundary means.

### A target in another schema

A third form names the schema, for a database this application shares with a
platform that owns schemas of its own:

```go
schema.ExternalRef("user", "auth.users.id").Enforced().OnDelete(schema.Cascade)
```

That renders `REFERENCES "auth"."users" ("id")`, and `introspect` reads such a
key back in the same spelling rather than binding it to a same-named local table
— a distinction a Supabase project needs, since it usually has both an
`auth.users` and a `users` of its own
([a foreign key may name another schema](../architecture.md#a-foreign-key-may-name-another-schema)).

The obligation it takes on is that **nothing here creates that schema**. Every
database the generated DDL reaches has to have it already, the scratch database
`sqlb migrate` replays the history into included — see
[Running sqlb on Supabase](../supabase.md#the-shadow-database), which is where
this form is usually wanted.

It gives up exactly what the paragraph above bought: two modules joined this way
can no longer be migrated or deployed independently, and neither can move to its
own database without dropping the constraint. That is the right trade when both
tables are already in one database with the constraint already there — an
adoption — and the wrong one across a boundary you intend to keep. Expansion is
still refused: a constraint says the row exists, it does not give this schema the
target's columns.

`introspect` produces this form for any foreign key whose target is outside the
tables it read, which is what stops a schema-vs-database gate proposing
`DROP CONSTRAINT` forever.

## What a reference cannot express

Two limits worth knowing before you meet them.

**A composite foreign key is not expressible through `Ref`.** Where a boundary
has to hold across an expansion — a task and its list belonging to the same
workspace — a hook cannot enforce it, because [hooks do not follow the
join](../rest/expand.md#hooks-do-not-follow-the-join). `example/tasks` writes
that pair as hand-written `migrate.Change` values, which is the documented
escape hatch and not a fork.

**Expansion resolves one level.** A relation expands to its row; that row's own
relations do not expand in turn, and there is no `?expand=list.workspace`. One
level is a join per relation and a bounded statement; nesting is where a depth
limit and a cost model have to be argued for
([ADR-0025](../architecture.md#expansion-is-one-statement)).

## Next

- [Expanding relations](../rest/expand.md) — what these look like on the wire
- [Migrations](../migrations/README.md) — the DDL a reference produces
- [ADR-0022](../architecture.md#references-declare-their-inverse) — why the inverse
  is declared rather than derived
