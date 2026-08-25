package codegen

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A vector column emits as sqlb.Vector, which is the second thing in a
// generated model that is not a plain Go type and the first that is a column.
// It brings the sqlb import with it — the type carries the codec that moves an
// embedding in binary, so a model holding one cannot be importable without sqlb
// the way the rest of them are.
func TestVectorModelEmitsTheTypeAndItsImport(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
		schema.Vector("embedding", 1536),
	)
	files, err := render(Options{Registry: r, Package: "gen", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	models := string(files["models_gen.go"])
	if models == "" {
		t.Fatal("no model file was rendered")
	}
	for _, want := range []string{
		`import "github.com/mind-vm/sqlb"`,
		"Embedding sqlb.Vector",
	} {
		if !strings.Contains(models, want) {
			t.Errorf("the model is missing %q:\n%s", want, models)
		}
	}

	// Hidden comes from the constructor and is not optional, so the column is
	// excluded from JSON at the type as well as by the REST layer. Twenty
	// kilobytes of float per row reaching a response is the failure this
	// prevents, and one stray json.Marshal is all it would take.
	if !strings.Contains(models, `json:"-"`) {
		t.Errorf("the embedding is not excluded from JSON:\n%s", models)
	}
}

// And it reaches nothing that faces the wire. A vector has no spelling in a
// REST body, in the manifest the generated clients are built from, or in the
// clients themselves, and Hidden is what keeps it out of all of them — so a
// generated client never has to decide what an embedding looks like on the
// wire.
//
// The Go-side artefacts are a different matter and are deliberately excluded
// from this: the typed update has a SetEmbedding, because writing an embedding
// from Go is the whole point, and the typed *column* is omitted along with
// every other hidden one so that a predicate against it does not compile. Both
// are what ADR-0026 means by "Go callers through the query engine still get it".
func TestAVectorColumnStaysOutOfTheGeneratedClients(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
		schema.Vector("embedding", 1536),
	).Expose(schema.REST{Path: "/chunks", Ops: schema.CRUD | schema.OpList})

	files, err := render(Options{Registry: r, Package: "gen", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The artefacts a client reads. models_gen.go and columns_gen.go are Go and
	// are covered above.
	facing := []string{"rest_gen.go", "sqlb.json"}
	for _, name := range facing {
		content, ok := files[name]
		if !ok {
			t.Fatalf("%s was not rendered, so this test checked nothing", name)
		}
		text := string(content)
		if strings.Contains(text, "embedding") || strings.Contains(text, "Embedding") {
			t.Errorf("%s mentions the embedding column, which is Hidden and should not reach it:\n%s",
				name, text)
		}
	}
}
