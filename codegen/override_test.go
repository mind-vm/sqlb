package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// overrideSchema is the shape the boundary tests need: a uuid key, a uuid
// reference, an enum, an amount worth a decimal, a nullable and an array
// column — so each of the ways an override composes has something to compose
// with.
func overrideSchema() *schema.Registry {
	r := schema.NewRegistry()
	orgs := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
	)
	r.Table("invoices",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).Filterable(),
		schema.Numeric("amount").Filterable().Sortable(),
		schema.Numeric("discount").Nullable(),
		schema.Enum("status", "draft", "sent", "paid").Default(schema.Value("draft")).Filterable(),
		schema.Text("tags").Array(),
		schema.Timestamps(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

// generateWith is the sibling of generate() in codegen_test.go, keyed by path
// relative to the output directory rather than by basename — the client
// emitters write into subdirectories, and which subdirectory is part of what
// these tests assert.
func generateWith(t *testing.T, opts codegen.Options) map[string]string {
	t.Helper()
	if opts.Registry == nil {
		opts.Registry = overrideSchema()
	}
	if opts.Package == "" {
		opts.Package = "gen"
	}
	opts.Dir = t.TempDir()
	if opts.ClientImportPath == "" {
		// Absolute Dir, so nothing can derive it. See Options.ClientImportPath.
		opts.ClientImportPath = "example.com/app/cli/client"
	}

	files, err := codegen.Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(opts.Dir, f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)] = string(b)
	}
	return out
}

func uuidOverride() codegen.TypeOverride {
	return codegen.TypeOverride{
		Type: schema.TypeUUID, GoType: "uuid.UUID", Import: "github.com/google/uuid",
	}
}

func TestOverrideReachesTheGeneratedGo(t *testing.T) {
	files := generateWith(t, codegen.Options{Types: []codegen.TypeOverride{uuidOverride()}})

	models := files["models_gen.go"]
	for _, want := range []string{
		`"github.com/google/uuid"`,
		"ID uuid.UUID `db:\"id\"",
		"OrgID uuid.UUID `db:\"org_id\"",
	} {
		if !contains(models, want) {
			t.Errorf("models are missing %q:\n%s", want, models)
		}
	}

	// The typed facade carries the overridden type as its parameter, so a
	// comparison against a bare string stops compiling in the consumer.
	cols := files["columns_gen.go"]
	if !contains(cols, "sqlb.Col[uuid.UUID]") {
		t.Errorf("the typed facade did not take the override:\n%s", cols)
	}
	// And the REST body, which is the third place a Go type is written.
	if rest := files["rest_gen.go"]; !contains(rest, "uuid.UUID") {
		t.Errorf("the REST bodies did not take the override:\n%s", rest)
	}
}

// An override's import belongs to the files that render the overridden column,
// and rest_gen.go renders a strict subset of them: the body columns of the
// exposed tables, which exclude every primary key, every read-only and hidden
// column, and every table that is not exposed at all.
//
// The models and columns emitters render every column of every table, so the
// registry-wide set ov.imports returns is exactly right for them. Taking it
// here imported uuid into a rest_gen.go for a schema whose only uuid column is
// a primary key — the same unused-import defect as the "time" one next door,
// arriving through a different door.
func TestRestImportsAnOverrideOnlyWhereABodyNamesIt(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("title")).
		Expose(schema.REST{Ops: schema.OpList})
	files := generateWith(t, codegen.Options{
		Registry: r, Types: []codegen.TypeOverride{uuidOverride()},
	})

	if rest := files["rest_gen.go"]; contains(rest, "github.com/google/uuid") {
		t.Errorf("the only uuid column is a primary key, which no body carries:\n%s", rest)
	}
	// The models file renders that same primary key, so there the import is
	// not merely allowed but required — this is a narrowing, not a removal.
	if models := files["models_gen.go"]; !contains(models, "github.com/google/uuid") {
		t.Errorf("models names uuid.UUID without importing it:\n%s", models)
	}

	// And a body that does carry an overridden column still gets it. The
	// reference is not a primary key, so it is a column a create body writes.
	r = schema.NewRegistry()
	orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
	).Expose(schema.REST{Ops: schema.OpCreate})
	rest := generateWith(t, codegen.Options{
		Registry: r, Types: []codegen.TypeOverride{uuidOverride()},
	})["rest_gen.go"]

	if !contains(rest, "github.com/google/uuid") {
		t.Errorf("a create body carrying a uuid.UUID does not import it:\n%s", rest)
	}
}

// An override replaces a column's Go type and not the fact that it is
// computed, so the ComputedColumns method is emitted either way and needs its
// sqlb import either way.
//
// The import was recorded behind the guard that skips an overridden column,
// which is right for the stdlib imports the default mapping decides — an
// override brings its own — and wrong for this one, which is earned by the
// method rather than by the field's type. An overridden computed column
// produced a models file declaring ComputedColumns() []sqlb.Computed with
// nothing importing sqlb.
func TestOverriddenComputedColumnStillImportsSqlb(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Int("open_tasks"),
		schema.Computed("is_overdue", schema.TypeBool,
			schema.FromSQL("open_tasks > 0")).NotNull().Filterable(),
	)
	// The override has to match the computed column, which is what puts it
	// behind the guard. Matching by type is the narrowest way to say so.
	models := generateWith(t, codegen.Options{
		Registry: r,
		Types: []codegen.TypeOverride{
			{Type: schema.TypeBool, GoType: "pgtype.Bool", Import: "github.com/jackc/pgx/v5/pgtype"},
		},
	})["models_gen.go"]

	for _, want := range []string{
		"IsOverdue pgtype.Bool", // the override did apply
		"func (Project) ComputedColumns() []sqlb.Computed {",
		`"github.com/mind-vm/sqlb"`,
		`"github.com/jackc/pgx/v5/pgtype"`,
	} {
		if !contains(models, want) {
			t.Errorf("models missing %q:\n%s", want, models)
		}
	}
}

// The four things an override must not reach. This is the record's actual
// claim, and it is why the test names each of them separately rather than
// asserting one golden file.
func TestOverrideDoesNotReachTheWire(t *testing.T) {
	files := generateWith(t, codegen.Options{
		Types:         []codegen.TypeOverride{uuidOverride()},
		TSDir:         "web",
		DartDir:       "mobile",
		CLIDir:        "cli",
		TSQueriesFile: "-",
	})

	for name, src := range files {
		switch {
		// The generated Go is where the override belongs — except the CLI,
		// which is Go that speaks HTTP and therefore reads the wire.
		case strings.HasSuffix(name, ".go") && !strings.Contains(name, "cli"):
			continue
		// The manifest describes the generated Go rather than the API, and
		// TestOverrideReachesTheManifest asserts that it does.
		case name == "sqlb.json":
			continue
		}
		if strings.Contains(src, "uuid.UUID") {
			t.Errorf("%s mentions the overridden Go type; the wire did not change", name)
		}
	}

	// Positively, rather than only by absence: the clients still describe a
	// uuid as the string it is on the wire.
	if ts := files["web/client.gen.ts"]; !contains(ts, "id: string;") {
		t.Errorf("the TypeScript client should still call a uuid a string:\n%s", ts)
	}
	if dart := files["mobile/client.gen.dart"]; !contains(dart, "String get id") {
		t.Errorf("the Dart client should still call a uuid a String")
	}
}

// The manifest describes what was generated, because that is what a reader
// deciding how to call the generated code needs.
func TestOverrideReachesTheManifest(t *testing.T) {
	files := generateWith(t, codegen.Options{Types: []codegen.TypeOverride{uuidOverride()}})
	manifest := files["sqlb.json"]

	if !contains(manifest, `"goType": "uuid.UUID"`) {
		t.Errorf("the manifest still reports the default mapping:\n%s", manifest)
	}
	// The logical type is untouched: it is what the DDL and the clients read.
	if !contains(manifest, `"type": "uuid"`) {
		t.Error("the manifest's logical type should not have moved")
	}
}

// Nullable and Array wrap whatever the override named, in the place they always
// did — so neither needs the override to know about it.
func TestOverrideComposesWithNullableAndArray(t *testing.T) {
	files := generateWith(t, codegen.Options{Types: []codegen.TypeOverride{
		{Type: schema.TypeNumeric, GoType: "decimal.Decimal", Import: "github.com/shopspring/decimal"},
		{Column: "tags", GoType: "tag.Tag", Import: "example.com/tag"},
	}})
	models := files["models_gen.go"]

	for _, want := range []string{
		"Amount decimal.Decimal",    // required
		"Discount *decimal.Decimal", // nullable
		"Tags []tag.Tag",            // array
	} {
		if !contains(models, want) {
			t.Errorf("models are missing %q:\n%s", want, models)
		}
	}
}

// Specificity, and the refusal when two rules tie.
func TestOverrideSpecificity(t *testing.T) {
	files := generateWith(t, codegen.Options{Types: []codegen.TypeOverride{
		uuidOverride(),
		// Narrower than the type rule, so it wins for this one column.
		{Table: "invoices", Column: "org_id", GoType: "orgs.ID", Import: "example.com/orgs"},
	}})
	models := files["models_gen.go"]
	if !contains(models, "OrgID orgs.ID") {
		t.Errorf("the narrower override should have won:\n%s", models)
	}
	if !contains(models, "ID uuid.UUID `db:\"id\"") {
		t.Errorf("the broader override should still apply elsewhere:\n%s", models)
	}
}

func TestOverrideRefusals(t *testing.T) {
	tests := []struct {
		name string
		over []codegen.TypeOverride
		want string
	}{
		{
			// The value set is the feature; replacing the type discards it.
			name: "an enum cannot be overridden",
			over: []codegen.TypeOverride{{Column: "status", GoType: "Status"}},
			want: "is an enum and cannot be type-overridden",
		},
		{
			name: "no GoType",
			over: []codegen.TypeOverride{{Type: schema.TypeUUID}},
			want: "has no GoType",
		},
		{
			name: "no matcher matches everything",
			over: []codegen.TypeOverride{{GoType: "any"}},
			want: "matches every column",
		},
		{
			// Which one applied would otherwise depend on the order written.
			name: "two rules of equal specificity",
			over: []codegen.TypeOverride{
				{Column: "amount", GoType: "decimal.Decimal"},
				{Column: "amount", GoType: "money.Amount"},
			},
			want: "equal specificity",
		},
		{
			// Almost always a typo, and it fails silently by definition.
			name: "a rule that matches nothing",
			over: []codegen.TypeOverride{{Table: "invoices", Column: "amuont", GoType: "x.Y"}},
			want: "matches no column in the schema",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := codegen.Generate(codegen.Options{
				Registry: overrideSchema(),
				Package:  "gen",
				Dir:      t.TempDir(),
				Types:    tt.over,
			})
			if err == nil {
				t.Fatalf("the override was accepted, want a refusal mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// No overrides is the common case and has to be exactly what it was.
func TestNoOverridesChangesNothing(t *testing.T) {
	plain := generateWith(t, codegen.Options{})
	if !contains(plain["models_gen.go"], "ID string") {
		t.Errorf("the default mapping moved:\n%s", plain["models_gen.go"])
	}
	// The import, not the word: since #84 the struct tag carries the logical
	// type, so every uuid column now says "uuid" in its tag and a bare
	// substring check passes for the wrong reason.
	if contains(plain["models_gen.go"], `"github.com/google/uuid"`) {
		t.Error("an unconfigured project should not acquire a uuid import")
	}
}
