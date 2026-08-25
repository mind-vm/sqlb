// The gate that closes the loop `sqlb migrate` opened.
//
// Everything else in this repository checks that generated *files* match the
// schema. This checks the thing no file comparison can: that the checked-in
// migration history, applied by the runner that actually applies it, produces
// the schema taskschema declares. Someone editing schema.go and forgetting the
// migration passes every other gate in the build and fails this one.
//
// It is `sqlb migrate -check ./taskschema` with the shell taken off — replay,
// normalise, diff, against the same registry — so a green run here is a
// statement about the command as much as about the example.
//
// # Why a package of its own
//
// Two reasons, both about isolation, and both of which would have made this a
// bad addition to example/tasks/app.
//
// It imports taskschema for its side effects, which populates
// schema.DefaultRegistry() for the whole test binary. The app package's tests
// go through the generated code and do not want a registry appearing underneath
// them.
//
// And shadow.Normalize rewrites the declared check expressions in place —
// there is one registry, and no way to diff against a copy. A test binary of
// its own bounds that to this file.
package migrations_test

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"

	// Imported for its side effects: declaring a table registers it, and the
	// registry is the whole subject of this test.
	_ "github.com/mind-vm/sqlb/example/tasks/taskschema"
)

// pgEnv names the Postgres this package runs against, for the reason
// app/main_test.go gives — provisioning lives outside the test binary now. It
// must be 18: the history was generated with migrate.MinPostgres(18), so it
// uses the built-in uuidv7() and would fail on 17 at the first CREATE TABLE.
const pgEnv = "SQLB_TEST_POSTGRES"

var dsn string

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		log.Fatalf("migrations: %v", err)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	ctx := context.Background()

	base := os.Getenv(pgEnv)
	if base == "" {
		return 0, fmt.Errorf(
			"%s is not set.\n"+
				"These tests need a Postgres; they no longer start one.\n"+
				"  locally: mise run pg-up   (then mise run test-demo)\n"+
				"  CI:      the service containers in .github/workflows/ci.yml",
			pgEnv)
	}
	u, err := url.Parse(base)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid URL: %w", pgEnv, err)
	}
	withDatabase := func(database string) string {
		v := *u
		v.Path = "/" + database
		return v.String()
	}

	admin, err := pgxpool.New(ctx, withDatabase("postgres"))
	if err != nil {
		return 0, fmt.Errorf("opening the admin connection: %w", err)
	}
	defer admin.Close()

	// A database of this package's own. It used to be the only one on a
	// container of its own, which made "written to by nothing else" free;
	// on a shared server it has to be asked for, and it still matters for the
	// same reason: shadow.Build refuses a database that already has tables.
	name := fmt.Sprintf("t_drift_%d", time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		return 0, fmt.Errorf("dropping %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		return 0, fmt.Errorf("creating %s: %w", name, err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			log.Printf("migrations: dropping %s: %v", name, err)
		}
	}()

	dsn = withDatabase(name)
	return m.Run(), nil
}

// unexpressible is what introspection is expected to be unable to read back
// into the DSL, by constraint name.
//
// Listed rather than tolerated in bulk, because the interesting failure is a
// *new* entry appearing: that means the history has grown a construct the
// schema cannot describe, and every diff computed against it from then on is
// working from a partial picture. The two here are the composite foreign keys
// that make a task in another workspace unrepresentable — see cmd/migrate.
var unexpressible = []string{
	"comments_task_in_same_workspace",
	"tasks_list_in_same_workspace",
}

func TestTheHistoryStillBuildsTheDeclaredSchema(t *testing.T) {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(pool.Close)

	// shadow.Build rather than migrations.Apply, and the difference is not
	// stylistic. goose records what it has applied in a goose_db_version table
	// of its own, which is bookkeeping rather than schema — but introspection
	// cannot tell, so it reads it back as a table the declaration does not have
	// and the diff proposes dropping it. shadow replays the same files and
	// writes no version table, on purpose, which is what makes the result
	// comparable with a registry at all. The first version of this test used
	// the runner and failed with exactly that spurious DROP TABLE.
	//
	// It is also what `sqlb migrate -check` does, so this stays a test of the
	// command rather than of something adjacent to it.
	current, report, res, err := shadow.Build(ctx, pool, shadow.Options{Dir: "."})
	if err != nil {
		t.Fatalf("replaying the migration history: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("the replay applied no files, so everything below compares against nothing")
	}
	assertOnlyKnownGaps(t, report)

	target := schema.DefaultRegistry()
	defer restore(snapshotChecks(target))

	// Without this the CHECK on tasks comes back in Postgres's spelling and the
	// declaration in the author's, and the diff below is never empty — issue
	// #24, which is exactly the failure this gate would otherwise report every
	// run and train everyone to ignore.
	unprobed, err := shadow.Normalize(ctx, pool, target, shadow.Options{})
	if err != nil {
		t.Fatalf("normalising the declared checks: %v", err)
	}
	if len(unprobed) > 0 {
		t.Fatalf("every declared check should be probeable against a database the whole "+
			"history has been applied to, but these were not: %v", unprobed)
	}

	// MinPostgres(18) for the same reason cmd/migrate passes it: it decides
	// which spelling of the UUIDv7 generator the declared schema renders, and
	// the history was written with the built-in one.
	changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("diffing the history against the declaration: %v", err)
	}
	if len(changes) > 0 {
		t.Fatalf("the migration history no longer builds what taskschema declares.\n\n"+
			"Someone edited schema.go without adding a migration, or edited a migration "+
			"without the schema. What is missing:\n\n%s\n"+
			"Generate it with:\n"+
			"    sqlb migrate -name <what-changed> ./taskschema\n",
			describe(changes))
	}
}

func assertOnlyKnownGaps(t *testing.T, report *introspect.Report) {
	t.Helper()
	if report.Empty() {
		// Not a pass. The two composite foreign keys are in the history and the
		// DSL cannot express them, so an empty report means introspection has
		// stopped seeing something rather than that the gap closed — and this
		// whole test would then be comparing against a picture it believes is
		// complete and is not.
		t.Fatalf("introspection reported no gaps at all, but %d are expected: %v",
			len(unexpressible), unexpressible)
	}

	text := report.String()
	for _, name := range unexpressible {
		if !strings.Contains(text, name) {
			t.Errorf("expected %s to be reported as unexpressible and it was not:\n%s", name, text)
		}
	}
	// And the other direction: anything reported that is not on the list is a
	// construct the history grew and the schema cannot describe.
	if got := len(report.Skipped); got != len(unexpressible) {
		t.Errorf("introspection reported %d gaps, want the %d known ones — a new entry means "+
			"the diff above is computed against a partial picture:\n%s",
			got, len(unexpressible), text)
	}
}

// snapshotChecks records the declared check expressions so they can be put back
// after Normalize rewrites them.
//
// Unnecessary today, since this binary runs one test and then exits. It is here
// so that the second test added to this package does not silently inherit a
// registry whose checks are in Postgres's spelling rather than the author's —
// which would be a confusing thing to debug and an easy thing to prevent.
func snapshotChecks(reg *schema.Registry) map[*schema.TableDef]map[string]string {
	out := map[*schema.TableDef]map[string]string{}
	for _, t := range reg.Tables() {
		if len(t.Checks()) == 0 {
			continue
		}
		exprs := map[string]string{}
		for _, c := range t.Checks() {
			exprs[c.Name] = c.Expr
		}
		out[t] = exprs
	}
	return out
}

func restore(snapshot map[*schema.TableDef]map[string]string) {
	for t, exprs := range snapshot {
		for name, expr := range exprs {
			t.ReplaceCheckExpr(name, expr)
		}
	}
}

// describe renders the outstanding changes the way a migration file would, so
// the failure message is the thing to paste rather than a summary of it.
func describe(changes []migrate.Change) string {
	var b strings.Builder
	for _, c := range changes {
		if c.Comment != "" {
			b.WriteString("  -- " + c.Comment + "\n")
		}
		for _, l := range strings.Split(strings.TrimSpace(c.Up), "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}
