# Your first app

[`example/blog`](../../example/blog/) is the smallest thing that is still a
whole thing: a worked schema, everything codegen emits from it, the hand-written
pieces that generated code cannot produce, and an assembled server. It is a real
test suite rather than a listing, so it cannot drift from the code.

This page walks it. Every file named here exists, and every snippet is quoted
from it.

## What is in it

```
blogschema/schema.go   three tables — the source of truth an author edits
gen/main.go            the codegen program, and its -check mode
models_gen.go          the row structs                    ┐
columns_gen.go         the typed column facade            │ generated
rest_gen.go            request bodies and Register        │
sqlb.json              the manifest                       ┘
hooks.go               the soft-delete predicate
post_ext.go            one method the generator cannot produce
deletes.go             the endpoint the generator cannot produce
server_test.go         the assembled server, and every claim above asserted
```

The generated four are committed, and
`sqlb check ./example/blog/blogschema` fails if they are stale. That is the whole staleness story: generated code is checked
in so it is reviewable in a diff, and a gate makes forgetting to regenerate
loud.

## The schema

Three tables, in [`blogschema/schema.go`](../../example/blog/blogschema/schema.go).
`Org` is a tenant, `Author` a person, `Post` the table the dynamic data views
are built over. The interesting one:

```go
var Post = schema.Table("posts",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Ref("org", Org).OnDelete(schema.Cascade),
	schema.Ref("author", Author).OnDelete(schema.Restrict).Filterable().Expandable().
		Inverse("posts").
		InverseExpandable(schema.ExpandOrder("-published_at"), schema.ExpandLimit(2)),

	schema.Text("title").Searchable().Sortable(),
	schema.Text("body").Searchable(),
	schema.Enum("status", "draft", "review", "published").
		Default(schema.Value("draft")).
		Filterable().
		Sortable(),

	schema.BigInt("view_count").Default(schema.Value(0)).Filterable().Sortable().ReadOnly(),
	schema.Timestamp("published_at").Nullable().Filterable().Sortable(),

	schema.Timestamps(),
	schema.SoftDelete(),
).
	Index("org_id", "status").
	Index("author_id").
	Check("published_posts_have_a_date",
		"status <> 'published' OR published_at IS NOT NULL").
	Describe("A blog post.").
	Expose(schema.REST{
		Path:            "/posts",
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 20,
		MaxPageSize:     100,
		MaxFilters:      12,
	})
```

Read the capabilities as a list of decisions, because that is what they are:

- **`body` is `Searchable` but not `Sortable` or `Filterable`.** It joins the
  `?search=` fan-out and has no filter or sort of its own. Sorting a page by
  article body is meaningless; filtering on it exactly is useless.
- **`view_count` is `ReadOnly`.** No request can set it. It is still
  `Filterable` and `Sortable`, so "most read this week" is a URL.
- **`authors.password_hash` is `Hidden`** — and deliberately not filterable
  either. A filterable secret can be recovered by probing one character at a
  time; a hidden one has no spelling anywhere.
- **`OpDelete` is absent from `Ops`.** The table declares `SoftDelete`, and the
  generated delete issues a real `DELETE`. The two halves of the alternative are
  below.

## The two halves generated code cannot produce

This is the part worth reading, because it is where a real application always
ends up.

**A soft delete needs an endpoint.** `schema.SoftDelete()` adds a `deleted_at`
column and stops — nothing in the runtime writes it or filters it out. No hook
can bridge that: `BeforeDelete` receives a `*Delete`, so it can abort the
statement or narrow it, but not turn it into an `UPDATE`. So
[`deletes.go`](../../example/blog/deletes.go) serves `DELETE /posts/{id}` by
hand, on the same router and in the same OpenAPI document:

```go
rows, err := sqlb.UpdateRows[Post]().
	Where(sqlb.F("id").Eq(in.ID), sqlb.F("deleted_at").IsNull()).
	SetExpr("deleted_at", sqlb.Raw{SQL: "now()"}).
	Exec(ctx, db)
```

The `deleted_at IS NULL` predicate makes a second delete a 404 rather than a
silent re-stamp — which matches what a reader of this API can see, since a post
already soft-deleted is, to every other endpoint, gone.

**And a hook to make it mean anything.** Without this, the endpoint would hide
nothing and the row would come straight back on the next list.
[`hooks.go`](../../example/blog/hooks.go) is the other half:

```go
func RegisterHooks() {
	sqlb.On[Post](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[Post]) error {
		q.Where(sqlb.F("deleted_at").IsNull())
		return nil
	})
}
```

One registration, and every read of `Post` is filtered — including the reads the
generated handlers issue. That is [where domain logic
goes](../concepts/domain-logic.md) in one file.

It is an exported function rather than an `init` so the registration is visible
at the call site, and so a test can choose not to make it.

**One more, on the update statement.** `view_count` is `ReadOnly`, so codegen
emits no `SetViewCount`. Incrementing it correctly is a decision rather than a
mechanism, so [`post_ext.go`](../../example/blog/post_ext.go) writes it:

```go
func (u *PostUpdate) AddViewCount(n int64) *PostUpdate {
	u.Stmt().SetExpr("view_count", sqlb.Raw{SQL: "view_count + ?", Args: []any{n}})
	return u
}
```

Computed by the database rather than read into Go and written back, so
concurrent increments do not lose updates.

## The assembled server

From [`server_test.go`](../../example/blog/server_test.go), and this is the
point of the whole exercise:

```go
router := chi.NewRouter()
router.Use(middleware.RequestID, middleware.Recoverer)

api := humachi.New(router, huma.DefaultConfig("Blog", "1.0.0"))
if err := blog.Register(api, db); err != nil {
	t.Fatalf("mounting the blog resources: %v", err)
}
blog.RegisterPostSoftDelete(api, db)
```

Your router, your middleware, one generated call that mounts every resource the
schema exposes, and one hand-written call beside it. Two calls rather than a
wrapper — the generated half and the hand-written half compose because neither
owns the router.

One detail in that file is worth knowing before you meet it: `posts` declares
`SoftDelete`, so **the resource does not mount until something filters the
column**. Registration returns an error naming the missing hook. That is
[ADR-0030](../architecture.md#declared-scope-is-required), and it is the difference
between a rule that can be forgotten and one that cannot.

## Then the tenant scope

The example has no authentication, so it does not scope by tenant — but its
tests show the shape, and it is one more predicate on the same hook:

```go
hooks.BeforeQuery(func(_ context.Context, q *sqlb.Builder[blog.Post]) error {
	q.Where(sqlb.F("org_id").Eq("acme"), sqlb.F("deleted_at").IsNull())
	return nil
})
```

The generated handlers know nothing about it. That is the argument.

## Where to go from here

- [`example/tasks`](../../example/tasks/) is the larger one, for when a page
  here raises a question this size of example cannot answer: a multi-tenant task
  manager, six tables, a workspace boundary held entirely by hooks, JWT
  authentication, a migration history, generated TypeScript, Dart and CLI clients,
  and a runnable server. It is tested against a real Postgres, so its claims
  about locking, triggers and constraints are checked rather than asserted. It
  also documents the two places it had to work around sqlb rather than use it.
- [Examples](../examples/README.md) — the gallery, including
  a lending library, a stock exchange, and the same domain built twice on sqlc
  for comparison.
- [Concepts](../concepts/README.md) — why any of this is shaped the way it is.
