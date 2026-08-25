package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// emitGo returns every generated file for a client/CLI run, keyed by its path
// relative to Dir — the path, not the base name, because the point of this
// group is which directory each half lands in.
func emitGo(t *testing.T, r *schema.Registry, opts codegen.Options) map[string]string {
	t.Helper()
	dir := t.TempDir()
	opts.Registry, opts.Dir, opts.Package = r, dir, "gen"
	if opts.ClientImportPath == "" {
		opts.ClientImportPath = "example.com/app/cli/client"
	}
	files, err := codegen.Generate(opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(rel)] = string(b)
	}
	return out
}

// The property the whole issue is about: a Go program that wants the typed
// client can take it without taking a command-line framework (#97).
func TestTheGeneratedClientImportsNoFramework(t *testing.T) {
	files := emitGo(t, cliFixture(), codegen.Options{CLIDir: "cli", CLIName: "blogctl"})

	client, ok := files["cli/client/client_gen.go"]
	if !ok {
		t.Fatalf("no client package was emitted; got %v", keysOf(files))
	}
	// The import block, and any use of the qualifier — not the whole file. The
	// package explains in a comment why it takes a context and a writer rather
	// than a *cobra.Command, and that sentence is worth keeping.
	imports := importBlock(client)
	body := withoutComments(client[strings.Index(client, ")")+1:])
	for _, forbidden := range []string{"cobra", "pflag", "spf13"} {
		if strings.Contains(imports, forbidden) {
			t.Errorf("the client package imports %q:\n%s", forbidden, imports)
		}
	}
	for _, qualifier := range []string{"cobra.", "pflag."} {
		if strings.Contains(body, qualifier) {
			t.Errorf("the client package refers to %s", qualifier)
		}
	}
	// And it is a client, not a fragment: the four things a caller needs.
	for _, want := range []string{
		"type Client struct {",
		"type Request struct {",
		"type Transport func(",
		"func (c *Client) Do(",
		"func (c *Client) Run(",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("the client package is missing %q", want)
		}
	}
}

// The CLI is the half that keeps cobra, and it imports the client rather than
// redeclaring it — two copies of Client would be two types a caller cannot
// pass between.
func TestTheCLIImportsTheClientRatherThanRepeatingIt(t *testing.T) {
	files := emitGo(t, cliFixture(), codegen.Options{CLIDir: "cli", CLIName: "blogctl"})

	cli := files["cli/cli_gen.go"]
	if !strings.Contains(cli, `"example.com/app/cli/client"`) {
		t.Errorf("the CLI does not import the client:\n%s", importBlock(cli))
	}
	if !strings.Contains(cli, "github.com/spf13/cobra") {
		t.Error("the CLI lost cobra")
	}
	for _, redeclared := range []string{"type Client struct {", "type Request struct {", "type Transport func("} {
		if strings.Contains(cli, redeclared) {
			t.Errorf("the CLI redeclares %q instead of importing it", redeclared)
		}
	}
}

// ClientDir alone is the server-to-server case: the typed encoder, and no
// command tree at all.
func TestClientDirAloneEmitsNoCommandTree(t *testing.T) {
	files := emitGo(t, cliFixture(), codegen.Options{ClientDir: "apiclient"})

	if _, ok := files["apiclient/client_gen.go"]; !ok {
		t.Fatalf("no client emitted; got %v", keysOf(files))
	}
	for name := range files {
		if strings.Contains(name, "cli_gen.go") {
			t.Errorf("a command tree was emitted for a client-only run: %s", name)
		}
	}
}

// The import set is derived from the rendered body rather than written down,
// because an import a body does not use is a file that gofmt accepts and the
// consumer's compiler rejects. A schema exposing only a create names no
// url.Values, so "net/url" must not appear.
func TestTheCLIImportSetFollowsWhatItEmits(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("notes",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
	).Expose(schema.REST{Path: "/notes", Ops: schema.OpCreate})

	cli := emitGo(t, r, codegen.Options{CLIDir: "cli", CLIName: "notectl"})["cli/cli_gen.go"]
	imports := importBlock(cli)
	if strings.Contains(imports, `"net/url"`) {
		t.Errorf("a create-only schema imported net/url:\n%s", imports)
	}
	// The imports it does declare have to be the ones it uses, which is the
	// property gofmt cannot check for us.
	for _, line := range strings.Split(imports, "\n") {
		path := strings.Trim(strings.TrimSpace(line), `"`)
		if path == "" || strings.HasPrefix(path, "import") || path == "(" || path == ")" {
			continue
		}
		qualifier := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			qualifier = path[i+1:]
		}
		if !strings.Contains(cli[strings.Index(cli, ")")+1:], qualifier+".") {
			t.Errorf("the CLI imports %q and never refers to it", path)
		}
	}
}

// Deriving the import path needs a module root and a relative Dir. When it
// cannot be derived the message says which knob to set, rather than emitting a
// package that does not compile.
func TestAnUndeterminableClientImportPathIsRefused(t *testing.T) {
	_, err := codegen.Generate(codegen.Options{
		Registry: cliFixture(), Dir: t.TempDir(), Package: "gen",
		CLIDir: "cli", CLIName: "blogctl",
	})
	if err == nil {
		t.Fatal("an absolute Dir with no ClientImportPath was accepted")
	}
	if !strings.Contains(err.Error(), "ClientImportPath") {
		t.Errorf("the error does not name the knob that fixes it: %v", err)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// withoutComments drops line comments, so an assertion about what the code
// refers to is not tripped by a sentence explaining why it does not. The
// client's Run carries one naming *cobra.Command, and that sentence is the
// reason the method has the signature it has.
func withoutComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// importBlock returns the file's import declaration, for an assertion that
// wants to talk about imports rather than about the whole file.
func importBlock(src string) string {
	i := strings.Index(src, "import (")
	if i < 0 {
		return ""
	}
	j := strings.Index(src[i:], ")")
	if j < 0 {
		return src[i:]
	}
	return src[i : i+j+1]
}
