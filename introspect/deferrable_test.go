package introspect

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// deferrableCatalog is one table with a deferred UNIQUE on each of the two
// forms, plus a deferred foreign key — the kind the DSL cannot say.
func deferrableCatalog() *catalog {
	return &catalog{
		tables: []tableRow{{Name: "products"}, {Name: "variants"}},
		columns: []columnRow{
			{Table: "products", Name: "id", Type: "uuid", NotNull: true},
			{Table: "variants", Name: "id", Type: "uuid", NotNull: true},
			{Table: "variants", Name: "product_id", Type: "uuid", NotNull: true},
			{Table: "variants", Name: "option_signature", Type: "text", NotNull: true, Default: "''::text"},
			{Table: "variants", Name: "sku", Type: "text", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "products", Name: "products_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "variants", Name: "variants_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "variants", Name: "variants_product_id_option_signature_key", Type: "u",
				Columns: []string{"product_id", "option_signature"}, Deferrable: true, Deferred: true,
				Def: "UNIQUE (product_id, option_signature) DEFERRABLE INITIALLY DEFERRED"},
			{Table: "variants", Name: "variants_sku_key", Type: "u", Columns: []string{"sku"},
				Deferrable: true, Deferred: true,
				Def: "UNIQUE (sku) DEFERRABLE INITIALLY DEFERRED"},
			{Table: "variants", Name: "variants_product_id_fkey", Type: "f",
				Columns: []string{"product_id"}, RefTable: "products", RefCols: []string{"id"},
				OnDelete: "c", OnUpdate: "a", Deferrable: true, Deferred: true,
				Def: "FOREIGN KEY (product_id) REFERENCES products(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED"},
		},
	}
}

// Both spellings of a deferred UNIQUE come back declared, which is what makes
// the round trip a fixpoint *about* the property rather than blind to it.
func TestDeferredUniqueIsImported(t *testing.T) {
	reg, _, err := build(deferrableCatalog(), Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	variants := reg.Get("variants")
	if variants == nil {
		t.Fatal("variants was not imported")
	}

	uniques := variants.Uniques()
	if len(uniques) != 1 {
		t.Fatalf("uniques = %+v, want the composite one", uniques)
	}
	if uniques[0].Deferrable != schema.DeferredCheck {
		t.Errorf("the composite constraint imported as %q, want deferred", uniques[0].Deferrable)
	}

	var sku *schema.FieldDesc
	for _, f := range variants.Fields() {
		if d := f.Desc(); d.Name == "sku" {
			sku = d
		}
	}
	if sku == nil {
		t.Fatal("sku was not imported")
	}
	if sku.UniqueDeferrable != schema.DeferredCheck {
		t.Errorf("the column's constraint imported as %q, want deferred", sku.UniqueDeferrable)
	}
}

// A deferred foreign key has no spelling, so it is reported rather than dropped
// in silence. That is the half of #154 that matters: the round trip used to hold
// because both sides were blind to the same property, which is ADR-0016's
// failure mode stated about a field rather than about an object.
func TestDeferredForeignKeyIsReported(t *testing.T) {
	_, rep, err := build(deferrableCatalog(), Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.Empty() {
		t.Fatal("a deferred foreign key was imported with nothing reported")
	}
	var found *Skip
	for i, s := range rep.Skipped {
		if s.Object == "variants_product_id_fkey" {
			found = &rep.Skipped[i]
		}
	}
	if found == nil {
		t.Fatalf("the deferred foreign key is not in the report:\n%s", rep)
	}
	if !strings.Contains(found.Reason, "deferrable") {
		t.Errorf("the reason does not name the property: %q", found.Reason)
	}
	// The definition rides along, so the constraint can be carried over by hand
	// without going back to the database to look it up.
	if !strings.Contains(found.Def, "DEFERRABLE") {
		t.Errorf("the report does not carry the definition: %q", found.Def)
	}
	// And the unique constraints are not reported, because those it can declare.
	for _, s := range rep.Skipped {
		if strings.HasSuffix(s.Object, "_key") {
			t.Errorf("a declarable constraint was reported: %+v", s)
		}
	}
}

// Nothing is reported for a schema that defers nothing, so the new entries
// cannot turn an ordinary adoption into a list of things to reconcile.
func TestUndeferredConstraintsAreNotReported(t *testing.T) {
	cat := deferrableCatalog()
	for i := range cat.constraints {
		cat.constraints[i].Deferrable = false
		cat.constraints[i].Deferred = false
	}
	_, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("nothing here defers, and this was reported:\n%s", rep)
	}
}
