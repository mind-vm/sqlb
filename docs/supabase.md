# Running sqlb on Supabase

Supabase is Postgres plus a platform. sqlb is a library over Postgres. The two
compose, and the interesting part is not whether they connect — they do, over
an ordinary DSN — but which half owns which job once they are both present.

This page is the arrangement: which connection each component needs, what to
point the shadow database at, how to declare a table keyed to `auth.users`, and
where authorization lives when the database already has a mechanism for it.

[How sqlb compares](comparisons.md) is the other question — whether to
adopt sqlb at all when Supabase already serves a REST API. This page assumes
that decision is made.

## What each side owns

sqlb never opens a connection. It takes an `Executor` — a `*pgxpool.Pool`, a
`*pgx.Conn`, a `pgx.Tx` — so a Supabase project is reached exactly the way any
other Postgres is, and nothing in the library needs to know which one it is.

| | |
|---|---|
| **Supabase provides** | the Postgres instance, backups, the connection pooler, Auth (`auth.users` and the tokens), Storage, and Realtime |
| **sqlb provides** | the schema as Go, the migrations, the models, the REST surface, the OpenAPI document, and the four generated clients |
| **You decide once** | whether a table's authorization is Go (sqlb hooks) or SQL (RLS policies) — [below](#authorization-pick-one-per-table) |

Your Go process runs wherever you already run Go. Supabase's Edge Functions are
Deno, so they are not a host for a sqlb server; the usual shape is a Go service
on your own platform with a Supabase project behind it.

## Which connection each component needs

Supabase offers three connection strings — the direct connection, the pooler in
session mode, and the pooler in transaction mode (port 6543). Copy the exact
strings from the project's **Connect** dialog; what matters here is which one
each component needs, and that a wrong choice is silent rather than loud.

| Component | Connection | Why |
|---|---|---|
| The query path — everything `sqlb.Query`, `rest` and the generated clients do | **Transaction pooler** | Nothing on this path is session-scoped. That is a decision, not an accident: [Pgbouncer in the path](architecture.md#pgbouncer-in-the-path) makes a transaction pooler the *default assumed topology* and forbids any feature that relies on a `SET` outliving its transaction, a session advisory lock, a temp table, or a cursor held across transactions. `pgtest/pgbouncer_test.go` runs this path through a real transaction pooler on every `mise run ci` |
| Migrations | **Direct or session** | A migration runner takes session advisory locks, and `CREATE INDEX CONCURRENTLY` cannot run inside a transaction. Both die under transaction pooling |
| `outbox.Dispatcher` | **Direct or session** | It holds a `LISTEN`. A transaction pooler *accepts* the `LISTEN` and then never delivers a notification — the failure the same decision measured, and the one that hides longest, because the dispatcher's fallback poll turns it into latency rather than data loss |
| The shadow database `sqlb migrate` replays into | **A local Postgres**, never the project | It runs `DROP SCHEMA public CASCADE` on every invocation. [Below](#the-shadow-database) |

`NOTIFY` needs no exception — it is transactional and fire-and-forget, so it
works from any pooled connection. Only the listener needs the direct path.

Note also that Supabase's direct connection is IPv6 unless the project has the
IPv4 add-on. If your host has no IPv6, the session-mode pooler is the direct
connection's stand-in for the two components above.

## Connecting

```go
cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL")) // the transaction pooler
if err != nil {
	return err
}
// Optional, and only if the pooler in front of Postgres turns out not to track
// named prepared statements: pgx caches them per connection, and a pooler that
// hands the next transaction a different server connection makes that cache
// lie. Supavisor does track them; PgBouncer does when max_prepared_statements
// is non-zero. If you see "prepared statement already exists", this is the fix.
//
//	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

pool, err := pgxpool.NewWithConfig(ctx, cfg)
if err != nil {
	return err
}
db := sqlb.New(pool)
```

That is the whole integration for the query path. Everything else on this page
is about the schema, the identity, and the two places the platform's own
opinions meet sqlb's.

## Declaring a table keyed to auth.users

`auth.users` belongs to the platform. Your schema does not declare it, does not
migrate it, and must not propose dropping it — but the row that points at it
usually wants a real foreign key, because that is what makes deleting a user
take their rows with it.

The spelling is an enforced external reference naming the schema:

```go
var Profile = schema.Table("profiles",
	schema.UUIDv7("id").PrimaryKey(),
	schema.ExternalRef("user", "auth.users.id").
		Enforced().
		OnDelete(schema.Cascade).
		Filterable(),
	schema.Text("display_name").Searchable().Sortable(),
	schema.Timestamps(),
).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
```

- **`ExternalRef`** because the target is a name rather than a declaration: sqlb
  never resolves `auth.users`, never generates it, and never diffs it.
- **`.Enforced()`** because the constraint is real. Without it no `FOREIGN KEY`
  is emitted, which is the right answer across a module boundary and the wrong
  one here — the database *has* this constraint, and a diff that cannot see it
  proposes dropping it forever ([issue #55](https://github.com/mind-vm/sqlb/issues/55)).
- **The `auth.` prefix** because a Supabase project usually has two tables that
  could answer to `users`. `introspect` reads the same key back in the same
  spelling, so a declaration and the database it built agree — which is what
  `pgtest/crossschema_test.go` asserts against a real Postgres.

What it costs is the thing to plan for: **every database this DDL reaches must
already have the `auth` schema in it**, including the shadow database. Nothing
in sqlb creates it, and the decision behind that — [a foreign key may name
another schema](architecture.md#a-foreign-key-may-name-another-schema) — says
why provisioning it is not sqlb's job to take on. A replay that stops on a
missing schema says so and points here.

Such a reference is not `Expandable` — expanding it would join a table this
schema does not own. Read the user through Supabase's own API, or keep the
fields you actually need on your own row.

## The shadow database

`sqlb migrate` diffs your declaration against a scratch database that has had
the whole migration history replayed into it, and it empties that database
first. Two consequences:

1. **It is never the Supabase project.** The scaffolded `shadowDB` runs
   `DROP SCHEMA public CASCADE; CREATE SCHEMA public`, which against a live
   project deletes the application.
2. **It needs the platform's schemas** the moment any migration names
   `auth.users`, or the replay fails on that statement.

The tidy answer is the Supabase CLI's local stack, which is the same Postgres
with the same `auth` schema in it:

```bash
supabase start
export SQLB_SHADOW_DSN='postgres://postgres:postgres@127.0.0.1:54322/postgres'
```

If you would rather not run the whole stack, a bare container plus the one
table your keys point at is enough — the shadow database is a scratch replay
target, not a replica:

```sql
CREATE SCHEMA IF NOT EXISTS auth;
CREATE TABLE IF NOT EXISTS auth.users (id uuid PRIMARY KEY);
```

Put that in the shadow database's bootstrap, after the `DROP SCHEMA public`
that empties it. It exists so a `REFERENCES auth.users (id)` has something to
resolve; nothing reads it.

## UUID defaults and the Postgres version

`schema.UUIDv7("id")` renders `uuid_generate_v7()`, which needs the `pg_uuidv7`
extension — and `migrate.MinPostgres(18)` renders the built-in `uuidv7()`
instead. Neither is available on a project running an older major without that
extension, so check the project's Postgres version before keeping the
scaffold's `MinPostgres: 18`.

Where the version is older, `schema.UUID("id").Default(schema.GenUUIDv4())`
renders `gen_random_uuid()`, which has been core since Postgres 13 and needs
nothing installed; generating the id in Go works everywhere too, including in a
test with no database.

Extensions otherwise take care of themselves: `Diff` emits
`CREATE EXTENSION IF NOT EXISTS` only for `vector` and `btree_gist`, only when
a column or constraint first needs one, and both are on Supabase's list of
available extensions — so the statement installs one that is not there yet and
is a no-op against a project that already has it in the `extensions` schema.

## Pointing sqlb at a project that already has tables

```bash
go run github.com/mind-vm/sqlb/cmd/sqlb introspect -dsn "$DIRECT_URL" -out schema.go
```

`introspect` reads one schema, `public` by default, so the platform's `auth`,
`storage` and `realtime` schemas are invisible to it and are never proposed for
dropping. Two things it does that are worth knowing here:

- A foreign key into another schema comes back as the qualified enforced
  reference above, rather than as a reference to a same-named local table.
- The **Report** lists everything the DSL could not express, and the extensions
  the database has installed. An adoption reads it once and reconciles by hand;
  it is not noise.

The columns it emits carry no capabilities and no exposure. Nothing is
filterable, sortable, selectable or served until a person says so
([capabilities are opt-in](architecture.md#capabilities-are-opt-in)), which is
the difference between this and pointing PostgREST at the same database.

## Authentication

[example/auth-supabase](../example/auth-supabase) is a `sqlb.Verifier[T]` for
Supabase access tokens, in its own module so its JWT dependency never reaches
sqlb core:

```go
verifier, err := authsupabase.New(ctx, "https://<ref>.supabase.co", mapClaims)
if err != nil {
	return err // the JWKS is fetched at startup, so an outage fails loudly here
}
mux.Handle("/", sqlb.Middleware[Principal](verifier, sqlb.BearerToken)(handler))
```

A project still on the legacy shared secret uses `NewWithSecret` instead; the
rest is identical.

**The trap it exists to close:** a project's `anon` key is itself a JWT signed
by that project, and it is published in every browser bundle. A verifier that
checks only the signature accepts it as a signed-in caller — and accepts the
`service_role` key with rather more authority than that. The adapter refuses
any token whose `role` claim is not `authenticated`, and any token with no
`sub`. An *anonymous sign-in* is allowed through and marked, because it is a
real user row and what it may do is your rule to write.

From there it is the ordinary sqlb story: the principal reaches a `BeforeQuery`
hook through the context, and one registration per model scopes every read of
it — see [Authenticating a request](rest/auth.md) and [Hooks](queries/hooks.md).

## Authorization: pick one per table

This is the decision that matters, and it is worth making explicitly rather
than discovering.

Supabase's own API serves tables through PostgREST, where authorization is
row-level security: the request carries the user's JWT, Postgres runs as a
restricted role, and policies decide which rows exist. sqlb puts the same rules
in Go, in hooks, and connects as the role its DSN names — `postgres` for the
strings the dashboard hands out, which owns these tables and is therefore not
filtered by their policies. That last part is a property of the role rather
than of sqlb: if you connect as a role you created yourself, check whether RLS
applies to it before assuming either answer.

Both work. **Serving one table through both is where it goes wrong**, because
the rules are then written twice, in two languages, and the day they disagree
is the day one of them is a leak. So:

- A table sqlb serves: authorization in hooks. Enabling RLS on it and writing
  no policy at all is worth doing anyway — the owner sqlb connects as is
  unaffected, and a policy set with nothing in it denies every other role, so
  the table stays unreachable if someone points a browser at PostgREST.
- A table the Supabase client reads directly from the browser: RLS policies, as
  the platform intends. Declare it in sqlb only if you want the migrations, and
  do not `Expose` it.

The [hooks page](queries/hooks.md) is the mechanism, and
[comparisons](comparisons.md#postgrest) is the argument for why a Go application
would want the rules there in the first place.

## Realtime and the change feed

sqlb's change feed is `outbox`: a row written in the same transaction as the
change, a `NOTIFY` to wake a dispatcher, and a fallback poll if the wake-up
never arrives. It works on Supabase with one constraint, the one in the table
above — **the dispatcher's pool must reach Postgres directly**, because a
transaction pooler swallows `LISTEN` silently.

Supabase Realtime is the alternative and needs no direct connection, since it
reads the write-ahead log rather than a Postgres session. It broadcasts row
changes to subscribed clients; it does not give you the delivery guarantees the
outbox does, and it does not run your Go code. Which one fits depends on
whether the consumer is a browser or your own backend.

## Storage

[example/attachments](../example/attachments) does presigned direct-to-client
uploads against any S3-compatible endpoint, with the ordering that matters —
the row before the bytes, the object removed only after the commit. Supabase
Storage speaks S3, so it is a configuration difference rather than a code one.

## Checklist

1. Transaction pooler for the app; direct or session for migrations and the
   outbox dispatcher.
2. `SQLB_SHADOW_DSN` at a **local** Postgres that has an `auth` schema — the
   CLI's local stack, or a container with the two-line bootstrap above.
3. Check the project's Postgres major before keeping `MinPostgres: 18`.
4. `schema.ExternalRef("user", "auth.users.id").Enforced()` for a row keyed to
   a Supabase user.
5. `example/auth-supabase` behind `sqlb.Middleware`, then a `BeforeQuery` hook
   per model.
6. Decide per table: hooks or RLS. Not both.
