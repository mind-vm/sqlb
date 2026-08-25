package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/codegen"
)

// mtimes reports the modification time of every file under dir, keyed by the
// path relative to it.
func mtimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

// TestGenerateLeavesUnchangedFilesAlone is the regression test for #267: a
// generate that produces the bytes already on disk must not touch the files.
//
// The cost of touching them is not the write. It is that gopls invalidates a
// package on the filesystem event and not on the content, so a rewrite with
// identical bytes discards the type information for the generated package and
// for every package that imports it — which, generated code being what a
// project is built on, is usually all of them. A schema author regenerates
// constantly; before this, each one cost a full re-typecheck of the module.
func TestGenerateLeavesUnchangedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	opts := codegen.Options{
		Registry: fixture(),
		Dir:      dir,
		Package:  "gen",
		TSDir:    "web",
	}

	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("first generate: %v", err)
	}
	before := mtimes(t, dir)
	if len(before) < 3 {
		t.Fatalf("expected the fixture to emit several files, got %d", len(before))
	}

	// Coarse filesystem timestamps would hide a rewrite that happened within
	// the same tick, so the second run is separated from the first.
	time.Sleep(20 * time.Millisecond)

	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("second generate: %v", err)
	}

	for name, was := range mtimes(t, dir) {
		if orig, ok := before[name]; !ok {
			t.Errorf("%s appeared only on the second run", name)
		} else if !was.Equal(orig) {
			t.Errorf("%s was rewritten by a generate that changed nothing", name)
		}
	}
}

// TestGenerateRewritesDriftedFiles is the other half of the guard above. Not
// writing is only correct while it stays conditional on the bytes matching: a
// generate that skipped a file whose content had drifted would leave stale
// code behind and report success, which is worse than the churn it avoids.
func TestGenerateRewritesDriftedFiles(t *testing.T) {
	dir := t.TempDir()
	opts := codegen.Options{Registry: fixture(), Dir: dir, Package: "gen"}

	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("first generate: %v", err)
	}

	target := filepath.Join(dir, "models_gen.go")
	want, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package gen // hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := mtimes(t, dir)
	time.Sleep(20 * time.Millisecond)

	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("second generate: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("a hand-edited generated file survived a regenerate")
	}
	for name, now := range mtimes(t, dir) {
		touched := !now.Equal(before[name])
		if name == "models_gen.go" && !touched {
			t.Error("the drifted file was not rewritten")
		}
		if name != "models_gen.go" && touched {
			t.Errorf("%s was rewritten although only models_gen.go had drifted", name)
		}
	}
}

// TestGenerateLeavesNoTemporaryFiles guards the rename. Generated files are
// replaced rather than truncated-and-filled so that a language server reading
// on the filesystem event never parses a half-written file — but a temporary
// left behind in the output directory would be a Go file in the package,
// breaking the build it was meant to protect.
func TestGenerateLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	opts := codegen.Options{Registry: fixture(), Dir: dir, Package: "gen"}
	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("generate: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.Contains(e.Name(), ".tmp") {
			t.Errorf("generate left a temporary behind: %s", e.Name())
		}
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("%s has mode %v, want 0644", e.Name(), perm)
		}
	}
}
