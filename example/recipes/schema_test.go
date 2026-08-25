package recipes_test

import (
	"fmt"

	"github.com/mind-vm/sqlb/schema"
)

// The declaration the models in models.go are generated from. A schema file
// writes `schema.Table(…)`, which registers in the default registry; these
// recipes each use a registry of their own so that one recipe cannot affect
// the next.
//
// Everything a column can do is opt-in and said here once. That single
// statement is what becomes the `sqlb` struct tag, the REST capability, the
// OpenAPI parameter, the TypeScript type and the CLI flag — which is the reason
// the declaration is worth reading closely and the generated files are not.
func Example_schemaDeclaringATable() {
	app := schema.NewRegistry()

	org := app.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
		schema.Text("slug").Unique().Filterable(),
		schema.Timestamps(),
	)

	post := app.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		// A reference declares the foreign key, what a delete of the parent
		// does, and — because it says so — that ?expand=org may follow it.
		schema.Ref("org", org).OnDelete(schema.Cascade).Filterable().Expandable(),

		schema.Text("title").Searchable().Sortable(),
		schema.Enum("status", "draft", "review", "published").
			Default(schema.Value("draft")).
			Filterable().Sortable(),
		schema.BigInt("view_count").Default(schema.Value(0)).Filterable().Sortable().ReadOnly(),
		schema.Timestamp("published_at").Nullable().Filterable().Sortable(),
		schema.Text("tags").Array().Filterable(),

		schema.Timestamps(),
		schema.SoftDelete(),
	).
		Index("org_id", "status").
		Check("published_posts_have_a_date", "status <> 'published' OR published_at IS NOT NULL").
		Describe("A blog post.")

	fmt.Println("table:", post.Name())
	for _, f := range post.Fields() {
		fmt.Println(" ", f.Name())
	}
	// Output:
	// table: posts
	//   id
	//   org_id
	//   title
	//   status
	//   view_count
	//   published_at
	//   tags
	//   created_at
	//   updated_at
	//   deleted_at
}

// Expose is what publishes a table over HTTP, and a table without it is
// reachable from Go and has no REST surface at all. Leaving an operation out of
// Ops is how a table gets a read API and no delete — which is what a table
// declaring SoftDelete wants, since the generated delete is a real DELETE.
func Example_schemaExposeOverHTTP() {
	app := schema.NewRegistry()

	app.Table("drafts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
	)
	posts := app.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Searchable().Sortable(),
	).Expose(schema.REST{
		Path:            "/posts",
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 20,
		MaxPageSize:     100,
		MaxFilters:      12,
	})

	fmt.Println("exposed:", len(app.Exposed()))
	fmt.Println("path:   ", posts.Rest().Path)
	// Output:
	// exposed: 1
	// path:    /posts
}

// Validate answers "is this schema well-formed?" and returns errors. Lint
// answers "will it behave badly in production?" and returns advice.
//
// The distinction is worth the two functions: a table can validate completely
// and still expose an unindexed filter that sequential-scans on every request
// — the kind of mistake that is invisible in review and obvious at three in the
// morning. Diagnostics are advisory; a filterable column on a table of twenty
// rows does not need an index.
func Example_schemaLint() {
	app := schema.NewRegistry()

	app.Table("events",
		schema.UUIDv7("id").PrimaryKey(),
		// Filterable, and no index. That is the finding.
		schema.Text("source").Filterable(),
	)

	for _, d := range app.Lint() {
		fmt.Printf("%s (%s.%s)\n  %s\n  fix: %s\n", d.Rule, d.Table, d.Column, d.Message, d.Fix)
	}
	// Output:
	// unindexed-filter (events.source)
	//   column is filterable but is not the leading column of any index, so filtering on it scans the table
	//   fix: add .Index("source") to the table, or drop .Filterable() from the column
}

// A module is a registry whose tables carry its name, so ownership is visible
// in the database and cannot be forgotten. The prefix is applied by the
// registry rather than written into each declaration — a convention repeated at
// every call site is a convention that drifts.
func Example_schemaModulePrefix() {
	billing := schema.NewModule("billing")
	invoice := billing.Table("invoices",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Numeric("amount_due").Filterable(),
	)

	fmt.Println("declared as:", invoice.LocalName())
	fmt.Println("stored as:  ", invoice.Name())
	// Output:
	// declared as: invoices
	// stored as:   billing_invoices
}
