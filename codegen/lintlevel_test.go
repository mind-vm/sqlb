package codegen_test

// #267: `check` answers two questions in one stream, and on a fourteen-table
// schema the advisory half was 102 lines long. These tests hold the two
// properties that fix costs nothing to keep: the detail is behind a flag, and
// the last line is the verdict.

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// lintyProject is a schema that lints noisily: a filterable, sortable column
// with no index, on a table exposed with page sizes and no list operation.
// Two severities, several rules, none of them wrong to allow.
func lintyProject(t *testing.T) codegen.Project {
	t.Helper()
	t.Chdir(t.TempDir())

	r := schema.NewRegistry()
	r.Table("widgets",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("label").Filterable().Sortable(),
	).Expose(schema.REST{Ops: schema.CRUD, DefaultPageSize: 20, MaxPageSize: 50})

	return codegen.Project{
		Options: codegen.Options{Registry: r, Dir: "out", Package: "gen"},
	}
}

// lastLine is what a reader sees without scrolling back, which is the whole
// subject of #267.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestCheckDefaultsToALintSummaryRatherThanTheWall(t *testing.T) {
	p := lintyProject(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	code, out := run(t, p, "check")
	if code != 0 {
		t.Fatalf("check must not fail on advisory diagnostics: exit %d, output:\n%s", code, out)
	}
	if !strings.Contains(out, "sqlb: lint:") {
		t.Errorf("the default check said nothing about lint at all, so a schema that has "+
			"never read its diagnostics is never told there are any:\n%s", out)
	}
	// The rule names are the wall. The count is not.
	if strings.Contains(out, "unindexed-filter") {
		t.Errorf("the default check listed a diagnostic; the detail belongs behind "+
			"-lint=warn/all:\n%s", out)
	}
	if !strings.Contains(out, "-lint=") {
		t.Errorf("the summary did not say how to see the diagnostics it counted:\n%s", out)
	}
}

func TestCheckLintOffPrintsNothingAboutLint(t *testing.T) {
	p := lintyProject(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	code, out := run(t, p, "check", "-lint=off")
	if code != 0 {
		t.Fatalf("check -lint=off: exit %d, output:\n%s", code, out)
	}
	if strings.Contains(out, "lint") {
		t.Errorf("-lint=off still mentioned lint, which is the one thing it promises "+
			"not to do:\n%s", out)
	}
}

// The floor the issue actually asked for: a project that has read its info
// notes once keeps the warn ones without re-reading sixty lines about
// sortable columns on tables that will never grow.
func TestCheckLintWarnKeepsTheWarningsAndCountsTheRest(t *testing.T) {
	p := lintyProject(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	all := mustCheck(t, p, "-lint=all")
	warn := mustCheck(t, p, "-lint=warn")

	if !strings.Contains(all, "[info]") || !strings.Contains(all, "[warn]") {
		t.Fatalf("the fixture no longer produces both severities, so this test proves "+
			"nothing:\n%s", all)
	}
	if !strings.Contains(warn, "[warn]") {
		t.Errorf("-lint=warn dropped the warnings:\n%s", warn)
	}
	if strings.Contains(warn, "[info]") {
		t.Errorf("-lint=warn listed an info diagnostic, so there is still no floor:\n%s", warn)
	}
	if !strings.Contains(warn, "info") {
		t.Errorf("-lint=warn hid the info diagnostics without saying how many there "+
			"were:\n%s", warn)
	}
}

func TestCheckRefusesAnUnknownLintLevelByName(t *testing.T) {
	p := lintyProject(t)
	code, out := run(t, p, "check", "-lint=quiet")
	if code != 2 {
		t.Fatalf("an unknown -lint level should be a usage error, got exit %d:\n%s", code, out)
	}
	// A rejection that does not name what would have been accepted sends the
	// reader to the source (docs/architecture.md, "Actionable errors").
	for _, want := range []string{"off", "summary", "warn", "all"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal did not name %q as an accepted level:\n%s", want, out)
		}
	}
}

// The property worth keeping whatever else changes: the last line of check is
// its verdict, at every level and on both outcomes.
func TestCheckClosesWithItsVerdict(t *testing.T) {
	p := lintyProject(t)
	if code, out := run(t, p, "generate"); code != 0 {
		t.Fatalf("generate: exit %d, output:\n%s", code, out)
	}

	for _, level := range []string{"off", "summary", "warn", "all"} {
		out := mustCheck(t, p, "-lint="+level)
		if got := lastLine(out); got != "sqlb: check passed" {
			t.Errorf("-lint=%s: the last line was %q, not the verdict:\n%s", level, got, out)
		}
	}
}

func TestCheckClosesWithItsVerdictWhenItFails(t *testing.T) {
	p := lintyProject(t)
	// Never generated, so every file is missing — a failure with a long
	// advisory block above it, which is the case #267 is about.
	code, out := run(t, p, "check", "-lint=all")
	if code == 0 {
		t.Fatalf("check passed against an empty directory:\n%s", out)
	}
	last := lastLine(out)
	if !strings.HasPrefix(last, "sqlb: check failed:") {
		t.Fatalf("the last line was %q, not the verdict:\n%s", last, out)
	}
	// The closing line has to say what failed, or it is just a second copy of
	// the exit code.
	if !strings.Contains(last, "generated files are out of date") {
		t.Errorf("the verdict did not name what failed: %q", last)
	}
}

func mustCheck(t *testing.T, p codegen.Project, args ...string) string {
	t.Helper()
	code, out := run(t, p, append([]string{"check"}, args...)...)
	if code != 0 {
		t.Fatalf("check %v: exit %d, output:\n%s", args, code, out)
	}
	return out
}
