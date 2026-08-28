# Column type reference

Checked against `schema/field.go` and `schema/type.go`. The guide page is
[Declaring tables](../schema/README.md).

## Constructors

| Constructor | SQL type | Go type | Pattern operators |
|---|---|---|---|
| `Text(name)` | `text` | `string` | yes |
| `Varchar(name, n)` | `varchar(n)` | `string` | yes |
| `Enum(name, values…)` | `text` + a `CHECK` | a named string type | yes |
| `UUID(name)` | `uuid` | `string` | yes |
| `SmallInt(name)` | `smallint` | `int16` | no |
| `Int(name)` | `int` | `int32` | no |
| `BigInt(name)` | `bigint` | `int64` | no |
| `SmallSerial(name)` | `smallserial` | `int16` | no |
| `Serial(name)` | `serial` | `int32` | no |
| `BigSerial(name)` | `bigserial` | `int64` | no |
| `Real(name)` | `real` | `float32` | no |
| `Float(name)` | `float` | `float64` | no |
| `Numeric(name)` | `numeric` | `float64` | no |
| `Numeric(name, p, s)` | `numeric(p, s)` | `float64` | no |
| `Bool(name)` | `bool` | `bool` | no |
| `Timestamp(name)` | `timestamptz` | `time.Time` | no |
| `Date(name)` | `date` | `time.Time` | no |
| `Time(name)` | `time` | `time.Time` | no |
| `JSON(name)` | `jsonb` | `json.RawMessage` | not filterable at all |
| `Bytes(name)` | `bytea` | `[]byte` | not filterable at all |
| `Vector(name, dim)` | `vector(dim)` | `sqlb.Vector` | not filterable at all |
| `Computed(name, t, expr)` | none — rendered as the expression | the Go type of `t` | follows `t` |

"Pattern operators" means `like`, `ilike`, `contains`, `startswith` and
`endswith`, which the filter grammar admits only where the Go field is a string
kind. `UUID` maps to `string`, so it accepts them — worth knowing, since
`?id=startswith.019` is a legal request.

`JSON` and `Bytes` cannot carry a filter value at all: coercion has no case for
them, so a filter naming one is rejected. They are queryable from Go with `Raw`.

`Vector(name, dim)` is a pgvector embedding, and the dimension is part of the
type rather than a constraint on it — changing it is a migration. **It is
`Hidden` and not optionally so**: twenty kilobytes of float has no client use,
and serialising one by accident is a bandwidth bill. Go callers reading through
the query engine still get it. It is storable and orderable and nothing else
yet — no index kind, no metric declaration, no REST search operation.

`Computed(name, t, expr)` is not a stored column: the query renders `expr` where
a column name would go. It is a column everywhere else that matters — the row
type, the JSON, the TypeScript and Dart types, the CLI column set — and
`Hidden`, `Filterable` and `Sortable` gate it exactly as they gate a stored one.

## Arrays

`Array()` is a modifier, not a set of parallel constructors: the constructor
keeps naming the element type and the column becomes a one-dimensional Postgres
array of it.

```go
schema.Text("labels").Array().Filterable().Default(schema.Value("{}"))
schema.Enum("channels", "web", "email").Array().Nullable()
```

| Declared | SQL type | Go type | TypeScript | Dart |
|---|---|---|---|---|
| `Text("tags").Array()` | `text[]` | `[]string` | `string[]` | `List<String>` |
| `Int("sizes").Array()` | `integer[]` | `[]int32` | `number[]` | `List<int>` |
| `Enum("c", "a", "b").Array()` | `text[]` + a containment `CHECK` | `[]TableC` | `TableC[]` | `List<TableC>` |

Every scalar constructor above may take it — text, varchar, enum, the numerics,
bool, uuid and the three time types. `JSON` and `Bytes` may not, and neither may
a second dimension.

**The Go type is the plain slice**, so a model described over an existing sqlc
struct carries one unchanged. A nullable array is still `[]string`: `nil` is
NULL and `[]string{}` is `{}`, which covers both values without a pointer.

Operators, and the three refusals:

| | |
|---|---|
| Accepts | `has`, `hasany`, `hasall`, `eq`, `ne`, `isnull`, `notnull` |
| Refuses | the ordering four, `between`, `in`/`nin`, and every pattern operator — including `contains`, which stays the text substring operator |
| `Sortable()` | refused — the keyset cursor encodes the ordering columns |
| `Searchable()` | refused — search is a text operation |
| `Filterable()` | requires a GIN index on the column, checked by `schema.Validate` |

```go
).AddIndex(schema.Index{Columns: []string{"labels"}, Method: "gin"})
```

Without it the filter still returns the right rows, by scanning the table for
them — so nothing reports the problem and only a plan would show it. See
[ADR-0033](https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#array-columns).

## Shorthands

```go
schema.UUIDv7("id").PrimaryKey()
```

A `uuid` column defaulting to a generated, time-ordered v7 value — the usual
primary key. **It defaults to `uuid_generate_v7()`**, the
[`pg_uuidv7`](https://github.com/fboulnois/pg_uuidv7) extension's spelling, so
the generated DDL does not apply to a stock install:

| Your Postgres | Do this |
|---|---|
| 18 or newer | Pass `migrate.MinPostgres(18)` to `Diff` — it emits the built-in `uuidv7()` |
| 13–17 | `schema.UUID("id").PrimaryKey().Default(schema.GenUUIDv4())` — `gen_random_uuid()`, built in since 13 |
| Any, with the extension installed | Nothing; the default is already correct |

## Auto-incrementing keys

An integer whose value comes from a sequence. Postgres has two spellings and
both are declarable, because an adoption meets the older one and a new schema
should reach for the newer:

```go
schema.BigSerial("id").PrimaryKey()          // bigserial
schema.BigInt("id").Identity().PrimaryKey()  // GENERATED BY DEFAULT AS IDENTITY
schema.Int("attempt").IdentityAlways()       // GENERATED ALWAYS AS IDENTITY
```

| Spelling | What it renders | An INSERT may name the column |
|---|---|---|
| `Serial(name)`, `BigSerial(name)`, `SmallSerial(name)` | `serial`, which Postgres expands into a column, a sequence and a `nextval` default | yes |
| `.Identity()` | `GENERATED BY DEFAULT AS IDENTITY` | yes |
| `.IdentityAlways()` | `GENERATED ALWAYS AS IDENTITY` | no — the column is read-only |

Prefer an identity for a new column: it is one object rather than three, which
is why Postgres introduced it. Declare a serial when the database already has
one — moving between them is a migration, not a rename.

**It is not a type.** A `bigserial` column *is* a `bigint`: the Go type, the
filter operators and the sort machinery are the plain integer's, and it reads
back as one. What the declaration adds is that the database supplies the value,
so an insert that does not name the column is not missing one — the same
property a `Default()` has, and the same one the REST create body reads.

The column may not be `Nullable()` and may not carry a `Default()`; `Validate()`
refuses both, as Postgres would.

When a column *becomes* a serial on a table that already has rows, the sequence
starts at 1. The generated change carries a hazard naming the `setval` to run
first — the row count is not in the schema, so nothing can compute it here.

## Groups

A `Group` inserts several columns as a unit. Two are built in:

| Group | Columns |
|---|---|
| `Timestamps()` | `created_at`, `updated_at` — both default `now()`, read-only, sortable |
| `SoftDelete()` | `deleted_at` — nullable, read-only |

`SoftDelete()` adds the column and **nothing else**: nothing writes it, nothing
filters it out, and the generated `DELETE` issues a real `DELETE`. What it does
do is oblige a hook — a resource over a soft-deleting model does not mount until
one confines it. See [Capabilities](../schema/capabilities.md).

Return your own `schema.Group` to factor a recurring set the same way.

## References

| Constructor | Emits |
|---|---|
| `Ref(name, target)` | A column `name_id` typed to the target's primary key, a foreign key, and a relation called `name` |
| `ExternalRef(relation, "table.column")` | The column and an index, and **no foreign key** — for a reference across a module boundary |

Delete actions: `NoAction`, `Restrict`, `Cascade`, `SetNull`, `SetDefault`.

An `ExternalRef` cannot be `Expandable` and cannot declare an `Inverse`: both
would reach a table this module does not own. See
[References and relations](../schema/references.md).

## `Map`, which is not a column

`schema.Map(name, valueType)` declares a **string-keyed map property of a
request or response body**. No table stores one, no DDL renders it, and
`Registry.Validate` refuses it on a table — it is in this reference because it
is part of the field vocabulary, not because it is a column type.

```go
Body: schema.Body(
    schema.Map("answers", schema.TypeUUID),   // question id -> option id
)
```

| | |
|---|---|
| Go | `map[string]string` — the value type decides the second half |
| OpenAPI | an object with `additionalProperties` |
| TypeScript | `Record<string, string>` |
| Dart | `Map<String, String>` |

It exists because the field vocabulary is the column vocabulary, and a column
is a scalar or an array of scalars — so a map-shaped body had to be declared
`JSON`, which reaches Go as `json.RawMessage` and every generated client as
`unknown`. Declaring the action was still worth it, but the client came out
*less* typed than the hand-written route it replaced, which is the opposite of
what a declared surface is for
([#327](https://github.com/mind-vm/sqlb/issues/327)).

Keys are always strings, because that is what a JSON object's keys are. The
value is a scalar: a map of maps, or of arrays, is a shape a generated client
gains nothing from over `unknown` — the same argument that keeps `jsonb` and
`bytea` out of array element types.

An array is not a substitute. `schema.UUID("option_ids").Array()` types cleanly
and is a worse contract: one option per question is a fact the map shape carries
for free, and a list lets a client answer the same question twice, so the server
grows a validation to refuse what the shape used to make unrepresentable.

**A `jsonb` column stays `jsonb`.** Whether a *stored* map should be declarable
is a larger question — the value there is stored rather than transported, and
`json.RawMessage` is defensible for storage in a way it is not for a request
whose shape the schema knows.

## Modifiers

| Method | Effect |
|---|---|
| `PrimaryKey()` | The key. Addresses a row for item endpoints, cursors and `One` |
| `Unique()` | A unique constraint on the column |
| `Nullable()` | Allows SQL NULL; codegen emits the Go field as a pointer |
| `Default(d)` | A database default: `schema.Value(v)` for a literal, `schema.Now()`, `schema.GenUUIDv4()` (`gen_random_uuid()`) or `schema.GenUUIDv7()` (`uuid_generate_v7()`) |
| `RenamedFrom(old)` | Declares a rename, so a diff emits `RENAME COLUMN` rather than a drop and an add |
| `Comment(text)` | Documentation for the column |
| `Named(column)` | Overrides the column name derived from the field |
| `ConstraintNamed(name)` | Names the constraint this field generates |
| `OfType(t)` | Overrides the SQL type |
| `OnDelete(a)` / `OnUpdate(a)` | Referential actions, on a `Ref` |
| `Pattern(re)` | A regular expression a text value must match, checked on a request |
| `Min(v)` / `Max(v)` | Inclusive bounds on a numeric value, checked on a request |

### Format rules

`Pattern`, `Min` and `Max` describe what a **request** may carry. They reach the
generated request bodies as the struct tags Huma reads, and from there the
OpenAPI document — so a caller with no compile step discovers the rule instead
of learning it from a rejected request:

```go
schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`),
schema.Int("quantity").Min(1).Max(999),
```

**They write no DDL.** A row inserted by a migration, a seed or a background job
is not checked against them; `Check(name, expr)` on the table is the rule the
database holds. The two are complementary, and a value that must never be wrong
in the table wants both.

The expression is [RE2](https://github.com/google/re2/wiki/Syntax), what Go's
`regexp` accepts, and a schema whose pattern does not compile is refused by
`Validate`. JSON Schema specifies ECMA-262, so the two disagree at the edges —
RE2 has no backreferences or lookaround, and `(?i)` is RE2 spelling a browser
will not read. Staying inside the common subset is what keeps the server and a
generated client agreeing about the same string.

Tightening a rule is a **wire break**: input that used to be accepted now gets a
422, with no type change to give it away. `sqlb impact` reports it as one.

A nullable column is typed as its base type in the
[typed column facade](../queries/typed-columns.md) — `*time.Time` on the model
but `Col[time.Time]` there — so the comparand is a value and NULL is expressed
with `IsNull`.

## Table-level

```go
schema.Table("posts", …).
    Index("org_id", "status").
    UniqueIndex("org_id", "slug").
    AddIndex(schema.Index{Columns: []string{"body"}, Method: "gin"}).
    Check("name", "status <> 'published' OR published_at IS NOT NULL").
    Describe("A blog post.").
    RenamedFrom("articles").
    Expose(schema.REST{…})
```

`AddIndex` takes a fully specified `Index` for what the shorthands do not cover:
a `Method` for GIN, a `Where` for a partial index, an `Orders` entry per column
for a `DESC` or `NULLS FIRST` index.

`Orders` maps a column name to `schema.IndexOrder{Desc, Nulls}`; an absent entry
is ascending with Postgres's default placement, and a placement that follows
from the direction is dropped when the DDL is rendered, so a declaration matches
what `pg_get_indexdef` hands back. `Where` is hand-written SQL, and
`sqlb migrate` normalises the declared predicate through Postgres before
diffing — write `latitude IS NOT NULL`, not the parenthesised form the catalog
stores.

`Expose` is what publishes the table over HTTP; without it the table is
reachable from Go and has no REST surface. Its fields:

| Field | Meaning |
|---|---|
| `Path` | URL path. Defaults to the table's local name |
| `Ops` | Bitmask: `OpCreate`, `OpRead`, `OpUpdate`, `OpDelete`, `OpList`, and `CRUD` for the first four |
| `DefaultPageSize` | Page size when the request does not ask. Package default 25 |
| `MaxPageSize` | Hard ceiling. Package default 200 |
| `MaxFilters` | Predicates one request may carry. Package default 24 |
| `Tag` | Groups the resource's operations in the OpenAPI document. Defaults to the table name |

The table-level methods, all chainable after `schema.Table(…)`: `Index`,
`UniqueIndex`, `AddIndex`, `Check`, `Describe`, `PrimaryKeyNamed`,
`RenamedFrom`, `Expose`.
