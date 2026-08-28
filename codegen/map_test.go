package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// A map-shaped request body (#327).
//
// The field vocabulary is the column vocabulary and a column is a scalar or an
// array of scalars, so the only spelling for a quiz submission — question id to
// chosen option id — was `JSON`. That reaches Go as json.RawMessage and every
// generated client as `unknown`, with the handler unmarshalling by hand. The
// declaration was still worth making, but the client came out *less* typed than
// the hand-written route it replaced, which is the opposite of what a declared
// surface is for.
//
// An array is not the substitute: one option per question is a fact the map
// carries for free, and a list lets a client answer the same question twice.

func mapFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.Reads}).
		AddAction(schema.Action{
			Name: "submit-quiz",
			Body: schema.Body(schema.Map("answers", schema.TypeUUID)),
		})
	return r
}

// generateClients is `generate` with the three client emitters switched on,
// since a map's whole point is what they render.
func generateClients(t *testing.T, r *schema.Registry) map[string]string {
	t.Helper()
	dir := t.TempDir()
	files, err := codegen.Generate(codegen.Options{
		Registry: r, Dir: dir, Package: "gen",
		ClientImportPath: "example.com/app/cli/client",
		TSDir:            "ts", DartDir: "dart", CLIDir: "cli",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = string(b)
	}
	return out
}

func TestAMapReachesEveryClientAsAMap(t *testing.T) {
	files := generateClients(t, mapFixture())

	for _, tc := range []struct{ file, want string }{
		{"rest_gen.go", "Answers map[string]string `json:\"answers\"`"},
		{"client.gen.ts", "answers: Record<string, string>;"},
		{"client.gen.dart", "final Map<String, String> answers;"},
		// The CLI sends the object rather than a string containing it. Without
		// this the server receives {"answers":"{...}"} and refuses it against
		// its own schema, which is a worse error arriving further away.
		{"cli_gen.go", `body["answers"] = json.RawMessage(valAnswers)`},
	} {
		if !strings.Contains(files[tc.file], tc.want) {
			t.Errorf("%s missing %q:\n%s", tc.file, tc.want, files[tc.file])
		}
	}
	// And none of them fell back to the shape this replaces.
	if strings.Contains(files["client.gen.ts"], "answers: unknown") {
		t.Error("the TypeScript client still types the map as unknown")
	}
	if strings.Contains(files["rest_gen.go"], "Answers json.RawMessage") {
		t.Error("the Go body still takes a raw document")
	}
}

// The value type is the half the Type does not carry, so it has to reach the
// clients too — otherwise every map is a map of strings and a declaration that
// said otherwise was decorative.
func TestAMapsValueTypeReachesTheClients(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("settings", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.Reads}).
		AddAction(schema.Action{
			Name: "set-limits",
			Body: schema.Body(schema.Map("limits", schema.TypeInt)),
		})
	files := generateClients(t, r)

	for _, tc := range []struct{ file, want string }{
		{"rest_gen.go", "Limits map[string]int32"},
		{"client.gen.ts", "limits: Record<string, number>;"},
		{"client.gen.dart", "final Map<String, int> limits;"},
	} {
		if !strings.Contains(files[tc.file], tc.want) {
			t.Errorf("%s missing %q:\n%s", tc.file, tc.want, files[tc.file])
		}
	}
}

// Map is body-only. A table carrying one has nothing to render as DDL, so it is
// refused where it is fixable rather than discovered as a migration that will
// not apply.
func TestAMapIsRefusedAsAColumn(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Map("answers", schema.TypeText),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("a Map column was accepted")
	}
	for _, want := range []string{"answers", "body property"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

// The two declarations a map can make and get wrong, both refused where the
// alternative is a client typed `unknown` — which is what Map exists to avoid.
func TestAMalformedMapDeclarationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value schema.Type
		want  string
	}{
		{"no value type", "", "names no value type"},
		{"a map of documents", schema.TypeJSON, "not a scalar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := schema.NewRegistry()
			r.Table("lessons", schema.UUIDv7("id").PrimaryKey()).
				Expose(schema.REST{Ops: schema.Reads}).
				AddAction(schema.Action{
					Name: "submit",
					Body: schema.Body(schema.Map("answers", tc.value)),
				})
			err := r.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error should say %q: %v", tc.want, err)
			}
		})
	}
}
