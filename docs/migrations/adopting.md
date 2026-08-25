# Adopting a database

## Where "current" comes from

Three sources, in increasing order of how much you should trust them.

**A live database**, via `introspect`:

```go
reg, report, err := introspect.Registry(ctx, db, introspect.Options{})
```

Tells you what the database looks like. It is the obvious source and the worst
one, because it cannot tell you whether the migration history *produces* that
state — a hand-applied hotfix, a migration edited after it ran, or a statement
someone skipped are all invisible, and the next migration gets computed against
a state no file describes.

**The migration history**, via `shadow`:

```go
reg, report, result, err := shadow.Build(ctx, scratchDB, shadow.Options{Dir: "db/migrations"})
```

Replays the checked-in history into an empty database and reads back what it
actually produced. This is a different and stronger claim: *this is the schema
the history builds*. It is the better source for the current side of a diff,
because an edited or skipped migration surfaces instead of being baked into the
next one.

This is not a migration runner. It applies all of them, in order, to an empty
database nobody depends on, and throws away the result. No version table is read
or written, nothing is skipped, and `Down` sections are never executed.

**Drift detection** needs no extra API: it is `migrate.Diff` between the
replayed registry and the live one. An empty result is the claim that the
history and the database agree.

## Adopting an existing database

Two calls, and then you own a schema file:

```go
reg, report, err := introspect.Registry(ctx, db, introspect.Options{})
if !report.Empty() {
    // Constructs the DSL cannot express. Read them: the schema does not
    // describe the database completely until this is empty.
    log.Print(report)
}

src, err := codegen.RenderSchema(reg, codegen.SchemaOptions{Package: "blogschema"})
os.WriteFile("blogschema/schema.go", src, 0o644)
```

`introspect` reports every construct it could not express rather than dropping
it, which is what makes the report worth reading rather than skipping.

The same two calls are a command, which is what you want the first fifty times —
before there is a package to put any of this in:

```bash
sqlb introspect -dsn "$DSN"
```

```
2 table(s) read

  bookings.bookings_coach_id_tstzrange_excl: constraint of a kind the DSL cannot
  declare (contype x)
      EXCLUDE USING gist (coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&)
      WHERE ((status = 'confirmed'::text))
the database has extensions installed, and no generated DDL creates them.
Create them in the target database first, or the first bootstrap fails
once per dependent table naming a function instead of the extension:
  CREATE EXTENSION IF NOT EXISTS "btree_gist";
```

Two tables were read and one construct was refused, so the other table — a
natural-key cache with a composite primary key — is adoptable as it stands.

It exits non-zero when something was skipped, so *why did the drift gate refuse
this module* is a command rather than a throwaway program. `-out schema.go`
writes the declaration instead of reporting, `-only`/`-exclude`/`-module` narrow
what is read, and `-migrations <dir>` replays a history into the given database
and reads back what it built rather than reading the database as it stands —
the stronger source, for the reason the next section gives.

Unlike every other `sqlb` verb it takes no package argument: it reads a database
rather than a declaration, so there is nothing to link.

That pair is the multiplier for a database of any size: sixty-nine tables become
sixty-nine declarations to *review*, not sixty-nine to write. So the two halves
have to agree about what the DSL can express — a type `introspect` reads and
`RenderSchema` refuses stops the bootstrap at the first column that has it. They
are held to that by a test: every type in `schema.Types()` must render, the
rendered source must compile, and the whole loop must be a fixpoint (below).

Everything imports with **no capabilities and nothing exposed over REST**,
because neither can be read from DDL — widening that is a deliberate edit, which
is the correct default for a surface that decides what the outside world can
reach. Table names are not singularised (`orgs` becomes `var Orgs`), because
guessing wrongly on *status* or *address* costs more than renaming a variable
the compiler checks for you.

Generating a migration and adopting a database are the same machinery pointed in
opposite directions.

## The round trip is a fixpoint

Read a database, write it back out, build a second database from what was read,
and the two are the same database:

```
apply(fixture)           → db
introspect(db)           → registry
RenderSchema(registry)   → source that compiles
apply(Diff(∅, registry)) → db'
db' == db
```

`pgtest/fixpoint_test.go` asserts exactly that against Postgres, over a schema
chosen to be awkward: a `vector` column with an HNSW index whose operator class
is its meaning, storage parameters, a partial index, a composite unique, a
deferred unique, an array, nullable `jsonb`, an enum-shaped check and a real one.

The comparison is between the two **databases**, through `pg_catalog`, rather
than between the two registries — and that is the load-bearing part. Two
registries agree about everything they both dropped, which is how an enum's
constraint name went missing for as long as it did: both sides lost it
identically, so every registry-level check passed while the rebuilt database
called the constraint something else and the next diff proposed dropping it.

A constraint's deferrability was the same shape and was found the same way
([#154](https://github.com/mind-vm/sqlb/issues/154)): the declaration could not
say it, the introspector did not read it, and the differ had no field to compare
— so a hand-written `DEFERRABLE INITIALLY DEFERRED` survived every gate by being
invisible to all of them, and a migration that recreated the constraint without
it would have too. It is declarable on `UNIQUE` now and reported on every other
constraint kind, and the fixture carries one so the property is asserted rather
than assumed.

## Adopting one table at a time

The realistic adoption declares a few tables while the database holds dozens, and
that shape used to fight the tooling at four points. All four are closed.

**Read only what you declared.** Diffing five declared tables against sixty-nine
imported ones reports sixty-four tables to drop, which is the adoption working
rather than drift:

```go
introspect.Registry(ctx, db, introspect.Options{
    Only:    []string{"projects", "project_members"},
    Exclude: []string{"goose_db_version"},
})
```

A name in either list that the database does not have is reported, because a
typo there silently shrinks what a gate covers.

**A corner the DSL cannot model no longer stops the rest.** A `tsvector` column
is skipped and reported — and now so is the index over it, the check that
constrains it, and any pinned name that depended on it. One unmodelable column
used to fail the whole import, which made a drift gate impossible to build for
every other table in the database.

**A real foreign key can point at a table you have not declared.** `Ref` needs
the target's declaration and `ExternalRef` emits no constraint, so a partial
declaration had to choose between describing the database inaccurately and
declaring all of it at once:

```go
schema.ExternalRef("org", "organizations.id").Enforced().Filterable()
```

The target is a name, not a resolution. See [`Enforced`](https://pkg.go.dev/github.com/mind-vm/sqlb/schema#Field.Enforced)
for what it gives up — everything [ADR-0015](../architecture.md#module-isolation)
bought by refusing the constraint, which is the right trade only when both tables
live in one database.

**Describe the indexes the database already has.** A declared index whose name
differs from the live one is a rename, and index names are not inert: Postgres
reports a violated constraint by name, and matching that name is the standard way
to tell one unique violation from another.

```go
).
    IndexNamed("idx_projects_org_id", "org_id").
    UniqueIndexNamed("idx_projects_org_code", "org_id", "code")
```

## The drift gate

Once a table is declared accurately, the question worth asking in CI is whether
it *stays* accurate — a hand-applied hotfix, a column added by another service, a
migration edited after the fact:

```bash
sqlb check -database "$DATABASE_URL" ./schema
```

`check` on its own compares committed generated files with the declaration and
opens no database. With `-database` it also reads the live one, narrowed to the
tables the schema declares, normalises the declared `CHECK` expressions against
that same Postgres — `ADD CONSTRAINT … NOT VALID` inside a transaction that is
always rolled back, so no table is scanned — and prints the differences as SQL,
exiting non-zero if there are any.

What it does not compare is what it says it cannot: constructs the DSL has no
way to describe are listed rather than silently skipped, so the gate never claims
to have checked something it never looked at.


## Next

- [Diffing and rendering](README.md)
- [Surveying an existing codebase](../surveying-a-codebase.md) — this page
  answers what the database allows; that one answers how many of the routes and
  queries in front of it sqlb would take
- [Using your own structs](../start/structs-first.md) — the other half of
  adopting sqlb into a project that already exists
- [ADR-0014](../architecture.md#migrations-and-import) — why the history beats
  production as a source of truth
