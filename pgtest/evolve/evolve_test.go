// The gate the whole schema-evolution example rests on: that the checked-in
// migration history still builds the schema evolveschema declares.
//
// example/evolve keeps the current state in Go and the history in SQL, and
// nothing in the repository can check that pairing by comparing files —
// generate-check compares generated code against the declaration, and both
// sides of that stay consistent when someone edits schema.go and forgets the
// migration. This replays the history into an empty database and asks Postgres
// what it built.
//
// # Why a package of its own
//
// It imports evolveschema for its side effects, which puts those tables into
// schema.DefaultRegistry() for the whole test binary. The pgtest package next
// door applies DefaultRegistry to a fresh database in several of its tests and
// expects to find the blog example's tables there and nothing else, so this
// cannot live beside them.
//
// shadow.Normalize also rewrites the declared check expressions in place,
// and there is one registry with no way to diff against a copy. A binary of its
// own bounds that too. example/tasks/migrations/drift_test.go is the same
// arrangement for the same two reasons.
package evolve_test

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

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"

	// Imported for its side effects: declaring a table registers it, and the
	// registry is the whole subject of this test.
	_ "github.com/mind-vm/sqlb/example/evolve/evolveschema"
)

// pgEnv names the Postgres this package runs against. Provisioning happens
// outside the module — see pgtest/main_test.go for why — and the version is
// pinned where the server is started. It must still be 18: the history was
// generated with migrate.MinPostgres(18), so it uses the built-in uuidv7() and
// would fail on 17 at the first CREATE TABLE.
const pgEnv = "SQLB_TEST_POSTGRES"

// dir is the migration history, relative to this package.
const dir = "../../example/evolve/migrations"

var dsn string

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		log.Fatalf("evolve: %v", err)
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
				"  locally: mise run pg-up   (then mise run test-pg)",
			pgEnv)
	}
	u, err := url.Parse(base)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid URL: %w", pgEnv, err)
	}

	admin, err := pgxpool.New(ctx, withDatabase(u, "postgres"))
	if err != nil {
		return 0, fmt.Errorf("opening the admin connection: %w", err)
	}
	defer admin.Close()

	// A database of this package's own, rather than whichever one the DSN
	// happened to name. Both tests below call shadow.Build, which refuses a
	// database that already has tables in it, and on a shared server the DSN's
	// default database is exactly where another suite's leftovers would be.
	name := fmt.Sprintf("t_evolve_%d", time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
		return 0, fmt.Errorf("dropping %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		return 0, fmt.Errorf("creating %s: %w", name, err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			log.Printf("evolve: dropping %s: %v", name, err)
		}
	}()

	dsn = withDatabase(u, name)
	return m.Run(), nil
}

// withDatabase points a base DSN at another database on the same server,
// leaving every other parameter as the caller wrote it.
func withDatabase(u *url.URL, database string) string {
	v := *u
	v.Path = "/" + database
	return v.String()
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func TestTheHistoryStillBuildsTheDeclaredSchema(t *testing.T) {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(pool.Close)

	// shadow.Build rather than a runner: goose would write a version table that
	// introspection cannot tell apart from schema, and the diff below would
	// then propose dropping it forever.
	current, _, res, err := shadow.Build(ctx, pool, shadow.Options{Dir: dir})
	if err != nil {
		t.Fatalf("replaying the migration history: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("the replay applied no files, so everything below compares against nothing")
	}
	// Six files for five revisions: revision 2 renders two, because its index
	// change needs a file with no transaction around it. If this number moves,
	// the history changed and the document describing it did not.
	if got, want := len(res.Files), 6; got != want {
		t.Errorf("replayed %d files, want %d: %v", got, want, res.Files)
	}

	target := schema.DefaultRegistry()
	defer restore(snapshotChecks(target))

	// Without this the enum CHECKs come back in Postgres's spelling and the
	// declaration in the author's, and the diff below is never empty.
	unprobed, err := shadow.Normalize(ctx, pool, target, shadow.Options{})
	if err != nil {
		t.Fatalf("normalising the declared checks: %v", err)
	}
	if len(unprobed) > 0 {
		t.Fatalf("every declared check should be probeable against a database the whole "+
			"history has been applied to, but these were not: %v", unprobed)
	}

	changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("diffing the history against the declaration: %v", err)
	}
	if len(changes) > 0 {
		t.Fatalf("the migration history no longer builds what evolveschema declares.\n\n"+
			"Someone edited schema.go without adding a migration, or edited a migration "+
			"without the schema. What is missing:\n\n%s\n"+
			"Generate it with:\n"+
			"    sqlb migrate -name <what-changed> ./example/evolve/evolveschema\n",
			describe(changes))
	}
}

// The three things the history did that a file comparison cannot see, checked
// against the database the replay produced rather than against the SQL that was
// supposed to produce it.
//
// Without this, the test above would pass on a history that reached the right
// shape by the wrong route — dropping and recreating the column a RENAME was
// supposed to move, for instance, which is the exact mistake ADR-0014 says
// inferring renames would cause.
func TestTheReplayedDatabaseShowsWhatEachRevisionDid(t *testing.T) {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, want := range []struct {
		what  string
		query string
		args  []any
	}{
		// Revision 4, the column rename.
		{"customers.email_address exists",
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_name = 'customers' AND column_name = 'email_address'`, nil},
		// Revision 4, the table rename.
		{"the support_agents table exists",
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_name = 'support_agents'`, nil},
		// Revision 2, the index that needed a file of its own.
		{"the composite index exists",
			`SELECT count(*) FROM pg_indexes
			 WHERE tablename = 'tickets' AND indexname = 'tickets_customer_id_status_idx'`, nil},
	} {
		var n int
		if err := pool.QueryRow(ctx, want.query, want.args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", want.what, err)
		}
		if n != 1 {
			t.Errorf("%s: found %d, want 1", want.what, n)
		}
	}

	// And the other direction, which is the half that would catch a rename done
	// as a drop-and-add: the old names must be gone, not merely shadowed.
	for _, gone := range []struct{ what, query string }{
		{"customers.email", `SELECT count(*) FROM information_schema.columns
			 WHERE table_name = 'customers' AND column_name = 'email'`},
		{"the agents table", `SELECT count(*) FROM information_schema.tables
			 WHERE table_name = 'agents'`},
		// Revision 5, the destructive one. It is live in the checked-in file
		// rather than commented out, which took -allow-destructive to render.
		{"tickets.legacy_ref", `SELECT count(*) FROM information_schema.columns
			 WHERE table_name = 'tickets' AND column_name = 'legacy_ref'`},
	} {
		var n int
		if err := pool.QueryRow(ctx, gone.query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", gone.what, err)
		}
		if n != 0 {
			t.Errorf("%s is still present after the history was replayed", gone.what)
		}
	}
}

// snapshotChecks records the declared check expressions so they can be put back
// after Normalize rewrites them, since the second test in this package
// should not inherit a registry in Postgres's spelling.
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
