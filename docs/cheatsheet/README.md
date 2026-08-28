# sqlb cheatsheet

Every surface of sqlb on one page: the schema DSL, the query builder, mutations,
hooks, the REST layer and its filter grammar, migrations, codegen and the CLI.

It is a **lookup table, not a tutorial**. Each entry says what the spelling is
and links to the page that says why. If you are meeting sqlb for the first time,
read the [quickstart](../start/quickstart.md) instead and come back here.

Written to be pasted into an agent's context: dense, complete, and organised so
one section answers one question. The per-project half — which columns *your*
schema declared capabilities on — is not here and cannot be; `sqlb generate`
writes that as a [generated skill](../../skills/README.md).

---

## The whole loop

```go
// 1. Declare — ordinary Go, in a package of its own
var Post = schema.Table("posts",
    schema.UUIDv7("id").PrimaryKey(),
    schema.Ref("author", Author).OnDelete(schema.Restrict).Expandable(),
    schema.Text("title").Searchable().Sortable(),
    schema.Enum("status", "draft", "published").Default(schema.Value("draft")).Filterable(),
    schema.Timestamps(),
).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
```

```bash
sqlb generate ./blogschema     # 2. models, typed columns, REST bodies, manifest, clients
sqlb migrate -name init ./blogschema
```

```go
// 3. Query — a value, not a statement that runs when built
posts, err := sqlb.Query[blog.Post]().
    Where(blog.PostCols.Status.Eq(blog.PostStatusPublished)).
    OrderBy(blog.PostCols.CreatedAt.Desc()).
    Limit(50).
    All(ctx, db)

// 4. Serve — one generated call per exposed table
err = blog.Register(api, db)

// 5. Confine — one registration constrains every read, REST included
sqlb.On[blog.Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[blog.Post]) error {
    org, ok := auth.OrgFrom(ctx)
    if !ok {
        return huma.Error400BadRequest("no tenant on the request")
    }
    q.Where(sqlb.F("org_id").Eq(org))
    return nil
})
```

Five facts that explain most of the rest:

| | |
|---|---|
| A query is a **value** | Nothing runs until a terminal method. Predicates can be added on a branch |
| Capabilities are **opt-in per column** | Nothing is filterable, sortable, searchable or expandable unless it says so |
| Values are **always bind parameters** | Only identifiers validated against the model are interpolated |
| Hooks are **the domain seam** | `BeforeQuery` sees the query itself, so one registration covers every read |
| A column has **one wire spelling** | Derived from the name by the schema's `WireCase`; no per-field override |

---

## Project layout

```
blogschema/          the declaration — schema.go plus a SqlbProject() func
  schema.go
  sqlb.go            func SqlbProject() codegen.Project
blog/                generated: models_gen.go, columns_gen.go, rest_gen.go, sqlb.json
migrations/          generated SQL, applied by your runner
cmd/server/          main, usually rest.Serve
```

`schema.Table` registers into the default registry as a side effect of
declaration, which is why the generator only has to import the package.
The declaration package and the generated package share type names
(`blogschema.Post` vs `blog.Post`), which is why they are two packages.

### `SqlbProject`

The package `sqlb` commands take must export exactly this:

```go
func SqlbProject() codegen.Project {
    return codegen.Project{
        Options: codegen.Options{
            Package:  "blog",
            Dir:      "blog",
            TSDir:    "web/src/api",     // optional
            CLIDir:   "cli",             // optional
            SkillDir: ".claude/skills",  // optional
        },
        MigrationsDir: "migrations",
        MinPostgres:   18,
        ShadowDB:      shadowDB,      // needed by `sqlb migrate`
    }
}
```

Paths resolve against the module root, so the commands mean the same thing from
a shell, a `//go:generate` directive and CI.

---

## CLI

```bash
sqlb init -module github.com/you/app [dir]   # scaffold a project
sqlb generate ./schema                        # write every declared artefact
sqlb check ./schema                           # CI drift gate; writes nothing
sqlb migrate -name adds_slug ./schema         # write the next migration
sqlb impact ./schema                          # how the edit changes the REST contract
sqlb eject ./schema                           # the exit: DDL + plain net/http handlers
sqlb docs ./schema                            # feature checklist, one section per endpoint
sqlb survey $SRC $SCRATCH                     # what sqlb could describe of a database
sqlb introspect -dsn $DSN -out schema.go      # database → declaration
sqlb version
```

| Flag | On | Means |
|---|---|---|
| `-database <dsn>` | `check` | also compare the declaration against a live database |
| `-lint <off\|summary\|warn\|all>` | `check` | how much of `schema.Lint` to print (advisory; never fails) |
| `-name <name>` | `migrate` | what the migration does; becomes part of the filename |
| `-check` | `migrate`, `eject` | report staleness, write nothing |
| `-dry-run` | `migrate` | print what would be written |
| `-unblock` | `migrate` | use the concurrent forms of long-lock statements |
| `-allow-destructive` | `migrate` | emit destructive statements live, not commented out |
| `-write` / `-error` | `impact` | record a new baseline / fail on a breaking change |
| `-modules`, `-exclude` | `survey` | group or skip tables |
| `-dsn`, `-migrations`, `-only`, `-out`, `-package` | `introspect` | read a database, or replay a history first |

`generate`, `impact`, `eject` and `docs` need no database; `check` needs one only
with `-database`. `migrate` replays the committed history into `ShadowDB`,
except for the first migration, which diffs against nothing.
`survey` takes two DSNs and no package. See [Go client and CLI](../cli/README.md).

---

## Schema DSL

### Column constructors

| Constructor | SQL | Go |
|---|---|---|
| `Text(name)` | `text` | `string` |
| `Varchar(name, n)` | `varchar(n)` — `text` with no size | `string` |
| `Enum(name, vals...)` | `text` + a `CHECK`, not a native enum | named `string` type + one const per value |
| `SmallInt(name)` | `smallint` | `int16` |
| `Int(name)` | `integer` | `int32` |
| `BigInt(name)` | `bigint` | `int64` |
| `SmallSerial` / `Serial` / `BigSerial` | `smallserial` / `serial` / `bigserial` | `int16` / `int32` / `int64` |
| `Real(name)` | `real` | `float32` |
| `Float(name)` | `double precision` | `float64` |
| `Numeric(name, precision...)` | `numeric`, or `numeric(p,s)` | `float64` |
| `Bool(name)` | `boolean` | `bool` |
| `UUID(name)` | `uuid` | `string` |
| `UUIDv7(name)` | `uuid` defaulting to a generated v7 | `string` |
| `Timestamp(name)` | `timestamptz` | `time.Time` |
| `Date(name)` | `date` | `time.Time` |
| `Time(name)` | `time` | `time.Time` |
| `JSON(name)` | `jsonb` | `json.RawMessage` |
| `Bytes(name)` | `bytea` | `[]byte` |
| `Vector(name, dim)` | pgvector `vector(dim)` | `sqlb.Vector` |
| `Computed(name, t, expr)` | no DDL — an expression | that type's Go type, **pointer unless `NotNull()`** |
| `Ref(name, target)` | FK column `<name>_id` + relation `<name>` | the target key's type |
| `ExternalRef(rel, target)` | FK to a table this registry does not declare | as above |

`Nullable()` makes the Go field a pointer — except `Bytes` (`[]byte`) and any
`Array()` column, where nil already spells absence.

**`UUIDv7` defaults to `uuid_generate_v7()`** (the `pg_uuidv7` extension). On
Postgres 18+ pass `migrate.MinPostgres(18)` to get the built-in `uuidv7()`; on
13–17 use `UUID("id").Default(schema.GenUUIDv4())`.

### Field modifiers

| Method | Effect |
|---|---|
| `PrimaryKey()` | the key; implies read-only and filterable |
| `NotNull()` / `Nullable()` | nullability (stored columns default to NOT NULL, computed to nullable) |
| `Default(d)` | `Value(v)`, `Now()`, `Expr(sql)`, `GenUUIDv4()`, `GenUUIDv7()` |
| `Unique()` / `Indexed()` | a single-column unique constraint / index |
| `Array()` | make it `type[]` — the constructor keeps naming the *element* |
| `Identity()` / `IdentityAlways()` | `GENERATED BY DEFAULT` / `ALWAYS AS IDENTITY` (`ALWAYS` implies read-only) |
| `Serial()` | the older sequence spelling |
| `Named(column)` | override the SQL column name |
| `OfType(t)` | override the logical type |
| `Comment(s)` | a SQL comment, and the field's doc comment |
| `RenamedFrom(old)` | declare a rename so the diff emits `ALTER … RENAME` |
| `SharedAs(name)` | name the Go type generated for this column's value set, so several columns share one (codegen only — no DDL) |
| `Needs(keys...)` | a computed column's per-request binds |
| `OnDelete(a)` / `OnUpdate(a)` | `NoAction`, `Restrict`, `Cascade`, `SetNull`, `SetDefault` |
| `Inverse(name)` / `InverseExpandable(opts...)` | the reverse relation, and whether `?expand` may follow it |
| `Deferred()` | check the constraint at `COMMIT` rather than at the statement |
| `Enforced()` | make an `ExternalRef` a real foreign key — costs the two modules their independent migration and deployment; `?expand` is still refused |
| `CheckNamed(n)` / `ConstraintNamed(n)` | name the generated constraint |

### Capabilities

```go
schema.Text("title").Searchable().Sortable()
schema.Text("password_hash").Hidden()
schema.Ref("org", Org).Filterable().ReadOnly().Scoped()
```

Four permit:

| | Allows |
|---|---|
| `Filterable()` | `?status=eq.draft` |
| `Sortable(nulls...)` | `?sort=-status`; `schema.NullsFirst` / `NullsLast` place NULLs |
| `Searchable()` | joins the `?search=` fan-out (implies `Filterable`) |
| `Expandable()` | a reference resolved inline via `?expand` (references only) |

Three restrict:

| | Effect |
|---|---|
| `ReadOnly()` | never settable through REST — the database or a hook owns it |
| `Immutable()` | settable at create, rejected on update |
| `Hidden()` | never serialised into a REST response, unusable as a filter, and absent from the typed column facade (the Go model still has the field) |

Two more:

| | Effect |
|---|---|
| `WriteOnly()` | accepted in a request body, never returned |
| `LookupKey()` | keeps a `Hidden` column's typed column, for a credential you find the row by |

And one obligation:

| | Effect |
|---|---|
| `Scoped()` | writes no predicate. `rest.Resource` **refuses to mount** the table until a hook satisfies every exposed operation |

Go code going through the builder is trusted and bypasses `ReadOnly` and
`Immutable`; they are enforced at the REST boundary. `Hidden` is enforced at the
projection. See [Capabilities](../schema/capabilities.md).

### Groups

```go
schema.Timestamps()   // created_at, updated_at — timestamptz, default now(), sortable, read-only
schema.SoftDelete()   // deleted_at — nullable, read-only, marked softdelete
```

**`SoftDelete` filters nothing.** Nothing on the request path reads the column;
declaring it obliges a `BeforeQuery` hook, and writing
`q.Where(sqlb.F("deleted_at").IsNull())` is still yours. What the declaration
buys is the refusal: `rest.Resource` will not mount a soft-deleting table whose
reads no hook constrains. Such a table usually leaves `OpDelete` off too, and
serves removal as an update.

### Table-level

```go
var Post = schema.Table("posts", fields...).
    Describe("A blog post.").
    Index("author_id").
    IndexNamed("posts_by_author", "author_id", "created_at").
    UniqueIndex("slug").
    Unique("org_id", "slug").
    UniqueNamed("posts_one_slug_per_org", "org_id", "slug").
    Check("published_have_a_date", "status <> 'published' OR published_at IS NOT NULL").
    PrimaryKeyColumns("org_id", "id").      // composite key
    RenamedFrom("articles").
    TypeName("Article").                     // the generated Go identifier
    Expose(schema.REST{...}).
    AddAction(schema.Action{...}).
    AddQuery(schema.Query{...}).
    AddIndex(schema.Index{Columns: []string{"labels"}, Method: "gin"}).
    AddUnique(schema.Unique{...}).
    AddExclude(schema.Exclusion{...}).
    AddField(f)                              // a field built outside the literal
```

`Unique` (a constraint) and `UniqueIndex` (an index) are different objects and
diff differently. An `EXCLUDE` constraint is the way to say "these two rows may
not overlap" — see [`example/rooms`](../../example/rooms/).

### References and relations

```go
schema.Ref("author", Author).OnDelete(schema.Restrict).Expandable()
```

Produces a **column** `author_id` and a **relation** `author`, typed to the
target's primary key. `?expand=author` names the relation; the column keeps its
own name and stays in the response.

```go
schema.Ref("list", List).Filterable().Expandable().
    Inverse("tasks").
    InverseExpandable(schema.ExpandOrder("position"), schema.ExpandLimit(20))
```

A task has a list; a list has tasks. The inverse name must be declared, never
derived. A reverse expansion is **capped** (50 by default) and arrives as
`sqlb.Collection[T]` — `{items, has_more}` — not a bare array. Past the cap the
caller follows `GET /tasks?list_id=eq.{id}`, which is why the key column wants
`Filterable`. See [References](../schema/references.md) and
[Expanding relations](../rest/expand.md).

### Computed columns

An expression rather than storage: no DDL, but a column to everything above
Postgres — the row type, the JSON, the clients, the OpenAPI document.

```go
schema.Computed("is_overdue", schema.TypeBool,
    schema.FromSQL("due_date < current_date AND open_tasks > 0")).Filterable(),

schema.Computed("total_tasks", schema.TypeInt,
    schema.FromSQL("(SELECT count(*) FROM tasks t WHERE t.project_id = projects.id)")).
    NotNull(),

schema.Computed("is_starred", schema.TypeBool,
    schema.FromSQL("EXISTS (SELECT 1 FROM stars s WHERE s.project_id = projects.id AND s.member_id = ?)")).
    Needs("viewer").Filterable(),
```

- **Nullable unless `NotNull()`** — the opposite of a stored column, and the same
  as SQL. `NotNull()` is a claim; nothing parses the expression.
- **Reading one is opt-in**, because the model is shared:
  `q.WithComputed("total_tasks")` on a query, `rest.Options{Computed: […]}` on a
  resource. A generated resource opts into every column its table declares.
- `Needs("viewer")` binds per request: `q.Bind("viewer", memberFrom(ctx))`,
  usually from a `BeforeQuery` hook. A resource with a `Needs` column and no hook
  to supply it **refuses to mount**.

### Exposure

```go
.Expose(schema.REST{
    Path:            "/posts",       // default "/" + table name
    Ops:             schema.CRUD | schema.OpList,
    DefaultPageSize: 25,
    MaxPageSize:     100,
    MaxFilters:      24,
    MaxSortTerms:    4,
    MaxOffset:       100_000,
    DefaultSort:     []string{"-pinned", "-published_at"},
    Tag:             "Blog",
})
```

| Op | Endpoint |
|---|---|
| `OpCreate` | `POST /posts` |
| `OpRead` | `GET /posts/{id}` |
| `OpUpdate` | `PATCH /posts/{id}` |
| `OpDelete` | `DELETE /posts/{id}` |
| `OpList` | `GET /posts` — filter, sort, search, page, `?expand` |
| `OpSingleton` | `GET /posts` — the caller's one row as a bare object; removes `{id}` from the whole resource, so `OpUpdate` is `PATCH /posts` |

`schema.CRUD` = create|read|update|delete. `schema.Reads` = read|list.
Without `Expose` a table has no HTTP surface at all. An op that is not exposed
has **no endpoint** — not a 405 — and no client function or CLI subcommand.

`OpSingleton` requires a `Scoped()` column and refuses `OpList`/`OpRead`
alongside it; every term of `DefaultSort` must name a `Sortable` column.

### Actions and queries

A **domain verb** on a row:

```go
Task.AddAction(schema.Action{
    Name:    "complete",                                  // POST /tasks/{id}/complete
    Body:    schema.Body(schema.Text("note").Nullable()),
    Writes:  []string{"status", "completed_at"},          // enforced; also takes FOR UPDATE
    Touches: []string{"audit_log"},                       // documentation, not enforcement
    Summary: "Complete a task",
})
```

Generated: the route, the operation, the body type, the id parse, the scoped
fetch, the 404, the transaction, the row lock, the write of `Writes`, the
response, and the same verb in every client. Not generated: the transition.

```go
func completeTask(ctx context.Context, task *tasks.Task, in tasks.CompleteTaskInput) error {
    if task.Status == tasks.TaskStatusDone {
        return &rest.Problem{Status: http.StatusConflict, Detail: "already done"}
    }
    now := time.Now().UTC()
    task.Status, task.CompletedAt = tasks.TaskStatusDone, &now
    return nil
}

err := tasks.Register(api, db, tasks.Actions{CompleteTask: completeTask})
```

A `Path` with no `{id}` is a **collection action**: no row, no generated fetch,
no `BeforeQuery` closure, answers 204.

**An action has no custom response type.** The do func returns `error`, and the
envelope answers 200 with the row (204 for a collection action). A verb whose
answer is not the row is a declared query, or a hand-written route. Failure is a
`*rest.Problem`, which carries its own status.

A **declared read**:

```go
Task.AddQuery(schema.Query{
    Name:   "overdue",                                    // GET /tasks/overdue
    Params: schema.Body(schema.Timestamp("as_of")),
    Reads:  []*schema.TableDef{List},                     // for client cache invalidation
})
// func(context.Context, sqlb.Executor, OverdueTaskParams) ([]Task, error)
```

No fetch, no transaction, no obligation check — a query is exactly as confined
as the statements its func issues, which is why `Reads` is documentation rather
than enforcement.

**A read whose answer is not rows of the table declares its shape.** A rollup,
a per-bucket sum, a count per group — a row of no declared table:

```go
UsageEvent.AddQuery(schema.Query{
    Name:   "usage",
    Params: schema.Body(schema.Date("from"), schema.Date("to")),
    Returns: schema.Result(
        schema.Timestamp("bucket"),
        schema.Numeric("total", 18, 2),
    ),
})
// func(context.Context, sqlb.Executor, UsageUsageEventParams) ([]UsageUsageEventResult, error)
```

The emitted row type carries a `db` tag as well as a `json` one, because it is
what `sqlb.Collect` scans into:

```go
func usage(ctx context.Context, db sqlb.Executor, in UsageUsageEventParams) ([]UsageUsageEventResult, error) {
    return sqlb.Collect[UsageUsageEventResult](ctx, db, sqlb.Query[UsageEvent]().
        Where(sqlb.F("at").Gte(in.From), sqlb.F("at").Lt(in.To)).
        GroupByExpr(sqlb.Raw("date_trunc('day', at)")).
        Select(sqlb.Raw("date_trunc('day', at)").As("bucket"), sqlb.Sum(sqlb.F("amount")).As("total")))
}
```

Query hooks run for it, so a confined table stays confined through the
aggregate. Leaving `Returns` empty keeps `[]T`, which is what a read that
filters or orders differently and answers with the same rows wants.

The verb may not collide with an operation the table already exposes
(`create`, `get`, `update`, `delete`, `list`). See [Actions](../rest/actions.md).

### Registries, modules, wire case

```go
schema.Table(...)                    // the default registry
schema.DefaultRegistry()
r := schema.NewRegistry()
r.Table("posts", fields...)          // this registry only

m := schema.NewModule("billing")     // tables become billing_<name>
m.WireCase(schema.Camel)             // JSON and filter params in camelCase
```

`WireCase` is `Verbatim` (default) or `Camel`, and it is a property of the
*schema*, not a column — there is no per-field override, in either direction.

### Checking your work

```go
schema.Validate()          // error — refuses to generate
schema.Lint()              // Diagnostics — advisory: unindexed filterable column,
                           // a list endpoint with nothing sortable, an array
                           // column filterable with no GIN index
schema.WriteManifest(path) // sqlb.json — every column, capability, operator
```

---

## Generated code

`sqlb generate` writes into `Options.Dir`:

| File | Contents |
|---|---|
| `models_gen.go` | row structs with `db`, `json` and `sqlb` tags, plus enum types |
| `columns_gen.go` | the typed column facade and typed update statements |
| `rest_gen.go` | request bodies (`PostCreate`, `PostPatch`), `Actions`, `Register` |
| `sqlb.json` | the manifest |

Opt-in, each into the repository that consumes it:

| Option | Emits |
|---|---|
| `TSDir` | a typed TypeScript client (`client.gen.ts`, `queries.gen.ts`) |
| `DartDir` | a typed Dart client, with the cursor pager a mobile list needs |
| `CLIDir` + `CLIName` | a cobra command tree over the typed Go client |
| `ClientDir` | the typed Go client alone, standard library only |
| `SkillDir` (+ `SkillName`, `SkillSchemaPackage`) | the per-project agent skill |
| `WiringDir` + `WiringMigrations` / `WiringOperations` | uber-go/fx wiring |
| `Types` | per-column Go type overrides |

**Run `go mod tidy` again after generating**, not only before: generated code can
import a package nothing hand-written does yet.

### Typed columns

```go
q := sqlb.Query[blog.Post]().
    Where(blog.PostCols.Status.Eq(blog.PostStatusPublished)).
    Where(blog.PostCols.Title.Contains(search)).
    OrderBy(blog.PostCols.ViewCount.Desc())
```

| Does not compile | Why |
|---|---|
| `PostCols.Titel` | misspelled column |
| `PostCols.ViewCount.Eq("x")` | wrong comparand type |
| `PostCols.ViewCount.Contains("x")` | text operator on an integer |
| `AuthorCols.PasswordHash` | hidden columns are not in the struct |
| `PostCols.Labels.Contains("x")` | an array is not text |

`Col[T]` deliberately does **not** embed `Field`; pattern operators live on
`TextCol[T ~string]` and containment on `ArrayCol[E]`. A nullable column is
`Col[time.Time]`, not `Col[*time.Time]` — express NULL with `IsNull()`.
`c.Field()` drops to the untyped surface when you need an operator the typed one
does not carry.

### Typed updates

```go
_, err := blog.UpdatePost().
    SetStatus(blog.PostStatusPublished).
    Where(blog.PostCols.ID.Eq(id)).
    Stmt().                                 // the wrapper carries Set*, Where and Stmt
    Exec(ctx, db)
```

Worth preferring over `Set(string, any)`, which checks the column name but not
the value's type. Every writable column gets a setter — **including `ReadOnly`
and `Immutable` ones**, which are REST-boundary rules already defended at that
boundary. Only the primary key and computed columns stay out. `Stmt()` returns
the underlying `*sqlb.Update[T]` for everything the wrapper does not carry:
`Exec`, `One`, `Everything`, `SetExpr`, `Resolved`.

---

## Query builder

```go
q := sqlb.Query[Post]().Where(sqlb.F("status").Eq("published"))
if search != "" {
    q = q.Where(sqlb.F("title").Contains(search))
}
posts, err := q.OrderBy(sqlb.F("created_at").Desc()).Limit(50).All(ctx, db)
```

Methods mutate the builder and return it. `Clone()` before sharing one between
goroutines or request scopes. `Where` skips the zero `Pred`, so
`sqlb.If(cond, pred)` removes a clause entirely rather than making it false.

### Terminal methods

| | Returns |
|---|---|
| `All(ctx, db)` | every matching row |
| `One(ctx, db)` | the single match; `ErrNotFound` if none, an error if more than one |
| `First(ctx, db)` | the first match; pair with `OrderBy` |
| `Count(ctx, db)` | row count, ignoring pagination; group count when grouped |
| `Exists(ctx, db)` | whether anything matched |
| `SQL()` | statement + args, executing nothing |
| `Resolved(ctx, db)` | a copy with hooks applied — the statement that will run |
| `CursorFor(row)` | the cursor naming that row's position |

`sqlb.Collect[R](ctx, db, q)` scans into a type other than the model. Every field
of `R` must be filled by some result column.

### Builder methods

| | |
|---|---|
| `Select(items…)` / `ClearSelect()` | projection (default: every mapped column) |
| `Distinct()` | `SELECT DISTINCT` |
| `Where(preds…)` | conjoin |
| `OrderBy(orders…)` / `Stable()` | ordering; `Stable` appends the primary key |
| `Limit(n)` / `Offset(n)` / `Page(number, size)` | pagination (1-based pages) |
| `After(cursor)` | keyset pagination; a zero cursor is a no-op |
| `GroupBy(fields…)` / `GroupByExpr(exprs…)` / `Having(preds…)` | rollups |
| `Join(table, alias, on)` / `LeftJoin(…)` | joins — `table` and `alias` are **strings** |
| `As(alias)` | alias the table, for self-joins |
| `Expand(names…)` / `ExpandOnly(name, cols…)` | resolve relations inline |
| `WithComputed(names…)` | project computed columns |
| `Bind(key, value)` / `Bound()` | supply a computed column's `Needs` |
| `ForUpdate()` / `ForShare()` / `SkipLocked()` | row locks |
| `Clone()` / `Err()` / `Fail(err)` / `Model()` | plumbing |

### Predicates

`sqlb.F("column")` is the untyped reference.

| Group | Operators |
|---|---|
| Comparison | `Eq` `Neq` `Gt` `Gte` `Lt` `Lte` `Between` `NotBetween` `OneOf` `NotOneOf` `IsNull` `NotNull` `EqField` |
| Text | `Contains` `StartsWith` `EndsWith` `Like` `ILike` |
| Array | `Has` `HasAny` `HasAll` `NotHas` `NotHasAny` `NotHasAll` |
| JSON | `ContainsJSON` `NotContainsJSON` (renders `@> $1::jsonb`) |
| Time | `OnDay(day)` — a whole calendar day of a timestamp column |
| Subquery | `InQuery` `NotInQuery` |
| Combining | `And` `Or` `Not` `If(cond, p)` `Match(expr)` `Exists(q)` `NotExists(q)` `RawPred(sql, args…)` |
| Ordering | `Asc()` `Desc()`; `Order.NullsFirst()` / `NullsLast()`; `OrderBy(expr)` / `OrderByDesc(expr)` |
| Other | `Cast(typ)` `Qualify(table)` `Name()` `Table()` |

- `Contains`, `StartsWith` and `EndsWith` **escape LIKE metacharacters**;
  `Like` and `ILike` do not — use them only for patterns your own code wrote.
- `Eq(nil)` becomes `IS NULL`, never `= NULL`.
- `And`, `Or` and `Not` all skip zero predicates, so an absent filter stays
  absent rather than becoming always-false.
- `sqlb.Array("go", "sql")` binds a value list as one array parameter.

### Expressions and selections

| | |
|---|---|
| Aggregates | `Count()` `CountOf(f)` `CountDistinct(f)` `Sum(f)` `Avg(f)` `Min(f)` `Max(f)` |
| Values | `Val(v)` `Now()` `Add(a, b)` `Sub(a, b)` `Coalesce(exprs…)` |
| Wrapping | `Sel(expr)` `RawSel(sql, args…)`, then `.As("alias")` |

```go
type Revenue struct {
    Status string  `db:"status"`
    Total  float64 `db:"revenue"`
}
rows, err := sqlb.Collect[Revenue](ctx, db,
    sqlb.Query[Order]().
        GroupBy(sqlb.F("status")).
        Select(sqlb.F("status"), sqlb.Sum(sqlb.F("total")).As("revenue")))
```

Query hooks still run, so tenant scoping applies to aggregates too.

### Subqueries

```go
tagged := sqlb.Query[PostTag]().Select(sqlb.F("post_id")).Where(sqlb.F("tag_id").Eq(id))
posts, err := sqlb.Query[Post]().Where(sqlb.F("id").InQuery(tagged)).All(ctx, db)
```

**A nested query must be `Resolved` first if its model's reads are confined by a
hook.** Nesting compiles rather than runs, so the scope would be silently absent.
sqlb refuses rather than dropping it:

```go
sub, err := sqlb.Query[Post]().Select(sqlb.F("author_id")).Resolved(ctx, db)
```

### Paging a whole result set

```go
var cursor sqlb.Cursor
for {
    q := sqlb.Query[Post]().OrderBy(sqlb.F("created_at").Desc()).After(cursor).Limit(500)
    batch, err := q.All(ctx, db)
    if err != nil || len(batch) == 0 {
        return err
    }
    process(batch)
    if len(batch) < 500 {
        return nil
    }
    if cursor, err = q.CursorFor(batch[len(batch)-1]); err != nil {
        return err
    }
}
```

`After` and `CursorFor` both call `Stable()`. A cursor is valid only for the
ordering it was issued under — anything else fails wrapping `ErrBadCursor`.
Index the ordering *including* the tiebreaker: `(created_at DESC, id DESC)`.

### Vector search

```go
near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector(embedding))
rows, err := sqlb.Collect[Hit](ctx, db,
    sqlb.Query[Chunk]().
        Select(sqlb.F("id"), near.Similarity()).
        Where(near.AtLeast(0.75)).
        OrderBy(near.Nearest()).
        Limit(10))
```

One handle, one bind parameter, however many times it is mentioned.
`Similarity()` (larger is closer) and `Distance()` are both available. Register
the codec once: `cfg.AfterConnect = sqlb.RegisterVectorType`, or
`sqlb.VectorPoolConfig(dsn)`.

### The escape hatch

`sqlb.Raw{SQL: "…", Args: []any{…}}` and `sqlb.RawPred(sql, args…)` are not
validated; their `?` placeholders are renumbered by the compiler. Reach for them
for window functions, `WITH RECURSIVE`, `UNION`, `DISTINCT ON` — or write the
statement by hand, or use sqlc. See [`skills/sqlb-queries`](../../skills/sqlb-queries/SKILL.md)
for where the builder ends.

---

## Mutations

```go
post := Post{Title: "Hello", Status: "draft"}
stored, err := sqlb.InsertRows(&post).One(ctx, db)

_, err = sqlb.UpdateRows[Post]().
    Set("status", "archived").
    Where(sqlb.F("published_at").Lt(cutoff)).
    Exec(ctx, db)

n, err := sqlb.DeleteRows[Post]().Where(sqlb.F("id").Eq(id)).Exec(ctx, db)
```

Rows are passed as **pointers** so hooks and returned database values write back
into them. A column with a database default is omitted when its Go value is the
zero value — `Explicit(cols…)` says the zero was meant, which is what a
`Bool(…).Default(Value(true))` needs to be written `false`. Every statement
returns the stored rows.

**An update or delete with no `Where` is refused** with `ErrUnscoped`; call
`Everything()` when that is genuinely what you meant.

| | |
|---|---|
| `Only(cols…)` / `Omit(cols…)` | narrow the INSERT column list |
| `Explicit(cols…)` | write these columns at their zero value, without narrowing |
| `Set(col, v)` / `SetExpr(col, expr)` | assignment; `SetExpr` computes in the database |
| `From(name, subquery)` | `UPDATE … FROM` |
| `Clone()` / `Resolved(ctx, db)` | on `Update` and `Delete` (not `Insert`) |

### Upserts

```go
sqlb.InsertRows(&s).
    OnConflictUpdate([]string{"key"}, "payload").          // target is a SLICE
    OnConflictSet("updated_at", sqlb.Now()).
    OnConflictSet("hits", sqlb.Add(sqlb.Current("hits"), sqlb.Val(1))).
    OnConflictSet("note", sqlb.Coalesce(sqlb.Excluded("note"), sqlb.Current("note")).Expr()).
    Exec(ctx, db)
```

`Excluded(col)` is the proposed row, `Current(col)` the stored one. A bare
`F(col)` inside `DO UPDATE` is **refused** rather than guessed.

`OnConflictDoNothing(target…)` skips the row, so the terminal is `Exec` — `One`
after it is refused, because the only answer it could give is `ErrNotFound` on
exactly the path an idempotent insert exists to serve. To get the row back
either way, update the target to itself:
`OnConflictUpdate([]string{"idem_key"}, "idem_key").One(ctx, db)`.

When any row is skipped, **no** caller struct is written back; the returned slice
is the account of what happened.

### Constraint errors

```go
var c *sqlb.ConstraintError
if errors.As(err, &c) && c.Kind == sqlb.ConstraintUnique {
    return "already taken"
}
if errors.Is(err, sqlb.ErrConstraint) { … }   // the cheap class test
```

`Kind` is one of `ConstraintUnique`, `ConstraintForeignKey`, `ConstraintCheck`,
`ConstraintNotNull`, `ConstraintExclusion`. `Constraint` (the index name),
`Table`, `Column` and `Detail` are filled in from what Postgres reported.
`SetErrorClassifier` remains for errors wrapped past `errors.As`.

---

## Transactions

```go
db := sqlb.New(pool).WithHooks(reg)

err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
    order, err := sqlb.InsertRows(&o).One(ctx, tx)
    if err != nil {
        return err
    }
    _, err = sqlb.UpdateRows[Stock]().
        Set("reserved", true).
        Where(sqlb.F("sku").Eq(order.SKU)).
        Exec(ctx, tx)
    return err
})
```

**Pass the inner `ctx` onward**, not the enclosing one, or `TxFrom` will not find
the transaction inside your hooks. A panic rolls back and is re-raised. Nesting
**joins** rather than nests: the outermost call owns the commit.

| | |
|---|---|
| `sqlb.New(exec)` | a handle with an empty registry; `exec` may be a pool, conn or `pgx.Tx` |
| `WithHooks(r)` | a copy resolving hooks against `r` — the only way it gets rules |
| `WithTx` / `WithTxOptions(opts, …)` | run in a transaction; the second takes an isolation level |
| `WithoutScope(names…)` | release named hook scopes for this handle |
| `InTx()` / `CanBeginTx()` / `Tx()` | what this handle is sitting on |
| `AfterCommit(fn)` | run once the transaction commits, and not at all otherwise |
| `sqlb.TxFrom(ctx)` | the handle a hook is running under |
| `sqlb.AfterCommit(ctx, fn)` | the hook-side form, since a hook has a ctx and not a handle |

`DB.Tx()` reaches the underlying `pgx.Tx` so `CopyFrom`, `SendBatch` or a sqlc
`Queries` can join the same unit of work. **Do not commit or roll it back
yourself** — that leaves the after-commit callbacks unrun.

A failing after-commit callback does not stop the others, and the failures join
under `ErrAfterCommit`: the write is durable, the side effect is not.

---

## Hooks

```go
reg := sqlb.NewRegistry()

sqlb.On[Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Post]) error {
    org, ok := auth.OrgFrom(ctx)
    if !ok {
        return huma.Error400BadRequest("no tenant on the request")
    }
    q.Where(sqlb.F("org_id").Eq(org))
    return nil
})

db := sqlb.New(pool).WithHooks(reg)
```

| Hook | Receives | Use for |
|---|---|---|
| `BeforeQuery` | `*Builder[T]` | scoping every read, including the generated ones |
| `BeforeCreate` | `*T` | normalising, deriving, stamping the owner |
| `AfterCreate` | `*T`, defaults populated | validation — and **mutating the response** |
| `BeforeUpdate` | `*Update[T]` | forcing a column, narrowing affected rows |
| `AfterUpdate` | `[]T` | validation |
| `BeforeDelete` | `*Delete[T]` | narrowing, or refusing |
| `AfterDelete` | `int64` | validation |
| `AfterDeleteRows` | `[]T`, as they were | anything needing the rows' identity |

- **Every write path fires them**, not just generated handlers.
- All run **inside** the caller's transaction — right for validation, wrong for
  anything the outside world observes. Use `AfterCommit` for that.
- `AfterDeleteRows` costs a `DELETE … RETURNING`, so the clause is added only
  when such a hook is registered.
- `BeforeUpdate` cannot read the assignments it was handed; a rule about what a
  column is *becoming* belongs in a `BEFORE` trigger.
- **In a hook, take the lock in `BeforeCreate`, not `AfterCreate`.**
- **Return a `huma.StatusError`** (`huma.Error400BadRequest(…)`) from a hook that
  is refusing. A plain `errors.New` carries no status and lands as a 500.

### Scopes

```go
sqlb.On[Post](reg).Scope("tenant").BeforeQuery(scopeReads)
worker := db.WithoutScope("tenant")     // a background job releases one rule
```

`ScopedHooks` covers `BeforeQuery`, `BeforeUpdate` and `BeforeDelete`.
`BeforeCreate` is deliberately **not** releasable — it panics — because dropping
the hook that supplies the tenant column writes an unowned row.

### The obligation

A table declaring `Scoped()` will not mount until a hook satisfies each exposed
operation: `OpList`/`OpRead` need `BeforeQuery`, `OpUpdate` needs
`BeforeUpdate`, `OpDelete` needs `BeforeDelete`, `OpCreate` needs
`BeforeCreate`. The check proves a hook *exists*, not that it is right.

Prove the predicate reached the statement — see [Testing](#testing).
Full treatment: [Hooks](../queries/hooks.md).

---

## Authentication

```go
type Principal struct{ UserID, OrgID string }

v := sqlb.VerifierFunc[Principal](func(ctx context.Context, cred string) (Principal, error) {
    return verifyJWT(ctx, cred)
})

handler := sqlb.Middleware(v, sqlb.BearerToken)(next)
```

| | |
|---|---|
| `sqlb.Verifier[T]` / `VerifierFunc[T]` | credential → principal |
| `sqlb.CredentialExtractor` | `func(*http.Request) (string, bool)`; `sqlb.BearerToken` is the default |
| `sqlb.Middleware(v, extract)` | plain `net/http` middleware; 401 on a missing or rejected credential |
| `sqlb.WithPrincipal(ctx, p)` / `PrincipalFrom[T](ctx)` | the context seam hooks read |
| `sqlb.TransientError{Err}` | a provider outage — `Middleware` answers 500, not 401. Return it **by value**; a pointer is recognised too |

`PrincipalFrom` reports `false` both for "no principal" and "a principal of
another type". **Never treat `false` as "no restriction"** — fail closed.
See [Authenticating a request](../rest/auth.md) and
[`example/auth-workos`](../../example/auth-workos/).

---

## REST

```go
srv := rest.NewServer(rest.Config{Title: "Blog", Version: "1.0.0"})
rest.Must(blog.Register(srv.API, db))
http.ListenAndServe(":8080", srv.Handler)
```

Bring your own router — `rest.Resource` takes a `huma.API`, never a router:

```go
router := chi.NewRouter()
api := humachi.New(router, huma.DefaultConfig("Blog", "1.0.0"))
err := blog.Register(api, db)
```

### The whole server

```go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()

err := rest.Serve(ctx, rest.ServeConfig{
    DSN:        os.Getenv("DATABASE_URL"),
    Middleware: authn.Middleware,
    Migrate:    migrations.Apply,
}, func(srv *rest.Server, db *sqlb.DB) error {
    reg := sqlb.NewRegistry()
    blog.RegisterHooks(reg)   // your own, wherever they live — not generated
    return blog.Register(srv.API, db.WithHooks(reg))
})
```

| `ServeConfig` | Default | |
|---|---|---|
| `DSN` | — | required |
| `Addr` | `:8080` | |
| `Server` | zero `Config` | passed to `NewServer` |
| `Middleware` | nil | applied to the handler once `mount` returns |
| `Migrate` | nil | `func(ctx, *pgxpool.Pool) error`, before `mount` |
| `ShutdownTimeout` | 5s | |
| `Log` | `slog.Default()` | |

### `rest.Options`

Restates `schema.REST` at the runtime; codegen writes it from the declaration.

| Field | |
|---|---|
| `Path`, `Ops`, `Name`, `Tag`, `Description` | identity and exposure |
| `DefaultPageSize`, `MaxPageSize`, `MaxFilters`, `MaxSortTerms`, `MaxOffset` | request bounds |
| `DefaultSort` | ordering when the request names no `?sort` |
| `Expandable` | relation names `?expand` may name |
| `Computed` | computed columns this resource pays for — unlisted means *unreachable* |
| `Columns` | narrow this **mount** to these columns (one table, two surfaces) |
| `Unscoped` | scope names this resource releases |
| `DisableSearch` | reject `?search` even when columns are searchable |
| `DisableTransactions` | do not wrap generated writes in a transaction |
| `Security` | an OpenAPI security requirement on every operation |

Hand-mounting one resource:

```go
rest.Must(rest.Resource[blog.Post, blog.PostCreate, blog.PostPatch](api, db, rest.Options{
    Path: "/posts",
    Ops:  rest.CRUD | rest.OpList,
}))
```

`rest.None[T]` stands in for a body type when a resource exposes no writes.

### Filter grammar

```
?status=eq.published            operator form
?email=alice@example.com        shorthand — a bare value means eq
?age=gte.18&age=lt.65           repeated parameters conjoin
?tag=in.a,b,c                   value lists, quotable: in."a,b",c
?views=between.10,20            ranges
?deleted_at=isnull              null tests
?title=contains.ada             case-insensitive substring
?labels=has.urgent              array contains this element
?labels=hasany.a,b              overlaps; hasall.a,b contains all
?metadata=hasdoc.{"lang":"de"}  jsonb containment (@>)
?starts_at=day.2026-09-01       a whole calendar day of a timestamp column
?or=(status.eq.draft,age.lt.18) explicit disjunction, nestable
?and=(…)  ?not=(…)              the other groups; not=(a,b) is NOT (a AND b)
?filter={"op":"and",…}          a JSON expression tree, for arbitrary nesting
?sort=-created_at,title         "-" for descending; created_at.desc also works
?select=id,title                projection (the primary key is always kept)
?expand=author,comments         relations declared Expandable
?search=ada                     ILIKE fan-out over searchable columns
?page=2&per_page=50             offset pagination
?limit=50&offset=100            the same, spelled the other way
?cursor=eyJrIjpb…               keyset pagination — resume after a position
?count=exact                    ask for `total`, a second query
```

| Operator | Applies to |
|---|---|
| `eq` `ne` `neq` | any column (on an array: the whole array, element by element) |
| `gt` `gte` `lt` `lte` | ordered scalars |
| `in` `nin` | scalars, up to `MaxListValues` (100) members |
| `between` | ordered scalars |
| `isnull` `notnull` | any column |
| `like` `ilike` | text — the pattern is **not** escaped |
| `contains` `startswith` `endswith` | text — the operand **is** escaped |
| `has` `hasany` `hasall` (+ `nhas` `nhasany` `nhasall`) | array columns |
| `hasdoc` `nhasdoc` | jsonb columns |
| `day` | timestamp columns |

Refused, on purpose: ordering, `in` and `between` on arrays; `contains` on an
array (it means substring on text — `has` is the array spelling); ordering,
patterns and a bare-value shorthand on jsonb.

**A negation is not a complement.** `nhas` is `NOT (…)` under SQL's
three-valued logic, so a NULL column matches neither `has` nor `nhas`. Ask for
those rows explicitly: `?or=(labels.nhas.urgent,labels.isnull)`.

Nothing is filterable, sortable or searchable unless the column declared it. A
request naming an unknown or uncapable column is a **400 listing what would have
been accepted** — never a leak, never a silently ignored parameter. Every problem
in a request is reported, not just the first.

Defaults (override per resource): `DefaultPageSize` 25, `MaxPageSize` 200,
`MaxFilters` 24, `MaxSortTerms` 4, `MaxGroupDepth` 3, `MaxListValues` 100,
`MaxValueLength` 256, `MaxOffset` 100 000, `MaxTreeDepth` 4, `MaxTreeNodes` 64.

Using it by hand — this is exactly what a generated resource does:

```go
q, err := filter.Parse(r.URL.Query(), filter.Options{Model: sqlb.ModelOf[Post]()})
if filter.WriteError(w, err) {
    return
}
b := filter.Apply(sqlb.Query[Post](), q)
```

`filter.ParseFilterTree(data, opts)` compiles a standalone JSON tree (a POST
body, say). `filter.Coerce(s, t)` converts a URL token to a column's Go type —
what a path segment needs. `filter.AsErrors(err)` unwraps, and is what to use
instead of a type assertion. See [Filtering](../rest/filtering.md).

### List responses

```json
{
  "items": [ … ],
  "page": 1,
  "per_page": 25,
  "has_more": true,
  "next_cursor": "eyJrIjpb…",
  "total": 412
}
```

`total` is present only with `?count=exact`. `next_cursor` is present whenever
there is a next page and the model has a primary key — including on a request
that paged by offset, so switching to cursors needs no flag. An empty page is
`[]`, never `null`. Every list is ordered deterministically.

### Statuses

| | Cause |
|---|---|
| 400 | the query string could not be understood, or named something that has not opted in — carries `errors[]` with allow-lists |
| 404 | no row matched *after hooks applied their predicates* — a row scoped away is indistinguishable from one that does not exist |
| 409 | a unique or exclusion constraint refused the write |
| 422 | the body parsed but is not acceptable: FK, check or not-null constraint, `Row()` validation, or a hook's refusal |
| 500 | unclassified; the body says only that it failed, and the error is logged |

The constraint's *name* is deliberately absent from the body — it is on the Go
error value, where branching on it belongs. See [Rejections](../rest/errors.md).

### Change events

```go
broker := rest.NewBroker(rest.BrokerOptions{})
defer broker.Close()

rest.Must(rest.PublishChanges[blog.Post](reg, broker))
rest.Must(rest.Events(srv.API, rest.EventsOptions{Source: broker}))
```

The stream carries the **address** of a change, never the row:
`{"table":"posts","key":"p1","op":"update"}`. The client refetches through the
ordinary `GET`, so every rule that path enforces still holds.

| | `rest.Broker` | `outbox.Dispatcher` |
|---|---|---|
| Where the event lives | memory | a table, written by the transaction that made the change |
| Delivery | at-most-once | at-least-once |
| Replicas | **one** | any number |
| After a restart | reset — refetch everything | replayed from the table |
| Costs | nothing | a table to prune; writes to published models serialise |

```go
outbox.Install(ctx, pool, outbox.Options{})        // the table and its trigger
ob := outbox.Must(outbox.New(pool, outbox.Options{}))
rest.Must(rest.PublishChanges[blog.Post](reg, ob)) // unchanged

d := outbox.MustDispatcher(outbox.NewDispatcher(ctx, pool, outbox.DispatcherOptions{}))
go d.Run(ctx)
rest.Must(rest.Events(api, rest.EventsOptions{Source: d}))
```

`PublishChanges` registers hooks, not handler wrappers, so it covers the
generated CRUD, the generated actions, your own writes, the background job and
the admin script alike. Publication goes through `AfterCommit`, so a rolled-back
write announces nothing. See [Change events](../rest/events.md).

### API compatibility

```bash
sqlb impact ./schema           # how this edit changes the REST contract
sqlb impact -write ./schema    # record the current contract as the baseline
sqlb impact -error ./schema    # fail CI on a breaking change
```

See [API compatibility](../rest/compatibility.md).

---

## Migrations

sqlb writes files; **your runner applies them**. It does not track what has run.

```go
changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))

m := migrate.Migration{
    Version: migrate.TimestampVersion(time.Now()),
    Name:    "add_view_count",
    Changes: changes,
}
files, err := migrate.Write("db/migrations", m, migrate.Options{Format: migrate.Goose})
```

| | |
|---|---|
| `Diff(current, target, opts…)` | pure function over two registries → `[]Change` |
| `Render(m, opts)` | the files in memory |
| `Write(dir, m, opts)` | to disk; refuses to overwrite |
| `Split(m)` | split into separately-applied migrations |
| `Unblock(changes)` | the concurrent forms of long-lock statements |
| `TimestampVersion(t)` / `SequentialVersion(n)` | version strings |
| `ByName(s)` | a `Format` from a string — `Goose`, `GolangMigrate`, `Plain` |
| `Options{Format, AllowDestructive, Handwritten}` | rendering |
| `m.Destructive()` / `m.Blocking()` | what this migration will cost |

- **Destructive changes render commented out.** `AllowDestructive: true` when you
  mean it. Anything depending on a commented-out change is commented out too.
- **Renames are declared, never inferred**: `RenamedFrom("old")` on the field or
  table. Without it a rename diffs as a drop and an add.
- `Changes` is an ordinary slice, so hand-written `migrate.Change` values for
  what the DSL cannot express (triggers, composite FKs) are `append`, not a fork.
  Render those with `Options{Handwritten: true}` — the header is a claim that
  sqlb wrote everything in the file, and `sqlb check` trusts it.

Where `current` comes from: an empty registry (the baseline), a replay of the
committed history into a scratch database (what `sqlb migrate` does), or
`introspect` over a live one. See [Migrations](../migrations/README.md) and
[Adopting a database](../migrations/adopting.md).

---

## Testing

Two doubles, and the split most suites end up making anyway.

```go
exec := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id", "title"}})
db := sqlb.New(exec).WithHooks(reg)

if _, err := sqlb.Query[Post]().All(orgCtx("acme"), db); err != nil {
    t.Fatal(err)
}
if !strings.Contains(exec.LastStatement(), `"org_id" = $1`) {
    t.Errorf("the scoping hook did not reach the statement:\n%s", exec.LastStatement())
}
```

`sqlbtest.DB` records what your code produced: `Statements()`, `LastStatement()`,
`Args()`, `LastArgs()`, `Script(replies…)`, `Reset()`. It is **not a Postgres** —
it does not parse SQL or evaluate a predicate.

A real database, no container:

```go
pool := sqlbtest.Fresh(t, dsn, sqlbtest.Declared(schema.DefaultRegistry()))
```

One database per test on a DSN you supply. Options: `Declared(reg, opts…)`,
`Changes(changes)`, `SQL(statements…)`, `Extensions(names…)`, `Do(fn)`,
`MaxConns(n)`, `Configure(fn)`. `sqlbtest.DSN(t, env, hint)` reads the DSN and
skips with a useful message when it is unset. See [Testing](../queries/testing.md).

---

## Inspecting

| | |
|---|---|
| `q.SQL()` | what *this builder* holds — executes nothing, and does **not** include hooks |
| `q.Resolved(ctx, db)` | a copy with hooks and expansion scopes applied — the statement that runs |
| `sqlb.Explain(ctx, db, q)` | plan against the live schema without executing |
| `sqlb.ExplainAnalyze(ctx, db, q)` | real timings — **executes**, so on a write use a transaction you roll back |
| `plan.Diagnostics()` / `sqlb.Diagnostics(ds)` | plan smells, rendered |
| `plan.UsesIndex(name)` / `UsesSeqScan(rel)` | assertions for a test |
| `sqlb.SeqScanCostThreshold` / `SeqScanRowThreshold` | tune what is reported |

```go
plan, err := sqlb.Explain(ctx, db, q)
if err != nil {
    t.Fatal(err)   // the query is not valid against this database
}
if d := plan.Diagnostics(); len(d) > 0 {
    t.Errorf("plan regressed:\n%s", sqlb.Diagnostics(d))
}
```

`Explain` fails on the migration that was written and never applied — which a
compile-time column check structurally cannot.

Tracing needs no API: `Executor` is two methods, so wrap it.

```go
type tracer struct{ inner sqlb.Executor }

func (t tracer) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
    start := time.Now()
    rows, err := t.inner.Query(ctx, q, args...)
    slog.InfoContext(ctx, "sqlb", "sql", q, "dur", time.Since(start), "err", err)
    return rows, err
}
// Exec likewise, then pass the wrapper wherever you passed the pool.
```

Implement `Beginner` on the wrapper if `WithTx` should work through it.
See [Inspecting and tracing](../queries/inspecting.md).

---

## Without the DSL

The engine reflects over structs, so every feature is reachable without codegen.

```go
func init() {
    sqlb.Describe[Invoice]().
        Table("invoices").
        PrimaryKey("id").
        Defaulted("id").
        Timestamps("created_at").
        Filterable("customer_id", "amount_due").
        Sortable("amount_due", "created_at").
        Hidden("memo").
        Relation("Customer", "customer_id")
}
```

Call it from `init` — it panics otherwise. Naming a column that does not exist
panics at startup and lists the ones that do. Column names derive from field
names (`CustomerID` → `customer_id`).

Other `Description[T]` methods: `Column(field, column)`, `SQLType(name, cols…)`,
`Computed(col, expr, needs…)`, `ReadOnly`, `WriteOnly`, `Immutable`,
`Searchable`, `Scoped`, `SoftDeleted`, `SortNullsFirst`, `SortNullsLast`,
`Model()`.

### Struct tags

What codegen writes, and what you can write by hand:

```go
type Post struct {
    ID       string                    `db:"id" json:"id" sqlb:"type:uuid,pk,default,filter,readonly"`
    AuthorID string                    `db:"author_id" json:"author_id" sqlb:"type:uuid,filter,expand"`
    Title    string                    `db:"title" json:"title" sqlb:"type:text,filter,sort,search"`
    Secret   string                    `db:"secret" json:"-" sqlb:"hidden"`
    Author   *Author                   `db:"-" json:"author,omitempty" sqlb:"expands=author_id"`
    Comments *sqlb.Collection[Comment] `db:"-" json:"comments,omitempty" sqlb:"expands=post_id,order=created_at,limit=20"`
}

func (Post) TableName() string { return "posts" }
```

| Tag | Means |
|---|---|
| `pk` | primary key (implies `readonly` and `filter`) |
| `filter` `sort` `search` `expand` | capabilities; `sort:nullsfirst` / `sort:nullslast` place NULLs |
| `readonly` `immutable` `hidden` `writeonly` | restrictions |
| `scope` `softdelete` | the tenant column, the soft-delete column |
| `default` | the database supplies the value |
| `type:<pg type>` | the SQL type |
| `wire:<name>` | the wire spelling, when `WireCase` differs from the column name |
| `expands=<fk>` `order=<col>` `limit=<n>` `reverse` | a relation field (always `db:"-"`) |

What you give up: the typed column facade, generated request bodies, migrations
from a diff, and the TypeScript/Dart/CLI clients. The builder, the filter
grammar, capabilities, hooks and pagination are identical.
See [Using your own structs](../start/structs-first.md).

---

## Errors

| Sentinel | Meaning |
|---|---|
| `ErrNotFound` | `One` matched no rows |
| `ErrUnscoped` | an update or delete with no `Where` — call `Everything()` to confirm |
| `ErrConstraint` | the class of every write a database constraint refused |
| `ErrBadCursor` | malformed, or issued for a different ordering |
| `ErrAfterCommit` | committed, but an after-commit callback failed — **do not retry the write** |
| `rest.ErrNoTransaction` | an action needed a transaction and had none |
| `rest.ErrBrokerClosed` / `outbox.ErrDispatcherClosed` | the source is closed |
| `*sqlb.ConstraintError` | `Kind`, `Constraint`, `Table`, `Column`, `Detail` |
| `filter.Errors` | every rejected parameter, with allow-lists; `StatusCode()` is 400 |
| `sqlb.TransientError` | a dependency outage, not a bad credential |

---

## Traps

The failures that compile, pass their tests, and are wrong at runtime.

**`SQL()` is not what runs.** On a model with `BeforeQuery` hooks it renders the
builder without them. Use `Resolved(ctx, db)` when the text is going anywhere but
a log.

**A nested query runs nobody's hooks.** `InQuery` compiles rather than runs, so a
scoped model's confinement would be silently absent. `Resolved` the inner query
first — sqlb refuses rather than dropping it.

**An aggregate over an empty set is NULL.** `Sum` into an `int64` fails to scan.
Wrap it: `sqlb.Coalesce(sqlb.Sum(f).Expr(), sqlb.Val(0)).As("total")`.

**A `day` filter is not `eq` against a date.** A `timestamptz` compared to a date
compares against midnight and matches nothing, with no error. Use `OnDay` /
`?col=day.2026-09-01`.

**`OnConflictDoNothing` + `One` is refused**, because on a conflict its only
answer is `ErrNotFound` — on exactly the retry an idempotent insert exists to
serve. Update the target to itself instead.

**A hook returning `errors.New` is a 500.** Return a `huma.StatusError`.

**`PrincipalFrom` returning false is not "no restriction".** Fail closed.

**Read-modify-write loses under concurrency.** `SetExpr` computes in the
database; `ForUpdate` takes the lock. Take locks in a consistent order, and in
`BeforeCreate` rather than `AfterCreate`.

**`rest.Broker` is single-replica.** A crash between commit and fan-out loses the
event; a write served by another replica is invisible. Use `outbox.Dispatcher`
when there is more than one process.

**A computed column is nullable unless it says otherwise** — the opposite of a
stored one. `NotNull()` on the `count(*)`, and only there.

**`schema.SoftDelete()` writes no predicate.** It declares a column and an
obligation. Without a `BeforeQuery` hook that filters on it, deleted rows are
returned by every read — and `rest.Resource` refuses to mount rather than let
that happen quietly.

**An array column that is `Filterable` needs a GIN index.** Without one every
filter is a sequential scan that returns the right rows and reports nothing.
`schema.Lint` says so.

**`Set(string, any)` checks the column, not the value's type.** Prefer the
generated `UpdatePost().SetStatus(…)`.

**Pass the transaction's `ctx` onward**, not the enclosing one, or `TxFrom`
finds nothing inside your hooks.

More, with the evidence for each:
[`skills/sqlb-queries`](../../skills/sqlb-queries/SKILL.md).

---

## Where to go next

| | |
|---|---|
| [Quickstart](../start/quickstart.md) | schema → generate → query → serve → scope |
| [Concepts](../concepts/README.md) | the five ideas the rest rests on |
| [Declaring tables](../schema/README.md) · [Capabilities](../schema/capabilities.md) · [References](../schema/references.md) | the schema DSL in full |
| [Queries](../queries/README.md) · [Mutations](../queries/mutations.md) · [Hooks](../queries/hooks.md) · [Paging](../queries/paging.md) · [Testing](../queries/testing.md) · [Inspecting](../queries/inspecting.md) | the engine |
| [REST](../rest/README.md) · [Filtering](../rest/filtering.md) · [Pagination](../rest/pagination.md) · [Expand](../rest/expand.md) · [Actions](../rest/actions.md) · [Auth](../rest/auth.md) · [Events](../rest/events.md) · [Rejections](../rest/errors.md) | the HTTP surface |
| [Migrations](../migrations/README.md) · [TypeScript](../typescript/README.md) · [Dart](../dart/README.md) · [CLI](../cli/README.md) | everything downstream |
| [Architecture](../architecture.md) and its [Decisions](../architecture.md#decisions) | why the seams are where they are |
| [API reference](https://pkg.go.dev/github.com/mind-vm/sqlb) | the Go doc comments, which are the real reference |
| [`skills/`](../../skills/README.md) | agent skills — authoring, queries, adoption, and the generated per-project one |
