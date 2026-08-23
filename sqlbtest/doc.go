// Package sqlbtest is the two doubles an application testing on sqlb needs: a
// scripted Executor that needs no database, and a scratch database that needs
// no container.
//
// [DB] is the first. [Fresh] is the second, and the split between them is the
// split every suite ends up making anyway — most tests against the double,
// where they are fast and answer "which statement did this issue", and a
// smaller number against Postgres, which is the only thing that can say whether
// the SQL is valid.
//
// The engine's own suite runs without Docker — `mise run test` compiles and runs
// in seconds against an in-memory pgx double — and until now consumers got none
// of that. An application wanting to test a hook, a handler or a hand-written
// query had to either put Docker in its unit loop or write its own pgx.Rows
// fake, which is nine methods with subtle semantics and is exactly the work
// internal/pgfake had already done. That was the single biggest week-one
// friction in a simulated adoption (issue #77).
//
// So this is that double, with a deliberately small surface. The internal
// package stays free to move; what is frozen here is only what a consumer's test
// needs.
//
// # What it is not
//
// Not a Postgres. It does not parse SQL, it does not evaluate a predicate, and
// it does not know that the WHERE clause your hook added would have excluded the
// row it is about to hand back. It answers whatever the script says, and its
// value is in what it *records*: the statements your code produced and the
// values it bound.
//
// That makes it right for the questions unit tests actually ask —
//
//   - did the hook's predicate reach the statement?
//   - did the handler bind the tenant id from the request rather than the body?
//   - does the generated handler keep the hidden column out of its projection?
//   - did the write run inside a transaction, and did a failure roll it back?
//
// — and wrong for "does this query return the right rows", which needs a real
// database. Both are worth having; sqlb keeps the split by running its own
// round-trip suite against containers in a separate module, and an application
// adopting sqlb should expect the same shape.
//
// # Using it
//
//	db := sqlbtest.New(
//	    sqlbtest.Reply{Cols: []string{"id", "title"}, Rows: [][]any{{"p1", "Hello"}}},
//	)
//	handle := sqlb.New(db).WithHooks(hooks)
//
//	if _, err := myHandler(ctx, handle, req); err != nil {
//	    t.Fatal(err)
//	}
//	if !strings.Contains(db.LastStatement(), `"tenant_id" = $1`) {
//	    t.Errorf("the scoping hook did not reach the statement:\n%s", db.LastStatement())
//	}
//
// A [DB] is safe for concurrent use, because the code under test may not be
// sequential.
//
// # And when the double is not enough
//
// [Fresh] creates a database of its own per test on a server the caller names,
// applies what the test needs, and drops it afterwards:
//
//	db := sqlbtest.Fresh(t,
//	    sqlbtest.DSN(t, "SQLB_TEST_POSTGRES", "run `docker compose up -d` first"),
//	    sqlbtest.Declared(schema.DefaultRegistry()),
//	)
//	handle := sqlb.New(db).WithHooks(hooks)
//
// It starts nothing. fresh.go says why at length; the short version is that
// this repository ran the other experiment — a container per package, through
// testcontainers — and reversed it.
package sqlbtest
