package fxapp_test

import (
	"testing"

	"github.com/mind-vm/sqlb/sqlbtest"
)

// The tests that exercise the server run against a real Postgres, for the
// reason pgtest exists: a suite that asserts on generated SQL proves the
// generator produced what somebody expected, and a suite that runs it proves
// Postgres accepts it. The claims here are of the second kind — that the
// migrations apply, that the boot-time provisioning is idempotent, that a
// space boundary held by query hooks actually holds.
//
// There is no skip-when-the-database-is-absent path: a suite that passes
// silently when it cannot reach one reports coverage it does not have.
//
// The connection is resolved on first use rather than in TestMain, so that the
// tests which need no database — the graph validation, which constructs
// nothing — run without one configured. That is not the same thing as skipping:
// a test that needs Postgres and cannot have it still fails.

// pgEnv names the Postgres these tests run against. They do not start one:
// provisioning is `mise run pg-up` locally and a service container in CI.
//
// It must be Postgres 18. cmd/migrate passes migrate.MinPostgres(18), so the
// DDL uses the built-in uuidv7() and needs no extension; against 17 it fails at
// the first CREATE TABLE, which is a true statement about the example rather
// than a broken test. The version is pinned where the server is started.
const pgEnv = "SQLB_TEST_POSTGRES"

// freshDatabase returns the DSN of an empty database.
//
// Empty, not migrated: applying the history is the fxkit glue's job, so a test
// that migrated first would be testing a different program than the one that
// ships. Every boot in this file therefore also asserts that the migrations
// apply.
//
// A database per test rather than a server per test: CREATE DATABASE is
// milliseconds, and a test that shares tables with another eventually depends
// on the order they run in. That is sqlbtest.FreshDSN's doing now — this file
// used to hold the eighty lines that did it, and eight other suites in this
// repository held their own copy of the same eighty.
func freshDatabase(t *testing.T) string {
	t.Helper()
	return sqlbtest.FreshDSN(t, sqlbtest.DSN(t, pgEnv, "run `mise run pg-up` first"))
}
