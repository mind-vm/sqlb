package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/codegen"
)

// scratchDirs lists the driver directories left in a module.
//
// The subject of every test here, because a leftover is invisible in the
// command's output — it fails much later, as an untracked directory swept into
// somebody's commit.
func scratchDirs(t *testing.T, moduleDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		t.Fatalf("could not read %s: %v", moduleDir, err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), driverPrefix) {
			found = append(found, e.Name())
		}
	}
	return found
}

// A failed compile must not leave its scratch directory behind, which is what
// went wrong in practice: the directory is created inside the user's module, so
// a leftover is one `git add -A` away from being committed.
//
// The failure here is a module directory with no go.mod, so the build fails
// before it has to resolve anything. What is under test is the cleanup, not the
// compiler's opinion, and this way the test costs no build.
func TestScratchDirectoryIsRemovedWhenTheDriverDoesNotCompile(t *testing.T) {
	moduleDir := t.TempDir()

	// declaresProject parses this and finds the convention function, so drive
	// gets as far as writing and building a driver.
	pkgDir := t.TempDir()
	src := fmt.Sprintf("package fakeschema\n\nfunc %s() {}\n", codegen.ProjectFunc)
	if err := os.WriteFile(filepath.Join(pkgDir, "schema.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &pkg{ImportPath: "example.com/fake/fakeschema", Name: "fakeschema", Dir: pkgDir}
	p.Module.Path = "example.com/fake"
	p.Module.Dir = moduleDir

	var out, errOut strings.Builder
	if err := drive(p, []string{"check"}, &out, &errOut); err == nil {
		t.Fatal("a driver that cannot be compiled reported success")
	}

	if left := scratchDirs(t, moduleDir); len(left) != 0 {
		t.Errorf("the failed run left %v in the module, where git will offer to commit it", left)
	}
}

// The guard for what drive cannot observe. A run killed with SIGKILL leaves a
// directory no defer and no signal handler ever sees, and the next run is the
// only thing left that can remove it.
func TestStaleScratchDirectoriesAreSwept(t *testing.T) {
	moduleDir := t.TempDir()

	mkdir := func(name string, age time.Duration) string {
		path := filepath.Join(moduleDir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}

	abandoned := mkdir(driverPrefix+"409734944", staleAfter+time.Hour)
	// Both directions, per ADR-0016. A sweep that removed everything would
	// pass the assertion above while deleting a concurrent run's driver
	// mid-compile, so what it must leave alone is asserted too: a scratch
	// directory young enough to belong to a live run, and a neighbour that
	// merely lives in the same module.
	live := mkdir(driverPrefix+"11", 0)
	unrelated := mkdir("migrations", staleAfter+time.Hour)

	sweepScratch(moduleDir)

	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Errorf("an abandoned scratch directory survived the sweep (stat: %v), so nothing "+
			"ever removes one left by a kill", err)
	}
	for _, keep := range []string{live, unrelated} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("the sweep removed %s, which it must not touch: %v", filepath.Base(keep), err)
		}
	}
}

// The successful path, asserted where it is real: a check against this
// repository's own blog example compiles a driver in the repository root, and
// the root is a tree somebody is working in.
func TestASuccessfulRunLeavesNothingInTheModule(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a driver against the module; not part of the inner loop")
	}

	// The set before, because the module root is shared with every other
	// package `go test ./...` is running at this moment. An earlier version
	// asserted the root held no scratch directory at all and failed on one
	// another test's live run had just created — a guard against leaking that
	// reported a leak it had not observed (#78). What this run leaves is the
	// difference, not the total.
	before := make(map[string]bool)
	for _, name := range scratchDirs(t, "../..") {
		before[name] = true
	}

	code, out := invoke(t, "check", blog)
	if code != 0 {
		t.Fatalf("sqlb check %s reported exit %d:\n%s", blog, code, out)
	}

	// invoke has chdir'd to the repository root, which is the module root the
	// driver was compiled in. Polled rather than read once, because a directory
	// a concurrent run created after the snapshot above is a new name that is
	// not ours and is about to disappear; one that is still there after a
	// second is the leak this test exists to catch.
	var left []string
	for range 20 {
		left = left[:0]
		for _, name := range scratchDirs(t, ".") {
			if !before[name] {
				left = append(left, name)
			}
		}
		if len(left) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("a successful run left %v behind", left)
}

// newScratch's cleanup, asserted directly rather than through a whole run. The
// test above cannot be deterministic about a directory it does not name, and
// this one is: the module root is this test's alone.
func TestScratchCleanupRemovesTheDirectory(t *testing.T) {
	moduleDir := t.TempDir()

	var errOut strings.Builder
	tmp, done, err := newScratch(moduleDir, &errOut)
	if err != nil {
		t.Fatalf("newScratch: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("newScratch returned a directory that is not there: %v", err)
	}

	done()

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("the cleanup left %s behind (stat: %v)", tmp, err)
	}
	if errOut.Len() != 0 {
		t.Errorf("a successful removal reported %q", errOut.String())
	}
}

// A removal that cannot succeed must say so. The discarded error was half of
// what made the flake above unexplainable: the directory leaked and the run that
// leaked it printed nothing, so nothing tied the leftover to the run that left
// it.
func TestScratchCleanupReportsARemovalItCannotMake(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which is not refused write permission")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, driverPrefix+"1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unlinking a child needs write permission on the parent, so this is what
	// makes RemoveAll fail without needing a filesystem that misbehaves.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var errOut strings.Builder
	removeScratch(dir, &errOut)

	got := errOut.String()
	if !strings.Contains(got, dir) {
		t.Errorf("the report %q does not name the directory that leaked", got)
	}
}
