// Package evolveschema is the schema of a support desk, and the subject of
// docs/refactoring-a-database.md.
//
// What makes this example different from the others is what is *not* here. The
// declaration below is the current state and only the current state — there is
// no v1 package, no v2 package, and no record in Go of what any column used to
// be called. That is deliberate, because it is what a real project looks like:
// the history lives in ../migrations, as files a runner has already applied to
// databases you cannot edit.
//
// So the example is the pair. This file says what the schema *is*; the
// migration directory says how it got there; and pgtest/evolve_test.go replays
// the second and requires it to produce the first. Editing this file without
// adding a migration fails there, which is the only gate that can catch it —
// every file-comparison check in this repository would still pass.
//
// The one thing that does survive a revision here is a rename hint. See
// email_address below.
package evolveschema

import "github.com/mind-vm/sqlb/schema"

// Customer is who a ticket belongs to.
var Customer = schema.Table("customers",
	schema.UUIDv7("id").PrimaryKey(),

	// Revision 4 renamed this from "email". The hint is what makes it a RENAME
	// rather than a drop and an add, and a drop and an add would have deleted
	// every address in the table (ADR-0014).
	//
	// It is needed for exactly one release: the migration was generated once,
	// and after that no database has a column called "email" any more. A hint
	// whose old column no longer exists is ignored, so leaving it costs nothing
	// mechanically — but it reads as a claim about the current schema that
	// stopped being true, so it goes at the next edit to this table. It is still
	// here because the document walks through the revision that added it.
	schema.Text("email_address").RenamedFrom("email").Unique().Searchable(),

	schema.Text("name").Searchable().Sortable(),
	schema.Timestamps(),
).
	Describe("Whoever a ticket is on behalf of.").
	Expose(schema.REST{
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})

// SupportAgent is who a ticket is assigned to.
//
// Revision 2 added the table as "agents"; revision 4 renamed it. The hint is
// the table-level form of the one on email_address above, and carries the same
// expiry: it is here for one release, and for as long as the document needs to
// point at it.
var SupportAgent = schema.Table("support_agents",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("email").Unique(),
	schema.Text("name").Searchable().Sortable(),
	schema.Bool("active").Default(schema.Value(true)).Filterable(),
	schema.Timestamps(),
).
	RenamedFrom("agents").
	Describe("Someone who answers tickets.").
	Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

// Ticket is the table every revision in the history touched.
var Ticket = schema.Table("tickets",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Ref("customer", Customer).OnDelete(schema.Cascade).Filterable().Expandable(),

	// Revision 3 widened this from varchar(80). Widening is the safe direction:
	// every value that fit the old type fits the new one, so the ALTER needs no
	// scan of the data and cannot fail on a row. Going back the other way can,
	// which is why the document keeps them apart.
	schema.Text("subject").Searchable().Sortable(),

	schema.Text("body").Searchable(),
	schema.Enum("status", "open", "pending", "closed").
		Default(schema.Value("open")).
		Filterable().
		Sortable(),

	// Revision 2, with a default, which is what made it a safe addition.
	schema.Enum("priority", "low", "normal", "high", "urgent").
		Default(schema.Value("normal")).
		Filterable().
		Sortable(),

	schema.Timestamps(),
).
	// Revision 2 added this index alongside the priority column.
	Index("customer_id", "status").
	Describe("One request from one customer.").
	Expose(schema.REST{
		Ops:             schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		DefaultPageSize: 20,
		MaxPageSize:     100,
	})
