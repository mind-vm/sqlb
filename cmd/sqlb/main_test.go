package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
)

// The subject of the end-to-end tests is this repository's own blog example.
//
// Using a real package rather than a scaffolded temp module is what makes the
// assertion worth something: it proves the generated driver compiles against a
// module and reproduces output that is committed, which is the entire claim. A
// fixture module would prove only that the fixture compiled.
const blog = "./example/blog/blogschema"

// invoke runs the command from the repository root and reports the exit code
// alongside everything it printed.
func invoke(t *testing.T, args ...string) (int, string) {
	t.Helper()
	t.Chdir("../..")

	var out, errOut strings.Builder
	err := run(args, &out, &errOut)
	printed := out.String() + errOut.String()

	var code exitCode
	switch {
	case err == nil:
		return 0, printed
	case errors.As(err, &code):
		return int(code), printed
	default:
		// A failure before the driver ran, which main prints itself.
		return 1, printed + "sqlb: " + err.Error()
	}
}

// The load-bearing test. It compiles a driver against the root module, links in
// blogschema, runs every emitter, and compares the result with what is
// committed — so a green run here means the whole mechanism works and that it
// agrees with the hand-written generator it replaced.
func TestCheckAgreesWithTheCommittedOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a driver against the module; not part of the inner loop")
	}

	code, out := invoke(t, "check", blog)
	if code != 0 {
		t.Fatalf("sqlb check %s reported exit %d:\n%s", blog, code, out)
	}
	if !strings.Contains(out, "current") {
		t.Errorf("check passed without saying so, so a CI log would show nothing:\n%s", out)
	}
}

// A schema package under internal/ must be readable, and for a long time it was
// not: the driver was written to the system temp directory, and Go refuses an
// internal/ import from a file outside the module, so the command failed with
// "use of internal package … not allowed" before any emitter ran.
//
// That ruled the command out for repositories that group their modules under
// internal/ — a common layout, and the one that most wants a generator — which
// is a large hole to leave to a fixture nobody would think to write. The
// subject is internal/internalschema, a real package in this module; nothing
// else here lives under internal/, so nothing else would catch a regression.
func TestSchemaPackageUnderInternalIsReadable(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a driver against the module; not part of the inner loop")
	}

	const internal = "./internal/internalschema"
	code, out := invoke(t, "check", internal)
	if code != 0 {
		t.Fatalf("sqlb check %s reported exit %d:\n%s", internal, code, out)
	}
	// Naming the symptom, because the generic failure above would also fire for
	// an unrelated compile error and read as the same thing.
	if strings.Contains(out, "use of internal package") {
		t.Errorf("the driver was compiled from outside the module again:\n%s", out)
	}
	if !strings.Contains(out, "current") {
		t.Errorf("check passed without saying so, so a CI log would show nothing:\n%s", out)
	}
}

// ADR-0016: the test above passes for a command that silently does nothing, so
// this one proves the driver is really being compiled and run. A package with
// no SqlbProject must be refused, by name.
func TestPackageWithoutAProjectIsRefusedByName(t *testing.T) {
	code, out := invoke(t, "check", "./schema")
	if code == 0 {
		t.Fatalf("a package with no %s was accepted:\n%s", codegen.ProjectFunc, out)
	}
	for _, want := range []string{codegen.ProjectFunc, "github.com/mind-vm/sqlb/schema"} {
		if !strings.Contains(out, want) {
			t.Errorf("the error did not mention %q, and someone hitting it has no other "+
				"clue what to write:\n%s", want, out)
		}
	}
}

// A pattern matching several packages is the mistake `sqlb generate ./...`
// makes, and guessing at it would generate from whichever registries happened
// to be linked in.
func TestAmbiguousPatternIsRefused(t *testing.T) {
	code, out := invoke(t, "check", "./example/...")
	if code == 0 {
		t.Fatalf("a pattern matching several packages was accepted:\n%s", out)
	}
	if !strings.Contains(out, "blogschema") {
		t.Errorf("the error did not list what the pattern matched, which is the only way "+
			"to see how to narrow it:\n%s", out)
	}
}

func TestPackageMainIsRefused(t *testing.T) {
	code, out := invoke(t, "check", "./cmd/sqlb")
	if code == 0 {
		t.Fatalf("package main was accepted as a schema package:\n%s", out)
	}
	if !strings.Contains(out, "main") {
		t.Errorf("the error did not say what was wrong with it:\n%s", out)
	}
}

func TestUnknownCommandPrintsUsage(t *testing.T) {
	code, out := invoke(t, "regenerate", blog)
	if code == 0 {
		t.Fatalf("an unknown command succeeded:\n%s", out)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("an unknown command did not print usage:\n%s", out)
	}
}

func TestNoArgumentsPrintsUsage(t *testing.T) {
	code, out := invoke(t)
	if code != 2 {
		t.Errorf("bare sqlb exited %d, want 2 (usage), so a shell cannot tell a misuse "+
			"from a failed check:\n%s", code, out)
	}
	if !strings.Contains(out, funcSignature) {
		t.Errorf("usage does not say what a schema package must export:\n%s", out)
	}
}

func TestGenerateNeedsAPackage(t *testing.T) {
	code, out := invoke(t, "generate")
	if code == 0 {
		t.Fatalf("generate with no package succeeded:\n%s", out)
	}
	if !strings.Contains(out, "needs a package argument") {
		t.Errorf("the error did not say what was missing:\n%s", out)
	}
}

func TestVersionSaysSomething(t *testing.T) {
	code, out := invoke(t, "version")
	if code != 0 {
		t.Fatalf("version exited %d:\n%s", code, out)
	}
	if !strings.HasPrefix(out, "sqlb ") {
		t.Errorf("version printed %q, which does not name the tool", out)
	}
}

// introspect is the one verb with no package argument, so the two mistakes the
// other five train a caller into must both be answered rather than passed to
// flag.Parse as something unrecognisable (issue #112).
func TestIntrospectArguments(t *testing.T) {
	t.Run("needs a database", func(t *testing.T) {
		code, out := invoke(t, "introspect")
		if code == 0 {
			t.Fatalf("introspect with no -dsn must fail:\n%s", out)
		}
		if !strings.Contains(out, "-dsn") {
			t.Errorf("the error does not name the flag that is missing:\n%s", out)
		}
	})

	t.Run("rejects a package argument by saying why it has none", func(t *testing.T) {
		code, out := invoke(t, "introspect", "-dsn", "postgres://x", blog)
		if code == 0 {
			t.Fatalf("introspect with a package argument must fail:\n%s", out)
		}
		// The habit every other verb builds is to put the package last, so the
		// message has to explain rather than just refuse.
		if !strings.Contains(out, "no schema package to link") {
			t.Errorf("the error refuses without explaining:\n%s", out)
		}
	})

	t.Run("is listed in the usage", func(t *testing.T) {
		_, out := invoke(t, "help")
		if !strings.Contains(out, "sqlb introspect") {
			t.Errorf("introspect is missing from the usage:\n%s", out)
		}
		if !strings.Contains(out, "Flags for introspect:") {
			t.Errorf("introspect's flags are missing from the usage:\n%s", out)
		}
	})
}

func TestPackageFromPath(t *testing.T) {
	cases := []struct{ path, want string }{
		{"blogschema/schema.go", "blogschema"},
		{"./internal/orgschema/schema.go", "orgschema"},
		{"/abs/path/taskschema/schema.go", "taskschema"},
		// No directory to take a name from, so the default has to be a legal
		// package name rather than "" — an unbuildable file is a worse outcome
		// than a name the caller renames.
		{"schema.go", "schema"},
		{"/schema.go", "schema"},
	}
	for _, c := range cases {
		if got := packageFromPath(c.path); got != c.want {
			t.Errorf("packageFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestSplitFlag(t *testing.T) {
	got := splitFlag(" a , b ,, c,")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitFlag = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitFlag = %v, want %v", got, want)
		}
	}
	if len(splitFlag("")) != 0 {
		t.Errorf("an empty flag must be no entries, not one empty one")
	}
}
