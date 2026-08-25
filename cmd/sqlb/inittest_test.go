package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The scaffold's emitted test has to actually pass, and nothing else here would
// notice if it stopped.
//
// It is generated source referring to generated symbols — Task and TaskCols come
// out of codegen's naming, and sqlb.On, Builder.Where and sqlbtest's recorders
// are the library's own surface. A rename on either side leaves this template
// syntactically fine and broken for every project created after it, which is the
// worst kind of scaffold: the adopter's first `go test` fails and the failure is
// in code they did not write.
//
// So this runs the real thing: init, generate, test. Everything in the module
// path is either the local checkout (through a replace) or a dependency this
// repository already has, with one exception — cmd/server imports goose, which
// sqlb does not depend on and which resolving would need the network for. It is
// removed rather than tidied for, because the subject here is predicate_test.go
// and the emitted server is not what would break.
func TestTheScaffoldedTestCompilesAndPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated project with the real toolchain; not part of the inner loop")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	var out, errOut strings.Builder
	if err := initCmd([]string{"-module", "example.com/scaffoldcheck", dir}, &out, &errOut); err != nil {
		t.Fatalf("init: %v\n%s", err, errOut.String())
	}
	if err := os.RemoveAll(filepath.Join(dir, "cmd")); err != nil {
		t.Fatal(err)
	}

	gomod := filepath.Join(dir, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, "\nrequire github.com/mind-vm/sqlb v0.0.0\n\nreplace github.com/mind-vm/sqlb => "+root+"\n"...)
	if err := os.WriteFile(gomod, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// -mod=mod so the throwaway module may resolve sqlb's own dependencies from
	// the module cache this repository has already populated.
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s:\n%s", name, strings.Join(args, " "), out)
		}
	}
	run("go", "mod", "tidy")
	run("go", "generate", "./...")

	// vet as well as test: context.WithValue with an unkeyed basic type is the
	// mistake this template is most likely to acquire, and it passes its tests.
	run("go", "vet", ".")
	run("go", "test", ".")
}
