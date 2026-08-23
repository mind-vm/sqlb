package tasksevolved_test

// The database this module's one long test runs against: created per test,
// dropped after it, and never started here — sqlbtest.Fresh takes a DSN and
// starts nothing.
//
// This file used to carry that by hand, in the same eighty lines eight other
// suites carried. What is left is the one thing that is this module's own: it
// hands back a pool the whole walk shares, because the point of the test is
// that data carries from one non-additive change to the next rather than
// starting over.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/sqlbtest"
)

// pgEnv names the Postgres these tests run against. It must be Postgres 18:
// the schema uses schema.UUIDv7, and migrate.MinPostgres(18) emits the
// built-in uuidv7() rather than the pg_uuidv7 extension's spelling, so a
// migration generated for this module does not apply to an older server at
// all — a true statement about the module rather than a broken test.
const pgEnv = "SQLB_TEST_POSTGRES"

// freshDatabase returns a pool for an empty database. Empty, not migrated:
// building it is what the test is about.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sqlbtest.Fresh(t, sqlbtest.DSN(t, pgEnv, "run `mise run pg-up` first"))
}
