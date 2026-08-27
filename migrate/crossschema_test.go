package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// An enforced external reference naming another schema emits a foreign key to
// that schema, with both halves quoted separately — "auth"."users", never the
// single identifier "auth.users", which names a table with a dot in its name.
func TestEnforcedRefToAnotherSchemaRendersQualified(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("profiles",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("user", "auth.users.id").Enforced().OnDelete(schema.Cascade),
		)
	})

	sql := render(diff(t, schema.NewRegistry(), target))
	if !strings.Contains(sql, `REFERENCES "auth"."users" ("id") ON DELETE CASCADE`) {
		t.Errorf("the foreign key does not name the target's schema:\n%s", sql)
	}
	if strings.Contains(sql, `"auth.users"`) {
		t.Errorf("the schema and the table were quoted as one identifier:\n%s", sql)
	}
}

// A rename map names tables in the schema being migrated, so a reference out of
// it is not in that map. The case that proves it is the one a shared database
// produces: auth.users alongside a local users, where matching on the bare name
// would re-point a platform's foreign key at this application's table.
func TestRenamingALocalTableLeavesASameNamedTableInAnotherSchemaAlone(t *testing.T) {
	decl := func(local string) func(*schema.Registry) {
		return func(r *schema.Registry) {
			users := r.Table(local, schema.UUIDv7("id").PrimaryKey())
			if local != "users" {
				users.RenamedFrom("users")
			}
			r.Table("profiles",
				schema.UUIDv7("id").PrimaryKey(),
				schema.ExternalRef("account", "auth.users.id").Enforced(),
			)
		}
	}

	changes := diff(t, build(decl("users")), build(decl("members")))
	sql := render(changes)
	if strings.Contains(sql, "profiles_account_id_fkey") {
		t.Errorf("renaming the local users table disturbed a foreign key into auth:\n%s", sql)
	}
	if strings.Contains(sql, `REFERENCES "auth"."members"`) {
		t.Errorf("the rename was applied to a table in another schema:\n%s", sql)
	}
}
