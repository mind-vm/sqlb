package migrate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/migrate"
)

func addColumn() migrate.Change {
	return migrate.Change{
		Comment: "posts.view_count",
		Up:      `ALTER TABLE "posts" ADD COLUMN "view_count" bigint NOT NULL DEFAULT 0;`,
		Down:    `ALTER TABLE "posts" DROP COLUMN "view_count";`,
	}
}

func addIndex() migrate.Change {
	return migrate.Change{
		Up:    `CREATE INDEX CONCURRENTLY "posts_org_id_idx" ON "posts" ("org_id");`,
		Down:  `DROP INDEX CONCURRENTLY "posts_org_id_idx";`,
		Stage: migrate.StageConcurrent,
	}
}

func dropColumn() migrate.Change {
	return migrate.Change{
		Up:          `ALTER TABLE "posts" DROP COLUMN "legacy_slug";`,
		Destructive: true,
		Reason:      "drops a column and the data in it",
	}
}

func TestGooseFormat(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000", Name: "add_view_count",
		Changes: []migrate.Change{addColumn()},
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	body, ok := files["20260727120000_add_view_count.sql"]
	if !ok {
		t.Fatalf("unexpected filenames: %v", keys(files))
	}
	// goose is a single file with annotations, not separate up/down files.
	if len(files) != 1 {
		t.Errorf("goose emits one file per migration, got %d", len(files))
	}
	for _, want := range []string{"-- +goose Up", "-- +goose Down", "ADD COLUMN", "DROP COLUMN"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from:\n%s", want, body)
		}
	}
	if strings.Index(body, "-- +goose Up") > strings.Index(body, "-- +goose Down") {
		t.Error("goose requires Up before Down")
	}
}

// Transaction control is per file in goose, so a concurrent index change must
// not share a file with changes that want a transaction.
func TestConcurrentChangesGetTheirOwnFile(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000", Name: "add_view_count",
		Changes: []migrate.Change{addColumn(), addIndex()},
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want the index split into its own file, got %v", keys(files))
	}

	main := files["20260727120000_add_view_count.sql"]
	idx := files["20260727120001_add_view_count_indexes.sql"]
	if main == "" || idx == "" {
		t.Fatalf("unexpected filenames: %v", keys(files))
	}
	if strings.Contains(main, "NO TRANSACTION") {
		t.Error("the ordinary migration must keep its transaction")
	}
	if !strings.HasPrefix(idx, "-- +goose NO TRANSACTION") {
		t.Errorf("NO TRANSACTION must be the first line of the index file:\n%s", idx)
	}
	if !strings.Contains(idx, "CONCURRENTLY") || strings.Contains(main, "CONCURRENTLY") {
		t.Error("the concurrent statement landed in the wrong file")
	}
	// The follow-up must sort after its parent, or the runner applies the
	// index before the column it indexes exists.
	const parent, follow = "20260727120000_add_view_count.sql", "20260727120001_add_view_count_indexes.sql"
	if parent >= follow {
		t.Errorf("%q must sort before %q", parent, follow)
	}
}

func TestDestructiveChangesAreCommentedOutByDefault(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "00007", Name: "drop_legacy",
		Changes: []migrate.Change{dropColumn()},
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := files["00007_drop_legacy.sql"]
	if !strings.Contains(body, "-- DESTRUCTIVE: drops a column and the data in it") {
		t.Errorf("the reason must be stated:\n%s", body)
	}
	if !strings.Contains(body, `-- ALTER TABLE "posts" DROP COLUMN "legacy_slug";`) {
		t.Errorf("the SQL must be commented out:\n%s", body)
	}

	live, err := migrate.Render(migrate.Migration{
		Version: "00007", Name: "drop_legacy",
		Changes: []migrate.Change{dropColumn()},
	}, migrate.Options{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(live["00007_drop_legacy.sql"], "\nALTER TABLE \"posts\" DROP COLUMN") {
		t.Error("AllowDestructive should emit the statement live")
	}
}

func TestDependentChangesAreCommentedOutWithWhatTheyWaitFor(t *testing.T) {
	// A change that depends on a commented-out one is commented out by the same
	// flag, so the pair is uncommented together or not at all — which is the
	// only arrangement that leaves the file appliable in both states.
	dependent := migrate.Change{
		Up:        `ALTER TABLE "posts" ADD CONSTRAINT "posts_slug_key" UNIQUE ("slug");`,
		Down:      `ALTER TABLE "posts" DROP CONSTRAINT "posts_slug_key";`,
		DependsOn: `"posts"."slug", which an earlier change adds and is commented out`,
	}
	changes := []migrate.Change{dropColumn(), dependent}

	files, err := migrate.Render(migrate.Migration{
		Version: "00008", Name: "slug", Changes: changes,
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := files["00008_slug.sql"]
	if !strings.Contains(body, `-- DEPENDS ON: "posts"."slug"`) {
		t.Errorf("the dependency must be stated:\n%s", body)
	}
	if !strings.Contains(body, `-- ALTER TABLE "posts" ADD CONSTRAINT "posts_slug_key"`) {
		t.Errorf("the SQL must be commented out:\n%s", body)
	}
	// The Down too: dropping a constraint that was never added fails just as
	// the add would have.
	if !strings.Contains(body, `-- ALTER TABLE "posts" DROP CONSTRAINT "posts_slug_key";`) {
		t.Errorf("the Down must be commented out as well:\n%s", body)
	}

	live, err := migrate.Render(migrate.Migration{
		Version: "00008", Name: "slug", Changes: changes,
	}, migrate.Options{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(live["00008_slug.sql"], "\nALTER TABLE \"posts\" ADD CONSTRAINT") {
		t.Error("the flag that emits the change it waits for must emit this one too")
	}
}

func TestDestructiveChangeMustGiveAReason(t *testing.T) {
	_, err := migrate.Render(migrate.Migration{
		Version: "1", Name: "x",
		Changes: []migrate.Change{{Up: "DROP TABLE t;", Destructive: true}},
	}, migrate.Options{})
	if err == nil {
		t.Fatal("a destructive change with no reason should be rejected")
	}
}

// A missing Down should say so rather than render an empty section that looks
// like a successful rollback.
func TestIrreversibleChangesSayWhy(t *testing.T) {
	files, _ := migrate.Render(migrate.Migration{
		Version: "1", Name: "x",
		Changes: []migrate.Change{{Up: "ALTER TABLE t ALTER COLUMN c TYPE text;"}},
	}, migrate.Options{})
	if !strings.Contains(files["1_x.sql"], "Not reversible automatically") {
		t.Errorf("expected an explanation:\n%s", files["1_x.sql"])
	}
}

// Down statements must reverse in the opposite order to Up, or a dropped
// column's table may be recreated after the column that referenced it.
func TestDownReversesOrder(t *testing.T) {
	files, _ := migrate.Render(migrate.Migration{
		Version: "1", Name: "x",
		Changes: []migrate.Change{
			{Up: "CREATE TABLE a();", Down: "DROP TABLE a;"},
			{Up: "CREATE TABLE b();", Down: "DROP TABLE b;"},
		},
	}, migrate.Options{})
	body := files["1_x.sql"]
	down := body[strings.Index(body, "-- +goose Down"):]
	if strings.Index(down, "DROP TABLE b") > strings.Index(down, "DROP TABLE a") {
		t.Errorf("Down must reverse the order of Up:\n%s", down)
	}
}

func TestStatementBlocksWrapMultiStatementSQL(t *testing.T) {
	files, _ := migrate.Render(migrate.Migration{
		Version: "1", Name: "fn",
		Changes: []migrate.Change{{
			Up: "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql;",
		}},
	}, migrate.Options{})
	body := files["1_fn.sql"]
	if !strings.Contains(body, "-- +goose StatementBegin") || !strings.Contains(body, "-- +goose StatementEnd") {
		t.Errorf("SQL with internal semicolons needs explicit delimiters:\n%s", body)
	}
}

// The other direction of the same guard: a semicolon in a comment is prose, not
// a statement separator. A destructive change renders as nothing but comment
// lines, so getting this wrong wraps every one of them in delimiters it does
// not need.
func TestStatementBlocksIgnoreSemicolonsInComments(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "1", Name: "drop",
		Changes: []migrate.Change{{
			Up:          `DROP TABLE "posts";`,
			Destructive: true,
			Reason:      "deletes every row; the Down cannot bring them back",
		}},
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := files["1_drop.sql"]
	if strings.Contains(body, "+goose Statement") {
		t.Errorf("a single statement needs no delimiters:\n%s", body)
	}
}

func TestGolangMigrateFormatUsesSeparateFiles(t *testing.T) {
	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000", Name: "add_view_count",
		Changes: []migrate.Change{addColumn()},
	}, migrate.Options{Format: migrate.GolangMigrate})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("golang-migrate uses separate up/down files, got %v", keys(files))
	}
	if !strings.Contains(files["20260727120000_add_view_count.up.sql"], "ADD COLUMN") {
		t.Error("up file wrong")
	}
	if strings.Contains(files["20260727120000_add_view_count.up.sql"], "+goose") {
		t.Error("goose annotations leaked into the golang-migrate format")
	}
}

// Handwritten is how a Migration composed by hand — rather than produced by
// Diff — says its file should not claim Header. codegen's provenance scan
// (issue #178) trusts a header-bearing file enough to flag DDL sqlb never
// emits inside it, and a hand-composed migration (docs/migrations/README.md's
// worked example, example/tasks/cmd/migrate) legitimately contains exactly
// that DDL — a trigger, typically — so it must never carry the header in the
// first place.
func TestHandwrittenSkipsTheHeader(t *testing.T) {
	m := migrate.Migration{
		Version: "1", Name: "triggers",
		Changes: []migrate.Change{{Up: "CREATE TRIGGER t BEFORE UPDATE ON x FOR EACH ROW EXECUTE FUNCTION f();"}},
	}

	for _, format := range []migrate.Format{migrate.Goose, migrate.GolangMigrate, migrate.Plain} {
		files, err := migrate.Render(m, migrate.Options{Format: format, Handwritten: true})
		if err != nil {
			t.Fatalf("%s: Render: %v", format.Name(), err)
		}
		for name, body := range files {
			if strings.Contains(body, migrate.Header) {
				t.Errorf("%s: %s carries Header despite Handwritten:\n%s", format.Name(), name, body)
			}
		}
	}

	// The default is unchanged: nothing regresses for every existing caller
	// that never sets Handwritten.
	files, err := migrate.Render(m, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for name, body := range files {
		if !strings.Contains(body, migrate.Header) {
			t.Errorf("%s: missing Header with Handwritten left at its zero value:\n%s", name, body)
		}
	}
}

func TestByName(t *testing.T) {
	for name, want := range map[string]string{
		"": "goose", "goose": "goose", "golang-migrate": "golang-migrate", "plain": "plain",
	} {
		f, err := migrate.ByName(name)
		if err != nil {
			t.Fatalf("ByName(%q): %v", name, err)
		}
		if f.Name() != want {
			t.Errorf("ByName(%q) = %s, want %s", name, f.Name(), want)
		}
	}
	if _, err := migrate.ByName("liquibase"); err == nil {
		t.Error("an unknown format should be rejected")
	}
}

// An applied migration must never change under the runner's feet.
func TestWriteRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	m := migrate.Migration{Version: "1", Name: "x", Changes: []migrate.Change{addColumn()}}

	names, err := migrate.Write(dir, m, migrate.Options{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("wrote %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, names[0])); err != nil {
		t.Fatalf("file not written: %v", err)
	}

	if _, err := migrate.Write(dir, m, migrate.Options{}); err == nil {
		t.Error("rewriting an existing migration should be refused")
	}
}

func TestTimestampVersionMatchesGooseFormat(t *testing.T) {
	got := migrate.TimestampVersion(time.Date(2026, 7, 27, 12, 30, 5, 0, time.UTC))
	if got != "20260727123005" {
		t.Errorf("TimestampVersion = %q", got)
	}
	if migrate.SequentialVersion(7) != "00007" {
		t.Errorf("SequentialVersion = %q", migrate.SequentialVersion(7))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
