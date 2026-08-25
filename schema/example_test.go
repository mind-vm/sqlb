package schema_test

import (
	"fmt"

	"github.com/mind-vm/sqlb/schema"
)

// A schema is written as ordinary Go values, which is what lets one declaration
// be the source of truth for migrations, models, REST handlers and OpenAPI.
//
// Capabilities are opt-in per column: a column that does not declare one cannot
// be reached through it. That is the difference between this and exposing the
// database — the failure is a 400 naming the allowed columns, not a leak.
func ExampleTable() {
	// A registry of its own keeps this example out of the default one. A schema
	// file would call schema.Table, which registers into the default registry.
	reg := schema.NewRegistry()

	posts := reg.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Searchable().Sortable(),
		schema.Enum("status", "draft", "review", "published").
			Default(schema.Value("draft")).
			Filterable().
			Sortable(),
		// Readable by Go code, but never serialised into a response — and not
		// filterable either, since a filterable secret can be recovered by
		// probing it one value at a time.
		schema.Text("password_hash").Hidden(),
		schema.Timestamps(),
	).
		Index("status").
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList, MaxPageSize: 100})

	for _, f := range posts.Fields() {
		d := f.Desc()
		fmt.Printf("%-13s %-11s %s\n", d.Name, d.Type, d.Capabilities())
	}
	fmt.Println("path:", posts.Rest().Path)
	fmt.Println("ops: ", posts.Rest().Ops)

	// Output:
	// id            uuid        pk,default,filter,readonly
	// title         text        filter,sort,search
	// status        enum        default,filter,sort
	// password_hash text        hidden
	// created_at    timestamptz default,sort,readonly
	// updated_at    timestamptz default,sort,readonly
	// path: /posts
	// ops:  create|read|update|delete|list
}

// The capabilities render into the `sqlb` struct tag that codegen writes onto
// the model, which is how the runtime engine reads them back without importing
// this package. That import direction is what keeps the engine usable without
// the DSL.
func ExampleFieldDesc_Capabilities() {
	fmt.Println(schema.Text("email").Unique().Searchable().Desc().Capabilities())
	fmt.Println(schema.Text("secret").Hidden().Desc().Capabilities())
	fmt.Println(schema.BigInt("views").Filterable().Sortable().ReadOnly().Desc().Capabilities())
	// Output:
	// filter,search
	// hidden
	// filter,sort,readonly
}

// Lint reports schema problems that compile fine but produce a bad database or a
// bad API — an unindexed filterable column, a table exposed without a primary
// key. It is worth running from a test, so the schema is checked in CI.
func ExampleRegistry_Lint() {
	reg := schema.NewRegistry()
	reg.Table("events",
		schema.UUIDv7("id").PrimaryKey(),
		// Filterable, but nothing indexes it: every filtered request is a
		// sequential scan.
		schema.Text("kind").Filterable(),
	).
		Expose(schema.REST{Ops: schema.OpList})

	for _, d := range reg.Lint() {
		fmt.Println(d)
	}
	// Output:
	// [warn] unindexed-filter: events.kind: column is filterable but is not the leading column of any index, so filtering on it scans the table
	//     fix: add .Index("kind") to the table, or drop .Filterable() from the column
	// [info] list-without-sort: events: list endpoint has no sortable column, so every client gets the same primary-key order and none can ask for another
	//     fix: mark at least one column .Sortable(), conventionally created_at
	// [info] no-max-page-size: events: no MaxPageSize, so the package default applies as the hard ceiling
	//     fix: set MaxPageSize on the REST exposure to a value this table can serve
}
