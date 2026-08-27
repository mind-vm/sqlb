package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// sharedDatabase declares what an application looks like when it does not own
// the whole database: a table of its own keyed to a row of auth.users, which
// is a platform's table in a platform's schema — the arrangement a Supabase
// project puts every application into.
//
// The local users table is the part that makes this worth a database: two
// tables called users exist, in two schemas, and only one of them is this
// application's.
func sharedDatabase() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("nickname").Filterable(),
	)
	r.Table("workspaces",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Filterable(),
		schema.ExternalRef("owner", "auth.users.id").Enforced().OnDelete(schema.Cascade),
	)
	return r
}

// A declaration naming another schema applies, reads back as the same
// declaration, and proposes nothing against the database its own migration
// built.
func TestForeignKeyIntoAnotherSchemaIsAFixpoint(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`CREATE SCHEMA auth`,
		`CREATE TABLE auth.users (id uuid PRIMARY KEY, email text NOT NULL)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("creating the platform schema: %v\n%s", err, stmt)
		}
	}

	declared := sharedDatabase()
	applySchema(t, db, declared)

	// The constraint is real: Postgres accepted it, and it points where the
	// declaration said rather than at the local users table.
	var target string
	if err := db.QueryRow(ctx,
		`SELECT ft.relnamespace::regnamespace::text || '.' || ft.relname
		   FROM pg_constraint con
		   JOIN pg_class ft ON ft.oid = con.confrelid
		  WHERE con.conname = 'workspaces_owner_id_fkey'`).Scan(&target); err != nil {
		t.Fatalf("reading the constraint back: %v", err)
	}
	if target != "auth.users" {
		t.Fatalf("the foreign key points at %s; want auth.users", target)
	}

	current, report, err := introspect.Registry(ctx, sqlb.New(db), introspect.Options{})
	if err != nil {
		t.Fatalf("reading the database back: %v", err)
	}
	if !report.Empty() {
		t.Fatalf("a schema of declarable constructs reported gaps:\n%s", report)
	}
	if got := current.Get("workspaces").Field("owner_id").Desc().Ref; got == nil || got.Target != "auth.users.id" {
		t.Fatalf("the key imported as %+v; want an external ref to auth.users.id", got)
	}

	changes, err := migrate.Diff(current, declared)
	if err != nil {
		t.Fatalf("diffing the declaration against its own database: %v", err)
	}
	if len(changes) != 0 {
		var b strings.Builder
		for _, c := range changes {
			b.WriteString("\n  " + strings.TrimSpace(c.Up))
		}
		t.Errorf("a declaration naming another schema proposed %d change(s) against "+
			"the database its own migration built:%s", len(changes), b.String())
	}
}
