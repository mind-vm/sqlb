// Package pgtest runs sqlb against a real Postgres.
//
// # Where the database comes from
//
// Not from here. These tests read a DSN out of the environment and start
// nothing:
//
//	SQLB_TEST_POSTGRES    a plain Postgres 18
//	SQLB_TEST_PGVECTOR    one with the vector extension available
//	SQLB_TEST_PGBOUNCER   a PgBouncer in transaction pooling, in front of the first
//
// compose.yaml at the repository root defines all three, `mise run pg-up`
// starts them, and every database-backed mise task depends on that. In CI they
// are service containers on the same ports. Creating a database per test on one
// of them is [sqlbtest.Fresh]'s job — this module used to carry those eighty
// lines itself, in six variants, and so did eight other suites. There is deliberately no
// skip-when-absent path and no fallback that starts a container: a suite that
// silently passes when it cannot reach a database is worse than one that fails,
// because it reports coverage it does not have. An unset variable is a fatal
// error naming the task that fixes it.
//
// It was not always this way, and the reversal is worth recording. Each package
// used to start its own container through testcontainers, which cost a full
// `mise run ci` six servers, put docker/docker and forty modules in this go.mod,
// and shipped a reaper that reaps by label — and therefore removed long-lived
// containers belonging to entirely unrelated work on the same machine. Taking a
// DSN costs one line of setup and has none of those properties.
//
// # Why a separate module
//
// The reason is narrower than it used to be, and the honest version is short:
// the root module's suite must stay runnable with no database at all, and a
// nested module is excluded from the parent's `go list ./...` by construction.
// `mise run test` therefore cannot accidentally acquire a Docker dependency,
// which is what makes it a usable inner loop.
//
// The older and stronger reason has now gone. `deps-check` runs `go list -deps`,
// which does not report test-only imports, so testcontainers in the root module
// would have left that gate reporting a short list while go.mod grew forty
// modules — the failure mode ADR-0016 exists to prevent, and one this repository
// has already hit three times. With provisioning outside the test binary this
// module's own dependencies are pgx and sqlb, so that argument no longer
// applies. Folding these packages back into the root under a build tag is now a
// defensible thing to want; it is not done here because moving ten thousand
// lines of tests to save a go.mod is a bad trade, not because it would break
// anything.
//
// # What this is for
//
// The engine's own tests use an in-memory executor and need no database, which
// is a property worth keeping: it is what makes `mise run test` a fast inner
// loop. What that cannot answer is whether the SQL sqlb generates is *valid*
// rather than merely *expected*. Golden tests compare rendered DDL against a
// string somebody wrote; these apply it to Postgres and let Postgres judge.
//
// ADR-0040's port is the sharpest illustration this module has: both bugs the
// driver flip introduced — a catalog column misread, and a rejected write
// reported as a mapping error — were invisible to a canned result set and
// caught here.
//
// ADR-0014 holds at Medium confidence for exactly this reason — its round-trip
// was measured by hand, and the scripts that measured it are gone. The tests
// here are that measurement, committed.
//
// # Running
//
//	mise run test-pg
//
// That starts the servers if they are not already up, and runs with -parallel 8
// — a cap rather than the core-count default, because each test's pool is
// capped at 8 and the connection ceiling is the product of the two.
// [sqlbtest.Fresh]: https://pkg.go.dev/github.com/mind-vm/sqlb/sqlbtest#Fresh
package pgtest
