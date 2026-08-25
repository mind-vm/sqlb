package introspect

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A foreign key whose target is not in the schema being read used to be
// dropped, so a drift gate proposed dropping a live constraint forever. It
// imports as an enforced external reference instead (issue #55).
func TestForeignKeyOutOfSchemaImportsAsEnforced(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "projects"}},
		columns: []columnRow{
			{Table: "projects", Name: "id", Type: "uuid", NotNull: true},
			{Table: "projects", Name: "org_id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "projects", Name: "projects_pkey", Type: "p", Columns: []string{"id"}},
			{
				Table: "projects", Name: "projects_org_id_fkey", Type: "f",
				Columns: []string{"org_id"}, RefTable: "organizations", RefCols: []string{"id"},
				OnDelete: "c",
				Def:      "FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE",
			},
		},
	}

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	f := r.Get("projects").Field("org_id")
	if f == nil {
		t.Fatal("org_id was dropped")
	}
	ref := f.Desc().Ref
	switch {
	case ref == nil:
		t.Fatal("the foreign key was not imported at all, so a diff would propose dropping it")
	case !ref.External:
		t.Error("the target is not in this schema, so the reference has to be external")
	case !ref.Enforced:
		t.Error("the constraint is real, and an unenforced reference would diff as a drop")
	}
	if table, column, ok := ref.EnforcedTarget(); !ok || table != "organizations" || column != "id" {
		t.Errorf("target = %s.%s (ok=%t), want organizations.id", table, column, ok)
	}
	if ref.OnDelete != schema.Cascade {
		t.Errorf("ON DELETE = %q, want cascade", ref.OnDelete)
	}
	// The conventional constraint name is not pinned, for the reason every
	// other conventional name is not: a pin is noise when the generated name
	// already matches.
	if f.Desc().ConstraintName != "" {
		t.Errorf("constraint name should not be pinned when it is the conventional one, got %q",
			f.Desc().ConstraintName)
	}
	if strings.Contains(rep.String(), "imported without it") {
		t.Errorf("nothing should be reported as lost:\n%s", rep)
	}
}

// The column type comes from the column, not from a target this import cannot
// see — a bigint key stays a bigint.
func TestEnforcedExternalRefKeepsTheColumnType(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "projects"}},
		columns: []columnRow{
			{Table: "projects", Name: "id", Type: "uuid", NotNull: true},
			{Table: "projects", Name: "owner_id", Type: "bigint", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "projects", Name: "projects_pkey", Type: "p", Columns: []string{"id"}},
			{
				Table: "projects", Name: "fk_owner", Type: "f",
				Columns: []string{"owner_id"}, RefTable: "accounts", RefCols: []string{"account_id"},
				Def: "FOREIGN KEY (owner_id) REFERENCES accounts(account_id)",
			},
		},
	}

	r, _, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	d := r.Get("projects").Field("owner_id").Desc()
	if d.Type != schema.TypeBigInt {
		t.Errorf("type = %s, want bigint", d.Type)
	}
	if _, column, _ := d.Ref.EnforcedTarget(); column != "account_id" {
		t.Errorf("referenced column = %q, want account_id — a foreign key need not point at a primary key", column)
	}
	// A name that is not the convention is pinned, or the diff renames a live
	// constraint.
	if d.ConstraintName != "fk_owner" {
		t.Errorf("constraint name = %q, want fk_owner", d.ConstraintName)
	}
}

// A target that cannot be declared is still reported rather than smuggled
// through as a constraint against a name that is not an identifier.
func TestUndeclarableForeignKeyTargetIsStillReported(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "projects"}},
		columns: []columnRow{
			{Table: "projects", Name: "id", Type: "uuid", NotNull: true},
			{Table: "projects", Name: "org_id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "projects", Name: "projects_pkey", Type: "p", Columns: []string{"id"}},
			{
				Table: "projects", Name: "projects_org_id_fkey", Type: "f",
				Columns: []string{"org_id"}, RefTable: "userOrganizations", RefCols: []string{"id"},
				Def: "FOREIGN KEY (org_id) REFERENCES \"userOrganizations\"(id)",
			},
		},
	}

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if d := r.Get("projects").Field("org_id").Desc(); d.Ref != nil {
		t.Error("a target whose name the DSL cannot declare should not become a constraint")
	}
	if !strings.Contains(rep.String(), "cannot be declared") {
		t.Errorf("the report should say why:\n%s", rep)
	}
}
