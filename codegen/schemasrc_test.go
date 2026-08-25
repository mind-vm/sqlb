package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// adopted is a registry shaped like one introspect would return: real column
// types, pinned constraint names, an index that does not follow the naming
// convention, and no capabilities anywhere — because DDL does not record them.
func adopted() *schema.Registry {
	r := schema.NewRegistry()

	orgs := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Comment("display name"),
		schema.Text("slug").Unique().ConstraintNamed("orgs_slug_uniq"),
		schema.Varchar("region", 40).Nullable(),
		schema.Timestamp("created_at").Default(schema.Now()),
	).
		Describe("A tenant.").
		PrimaryKeyNamed("orgs_pk")

	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
		schema.Text("title"),
		schema.Enum("status", "draft", "published").Default(schema.Value("draft")),
		schema.Int("views").Default(schema.Value(0)),
		schema.Bool("pinned").Default(schema.Value(false)),
		schema.JSON("meta").Nullable(),
		schema.UUID("external_id").Nullable(),
	).
		Check("views_non_negative", "views >= 0").
		Index("org_id", "status").
		AddIndex(schema.Index{Name: "posts_meta_gin", Columns: []string{"meta"}, Method: "gin"})

	return r
}

func render(t *testing.T, r *schema.Registry) string {
	t.Helper()
	src, err := codegen.RenderSchema(r, codegen.SchemaOptions{Package: "adopted"})
	if err != nil {
		t.Fatalf("RenderSchema: %v", err)
	}
	return string(src)
}

// TestRenderedSchemaIsValidGo is carried by RenderSchema itself: it runs the
// output through go/format, which parses it. A generator bug that produces
// something unparseable fails here rather than at the consumer's next build.
func TestRenderedSchemaIsValidGo(t *testing.T) {
	src := render(t, adopted())

	for _, want := range []string{
		"package adopted",
		`import "github.com/mind-vm/sqlb/schema"`,
		`var Orgs = schema.Table("orgs",`,
		`var Posts = schema.Table("posts",`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
}

func TestConstructorsAndModifiers(t *testing.T) {
	src := render(t, adopted())

	for _, want := range []string{
		// UUIDv7 is a constructor, not a UUID with a default bolted on.
		`schema.UUIDv7("id").PrimaryKey()`,
		// A plain uuid with no generator stays UUID.
		`schema.UUID("external_id").Nullable()`,
		`schema.Varchar("region", 40).Nullable()`,
		`schema.Enum("status", "draft", "published").Default(schema.Value("draft"))`,
		`schema.Int("views").Default(schema.Value(0))`,
		`schema.Bool("pinned").Default(schema.Value(false))`,
		`schema.Timestamp("created_at").Default(schema.Now())`,
		`schema.Text("name").Comment("display name")`,
		`schema.Ref("org", Orgs).OnDelete(schema.Cascade)`,
		// Pinned names survive, or the first diff after adoption drops and
		// recreates the constraint they name.
		`ConstraintNamed("orgs_slug_uniq")`,
		`PrimaryKeyNamed("orgs_pk")`,
		`Check("views_non_negative", "views >= 0")`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
}

// TestIndexShorthandOnlyWhenItReproducesTheName guards the property that makes
// the shorthand safe. Index(cols...) derives a name; using it for an index
// called something else would silently rename it, and the first migration after
// adoption would drop the real one and build a new one under the derived name.
func TestIndexShorthandOnlyWhenItReproducesTheName(t *testing.T) {
	src := render(t, adopted())

	if !strings.Contains(src, `Index("org_id", "status")`) {
		t.Errorf("an index matching the derived name should use the shorthand:\n%s", src)
	}
	if !strings.Contains(src, `Name:    "posts_meta_gin"`) {
		t.Errorf("an index with its own name must keep it explicitly:\n%s", src)
	}
	if !strings.Contains(src, `Method:  "gin"`) {
		t.Errorf("a non-btree method must survive:\n%s", src)
	}
}

// TestNoCapabilitiesAreInvented is the ADR-0014 claim, executable. An imported
// schema exposes nothing and can be asked nothing, so widening is a deliberate
// edit rather than something a generator decided.
func TestNoCapabilitiesAreInvented(t *testing.T) {
	src := render(t, adopted())

	for _, forbidden := range []string{
		"Filterable()", "Sortable()", "Searchable()", "Expandable()",
		"Expose(", "Hidden()", "ReadOnly()", "Immutable()",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("rendered %q, which cannot be read from DDL:\n%s", forbidden, src)
		}
	}
}

// TestReferencesAreDeclaredBeforeUse is what makes the output compile. A table
// declared before the one it points at would reference a Go variable that does
// not exist yet.
func TestReferencesAreDeclaredBeforeUse(t *testing.T) {
	r := schema.NewRegistry()
	// Declared so that alphabetical order is the wrong order: zebras is
	// referenced by antelopes, and sorting by name alone would emit antelopes
	// first.
	zebras := r.Table("zebras", schema.UUIDv7("id").PrimaryKey())
	r.Table("antelopes",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("zebra", zebras),
	)

	src := render(t, r)
	if strings.Index(src, "var Zebras") > strings.Index(src, "var Antelopes") {
		t.Errorf("a reference target must be declared first:\n%s", src)
	}
}

// TestAForeignKeyCycleTerminatesWithAnActionableError.
//
// The DSL cannot express a cycle — a reference takes the target table's value,
// so the target must already exist — and introspect refuses one before it ever
// builds a registry. The cycle is closed here by writing through the aliased
// FieldDesc, which is the only way to construct the input at all.
//
// It is worth testing anyway, because the check is not a guard against a bug
// somewhere else: it is what makes the depth-first walk terminate. Without it
// this input recurses until the stack runs out, and RenderSchema is exported
// and takes any registry it is handed.
func TestAForeignKeyCycleTerminatesWithAnActionableError(t *testing.T) {
	cyclic := schema.NewRegistry()
	a := cyclic.Table("a", schema.UUIDv7("id").PrimaryKey(), schema.Text("placeholder"))
	b := cyclic.Table("b", schema.UUIDv7("id").PrimaryKey(), schema.Ref("a", a))
	// a.placeholder becomes a reference to b, which the DSL would not let us
	// declare because b did not exist when a was written.
	ph := a.Field("placeholder").Desc()
	ph.Type = schema.TypeUUID
	ph.Ref = &schema.Reference{Name: "b", Table: b, Column: "id"}

	_, err := codegen.RenderSchema(cyclic, codegen.SchemaOptions{Package: "cyclic"})
	if err == nil {
		t.Fatal("a foreign key cycle rendered without complaint; the output cannot compile")
	}
	// The error has to name the tables and the remedy, or it sends the reader
	// to a Go initialisation-cycle message that explains nothing about schemas.
	for _, want := range []string{"cycle", "ExternalRef"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestModuleRegistryRendersItsOwnRegistry(t *testing.T) {
	r := schema.NewModule("billing")
	r.Table("invoices", schema.UUIDv7("id").PrimaryKey())

	src, err := codegen.RenderSchema(r, codegen.SchemaOptions{Package: "billing"})
	if err != nil {
		t.Fatalf("RenderSchema: %v", err)
	}
	got := string(src)

	if !strings.Contains(got, `var Module = schema.NewModule("billing")`) {
		t.Errorf("module registry not declared:\n%s", got)
	}
	// The declaration uses the local name; the registry applies the prefix.
	if !strings.Contains(got, `Module.Table("invoices"`) {
		t.Errorf("table should be declared on the module registry by local name:\n%s", got)
	}
	if strings.Contains(got, `"billing_invoices"`) {
		t.Errorf("the prefix must come from the registry, not be baked into the name:\n%s", got)
	}
}

func TestRenderSchemaRejectsMissingInputs(t *testing.T) {
	if _, err := codegen.RenderSchema(nil, codegen.SchemaOptions{Package: "x"}); err == nil {
		t.Error("a nil registry should be refused")
	}
	if _, err := codegen.RenderSchema(schema.NewRegistry(), codegen.SchemaOptions{}); err == nil {
		t.Error("a missing package name should be refused")
	}
}

// The bootstrap turns a database into declarations to review, so a construct it
// can read and cannot write back leaves the port stuck at exactly the table it
// was there to unblock (issue #53's shape, #132's construct).
func TestAutoColumnsRenderBack(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("audit_log",
		schema.BigSerial("id").PrimaryKey(),
		schema.SmallSerial("bucket"),
		schema.Serial("n"),
	)
	r.Table("steps",
		schema.BigInt("seq").Identity().PrimaryKey(),
		schema.Int("attempt").IdentityAlways(),
	)

	src := render(t, r)
	for _, want := range []string{
		// A serial is its own constructor, the way UUIDv7 is.
		`schema.BigSerial("id").PrimaryKey()`,
		`schema.SmallSerial("bucket")`,
		`schema.Serial("n")`,
		// An identity is a modifier, because that is how SQL spells it.
		`schema.BigInt("seq").Identity().PrimaryKey()`,
		`schema.Int("attempt").IdentityAlways()`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
	// The nextval a serial carries in the database is the serial, not a default
	// of its own — rendering it would produce a declaration binding to a
	// sequence nothing creates.
	if strings.Contains(src, "nextval") {
		t.Errorf("a serial rendered its sequence as a default:\n%s", src)
	}
}
