package pgtest

// sqlbtest.Fresh against a real Postgres.
//
// The helper's own module cannot test this — the engine's suite stays runnable
// with no database, which is the property that makes it a fast inner loop — so
// the half that needs one is here, where every other database-backed claim in
// this repository is settled.
//
// It matters more than a helper usually would: nine suites' bootstrap is now
// this one function, so a defect in it is a defect in all of them at once.

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/schema"
	"github.com/jryannel/sqlb/sqlbtest"
)

// nameOf reads back which database a pool is connected to.
func nameOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatalf("asking which database this is: %v", err)
	}
	return name
}

// exists asks the server, through a connection that is not the pool under test.
func exists(t *testing.T, database string) bool {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), serverDSN(t))
	if err != nil {
		t.Fatalf("opening a connection to ask about %s: %v", database, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	var found bool
	if err := conn.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, database).Scan(&found); err != nil {
		t.Fatalf("asking whether %s exists: %v", database, err)
	}
	return found
}

// The base claim: a database of this test's own, empty, and reachable.
func TestFreshMakesAnEmptyDatabaseOfItsOwn(t *testing.T) {
	t.Parallel()
	pool := sqlbtest.Fresh(t, serverDSN(t))

	name := nameOf(t, pool)
	if !strings.Contains(name, "freshmakesanemptydatabase") {
		t.Errorf("database %q is not named after the test, which is what makes a failure findable", name)
	}

	var tables int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&tables); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if tables != 0 {
		t.Errorf("%d tables in a database that should be empty", tables)
	}
}

// Two calls in one test are two databases. survey_test.go depends on it: a
// round trip renders into a second, scratch database and compares.
func TestTwoCallsInOneTestAreTwoDatabases(t *testing.T) {
	t.Parallel()
	first := nameOf(t, sqlbtest.Fresh(t, serverDSN(t)))
	second := nameOf(t, sqlbtest.Fresh(t, serverDSN(t)))

	if first == second {
		t.Errorf("both calls landed in %s, so a test needing two scratch databases has one", first)
	}
}

// The cleanup is the half a leaked database would make expensive: a suite that
// left one behind per test would fill a server over an afternoon of runs.
func TestTheDatabaseIsDroppedWhenTheTestEnds(t *testing.T) {
	var name string
	t.Run("inner", func(t *testing.T) {
		name = nameOf(t, sqlbtest.Fresh(t, serverDSN(t)))
		if !exists(t, name) {
			t.Fatalf("%s does not exist while the test using it is running", name)
		}
	})

	if exists(t, name) {
		t.Errorf("%s outlived its test", name)
	}
}

// The options run in the order they are written, which is the property an
// extension before the schema that needs it depends on.
func TestOptionsBuildTheDatabaseInOrder(t *testing.T) {
	t.Parallel()
	pool := sqlbtest.Fresh(t, serverDSN(t),
		sqlbtest.SQL(`CREATE TABLE first (id int PRIMARY KEY)`),
		sqlbtest.SQL(`ALTER TABLE first ADD COLUMN second text`),
		sqlbtest.Do(func(ctx context.Context, pool *pgxpool.Pool) error {
			_, err := pool.Exec(ctx, `INSERT INTO first (id, second) VALUES (1, 'seeded')`)
			return err
		}),
	)

	var seeded string
	if err := pool.QueryRow(context.Background(), `SELECT second FROM first WHERE id = 1`).Scan(&seeded); err != nil {
		t.Fatalf("reading what the options built: %v", err)
	}
	if seeded != "seeded" {
		t.Errorf("second = %q, want the seeded row", seeded)
	}
}

// Declared is what eight of the nine converted suites use: the DDL the registry
// renders now, rather than a history replayed.
func TestDeclaredAppliesTheSchemaTheRegistryDescribes(t *testing.T) {
	t.Parallel()
	reg := schema.NewRegistry()
	reg.Table("widgets",
		schema.Text("id").PrimaryKey(),
		schema.Text("name").Searchable(),
	)

	pool := sqlbtest.Fresh(t, serverDSN(t), sqlbtest.Declared(reg))

	var found bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = 'public' AND table_name = 'widgets')`).Scan(&found); err != nil {
		t.Fatalf("looking for the table: %v", err)
	}
	if !found {
		t.Error("the declared table is not there")
	}
}

// FreshDSN is for the caller that opens its own connection — fxapp boots from a
// URL, pgbouncer needs a second route to the same database — and what it hands
// back has to be a database that is already built.
func TestFreshDSNReturnsABuiltDatabase(t *testing.T) {
	t.Parallel()
	dsn := sqlbtest.FreshDSN(t, serverDSN(t), sqlbtest.SQL(`CREATE TABLE ready (id int)`))

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("the DSN is not a URL: %v", err)
	}
	if parsed.Path == "/postgres" || parsed.Path == "" {
		t.Errorf("the DSN still names the maintenance database: %s", parsed.Path)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("opening what FreshDSN returned: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(context.Background(), `INSERT INTO ready (id) VALUES (1)`); err != nil {
		t.Errorf("the table the options created is not there: %v", err)
	}
}

// MaxConns is the ceiling every parallel suite runs into, so it has to reach
// the pool rather than being advice.
func TestMaxConnsReachesThePool(t *testing.T) {
	t.Parallel()
	pool := sqlbtest.Fresh(t, serverDSN(t), sqlbtest.MaxConns(3))

	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("MaxConns = %d, want 3", got)
	}
}
