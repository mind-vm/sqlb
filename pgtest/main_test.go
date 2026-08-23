package pgtest

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/sqlbtest"
)

// pgEnv names the Postgres these tests run against. Provisioning is the
// caller's job — `mise run pg-up` locally, a service container in CI — and this
// module no longer starts one itself.
//
// That is a deliberate reversal of how this suite began, and the reason is in
// doc.go: testcontainers brought docker/docker and forty modules behind it, and
// its reaper reaps by label, which means a test run could and did remove
// long-lived containers somebody else was using. A DSN has neither problem.
const pgEnv = "SQLB_TEST_POSTGRES"

// TestMain fails the whole module when there is no database configured, rather
// than letting each test discover it.
//
// There is deliberately no skip: a suite that passes quietly when it cannot
// reach a database reports coverage it does not have, which is the failure this
// module exists to prevent wearing a different hat.
func TestMain(m *testing.M) {
	if os.Getenv(pgEnv) == "" {
		log.Fatalf("pgtest: %s is not set.\n"+
			"These tests need a Postgres; they do not start one.\n"+
			"  locally: mise run pg-up   (then mise run test-pg)\n"+
			"  CI:      the service containers in .github/workflows/ci.yml", pgEnv)
	}
	os.Exit(m.Run())
}

// serverDSN is the server every database in this module is created on.
func serverDSN(t testing.TB) string {
	t.Helper()
	return sqlbtest.DSN(t, pgEnv, "run `mise run pg-up` first")
}

// poolSize caps what one test's pool may open. pgxpool's default is the core
// count, which is the wrong basis here: with the suite parallel, the total is
// that number squared, and on a large machine it walks into max_connections
// while every one of those connections sits idle.
//
// Eight rather than four, which would also fit every test today: census's queue
// test runs four workers competing for the tail of the same queue, and a pool
// sized exactly to the workers deadlocks the moment anything else in that test
// wants a connection at the same time.
//
// It pairs with the -parallel cap in mise.toml's test-pg. Eight connections by
// eight concurrent tests is 64, which fits inside a stock server's 100 without
// the harness having to configure the server at all.
const poolSize = 8

// bootstrap is what the generated DDL assumes exists and Postgres does not
// provide, as the options that install it.
//
// One function rather than two options written out at each call site, because
// the pairing is the point and it has already been broken once: freshDB,
// vectorDB and the shadow database each installed both by hand, and a refactor
// that kept the shim and dropped the extension turned three suites red at once
// on the only server that has neither. A database created from a template that
// happens to carry btree_gist cannot tell you that.
func bootstrap() []sqlbtest.Option {
	return []sqlbtest.Option{
		sqlbtest.SQL(shim),
		// btree_gist, because an exclusion that pairs a scalar `=` with a range
		// `&&` needs gist to have an operator class for the scalar — which is
		// the shape every real double-booking constraint has. It ships with
		// Postgres's contrib, so no image change; it does have to be created,
		// which is exactly the step Diff renders nothing for and the extension
		// report exists to name (issues #121, #115).
		sqlbtest.Extensions("btree_gist"),
	}
}

// withBootstrap is the caller's own options plus the bootstrap ones, which is
// how every database that has sqlb's DDL rendered into it is built.
func withBootstrap(opts ...sqlbtest.Option) []sqlbtest.Option {
	return append(opts, bootstrap()...)
}

// shim is the function the generated DDL assumes exists.
//
// schema.GenUUIDv7 emits uuid_generate_v7(), which is the pg_uuidv7 extension's
// spelling and is documented as requiring it. Postgres 18 has a built-in
// uuidv7(), so a one-line shim gives the generated DDL something to bind to
// without pulling an extension image in.
//
// Worth stating plainly, because the test cannot: generated DDL for a UUIDv7
// primary key does not apply to a stock Postgres. That is a real gap in what
// sqlb emits, not an artefact of this harness.
const shim = `
	CREATE FUNCTION uuid_generate_v7() RETURNS uuid
	LANGUAGE sql VOLATILE AS 'SELECT uuidv7()'
`

// freshDB creates an empty database with the shim installed, and returns a
// connection to it, dropped when the test ends. A database per test rather than
// a server per test: CREATE DATABASE is milliseconds, and the server is already
// running before `go test` starts.
//
// It is also what lets the suite run in parallel. Tests here share a Postgres
// but not a database, so they were already independent by construction — the
// serial run was leaving that on the table. Adding t.Parallel() to the tests
// that could take it cut the module from 49s to about 12s, and the per-test
// database is the whole reason that was safe rather than a rewrite.
//
// Two tests do not take it, and the compiler will not tell you why: t.Chdir and
// t.Setenv panic under t.Parallel, because they mutate process-wide state. Those
// are in drift_test.go and sqlbmigrate_test.go and they stay serial.
func freshDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return sqlbtest.Fresh(t, serverDSN(t), withBootstrap(sqlbtest.MaxConns(poolSize))...)
}

// freshStockDB is the same thing without the shim: a Postgres exactly as it
// ships. It is what proves migrate.MinPostgres(18) produces DDL that needs no
// extension, which is a claim the shimmed database cannot test — with
// uuid_generate_v7() defined, both spellings work and the difference is
// invisible.
func freshStockDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return sqlbtest.Fresh(t, serverDSN(t), sqlbtest.MaxConns(poolSize))
}

func mustExec(t testing.TB, db *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), query); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, strings.TrimSpace(query))
	}
}
