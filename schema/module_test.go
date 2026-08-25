package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// Two independent modules, arranged the way an fx application is: neither
// package imports the other, and both own tables.
func billingModule() *schema.Registry {
	m := schema.NewModule("billing")
	m.Table("invoices",
		schema.UUIDv7("id").PrimaryKey(),
		// The tenants module is not imported. The relationship is declared by
		// name and carries no foreign key.
		schema.ExternalRef("tenant", "tenants.id").Filterable().Indexed(),
		schema.Numeric("amount_due").Filterable().Sortable(),
		schema.Timestamp("created_at").Sortable(),
	).Index("created_at").Index("amount_due").
		Expose(schema.REST{Ops: schema.OpList, MaxPageSize: 100})
	return m
}

func auditModule() *schema.Registry {
	m := schema.NewModule("audit")
	// Same local name as nothing in billing, but the point is that a collision
	// would be impossible even if there were one.
	m.Table("invoices", schema.UUIDv7("id").PrimaryKey())
	return m
}

func TestModulesNamespaceTheirTables(t *testing.T) {
	billing, audit := billingModule(), auditModule()

	if got := billing.Tables()[0].Name(); got != "billing_invoices" {
		t.Errorf("storage name = %q, want billing_invoices", got)
	}
	if got := billing.Tables()[0].LocalName(); got != "invoices" {
		t.Errorf("local name = %q, want invoices", got)
	}
	if got := audit.Tables()[0].Name(); got != "audit_invoices" {
		t.Errorf("storage name = %q, want audit_invoices", got)
	}
	// The same local name in two modules must not collide.
	if billing.Tables()[0].Name() == audit.Tables()[0].Name() {
		t.Error("module prefixes did not disambiguate")
	}
}

// A module prefix is a storage concern. Leaking it into the URL would make
// moving a table between modules a breaking API change.
func TestModulePrefixDoesNotReachTheURL(t *testing.T) {
	rest := billingModule().Tables()[0].Rest()
	if rest.Path != "/invoices" {
		t.Errorf("REST path = %q, want /invoices", rest.Path)
	}
}

func TestExternalRefHasNoForeignKey(t *testing.T) {
	billing := billingModule()
	if err := billing.Validate(); err != nil {
		t.Fatalf("a cross-module reference should validate without its target: %v", err)
	}

	f := billing.Tables()[0].Field("tenant_id")
	if f == nil {
		t.Fatal("ExternalRef should produce tenant_id")
	}
	ref := f.Desc().Ref
	if !ref.External || ref.Target != "tenants.id" {
		t.Errorf("reference = %+v, want an external reference to tenants.id", ref)
	}
	if ref.Table != nil {
		t.Error("an external reference must not resolve to a table")
	}
}

// A soft foreign key exists to be joined on, so a module that declares one
// declares its index too — and the index is visible in the declaration rather
// than applied invisibly on the strength of the column being a reference.
func TestExternalRefIsIndexedWhenItSaysSo(t *testing.T) {
	billing := billingModule()
	var found bool
	for _, idx := range billing.Tables()[0].Indexes() {
		if len(idx.Columns) == 1 && idx.Columns[0] == "tenant_id" {
			found = true
			if !strings.HasPrefix(idx.Name, "billing_invoices_") {
				t.Errorf("index name %q should carry the storage table name", idx.Name)
			}
		}
	}
	if !found {
		t.Error("a column declaring Indexed should be indexed")
	}
	if w := billing.Lint().Warnings(); len(w) > 0 {
		t.Errorf("this module should lint clean, got:\n%s", w)
	}
}

// And a reference that does not say so is not indexed, which is the half that
// used to be untrue: the index arrived from the column's shape, so a registry
// introspect built out of a database claimed one that database did not have
// (#259). Lint is what carries the advice now.
func TestExternalRefIsNotIndexedByItself(t *testing.T) {
	m := schema.NewModule("billing")
	m.Table("invoices",
		schema.UUIDv7("id").PrimaryKey(),
		schema.ExternalRef("tenant", "tenants.id"),
	)

	if idx := m.Tables()[0].Indexes(); len(idx) != 0 {
		t.Errorf("an undeclared index appeared: %+v", idx)
	}

	var named bool
	for _, d := range m.Lint() {
		if d.Rule == "unindexed-ref" && d.Column == "tenant_id" {
			named = true
			if !strings.Contains(d.Fix, ".Indexed()") {
				t.Errorf("the fix should name the word that adds the index, got %q", d.Fix)
			}
		}
	}
	if !named {
		t.Errorf("Lint said nothing about an unindexed reference:\n%s", m.Lint().Warnings())
	}
}

// Expanding across a module boundary would join a table the module does not
// own, which is the coupling the architecture exists to prevent.
func TestExternalRefCannotBeExpanded(t *testing.T) {
	m := schema.NewModule("billing")
	m.Table("invoices",
		schema.UUIDv7("id").PrimaryKey(),
		schema.ExternalRef("tenant", "tenants.id").Expandable(),
	)
	err := m.Validate()
	if err == nil {
		t.Fatal("expanding across a module boundary should be rejected")
	}
	if !strings.Contains(err.Error(), "module boundary") {
		t.Errorf("the error should explain why: %v", err)
	}
}

func TestExternalRefTypeIsOverridable(t *testing.T) {
	m := schema.NewModule("legacy")
	m.Table("rows",
		schema.UUIDv7("id").PrimaryKey(),
		schema.ExternalRef("account", "accounts.id").OfType(schema.TypeBigInt).Named("acct_no"),
	)
	f := m.Tables()[0].Field("acct_no")
	if f == nil || f.Desc().Type != schema.TypeBigInt {
		t.Errorf("type and name overrides did not apply: %+v", f)
	}
}

func TestModuleAppearsInTheManifest(t *testing.T) {
	man := billingModule().BuildManifest()
	if man.Module != "billing" {
		t.Errorf("manifest module = %q", man.Module)
	}
	tm := man.Tables[0]
	if tm.Name != "billing_invoices" || tm.LocalName != "invoices" {
		t.Errorf("table manifest = %+v", tm)
	}

	// The relationship must be visible even though the database does not
	// enforce it, or nothing downstream knows it exists.
	var ref *schema.RefManifest
	for _, c := range tm.Columns {
		if c.Name == "tenant_id" {
			ref = c.References
		}
	}
	if ref == nil {
		t.Fatal("the external reference is missing from the manifest")
	}
	if !ref.External || ref.Enforced || ref.Target != "tenants.id" {
		t.Errorf("reference manifest = %+v", ref)
	}
}

func TestModuleNameMustBeAValidPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an invalid module name should panic at declaration")
		}
	}()
	schema.NewModule("Billing Module")
}
