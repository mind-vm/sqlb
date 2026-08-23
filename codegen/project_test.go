package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/schema"
)

// run drives the driver half of the sqlb command the way cmd/sqlb does, and
// returns the exit code with everything it printed.
func run(t *testing.T, p codegen.Project, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := codegen.Run(p, args, &out, &errOut)
	return code, out.String() + errOut.String()
}

// project moves the test into a temp directory and points a Project at a
// relative path inside it.
//
// The chdir is the point rather than a convenience: cmd/sqlb runs the driver
// with the working directory set to the module root, and a Project's paths are
// relative to it. A test holding an absolute Dir would be testing something the
// command cannot do — as the first version of this helper discovered, by being
// refused by Validate.
func project(t *testing.T) codegen.Project {
	t.Helper()
	t.Chdir(t.TempDir())
	return codegen.Project{
		Options: codegen.Options{
			Registry: fixture(),
			Dir:      "out",
			Package:  "gen",
		},
	}
}

func TestProjectGenerateThenCheckIsClean(t *testing.T) {
	p := project(t)

	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}
	code, out := run(t, p, "check")
	if code != 0 {
		t.Fatalf("check straight after generate reported exit %d, so the emitters are "+
			"not reproducible:\n%s", code, out)
	}
	if !strings.Contains(out, "current") {
		t.Errorf("check said nothing about being current:\n%s", out)
	}
}

// #204: a schema that newly reaches for a feature can make generate write an
// import `go mod tidy`, run before generate produced anything, had no way to
// see — outbox/events pulling in huma's SSE adapter package is the case that
// surfaced it. A first generate into an empty directory writes every file for
// the first time, which is exactly that shape, so it is the cheap way to
// prove the nudge fires without needing a schema that actually adds a new
// package import.
func TestProjectGenerateNudgesWhenOutputChanges(t *testing.T) {
	p := project(t)
	code, out := run(t, p, "generate")
	if code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "go mod tidy") {
		t.Errorf("a first generate — every file newly written — did not nudge to run "+
			"`go mod tidy` again:\n%s", out)
	}
}

// The other half of #204: a rerun that writes exactly what was already on
// disk changed nothing about the dependency graph, so nagging about it every
// time generate runs would train people to ignore the message by the time it
// matters.
func TestProjectGenerateDoesNotNudgeWhenOutputIsUnchanged(t *testing.T) {
	p := project(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("first generate: exit %d, output:\n%s", code, out)
	}

	code, out := run(t, p, "generate")
	if code != 0 {
		t.Fatalf("second generate: exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "go mod tidy") {
		t.Errorf("a second generate against unchanged output nudged to run `go mod tidy` "+
			"again anyway:\n%s", out)
	}
}

// The other direction, which is the one that matters: ADR-0016. A check that
// cannot fail is not a gate, and this one is about to become the gate every
// sqlb project runs in CI.
func TestProjectCheckReportsADriftedFile(t *testing.T) {
	p := project(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	path := filepath.Join(p.Options.Dir, "models_gen.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte("\n// edited by hand\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := run(t, p, "check")
	if code == 0 {
		t.Fatalf("check passed on a hand-edited models_gen.go, so it verifies nothing:\n%s", out)
	}
	if !strings.Contains(out, "models_gen.go") {
		t.Errorf("check failed without naming the stale file, which is the only part of "+
			"the message a CI log makes useful:\n%s", out)
	}
	// The message has to name the fix, because it is read by someone who is
	// not in the directory and did not write the generator.
	if !strings.Contains(out, "sqlb generate") {
		t.Errorf("check failed without naming the command that fixes it:\n%s", out)
	}
}

// `check` runs schema.Lint() as a second, advisory pass — issue #201. Nothing
// here should ever fail the command over a diagnostic: exit 0 with the
// message printed is the claim, and this is the test that would catch a
// change that turned it into a gate by accident.
func TestProjectCheckReportsLintDiagnostics(t *testing.T) {
	t.Chdir(t.TempDir())

	// Ops == CRUD with DefaultPageSize/MaxPageSize set and no OpList: the exact
	// shape #201 found on all sixteen tables of a real port, unflagged until
	// end-to-end testing found it.
	r := schema.NewRegistry()
	r.Table("widgets", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.CRUD, DefaultPageSize: 20, MaxPageSize: 50})

	p := codegen.Project{
		Options: codegen.Options{Registry: r, Dir: "out", Package: "gen"},
	}
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	code, out := run(t, p, "check", "-lint=all")
	if code != 0 {
		t.Fatalf("check must not fail on an advisory lint diagnostic: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "page-size-without-list") {
		t.Errorf("check did not report the lint diagnostic:\n%s", out)
	}
	if !strings.Contains(out, "widgets") {
		t.Errorf("check did not name the table the diagnostic is about:\n%s", out)
	}
}

func TestProjectCheckReportsAMissingFile(t *testing.T) {
	p := project(t)

	code, out := run(t, p, "check")
	if code == 0 {
		t.Fatalf("check passed against an empty directory:\n%s", out)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("a never-generated tree was not reported as missing:\n%s", out)
	}
}

// Dir empty means the module root. Options refuses an empty Dir, so this is
// Project's own defaulting and it needs its own assertion.
func TestProjectDirDefaultsToTheModuleRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	p := codegen.Project{Options: codegen.Options{Registry: fixture(), Package: "gen"}}
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate with no Dir: exit %d, output:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "models_gen.go")); err != nil {
		t.Errorf("an empty Dir did not write into the working directory: %v", err)
	}
}

func TestProjectRefusesAnAbsoluteDir(t *testing.T) {
	p := codegen.Project{
		Options: codegen.Options{Registry: fixture(), Dir: t.TempDir(), Package: "gen"},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("an absolute Dir validated; a Project's paths resolve against the module " +
			"root, so an absolute one writes somewhere different on every machine")
	}

	// And the positive control, without which the assertion above would pass
	// for a Validate that rejected everything.
	rel := codegen.Project{
		Options: codegen.Options{Registry: fixture(), Dir: "example/blog", Package: "gen"},
	}
	if err := rel.Validate(); err != nil {
		t.Fatalf("a relative Dir was refused too, so the check is not about absoluteness: %v", err)
	}
}

func TestProjectRefusesAnUnknownVerb(t *testing.T) {
	code, out := run(t, project(t), "regenerate")
	if code == 0 {
		t.Fatalf("an unknown verb succeeded:\n%s", out)
	}
	if !strings.Contains(out, "regenerate") {
		t.Errorf("the error did not quote the verb it did not understand:\n%s", out)
	}
}

// #290's layout, both ways round: a Go module with the clients beside it.
//
//	sokrates/
//	├── server/   the Go module — Dir is "sqlbdata" inside it
//	├── web/      React, consumes the TypeScript client
//	└── mobile/   Flutter, consumes the Dart client
//
// Reaching web/ from Dir takes two levels. With one, the client lands in
// server/web/src/api: created, correct, and imported by nothing, while the
// real web app goes on compiling against the client it already had. Every
// build stays green and the reported path — "web/src/api/client.gen.ts", which
// filepath.Join cleaned back down to a module-root-relative path — reads
// exactly like the right answer.
func TestGenerateWarnsWhenAClientDirectoryHadToBeInvented(t *testing.T) {
	root := t.TempDir()
	// The repository as it really is: the frontends exist, beside the module.
	for _, dir := range []string{"server/sqlbdata", "web/src", "mobile/lib"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(filepath.Join(root, "server"))

	p := codegen.Project{Options: codegen.Options{
		Registry: fixture(),
		Dir:      "sqlbdata",
		Package:  "gen",
		TSDir:    "../web/src/api",       // one ../ short
		DartDir:  "../../mobile/lib/api", // correct
	}}

	code, out := run(t, p, "generate")
	if code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "TSDir") {
		t.Errorf("the TypeScript client was written into a tree that did not exist and "+
			"nothing said so:\n%s", out)
	}
	// The absolute path is the half the relative report could not carry: it is
	// the only spelling that distinguishes server/web from web.
	if want := filepath.Join(root, "server", "web", "src", "api"); !strings.Contains(out, want) {
		t.Errorf("the warning should name where the client actually landed (%s):\n%s", want, out)
	}
	// The correct one is beside an existing mobile/lib, so it must be silent —
	// a warning that fires on the documented layout is a warning nobody reads.
	if strings.Contains(out, "DartDir") {
		t.Errorf("the Dart client landed beside the Flutter app and was still flagged:\n%s", out)
	}

	// Second run: the directory exists now, so there is nothing left to notice.
	// This is why it is a warning and not a refusal — and why it has to be
	// asked before generate rather than after.
	if _, again := run(t, p, "generate"); strings.Contains(again, "TSDir") {
		t.Errorf("the warning repeated on a run that created nothing:\n%s", again)
	}
}
