// Package noteschema is the schema definition for the fx example: the single
// source of truth that an author, or an agent, edits.
//
// It lives in its own package because the declarations here and the model
// structs generated from them share names — noteschema.Note is the table
// declaration, store.Note is the row struct. Keeping them apart is what lets
// both be called Note.
//
// # What this example is for
//
// example/blog is the shortest path from a schema to a server, and
// example/tasks is the same machinery at application scale. This one is about
// a third thing: how the pieces are *wired* when the application is assembled
// by a dependency-injection container rather than by a function that news
// everything up in order.
//
// The schema is therefore deliberately small — two tables, one tenant
// boundary — because it is not the subject. What it has to have is exactly
// one property: a Scoped column, so that the resources refuse to mount until
// the hooks that scope them are registered (ADR-0030). That refusal is what
// makes the container's ordering guarantee worth stating, and what the boot
// test asserts by taking the hooks away.
package noteschema

import "github.com/mind-vm/sqlb/schema"

// Space is the tenant. Every note belongs to exactly one, and which space a
// request may see is decided by the bearer key it presents — see the access
// package.
var Space = schema.Table("spaces",
	// Scoped on the key, because on this table the row *is* the tenant. There
	// is no space_id to point at, and a boundary that only covered the tables
	// carrying the column would leave GET /spaces listing every tenant in the
	// installation.
	schema.UUIDv7("id").PrimaryKey().Scoped(),
	schema.Text("name").Searchable().Sortable(),

	// The name the configured keys are written against, so it is the one
	// column the access module looks a space up by. Unique because two spaces
	// answering to one slug would make that lookup ambiguous in a way no
	// request could report.
	schema.Text("slug").Unique().Filterable(),
	schema.Timestamps(),
).
	Describe("A tenant. Every note belongs to exactly one.").
	// No create and no update: spaces are provisioned at boot from the
	// configured keys, because a space nobody holds a key for is unreachable
	// and a space anybody may create is not a boundary.
	Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

// Note is the table with a real REST surface, and the one every request is
// scoped to.
var Note = schema.Table("notes",
	schema.UUIDv7("id").PrimaryKey(),

	// ReadOnly, and it is the load-bearing word.
	//
	// ReadOnly keeps the column out of the generated create and patch bodies,
	// so no request can name the space it is writing into — and leaves the
	// BeforeCreate hook free to supply it from the verified key. A client that
	// sends one is not silently overruled, because there is nothing to send.
	//
	// Expandable so ?expand=space returns the tenant alongside the note; the
	// hook scopes that read too, since it is the same sqlb.Query the handler
	// issues.
	schema.Ref("space", Space).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped().
		Expandable().
		Inverse("notes").
		InverseExpandable(schema.ExpandOrder("-created_at"), schema.ExpandLimit(10)),

	schema.Text("title").Searchable().Sortable().Filterable(),
	schema.Text("body").Searchable(),
	schema.Enum("status", "draft", "published", "archived").
		Default(schema.Value("draft")).
		Filterable().
		Sortable(),
	schema.Bool("pinned").Default(schema.Value(false)).Filterable().Sortable(),
	schema.Timestamps(),
).
	// The index the scoped list endpoint needs: every read this server issues
	// has space_id in its WHERE clause, because the hook puts it there.
	Index("space_id", "status").
	Describe("A note, belonging to exactly one space.").
	Expose(schema.REST{
		Path:            "/notes",
		Ops:             schema.CRUD | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})
