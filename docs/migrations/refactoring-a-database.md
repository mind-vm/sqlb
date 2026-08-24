# Refactoring a database

A schema is not finished when it is declared. It gets a column, loses one, has
a badly-named one renamed, and each of those is a different kind of risk: some
are free, some destroy data, and some are free in the database and expensive
everywhere else.

This is what each of them looks like with sqlb, worked through
[example/evolve](../../example/evolve) — a support desk whose schema changed five
times. [Migrations](README.md) is the reference for the API; this is
the narrative, and the claims in it are gated rather than asserted.

## The arrangement

There is one schema file and one migration directory, which is what a real
project has:

```
example/evolve/evolveschema/schema.go   the current state, and only that
example/evolve/migrations/              how it got there
```

No `v1` package, no `v2` package. That has a cost — no intermediate state is
readable as Go — and it buys the thing that matters: the example is arranged the
way the problem is. The migrations have already been applied to databases nobody
can go back and edit, and the schema file is the only thing anyone edits now.

### The gate that makes it a pair

Nothing that compares files can tell you whether those two agree. Generated code
matches the declaration whether or not a migration was ever written, so
`generate-check` passes on a schema edit that shipped with no migration at all.

So [pgtest/evolve](../../pgtest/evolve/evolve_test.go) replays the whole history
into an empty database with `shadow.Build`, reads back what Postgres actually
built, and diffs it against the declaration. Adding a column to `schema.go`
without a migration fails there, with the migration you forgot in the failure
message:

```
the migration history no longer builds what evolveschema declares.

Someone edited schema.go without adding a migration, or edited a migration
without the schema. What is missing:

  -- add column tickets.resolution
  ALTER TABLE "tickets" ADD COLUMN "resolution" text;

Generate it with:
    sqlb migrate -name <what-changed> ./example/evolve/evolveschema
```

`shadow.Build` rather than the runner, because goose writes a version table that
introspection cannot tell apart from schema, and the diff would then propose
dropping it forever.

## The safe changes

Revisions 2 and 3 — a column with a default, a table, an index, and a widened
type. They get one section between them because there is not much to say: you
edit the schema, run `sqlb migrate`, and read the file.

```go
schema.Enum("priority", "low", "normal", "high", "urgent").
	Default(schema.Value("normal")).
	Filterable().
	Sortable(),
```

```sql
ALTER TABLE "tickets" ADD COLUMN "priority" text NOT NULL DEFAULT 'normal';
```

The default is what makes it safe. `NOT NULL` without one fails against any
table that already has rows.

**Widening is free; narrowing is not.** Revision 3 turned `varchar(80)` into
`text`, and the generated comment says which direction it is in:

```sql
-- change type of tickets.subject from varchar(80) to text (free in this
--   direction; reversing it rewrites the table)
ALTER TABLE "tickets" ALTER COLUMN "subject" TYPE text;
```

Every value that fit the old type fits the new one, so Postgres needs no scan
and the statement cannot fail on a row. The `Down` section is the narrowing, and
it can fail — which is worth knowing before you rely on a rollback.

### Two things the generator will tell you that are easy to miss

**An index gets a file of its own.** Revision 2 rendered two files, because an
index on a live table is created `CONCURRENTLY` and that cannot run inside a
transaction:

```sql
-- +goose NO TRANSACTION
CREATE INDEX CONCURRENTLY "tickets_customer_id_status_idx" ON "tickets" ("customer_id", "status");
```

Keeping it with the others would have removed the rollback guarantee from all of
them. `migrate.Split` is what separates them, which is why the versions in this
example are spaced by ten.

**A CHECK constraint takes a lock.** The enum above also emits its constraint,
with this attached:

```
-- LOCK ACCESS EXCLUSIVE: checking that every row of tickets already
--   satisfies the constraint scans the whole table with all access blocked.
--   Add it NOT VALID first, then VALIDATE CONSTRAINT in a later migration.
--   ...migrate.Unblock writes that sequence for you.
```

On an empty table this is nothing. On a large one it is an outage, and the
comment is in the file rather than in a document nobody reads at 2am.

## Renaming

**A rename is declared, never inferred.** From the before and after states alone
a rename is indistinguishable from a drop plus an add, and guessing from a
similar name and type destroys data every time the guess is wrong
([ADR-0014](../architecture.md#migrations-and-import)). So you say so:

```go
schema.Text("email_address").RenamedFrom("email").Unique().Searchable(),
```

```sql
ALTER TABLE "customers" RENAME COLUMN "email" TO "email_address";
ALTER TABLE "customers" RENAME CONSTRAINT "customers_email_key" TO "customers_email_address_key";
```

The constraint is renamed too, which is the sort of thing that is tedious to
remember and cheap to generate. `TableDef.RenamedFrom` does the same for a whole
table — revision 4 also renamed `agents` to `support_agents`, and the migration
carries its primary key and unique constraint across with it.

**The hint expires.** It is needed for exactly one release: the migration is
generated once, and after that no database has a column called `email` any more.
A hint whose old column no longer exists is ignored, so leaving one costs nothing
mechanically — but it reads as a claim about the current schema that stopped
being true, so delete it at your next edit to that table.

**Not every naming problem is a rename.** `board_columns` singularising to
`BoardColumn` can collide with a name a different table's codegen already
derives — a `Board`'s selectable-column union, say. `RenamedFrom` fixes that by
actually renaming the table, which moves data and needs the migration this
section is about. `TableDef.TypeName("KanbanColumn")` fixes it without touching
storage at all: it only pins the generated Go/TypeScript/Dart identifier, so the
table stays `board_columns`, the migration this edit needs is none, and every
hand-written reference to the old generated name is what actually needs
updating — the same cost a rename would have had anyway, just without the data
movement.

### The rename is where the two gates disagree

This is the part worth the section. That migration is clean: one statement, no
data movement, nothing to roll back. It regenerates cleanly too. And it breaks
every client:

```
breaking /customers response.email_address renamed from "email"; the migration is a clean RENAME but the wire name changed
breaking /customers filter.email_address   renamed from "email"; ?email=… now 400s
breaking /agents    resource               resource is no longer exposed; every endpoint under it is gone
```

That is `sqlb impact` output, and
[history_test.go](../../example/evolve/history_test.go) asserts it by diffing the
current contract against the one that stood before revision 4. Two gates,
looking at the same edit, seeing different things
([ADR-0039](../architecture.md#a-schema-edit-is-an-api-edit)):

| | What it compares | Verdict on revision 4 |
|---|---|---|
| the replay gate | history vs declaration | **clean** — the rename is exact |
| `impact-check` | contract vs committed baseline | **breaking** — three ways |

The database's opinion and the client's are different opinions, and a schema
change is not safe because one of them is happy. When the break is intended, you
re-record the baseline with `sqlb impact -write`, which makes accepting it a
deliberate act with a diff attached.

The same test asserts the other direction — that adding a nullable column
reports no breaks at all — because a gate that called everything breaking would
pass the first test while being useless ([ADR-0016](../architecture.md#guards-proven-both-ways)).

## Dropping

Revision 5 dropped `tickets.legacy_ref`. This is the one the generator refuses
to do quietly:

```sql
-- +goose Up
-- drop column tickets.legacy_ref
-- DESTRUCTIVE: dropping tickets.legacy_ref deletes its contents. The Down
--   restores the column but not the values
-- Review, then uncomment to apply. Generated commented out on purpose.
-- ALTER TABLE "tickets" DROP COLUMN "legacy_ref";
```

Commented out, with the reason and with what the `Down` will and will not do.
Applying it is a deliberate act: `sqlb migrate -allow-destructive`, or
`Options{AllowDestructive: true}`. The
[checked-in file](../../example/evolve/migrations/00050_drop_legacy_ref.sql) is the
flag's output, because the history has to actually drop the column for the replay
gate to agree with the declaration.

Anything depending on a commented-out change is commented out too, and comes back
live when that one does — otherwise the file would not be a reviewable no-op but
a migration that fails halfway, with a constraint naming a column the skipped
statement never added.

A drop is also a wire break, for the same reason a rename is: `legacy_ref` was in
every response, and now it is not. It appears in the impact output above.

## What is not here

**Extracting a table, and anything else needing a backfill.** `Diff` compares two
schemas; it cannot know that the values in the column you are dropping should
first be copied into the table you are adding. That migration is generated and
then hand-edited, and the honest version of this document would show what that
costs. It is a gap in the example, not a gap that the example hides.

**Narrowing a type, adding `NOT NULL` to a populated column, adding a `UNIQUE`
that existing rows violate.** These fail against real data rather than against
the schema, so demonstrating them needs a fixture with the offending row in it.
The lock warning above is the closest this example comes.

## The order to do things in

1. **Edit `schema.go`.** One declaration, and it is the only thing you edit.
2. **`sqlb migrate -name <what-changed>`** to generate the migration, then read
   it. The comments are where the generator tells you what it cannot decide for
   you — a lock, a destructive change, a direction that is free one way.
3. **`sqlb generate`** for the models, columns and resources.
4. **`sqlb impact`** to find out what it does to clients, and `-write` to accept
   it when the answer is "it breaks them and that is intended".
5. **Apply it with your runner.** sqlb does not apply migrations and does not
   track which have run — you already have a runner, and replacing a working one
   is a much larger ask than adopting a code generator.
