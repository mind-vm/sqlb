package codegen_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/codegen"
)

// Everything here is the half of `sqlb migrate` that needs no Postgres: the
// refusals, and the baseline path. The loop itself — replay, diff, write — is
// in pgtest/sqlbmigrate_test.go, because answering "does the history build this
// schema" without a database is exactly the thing that cannot be faked.

// migrateProject moves the test into a temp directory, the way the driver runs
// with the working directory set to the module root.
func migrateProject(t *testing.T) codegen.Project {
	t.Helper()
	t.Chdir(t.TempDir())
	return codegen.Project{
		Options:       codegen.Options{Registry: fixture(), Dir: "out", Package: "gen"},
		MigrationsDir: "migrations",
	}
}

// The baseline: an empty history diffs against nothing, so the first migration
// of a project needs no scratch database. That is worth asserting rather than
// assuming — it is the difference between adopting sqlb costing a Postgres up
// front and costing nothing until the second migration.
func TestMigrateBaselineNeedsNoDatabase(t *testing.T) {
	p := migrateProject(t)
	// Deliberately nil: if this path reaches for a database the test fails
	// with a nil dereference rather than passing quietly.
	p.ShadowDB = nil

	code, out := run(t, p, "migrate", "-name", "initial_schema")
	if code != 0 {
		t.Fatalf("baseline migrate exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "baseline") {
		t.Errorf("the baseline path did not say it was diffing against nothing:\n%s", out)
	}

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("reading the migration directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the baseline wrote no files")
	}

	body := read(t, filepath.Join("migrations", entries[0].Name()))
	for _, want := range []string{"CREATE TABLE", "blog_entries", "+goose Up", "+goose Down"} {
		if !strings.Contains(body, want) {
			t.Errorf("the baseline migration does not contain %q:\n%s", want, body)
		}
	}
}

// The second run has a history, so it needs the database the project did not
// declare — and the refusal has to name the field, because there is nothing
// else to go on.
func TestMigrateWithAHistoryAndNoShadowDBIsRefusedByName(t *testing.T) {
	p := migrateProject(t)
	if code, out := run(t, p, "migrate", "-name", "initial_schema"); code != 0 {
		t.Fatalf("baseline migrate exited %d:\n%s", code, out)
	}

	code, out := run(t, p, "migrate", "-name", "second")
	if code == 0 {
		t.Fatalf("migrate against an existing history succeeded with no ShadowDB:\n%s", out)
	}
	for _, want := range []string{"ShadowDB", codegen.ProjectFunc} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q, so it does not say what to add:\n%s", want, out)
		}
	}
}

func TestMigrateWithoutAMigrationsDirIsRefused(t *testing.T) {
	p := migrateProject(t)
	p.MigrationsDir = ""

	code, out := run(t, p, "migrate", "-name", "x")
	if code == 0 {
		t.Fatalf("migrate succeeded with nowhere to write:\n%s", out)
	}
	if !strings.Contains(out, "MigrationsDir") {
		t.Errorf("the refusal does not name the field to set:\n%s", out)
	}
}

// -name lands in a filename that migrate.Write refuses to overwrite afterwards,
// so an unnamed migration is a mistake you cannot take back.
func TestMigrateWriteRequiresAName(t *testing.T) {
	code, out := run(t, migrateProject(t), "migrate")
	if code == 0 {
		t.Fatalf("migrate wrote a migration with no name:\n%s", out)
	}
	if !strings.Contains(out, "-name") {
		t.Errorf("the refusal does not name the flag:\n%s", out)
	}

	// The positive control for the assertion above: the same run with a name
	// must succeed, or the test proves only that migrate always fails.
	if code, out := run(t, migrateProject(t), "migrate", "-name", "initial_schema"); code != 0 {
		t.Fatalf("migrate with a name exited %d, so the refusal above was not about the name:\n%s", code, out)
	}
}

// -dry-run and -check are the two modes that write nothing, and "writes
// nothing" is the whole of what they promise.
func TestMigrateDryRunWritesNothing(t *testing.T) {
	p := migrateProject(t)

	code, out := run(t, p, "migrate", "-dry-run")
	if code != 0 {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "CREATE TABLE") {
		t.Errorf("a dry run printed no SQL, which is the only thing it is for:\n%s", out)
	}
	if _, err := os.Stat("migrations"); !os.IsNotExist(err) {
		t.Errorf("a dry run created the migration directory (stat error: %v)", err)
	}
}

func TestMigrateCheckReportsThatTheSchemaMovedAhead(t *testing.T) {
	p := migrateProject(t)

	code, out := run(t, p, "migrate", "-check")
	if code == 0 {
		t.Fatalf("check passed with no migrations at all against a schema with tables:\n%s", out)
	}
	if !strings.Contains(out, "sqlb migrate") {
		t.Errorf("the failure does not name the command that fixes it:\n%s", out)
	}
	if _, err := os.Stat("migrations"); !os.IsNotExist(err) {
		t.Errorf("-check created the migration directory (stat error: %v)", err)
	}

	// The other direction: once the history covers the schema, -check passes.
	// Without this the assertion above holds for a -check that always fails.
	if code, out := run(t, p, "migrate", "-name", "initial_schema"); code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, out)
	}
	// The baseline path diffs against nothing, so a *second* -check still sees
	// the whole schema as new — a history is only read back through a shadow
	// database, which this test does not have. What can be asserted here is
	// that the file it wrote is the one -check was asking for.
	if entries, err := os.ReadDir("migrations"); err != nil || len(entries) == 0 {
		t.Fatalf("migrate wrote nothing after -check said it should (err: %v)", err)
	}
}

func TestMigrateRefusesAnUnknownFormat(t *testing.T) {
	p := migrateProject(t)
	p.MigrationFormat = "liquibase"

	code, out := run(t, p, "migrate", "-name", "x")
	if code == 0 {
		t.Fatalf("an unknown migration format was accepted:\n%s", out)
	}
	if !strings.Contains(out, "goose") {
		t.Errorf("the refusal does not list the formats that do exist:\n%s", out)
	}
}

func TestMigrateRefusesAnUnknownFlag(t *testing.T) {
	code, out := run(t, migrateProject(t), "migrate", "-squash")
	if code == 0 {
		t.Fatalf("an unknown flag was accepted:\n%s", out)
	}
	if !strings.Contains(out, "squash") {
		t.Errorf("the error does not quote the flag it did not understand:\n%s", out)
	}
}

func TestProjectRefusesAnAbsoluteMigrationsDir(t *testing.T) {
	p := codegen.Project{
		Options:       codegen.Options{Registry: fixture(), Dir: "out", Package: "gen"},
		MigrationsDir: t.TempDir(),
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("an absolute MigrationsDir validated; it resolves against the module root, " +
			"so an absolute one writes somewhere different on every machine")
	}
	// Naming the right field matters: Options.Dir is checked by the same loop,
	// and a message pointing at the wrong one sends the reader to a field that
	// is fine.
	if !strings.Contains(err.Error(), "MigrationsDir") {
		t.Errorf("the refusal names the wrong field: %v", err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// A ShadowDB that returns neither a database nor an error is a bug in the
// project's own code, and the message has to say so rather than panicking two
// frames later inside shadow.
func TestMigrateRefusesAShadowDBThatReturnsNothing(t *testing.T) {
	p := migrateProject(t)
	if code, out := run(t, p, "migrate", "-name", "initial_schema"); code != 0 {
		t.Fatalf("baseline migrate exited %d:\n%s", code, out)
	}
	p.ShadowDB = func(context.Context) (*pgxpool.Pool, error) { return nil, nil }

	code, out := run(t, p, "migrate", "-name", "second")
	if code == 0 {
		t.Fatalf("a nil database was accepted:\n%s", out)
	}
	if !strings.Contains(out, "ShadowDB") {
		t.Errorf("the error does not say which function misbehaved:\n%s", out)
	}
}
