package pgtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"

	"github.com/mind-vm/sqlb/schema"
)

// TestAdoptionIsAClosedLoop is the end-to-end claim ADR-0014 makes about
// adoption: point sqlb at an existing database and get back a schema.go that
// describes it.
//
// Every step of that is already tested somewhere. What is not, and what this
// covers, is that the rendered Go actually *compiles* and declares the schema it
// was rendered from. A generator that emits plausible-looking source which does
// not build, or which builds into a subtly different schema, passes every string
// assertion in codegen's own tests.
//
// So the rendered file is written into a throwaway module, compiled, and asked
// what it declares. The comparison is the DDL each registry produces: two
// schemas that generate identical CREATE TABLE statements are the same schema
// for every purpose sqlb has.
func TestAdoptionIsAClosedLoop(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	declared := schema.NewRegistry()
	declare(declared)
	applySchema(t, db, declared)

	imported := importRegistry(t, db)

	src, err := codegen.RenderSchema(imported, codegen.SchemaOptions{Package: "adopted"})
	if err != nil {
		t.Fatalf("RenderSchema: %v", err)
	}
	t.Logf("rendered %d bytes of schema.go", len(src))

	got := ddlFromCompiledSource(t, src)
	want := ddlFrom(t, imported)

	if got != want {
		t.Errorf("the compiled schema.go does not describe the database it was rendered from.\n"+
			"--- from the rendered source ---\n%s\n--- from the imported registry ---\n%s",
			got, want)
	}
}

// ddlFrom renders the statements that would create a registry from nothing.
func ddlFrom(t *testing.T, r *schema.Registry) string {
	t.Helper()
	var b strings.Builder
	for _, c := range diff(t, schema.NewRegistry(), r) {
		b.WriteString(strings.TrimSpace(c.Up) + "\n")
	}
	return b.String()
}

// ddlFromCompiledSource writes rendered schema source into a throwaway module,
// compiles it, and returns the DDL the schema it declares produces.
//
// It runs a real `go run` rather than parsing the source back, because parsing
// would re-implement the thing under test: the question is what the Go compiler
// and the schema package make of this file, not what a second reader of it
// thinks it says.
func ddlFromCompiledSource(t *testing.T, src []byte) string {
	t.Helper()

	dir := t.TempDir()
	root := repoRoot(t)

	// A module of its own, pointed at the working tree — so this tests the
	// source in front of us rather than whatever is published.
	write(t, filepath.Join(dir, "go.mod"), `module adopttest

go 1.25.7

require github.com/mind-vm/sqlb v0.0.0

replace github.com/mind-vm/sqlb => `+root+`
`)

	// The rendered file, in a package of its own exactly as a project would
	// keep it.
	if err := os.MkdirAll(filepath.Join(dir, "adopted"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, filepath.Join(dir, "adopted", "schema.go"), string(src))

	// The schema package registers package-level declarations into the default
	// registry, so importing the package for its side effects is what makes
	// them visible — which is also how a real project's generator reads them.
	write(t, filepath.Join(dir, "main.go"), `package main

import (
	"fmt"
	"os"
	"strings"

	_ "adopttest/adopted"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

func main() {
	changes, err := migrate.Diff(schema.NewRegistry(), schema.DefaultRegistry())
	if err != nil {
		fmt.Fprintln(os.Stderr, "diff:", err)
		os.Exit(1)
	}
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(strings.TrimSpace(c.Up) + "\n")
	}
	fmt.Print(b.String())
}
`)

	// The sqlb module's own dependencies are already in the module cache,
	// because this test binary was built against them. Resolving from the cache
	// keeps the test working with no network.
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the rendered schema.go does not compile: %v\n%s\n\n--- source ---\n%s",
			err, out, src)
	}
	return string(out)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("locating the repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
