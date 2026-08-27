package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// An enforced external reference may name the schema its target lives in, for
// an application sharing a database with schemas it does not own — a Supabase
// project's public tables referencing auth.users being the first case of it.
func TestEnforcedTargetAcceptsASchemaQualifiedTable(t *testing.T) {
	cases := []struct {
		target                string
		wantSchema, wantTable string
		wantColumn            string
		wantOK                bool
	}{
		{target: "organizations", wantTable: "organizations", wantColumn: "id", wantOK: true},
		{target: "organizations.id", wantTable: "organizations", wantColumn: "id", wantOK: true},
		{target: "auth.users.id", wantSchema: "auth", wantTable: "users", wantColumn: "id", wantOK: true},
		// The module boundary is still the boundary: a name with a module in
		// it cannot carry a constraint, however many dots follow.
		{target: "platform/users.users.id"},
		{target: "one.two.three.four"},
		{target: "auth..id"},
		{target: ""},
	}
	for _, c := range cases {
		ref := schema.ExternalRef("user", c.target).Enforced().Desc().Ref
		gotSchema, gotTable, gotColumn, ok := ref.EnforcedTarget()
		if ok != c.wantOK {
			t.Errorf("EnforcedTarget(%q) ok = %v; want %v", c.target, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if gotSchema != c.wantSchema || gotTable != c.wantTable || gotColumn != c.wantColumn {
			t.Errorf("EnforcedTarget(%q) = %q, %q, %q; want %q, %q, %q",
				c.target, gotSchema, gotTable, gotColumn, c.wantSchema, c.wantTable, c.wantColumn)
		}
	}
}

// Validate names the qualified form, because a rejection that lists only the
// spellings that existed before it would send a reader looking for a feature
// that is there.
func TestValidateOffersTheSchemaQualifiedFormForATargetItRefuses(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("profiles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.ExternalRef("user", "platform/users.users.id").Enforced(),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("a module-qualified target was accepted as enforced")
	}
	if !strings.Contains(err.Error(), "auth.users.id") {
		t.Errorf("the rejection does not name the schema-qualified form:\n%v", err)
	}
}
