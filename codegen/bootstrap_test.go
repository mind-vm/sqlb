package codegen_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// The bootstrap: a database read by introspect, written back out as the schema
// package a project then owns and edits.
//
// It is the multiplier for adopting an existing database — sixty-nine
// declarations to review rather than sixty-nine to write — so a type it cannot
// write is not a cosmetic gap. It is the difference between the tool doing the
// work and the tool watching (issue #53).

// sample builds a column of every logical type, which is the point: the test
// walks schema.Types() rather than a list somebody remembered to extend.
func sample(t schema.Type) *schema.Field {
	switch t {
	case schema.TypeText:
		return schema.Text("f")
	case schema.TypeVarchar:
		return schema.Varchar("f", 200)
	case schema.TypeSmallInt:
		return schema.SmallInt("f")
	case schema.TypeInt:
		return schema.Int("f")
	case schema.TypeBigInt:
		return schema.BigInt("f")
	case schema.TypeReal:
		return schema.Real("f")
	case schema.TypeFloat:
		return schema.Float("f")
	case schema.TypeNumeric:
		return schema.Numeric("f")
	case schema.TypeBool:
		return schema.Bool("f")
	case schema.TypeUUID:
		return schema.UUID("f")
	case schema.TypeTimestamp:
		return schema.Timestamp("f")
	case schema.TypeDate:
		return schema.Date("f")
	case schema.TypeTime:
		return schema.Time("f")
	case schema.TypeJSON:
		return schema.JSON("f")
	case schema.TypeBytes:
		return schema.Bytes("f")
	case schema.TypeEnum:
		return schema.Enum("f", "a", "b")
	case schema.TypeVector:
		return schema.Vector("f", 1536)
	}
	return nil
}

// Every type the DSL has can be written back out as source. A type that
// introspect can read and RenderSchema cannot write blocks the whole bootstrap
// on one column.
func TestRenderSchemaWritesEveryType(t *testing.T) {
	for _, typ := range schema.Types() {
		t.Run(string(typ), func(t *testing.T) {
			f := sample(typ)
			if f == nil {
				t.Fatalf("schema.Types() lists %q and this test has no sample for it; "+
					"add one, because the gap it would otherwise hide is the whole finding", typ)
			}

			r := schema.NewRegistry()
			r.Table("things", schema.UUIDv7("id").PrimaryKey(), f)

			src, err := codegen.RenderSchema(r, codegen.SchemaOptions{Package: "thingschema"})
			if err != nil {
				t.Fatalf("a type the DSL declares must be writable as source: %v", err)
			}
			if !strings.Contains(string(src), `"f"`) {
				t.Errorf("the column is missing from the rendered source:\n%s", src)
			}
		})
	}
}

// The vector column specifically, because it is the one that was blocked and
// because its dimension is part of the type rather than a modifier.
func TestRenderSchemaWritesAVectorWithItsDimension(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("document_chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
		schema.Vector("embedding", 1536),
	)

	src, err := codegen.RenderSchema(r, codegen.SchemaOptions{Package: "ragschema"})
	if err != nil {
		t.Fatalf("RenderSchema: %v", err)
	}
	if !contains(string(src), `schema.Vector("embedding", 1536)`) {
		t.Errorf("the vector column should render with its width:\n%s", src)
	}
}

// A rendered schema has to compile, which is the property the bootstrap
// actually depends on — and the one a string comparison would not check.
func TestRenderedSchemaCompiles(t *testing.T) {
	r := schema.NewRegistry()
	org := r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	r.Table("document_chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", org).OnDelete(schema.Cascade),
		schema.Varchar("title", 200).Nullable(),
		schema.Numeric("score").Nullable(),
		schema.Enum("state", "draft", "ready").Default(schema.Value("draft")),
		schema.JSON("meta").Nullable().Default(schema.Value("{}")),
		schema.Text("tags").Array().Nullable(),
		schema.Vector("embedding", 768),
		schema.Timestamps(),
	).
		Check("score_positive", "score >= 0").
		AddIndex(schema.Index{Name: "idx_chunks_org", Columns: []string{"org_id"}}).
		AddIndex(schema.Index{Columns: []string{"tags"}, Method: "gin"})

	src, err := codegen.RenderSchema(r, codegen.SchemaOptions{Package: "ragschema"})
	if err != nil {
		t.Fatalf("RenderSchema: %v", err)
	}
	if err := compilesSchema(t, string(src)); err != nil {
		t.Fatalf("the rendered schema does not compile: %v\n%s", err, src)
	}
}

// compilesSchema builds the rendered schema in a throwaway module that replaces
// sqlb with this checkout. Distinct from compile_test.go's compiles, which
// builds *generated* packages inside this module: a rendered schema declares
// the DSL rather than consuming it, so sqlb is the only dependency it needs and
// a module of its own is the smaller check.
//
// go/parser would only prove the source parses, which gofmt already did on the
// way out. What the bootstrap depends on is that it *builds*: a constructor
// with the wrong arity, a modifier that does not exist on that type, a
// dimension rendered as a string — each of those parses perfectly and none of
// them compiles.
func compilesSchema(t *testing.T, src string) error {
	t.Helper()

	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module bootstrapcheck\n\ngo 1.25.0\n\n" +
			"require github.com/mind-vm/sqlb v0.0.0\n\n" +
			"replace github.com/mind-vm/sqlb => " + root + "\n",
		"schema.go": src,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}

	// GOFLAGS=-mod=mod so the throwaway module may resolve sqlb's own
	// dependencies from the module cache the repository already populated.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}
