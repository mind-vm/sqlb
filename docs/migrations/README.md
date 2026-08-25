# Migrations

**sqlb does not apply migrations and does not track which have run.** You
already have a runner — goose, golang-migrate, atlas, a shell script — and
replacing a working one is a far larger ask than adopting a code generator, for
no benefit sqlb could offer. This package produces files; your runner applies
them.

There are three separable layers, and the separation is deliberate:

| Layer | Knows about |
|---|---|
| `Diff` | Two schema registries. A pure function; no database, nothing applied |
| The DDL layer | Postgres |
| A `Format` | goose, or golang-migrate, or plain files |

## Diffing a change

You edited a schema file. `Diff` tells you what that means in DDL:

```go
changes, err := migrate.Diff(current, target)
for _, c := range changes {
    fmt.Println(c.Up)
    fmt.Println(c.Down)
}
```

```sql
ALTER TABLE "posts" ADD COLUMN "view_count" bigint NOT NULL DEFAULT 0;
ALTER TABLE "posts" DROP COLUMN "view_count";
```

`target` is your declared schema — `schema.DefaultRegistry()`. Where `current`
comes from is the interesting question, and there are three answers; see
[Where "current" comes from](adopting.md#where-current-comes-from).

### Destructive changes are commented out

```go
changes, _ := migrate.Diff(current, target)  // a column was removed from the schema
```

```
ALTER TABLE "posts" DROP COLUMN "legacy_slug";
destructive: true
reason: dropping posts.legacy_slug deletes its contents. The Down restores the column but not the values
```

Rendering emits a destructive change **commented out**, so applying it is a
deliberate act rather than something a generator did on your behalf. Pass
`Options{AllowDestructive: true}` when you mean it — a flag with a decision
attached to it.

Anything depending on a commented-out change is commented out too, and comes
back live when that one does. Otherwise the file would not be a reviewable
no-op but a migration that fails partway through, with a constraint naming a
column the skipped statement never added.

### An index drop says where the index came from

`DROP INDEX` is not destructive in the sense above — no row is lost — so it is
rendered live. But three different situations produce the same statement: an
index you have stopped declaring, one somebody built by hand that the
declaration never knew about, and, for one migration after upgrading past
v0.14, one sqlb used to create by implication from a reference and no longer
does ([#259](https://github.com/mind-vm/sqlb/issues/259)). Only the first is
an intended loss, and rebuilding a large index under `CONCURRENTLY` is not
cheap.

So `sqlb migrate` and `sqlb check -database` annotate the two they can
recognise ([#268](https://github.com/mind-vm/sqlb/issues/268)):

```sql
-- drop index messages_thread_id_idx
-- note: no sqlb-generated migration in migrations ever created this index, so it was built by hand or by another tool and nothing in the declaration will put it back
-- note: this indexes messages.thread_id, which the declaration still calls a reference and no declared index covers — sqlb v0.14 and earlier created this index by implication and v0.15 does not (#259). If it is wanted, declare it with .Indexed() on the reference rather than letting this drop it
DROP INDEX CONCURRENTLY "messages_thread_id_idx";
```

The first note comes from the same scan `sqlb check` already makes over
`MigrationsDir` for header provenance; the second from the index's shape
against the declaration. A drop of an index sqlb built because the declaration
asked for one carries neither, which is what keeps them worth reading.

### Renames are declared, never inferred

A rename is indistinguishable from a drop plus an add when only the before and
after states are known, and inferring one from a similar name and type destroys
data whenever the inference is wrong. So you declare it
([ADR-0014](../architecture.md#migrations-and-import)):

```go
schema.Text("email_address").RenamedFrom("email")
```

```sql
ALTER TABLE "authors" RENAME COLUMN "email" TO "email_address";
```

The hint is needed for exactly one release: the migration is generated once, and
after that the old name is gone from every database it has been applied to. A
hint whose old column no longer exists is ignored, so leaving one behind is
harmless — but delete it at your next edit, because a stale hint reads as a
claim about the current schema that is no longer true.

`TableDef.RenamedFrom` does the same for a whole table.

## Rendering files

```go
m := migrate.Migration{
    Version: migrate.TimestampVersion(time.Now()),
    Name:    "add_view_count",
    Changes: changes,
}

files, err := migrate.Write("db/migrations", m, migrate.Options{Format: migrate.Goose})
```

`Render` returns the files in memory if you want to inspect before writing.
Goose is the default because its single-file Up/Down format is the one most
likely to be pasted into by hand afterwards; `GolangMigrate` and `Plain` are
also available, and `ByName` resolves a format from a string.

```sql
-- 20260727120000_add_view_count.sql
-- Generated by sqlb. Review before applying.

-- +goose Up
-- posts.view_count
ALTER TABLE "posts" ADD COLUMN "view_count" bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE "posts" DROP COLUMN "view_count";
```

`SequentialVersion(n)` is there if your runner numbers migrations rather than
timestamping them.

An empty `Down` renders a comment explaining that the change is not
automatically reversible, rather than a silently missing section — a `Down` that
does nothing is worse than one that says why.

[`example/tasks/cmd/migrate`](../../example/tasks/cmd/migrate/main.go) is a
worked version of this page: a baseline diffed from an empty registry, then a
second migration of hand-written `migrate.Change` values for what the DSL cannot
express — trigger functions for `updated_at` and for a `completed_at` that has
to agree with a check constraint, plus a pair of composite foreign keys.
`Changes` is an ordinary slice, so the escape hatch is `append` rather than a
fork, and hand-written SQL is rendered, ordered and split by the same code as
generated SQL. A multi-statement body gets goose's
`StatementBegin`/`StatementEnd` without being asked.

That second migration renders with `migrate.Options{Handwritten: true}`, which
skips the header above. The header is a claim that sqlb wrote everything in the
file, and `sqlb check` (issue #178) trusts it enough to fail if a header-bearing
file contains DDL none of sqlb's emitters write — `CREATE TRIGGER`,
`CREATE FUNCTION` or a `DO $$` block, the largest class of thing the DSL cannot
express. A migration whose `Changes` were composed by hand rather than produced
by `Diff` cannot make that claim honestly, so it should not carry the header in
the first place; `Handwritten` is how a caller says so.

It also shows the cost of `Write` refusing to overwrite — right, because a
migration already applied somewhere must not change under the runner's feet.
Regenerating during development means deleting first, and that example deletes
only the files `Render` says it is about to write.

## Which Postgres

There is no hard minimum; the generated DDL is mostly SQL that has been valid
for a decade. Three places are version sensitive:

- **`schema.GenUUIDv7`** emits `uuid_generate_v7()`, which needs the
  [`pg_uuidv7`](https://github.com/fboulnois/pg_uuidv7) extension — so by
  default a UUIDv7 primary key produces DDL that will *not* apply to a stock
  install. Postgres 18 has `uuidv7()` built in: pass `migrate.MinPostgres(18)`
  to `Diff` and it emits that instead. On an older server without the extension,
  use `schema.GenUUIDv4`, built in since Postgres 13. `example/tasks` passes
  `MinPostgres(18)` for exactly this reason, which is also why it needs 18 — a
  demo requiring an extension before it will run is not much of a demo.
- **`Unblock`'s `SET NOT NULL` sequence** is correct on any version but only
  *fast* from Postgres 12.
- **`introspect`** handles the `NOT NULL` constraint rows Postgres 18
  introduced, and ignores them on older versions.


## Next

- [Locks and rollout](rollout.md) — the statements that block writers, and what
  rewrites them
- [Adopting a database](adopting.md) — where the "current" side of a diff comes
  from, and importing a schema you already have
- [Refactoring a database](refactoring-a-database.md) — a schema that changed
  five times, worked through: what is free, what destroys data, and the rename
  that is a clean migration and a broken client at once
- [Declaring tables](../schema/README.md) — the declarations these diffs are
  computed from
- [ADR-0014](../architecture.md#migrations-and-import) — why renames are declared,
  and why the history beats production as a source of truth
