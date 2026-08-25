// Package appschema is the host: an application that owns two tables of its
// own and composes a library's third into the same registry.
//
// This is the composition the convention exists for. The application imports
// sessionkit; sessionkit imports nothing of the application. What crosses is a
// registry and one *schema.TableDef, and what comes back is a declaration the
// host's own `sqlb generate`, `sqlb migrate` and drift check treat exactly like
// the ones written here.
//
// The alternative — a library owning its own goose sequence — costs the two
// foreign keys below, which is not a small thing: without them, deleting a user
// leaves their sessions behind and nothing but application code says otherwise.
package appschema

import (
	"github.com/mind-vm/sqlb/example/libtables/sessionkit"
	"github.com/mind-vm/sqlb/schema"
)

// Registry is the application's one registry. Everything that ends up in this
// database is declared into it, whoever wrote the declaration.
var Registry = schema.NewRegistry()

// Workspace is the tenant, and the host's own table.
var Workspace = Registry.Table("workspaces",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("name").Searchable().Sortable(),
	schema.Timestamps(),
).Describe("A tenant.")

// User is the table the library will point at, and it is declared before the
// call that references it — a reference resolves against a *schema.TableDef
// value, so the ordering here is Go's, not the database's.
var User = Registry.Table("users",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Ref("workspace", Workspace).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),
	schema.Text("email").Unique().Searchable(),
	schema.Timestamps(),
).Describe("A person with an account.")

// Session is the library's table, in this application's registry.
//
// Options is where the host answers what the library cannot know: that there is
// a user table to point at, and that rows here are confined by workspace like
// everything else in this schema.
var Session = sessionkit.Declare(Registry, sessionkit.Options{
	Users: User,
	Scope: "workspace_id",
}).Sessions
