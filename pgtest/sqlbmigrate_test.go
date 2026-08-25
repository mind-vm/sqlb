package pgtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/sqlbtest"
)

// `sqlb migrate`, end to end.
//
// codegen/migrate_test.go covers the refusals and the baseline, both of which
// need no database. What needs one is the claim the verb actually makes: that
// the *current* side of the diff is the schema the checked-in history builds,
// read back by replaying it rather than by remembering what was written. That
// cannot be faked — a fake would be the generator's own idea of what it emitted,
// which is the thing being checked (ADR-0014).

// shadowDSN creates a scratch database for the test and returns a connection
// string for it.
//
// A DSN rather than an open pool, because the Project's ShadowDB has to hand
// back a fresh one on every call — the command closes what it is given, and a
// real project is called more than once per session.
func shadowDSN(t *testing.T) string {
	t.Helper()
	// The same bootstrap freshDB installs, for the same reason: the shadow
	// database has the schema rendered into it, so it needs everything that
	// DDL assumes exists.
	return sqlbtest.FreshDSN(t, serverDSN(t), bootstrap()...)
}

// shadowFunc is the pattern codegen.Project documents, written out: open a
// connection, empty the schema, hand it over. The destructive statement is here
// rather than in sqlb because this is the only place that knows the database is
// scratch.
func shadowFunc(t *testing.T, connStr string) func(context.Context) (*pgxpool.Pool, error) {
	t.Helper()
	return func(ctx context.Context) (*pgxpool.Pool, error) {
		pool, err := pgxpool.New(ctx, connStr)
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
			pool.Close()
			return nil, fmt.Errorf("emptying the shadow database: %w", err)
		}
		if _, err := pool.Exec(ctx, `
			CREATE FUNCTION uuid_generate_v7() RETURNS uuid
			LANGUAGE sql VOLATILE AS 'SELECT uuidv7()'
		`); err != nil {
			pool.Close()
			return nil, fmt.Errorf("reinstalling the uuid shim: %w", err)
		}
		return pool, nil
	}
}

func migrateProject(t *testing.T, reg *schema.Registry, connStr string) codegen.Project {
	t.Helper()
	return codegen.Project{
		Options:       codegen.Options{Registry: reg, Dir: "out", Package: "gen"},
		MigrationsDir: "migrations",
		ShadowDB:      shadowFunc(t, connStr),
	}
}

func runMigrate(t *testing.T, p codegen.Project, args ...string) (int, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := codegen.Run(p, append([]string{"migrate"}, args...), &out, &errOut)
	printed := out.String() + errOut.String()
	t.Logf("sqlb migrate %s → %d\n%s", strings.Join(args, " "), code, printed)
	return code, printed
}

func migrationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("reading the migration directory: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	return names
}

// readMigration returns the one migration whose filename contains want, and
// fails if that is not exactly one file.
//
// By name rather than by position, because the version prefixes are timestamps:
// an earlier draft sliced a concatenation of every file from the first mention
// of a name, which silently included the *next* file too — and read its Down
// section, where a CREATE TABLE's reversal is a live DROP TABLE. The test
// failed for a reason that had nothing to do with the code under test.
// version is the leading digits of a migration filename.
func version(name string) string {
	for i, r := range name {
		if r < '0' || r > '9' {
			return name[:i]
		}
	}
	return name
}

func readMigration(t *testing.T, want string) string {
	t.Helper()
	var found []string
	for _, name := range migrationFiles(t) {
		if strings.Contains(name, want) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one migration matching %q, found %v (all: %v)",
			want, found, migrationFiles(t))
	}
	body, err := os.ReadFile(filepath.Join("migrations", found[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// upSection is the half of a goose file that runs forwards.
//
// The distinction is load-bearing for anything asserting about destructive SQL:
// the Down of a CREATE TABLE is a DROP TABLE, and it is neither destructive nor
// commented out, because reversing a migration that created a table is supposed
// to drop it.
func upSection(t *testing.T, body string) string {
	t.Helper()
	_, after, ok := strings.Cut(body, "-- +goose Up")
	if !ok {
		t.Fatalf("no goose Up section in:\n%s", body)
	}
	up, _, ok := strings.Cut(after, "-- +goose Down")
	if !ok {
		t.Fatalf("no goose Down section in:\n%s", body)
	}
	return up
}

// v1 and v2 differ by one added column and one added table, which is enough for
// the second diff to be wrong in a visible way if the history is not really
// being replayed: diffing against nothing would propose creating orgs a second
// time, and migrate.Write would then refuse or the DDL would fail.
func v1() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	)
	return r
}

func v2() *schema.Registry {
	r := schema.NewRegistry()
	orgs := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
		schema.Text("slug").Unique().Nullable().Filterable(),
	)
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
		schema.Text("title"),
	)
	return r
}

// The whole loop: baseline, then a schema change, then agreement.
func TestMigrateReadsTheCurrentSchemaByReplayingTheHistory(t *testing.T) {
	t.Chdir(t.TempDir())
	conn := shadowDSN(t)

	// 1. The baseline. No history, so no database is touched.
	if code, out := runMigrate(t, migrateProject(t, v1(), conn), "-name", "initial_schema"); code != 0 {
		t.Fatalf("baseline exited %d:\n%s", code, out)
	}
	if got := len(migrationFiles(t)); got != 1 {
		t.Fatalf("the baseline wrote %d files, want 1", got)
	}

	// 2. The history now builds v1, so v1 has nothing left to say. This is the
	//    assertion that the replay happened at all: without it the diff would
	//    be against an empty registry and would propose creating orgs again.
	code, out := runMigrate(t, migrateProject(t, v1(), conn), "-check")
	if code != 0 {
		t.Fatalf("-check reported drift against the schema its own baseline was generated "+
			"from, so the replay did not produce that schema:\n%s", out)
	}

	// 3. The schema moves ahead. -check must now fail, and name what moved.
	moved := migrateProject(t, v2(), conn)
	code, out = runMigrate(t, moved, "-check")
	if code == 0 {
		t.Fatalf("-check passed after a column and a table were added:\n%s", out)
	}
	if !strings.Contains(out, "slug") && !strings.Contains(out, "posts") {
		t.Errorf("-check did not say what had moved:\n%s", out)
	}

	// 4. Write it, and the migration must contain only the difference.
	if code, out := runMigrate(t, moved, "-name", "adds_slug_and_posts"); code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, out)
	}
	files := migrationFiles(t)
	if got := len(files); got != 2 {
		t.Fatalf("wrote %d migration files in total, want 2", got)
	}

	// Both were written inside the same second, and goose's timestamp format
	// has one-second resolution — so this is the assertion that versions come
	// from the directory rather than from the clock alone. Without it the two
	// files collide, and the symptom is not a duplicate filename but shadow
	// refusing to replay the history at all, several steps later.
	if v1, v2 := version(files[0]), version(files[1]); v1 == v2 {
		t.Fatalf("both migrations were written with version %s, so the history cannot be "+
			"replayed in a known order: %v", v1, files)
	}

	second := upSection(t, readMigration(t, "adds_slug_and_posts"))
	for _, want := range []string{"ADD COLUMN", "slug", "CREATE TABLE", "posts"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second migration does not contain %q:\n%s", want, second)
		}
	}
	// The failure this whole mechanism exists to prevent: a second migration
	// that recreates what the first one already made.
	if strings.Contains(second, `CREATE TABLE "orgs"`) {
		t.Errorf("the second migration creates orgs again, so `current` was empty:\n%s", second)
	}

	// 5. And the loop closes: with both migrations applied, nothing is left.
	if code, out := runMigrate(t, moved, "-check"); code != 0 {
		t.Fatalf("-check still reports drift after the migration it asked for was written:\n%s", out)
	}
}

// The generated migration has to be valid, not merely expected. Replaying the
// history is what proves it: shadow.Build applies every file to Postgres, so a
// second `-check` that passes has already run the DDL the first one produced.
// This asserts the same thing from the other end — that what came back matches
// what was declared, column for column.
func TestMigrateProducesDDLPostgresAcceptsAndIntrospectionAgreesWith(t *testing.T) {
	t.Chdir(t.TempDir())
	conn := shadowDSN(t)

	p := migrateProject(t, v2(), conn)
	if code, out := runMigrate(t, p, "-name", "initial_schema"); code != 0 {
		t.Fatalf("baseline exited %d:\n%s", code, out)
	}

	code, out := runMigrate(t, p, "-check")
	if code != 0 {
		t.Fatalf("the schema read back from Postgres differs from the one the migration "+
			"was generated from:\n%s", out)
	}
}

// A destructive change comes out commented out, and the command has to say so —
// the file looks complete and is not, and a runner will apply the rest of it.
func TestMigrateWarnsWhenItCommentsOutADestructiveChange(t *testing.T) {
	t.Chdir(t.TempDir())
	conn := shadowDSN(t)

	if code, out := runMigrate(t, migrateProject(t, v2(), conn), "-name", "initial_schema"); code != 0 {
		t.Fatalf("baseline exited %d:\n%s", code, out)
	}

	// v1 is v2 with a column and a table removed, so diffing towards it drops
	// both.
	code, out := runMigrate(t, migrateProject(t, v1(), conn), "-name", "drops_posts")
	if code != 0 {
		t.Fatalf("migrate exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "destructive") {
		t.Errorf("a migration that drops a table did not warn that it was commented out:\n%s", out)
	}

	up := upSection(t, readMigration(t, "drops_posts"))
	if !strings.Contains(up, "DROP TABLE") {
		t.Fatalf("the drop is not in the file at all:\n%s", up)
	}
	// Commented out, which is what makes the warning above true rather than
	// decorative.
	for _, l := range strings.Split(up, "\n") {
		if strings.Contains(l, "DROP TABLE") && !strings.HasPrefix(strings.TrimSpace(l), "--") {
			t.Errorf("a DROP TABLE was emitted live in the Up section without "+
				"-allow-destructive: %q", l)
		}
	}

	// The other direction, so the assertion above is about the flag rather
	// than about DROP TABLE never being emitted live at all.
	if code, out := runMigrate(t, migrateProject(t, v1(), conn),
		"-name", "drops_posts_for_real", "-allow-destructive"); code != 0 {
		t.Fatalf("migrate -allow-destructive exited %d:\n%s", code, out)
	}
	live := upSection(t, readMigration(t, "drops_posts_for_real"))
	var found bool
	for _, l := range strings.Split(live, "\n") {
		if strings.Contains(l, "DROP TABLE") && !strings.HasPrefix(strings.TrimSpace(l), "--") {
			found = true
		}
	}
	if !found {
		t.Errorf("-allow-destructive still commented the drop out, so the flag does "+
			"nothing:\n%s", live)
	}
}
