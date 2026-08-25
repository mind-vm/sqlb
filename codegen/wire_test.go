package codegen_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// wireFixture is one table with one column whose two spellings differ, exposed
// so every emitter has something to say about it.
func wireFixture(c schema.WireCase) *schema.Registry {
	r := schema.NewRegistry().WireCase(c)
	r.Table("articles",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Filterable().Sortable(),
		schema.Timestamp("created_at").Filterable().Sortable(),
		// Nullable, so a patch can say null, which is the one thing a value
		// flag cannot express and so the one thing --set-null has to spell.
		schema.Text("published_by").Nullable(),
	).Expose(schema.REST{Path: "/articles", Ops: schema.CRUD | schema.OpList})
	return r
}

func generateAll(t *testing.T, r *schema.Registry) map[string]string {
	t.Helper()
	dir := t.TempDir()
	files, err := codegen.Generate(codegen.Options{
		Registry: r, Dir: dir, Package: "gen",
		TSDir: "web", DartDir: "mobile", CLIDir: "cli", CLIName: "artctl",
		ClientImportPath: "example.com/app/cli/client",
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

// One setting, five surfaces. The whole claim of ADR-0036 is that they cannot
// disagree, so this asserts the same spelling reaches all of them from one
// declaration rather than testing each emitter's own idea of it.
func TestWireCaseReachesEverySurface(t *testing.T) {
	files := generateAll(t, wireFixture(schema.Camel))

	for name, want := range map[string]string{
		"models_gen.go":   `json:"createdAt"`,
		"client.gen.ts":   "createdAt",
		"client.gen.dart": "'createdAt'",
	} {
		src, ok := files[name]
		if !ok {
			t.Fatalf("%s was not generated (got %v)", name, wireKeysOf(files))
		}
		if !strings.Contains(src, want) {
			t.Errorf("%s does not carry the wire spelling %q", name, want)
		}
	}

	// The model also has to *tell the runtime*, because nothing on the request
	// path may import the schema package and so nothing there can compute a
	// spelling of its own.
	if !strings.Contains(files["models_gen.go"], "wire:createdAt") {
		t.Errorf("models.go does not carry the wire spelling into the runtime:\n%s", files["models_gen.go"])
	}
	// And the database's own name is still what the row scans from.
	if !strings.Contains(files["models_gen.go"], `db:"created_at"`) {
		t.Error("models.go lost the column name, which is what reaches Postgres")
	}

	// The request bodies are the surface a client writes *to*, and they were
	// the one this test did not look at. A body tagged with the column name is
	// a POST or a PATCH no generated client can make: the TypeScript client
	// sends createdAt and the server binds created_at.
	rest := files["rest_gen.go"]
	for _, want := range []string{
		`json:"createdAt"`,           // the create body
		`json:"createdAt,omitempty"`, // the patch body
		`u.present["createdAt"]`,     // and what a patch counts as present
	} {
		if !strings.Contains(rest, want) {
			t.Errorf("rest_gen.go does not carry the wire spelling %q:\n%s", want, rest)
		}
	}
	// What Changes() hands the statement is still the column, which is the one
	// place in that file the two spellings are supposed to differ.
	if !strings.Contains(rest, `out["created_at"]`) {
		t.Errorf("rest_gen.go stopped naming the column the UPDATE has to set:\n%s", rest)
	}
	if strings.Contains(rest, `json:"created_at"`) {
		t.Errorf("rest_gen.go tags a body property with the database spelling:\n%s", rest)
	}
}

// Verbatim is the default and emits exactly what it always did — no wire entry,
// no camel anywhere. This is what makes the amendment additive: an existing
// project regenerates to byte-identical output.
func TestVerbatimEmitsNothingNew(t *testing.T) {
	files := generateAll(t, wireFixture(schema.Verbatim))

	if !strings.Contains(files["models_gen.go"], `json:"created_at"`) {
		t.Error("the default stopped spelling a column the way the database does")
	}
	if strings.Contains(files["models_gen.go"], "wire:") {
		t.Error("Verbatim wrote a wire entry, so existing output is no longer byte-identical")
	}
	// Checked against the *wire* strings rather than the whole file. A Dart
	// getter is createdAt under either setting, because dartMember camel-cases
	// a column name to make a legal Dart identifier — that is a language
	// convention and not a wire format, and conflating the two would assert
	// something false about the default.
	for name, want := range map[string]string{
		"client.gen.ts":   "created_at",
		"client.gen.dart": "'created_at'",
	} {
		if !strings.Contains(files[name], want) {
			t.Errorf("%s does not spell the column %q on the wire", name, want)
		}
	}
	if strings.Contains(files["client.gen.dart"], "'createdAt'") {
		t.Error("client.gen.dart sends a camelCase key under Verbatim")
	}
}

// A CLI flag is a local affordance rather than a wire format, so it is stable
// across the setting: switching WireCase must not rewrite every documented
// command line.
func TestCLIFlagsAreStableAcrossWireCases(t *testing.T) {
	camel := generateAll(t, wireFixture(schema.Camel))["cli_gen.go"]
	verbatim := generateAll(t, wireFixture(schema.Verbatim))["cli_gen.go"]

	for _, src := range []string{camel, verbatim} {
		if !strings.Contains(src, "created-at") {
			t.Errorf("the flag is not kebab-cased:\n%s", firstLines(src, 40))
		}
	}
	// But what it sends does move.
	if !strings.Contains(camel, `q.Add("createdAt"`) {
		t.Error("the CLI sends the column spelling rather than the wire spelling")
	}
	if !strings.Contains(verbatim, `q.Add("created_at"`) {
		t.Error("Verbatim's CLI stopped sending the column name")
	}

	// --set-null is a body assignment too, and it is the one the value flags do
	// not cover. The flag still names the column — that is the documented
	// command line — and the key it writes moves with the setting.
	for _, src := range []string{camel, verbatim} {
		if !strings.Contains(src, `registerCompletion(cmd, "set-null", []string{"published_by"})`) {
			t.Errorf("--set-null stopped accepting the column's own spelling:\n%s", src)
		}
	}
	if !strings.Contains(camel, `[]nullableColumn{{"published_by", "publishedBy"}}`) {
		t.Error("--set-null sends the column spelling rather than the wire spelling")
	}
	if !strings.Contains(verbatim, `[]nullableColumn{{"published_by", "published_by"}}`) {
		t.Error("Verbatim's --set-null stopped sending the column name")
	}
}

// The emitted skill is read as instructions, so a wrong spelling there is worse
// than a wrong spelling anywhere else this setting reaches: an agent that does
// exactly what the file says sends `?created_at=eq.…` and gets a 400, and the
// capability list it consulted is what told it that would be accepted (#143).
//
// Everything under an exposed resource is checked here, because the bug was not
// one table — the manifest reported column names for the whole REST section and
// the skill printed them faithfully.
func TestSkillSpellsTheWireNotTheColumn(t *testing.T) {
	skill := skillOf(t, wireFixture(schema.Camel))

	for _, want := range []string{
		"| Filterable | `id`, `title`, `createdAt` |",
		"| Sortable | `title`, `createdAt` |",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill's capability table is missing %q:\n%s", want, skill)
		}
	}
	// The sharper half of the bug: the table could be read as "these are columns,
	// translate them yourself", and this sentence explicitly forecloses that.
	if strings.Contains(skill, "the column names above are also the JSON field names") {
		t.Error("the skill asserts a mapping-free wire under a declared WireCase")
	}
	if !strings.Contains(skill, "WireCase(schema.Camel)") {
		t.Errorf("the skill does not name the case a reader has to apply:\n%s", skill)
	}
	// No database spelling anywhere a request is being described. Checked over
	// the whole document up to the honesty section rather than over the one table
	// asserted above, because a resource block that got the capability rows right
	// and the enum line or the key wrong is exactly the failure this watches for.
	// The honesty section is excluded on purpose: it *quotes* both spellings to
	// explain the mapping, which is the one place the column name belongs.
	surface, _, ok := strings.Cut(skill, "## What this file does not say")
	if !ok {
		t.Fatalf("the skill has no honesty section, so this guard is checking the wrong thing:\n%s", skill)
	}
	if strings.Contains(surface, "created_at") {
		t.Errorf("the skill carries the database spelling of a column:\n%s", surface)
	}
}

// And the default is unchanged, sentence included. The claim is stronger there —
// there is genuinely no mapping — so it should still be made.
func TestSkillKeepsTheMappingFreeSentenceUnderVerbatim(t *testing.T) {
	skill := skillOf(t, wireFixture(schema.Verbatim))

	if !strings.Contains(skill, "| Filterable | `id`, `title`, `created_at` |") {
		t.Errorf("Verbatim's capability table moved:\n%s", skill)
	}
	if !strings.Contains(skill, "the column names above are also the JSON field names") {
		t.Errorf("the default lost the sentence that says not to look for a mapping:\n%s", skill)
	}
	if strings.Contains(skill, "WireCase(") {
		t.Error("Verbatim's skill talks about a setting the schema did not make")
	}
}

// skillOf generates r's skill and returns it.
func skillOf(t *testing.T, r *schema.Registry) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := codegen.Generate(codegen.Options{
		Registry: r, Dir: dir, Package: "gen", SkillDir: ".claude/skills",
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claude", "skills", "sqlb-schema", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading emitted skill: %v", err)
	}
	return string(b)
}

func wireKeysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Two modules emitted into one directory share the runtime, which is the whole
// of issue #110.
//
// The TypeScript half is a size question — structural typing means duplicate
// Page interoperate — but the Dart half is a correctness one: nominal typing
// makes two Page<T> unrelated classes, so before this an application could not
// pass a page from either module to one widget, nor give both clients one
// Transport.
func TestTwoModulesShareOneRuntime(t *testing.T) {
	family := schema.NewModule("family")
	family.Table("children", schema.UUIDv7("id").PrimaryKey(), schema.Text("name")).
		Expose(schema.REST{Path: "/children", Ops: schema.Reads})

	tutor := schema.NewModule("tutor")
	tutor.Table("sessions", schema.UUIDv7("id").PrimaryKey(), schema.Text("topic")).
		Expose(schema.REST{Path: "/sessions", Ops: schema.Reads})

	dir := t.TempDir()
	emit := func(r *schema.Registry, tsFile, dartFile string) map[string][]byte {
		t.Helper()
		files, err := codegen.Generate(codegen.Options{
			Registry: r, Dir: dir, Package: "gen",
			TSDir: "web", DartDir: "mobile",
			TSClientFile: tsFile, TSQueriesFile: "-", DartFile: dartFile,
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		out := map[string][]byte{}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out[filepath.Base(f)] = b
		}
		return out
	}

	a := emit(family, "family.gen.ts", "family.gen.dart")
	b := emit(tutor, "tutor.gen.ts", "tutor.gen.dart")

	// The runtime is byte-identical, which is what lets the second module write
	// the same path without the first noticing — and what keeps `check`
	// meaningful for both.
	for _, name := range []string{"runtime.gen.ts", "runtime.gen.dart"} {
		if !bytes.Equal(a[name], b[name]) {
			t.Errorf("%s differs between two modules, so they cannot share it", name)
		}
		if len(a[name]) == 0 {
			t.Fatalf("%s was not emitted", name)
		}
	}

	// And neither module declares the shared types itself any more. A second
	// declaration is exactly what produced the Dart ambiguous_import.
	for name, src := range map[string][]byte{
		"family.gen.dart": a["family.gen.dart"],
		"tutor.gen.dart":  b["tutor.gen.dart"],
	} {
		if bytes.Contains(src, []byte("\nclass Page<T> extends Collection<T> {")) {
			t.Errorf("%s declares its own Page, so two modules would have two", name)
		}
		if !bytes.Contains(src, []byte("export 'runtime.gen.dart';")) {
			t.Errorf("%s does not export the runtime, so importing it offers no Page", name)
		}
	}
	for name, src := range map[string][]byte{
		"family.gen.ts": a["family.gen.ts"],
		"tutor.gen.ts":  b["tutor.gen.ts"],
	} {
		if bytes.Contains(src, []byte("export interface Page<T>")) {
			t.Errorf("%s declares its own Page", name)
		}
		if !bytes.Contains(src, []byte("export * from './runtime.gen.ts';")) {
			t.Errorf("%s does not re-export the runtime", name)
		}
	}
}

// Row and the filter conditions stay with each client, and that is deliberate:
// Dart privacy is per library and both keep a private contract with the
// generated code, so sharing them would need that protocol made public.
func TestDartKeepsRowAndConditionsPerClient(t *testing.T) {
	files := generateAll(t, wireFixture(schema.Verbatim))
	client, runtime := files["client.gen.dart"], files["runtime.gen.dart"]

	for _, name := range []string{"abstract class Row {", "class Cond<T extends Object> {"} {
		if !strings.Contains(client, name) {
			t.Errorf("the client should still declare %q", name)
		}
		if strings.Contains(runtime, name) {
			t.Errorf("the shared runtime declares %q, whose contract with the client is private", name)
		}
	}
	// The shared half is what an application names across modules.
	for _, name := range []string{"class Page<T>", "typedef Transport", "class Problem {"} {
		if !strings.Contains(runtime, name) {
			t.Errorf("the shared runtime should declare %q", name)
		}
	}
}
