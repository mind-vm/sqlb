package introspect

import (
	"strings"
	"testing"
)

// crossSchemaCatalog is the shape a Supabase project's public schema has: a
// local users table, and a workspace owned by a row of auth.users — a table in
// a schema this application shares the database with and does not own.
//
// The names are chosen so the collision can actually happen: tables are built
// in dependency order, which falls back to alphabetical, so "users" is built
// before "workspaces" and is therefore *available* to be bound to by mistake
// when the key naming it is read. A referencing table sorting first would pass
// this test with no guard in the code at all.
func crossSchemaCatalog() *catalog {
	return &catalog{
		tables: []tableRow{{Name: "users"}, {Name: "workspaces"}},
		columns: []columnRow{
			{Table: "workspaces", Name: "id", Type: "uuid", NotNull: true},
			{Table: "workspaces", Name: "owner_id", Type: "uuid", NotNull: true},
			{Table: "users", Name: "id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "workspaces", Name: "workspaces_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "users", Name: "users_pkey", Type: "p", Columns: []string{"id"}},
			{
				Table: "workspaces", Name: "workspaces_owner_id_fkey", Type: "f",
				Columns: []string{"owner_id"}, RefTable: "users", RefSchema: "auth",
				RefCols: []string{"id"}, OnDelete: "c",
				Def: "FOREIGN KEY (owner_id) REFERENCES auth.users(id) ON DELETE CASCADE",
			},
		},
	}
}

// A foreign key out of the schema being read imports as an enforced external
// reference that names the schema, so the declaration says what the database
// says and the next diff proposes nothing.
func TestForeignKeyIntoAnotherSchemaKeepsItsSchema(t *testing.T) {
	r, rep, err := build(crossSchemaCatalog(), Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("a declarable foreign key was reported as a gap:\n%s", rep)
	}
	ref := r.Get("workspaces").Field("owner_id").Desc().Ref
	if ref == nil {
		t.Fatal("the foreign key was dropped")
	}
	if ref.Target != "auth.users.id" {
		t.Errorf("target = %q; want auth.users.id", ref.Target)
	}
	refSchema, table, column, ok := ref.EnforcedTarget()
	if !ok || refSchema != "auth" || table != "users" || column != "id" {
		t.Errorf("EnforcedTarget() = %q, %q, %q, %v; want auth, users, id, true",
			refSchema, table, column, ok)
	}
}

// The bare name is not enough to match on. A local users table alongside
// auth.users is the arrangement every Supabase project that keeps its own
// user rows has, and binding the key to the local one would declare a
// reference the database does not have — then generate a migration that
// makes the database agree with the declaration.
func TestForeignKeyIntoAnotherSchemaDoesNotBindToTheLocalTableOfThatName(t *testing.T) {
	r, _, err := build(crossSchemaCatalog(), Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ref := r.Get("workspaces").Field("owner_id").Desc().Ref
	if ref == nil {
		t.Fatal("the foreign key was dropped")
	}
	if !ref.External {
		t.Fatalf("the key bound to the local %q table", ref.Table.Name())
	}
	if strings.Count(ref.Target, ".") != 2 {
		t.Errorf("target = %q; want the schema-qualified auth.users.id", ref.Target)
	}
}
