package app_test

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
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/mind-vm/sqlb/example/tasks/migrations"
)

// The tests run against a real Postgres, for the reason pgtest exists: a suite
// that asserts on generated SQL proves the generator produces what somebody
// expected, and a suite that runs it proves Postgres accepts it. This example
// makes claims that only the second kind can check — that the composite foreign
// keys reject a cross-workspace reference, that the completed_at trigger fires,
// that a rolled-back transaction leaves no comment behind.
//
// There is deliberately no skip-when-the-database-is-absent path. A suite that
// passes silently when it cannot reach one reports coverage it does not have.

// pgEnv names the Postgres these tests run against. They do not start one:
// provisioning is `mise run pg-up` locally and a service container in CI, which
// is what lets six suites share one server instead of starting six.
//
// It must be Postgres 18. cmd/migrate passes migrate.MinPostgres(18), so the
// DDL uses the built-in uuidv7() and needs no extension; against 17 it would
// fail at the first CREATE TABLE, which is a true statement about the demo
// rather than a broken test. The version is pinned in compose.yaml and in the
// workflow, where the server is started.
const pgEnv = "SQLB_TEST_POSTGRES"

var (
	admin *pgxpool.Pool
	dsn   func(database string) string
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		log.Fatalf("tasks: %v", err)
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
				"  locally: mise run pg-up   (then mise run test-demo)",
			pgEnv)
	}
	u, err := url.Parse(base)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid URL: %w", pgEnv, err)
	}
	// Swapping one path segment, rather than rebuilding the DSN from parsed
	// components: everything else the caller wrote — sslmode, an
	// application_name — should survive untouched.
	dsn = func(database string) string {
		v := *u
		v.Path = "/" + database
		return v.String()
	}

	admin, err = pgxpool.New(ctx, dsn("postgres"))
	if err != nil {
		return 0, fmt.Errorf("opening the admin connection: %w", err)
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		return 0, fmt.Errorf("%s is set but nothing answered: %w", pgEnv, err)
	}

	return m.Run(), nil
}

// freshDB returns a connection to an empty database with the migrations
// applied — which is also a test of the migrations, run once per test.
//
// A database per test rather than a container per test: starting Postgres
// dominates, CREATE DATABASE is milliseconds, and a test that shares tables
// with another eventually depends on the order they run in.
func freshDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	name := databaseName(t)
	// Dropped first, so that a crashed run leaves nothing that makes the next
	// one fail with "already exists" instead of its real problem.
	mustExec(t, admin, `DROP DATABASE IF EXISTS `+quoteIdent(name))
	mustExec(t, admin, `CREATE DATABASE `+quoteIdent(name))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn(name))
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		// A database with open connections cannot be dropped, and a pool holds
		// them, so closing is not always enough on its own.
		_, _ = admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`)
	})

	// goose is a database/sql runner; it gets a handle over this pool rather
	// than a second connection to the same database.
	gooseDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = gooseDB.Close() }()
	if err := migrations.Apply(ctx, gooseDB); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return pool
}

// databaseName derives a legal, unique database name from the test name.
func databaseName(t *testing.T) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, t.Name())

	// Postgres truncates identifiers at 63 bytes, which would collide two long
	// subtests into one database and produce a failure that looks like a bug in
	// the code under test.
	const max = 40
	if len(name) > max {
		name = name[:max]
	}
	return fmt.Sprintf("t_%s_%d", name, time.Now().UnixNano()%1e9)
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func mustExec(t *testing.T, pool *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), query); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, strings.TrimSpace(query))
	}
}
