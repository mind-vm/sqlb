package main

// The scaffold's one test, and the reason it exists is placement rather than
// coverage.
//
// #287: a consumer ported a nine-table application onto sqlb — five scoped
// models, hooks for query/create/update/delete — and wrote the whole tenant
// boundary's test suite against a real Postgres without ever discovering
// sqlbtest. That is not a documentation-reading failure; sqlbtest/doc.go argues
// its own case better than anything else here does. The scaffold is what an
// adopter copies from, and the scaffold said nothing. Neither did sqlb.md,
// which does not otherwise contain the word "test".
//
// The two tests it emits are the two questions a round-trip test cannot answer
// at all, rather than the ones it answers more slowly:
//
//   - Did the predicate reach the statement? A round trip sees the right rows
//     whether the hook narrowed the query or the fixture happened to hold only
//     matching rows.
//   - Did the refusal issue no statement? A round trip sees zero rows and
//     cannot tell "the query ran and matched nothing" from "the query never
//     ran" — which is the difference between a boundary and a filter that
//     happened to be empty.
//
// It emits real, passing tests rather than a commented-out example, because a
// commented-out test is a thing to delete and a passing one is a thing to edit.
//
// Deliberately not scaffolded: testcontainers. The round-trip half is worth
// having, and it is a choice with consequences a scaffold should not make
// silently — the same reporter measured a container per package at 17.3s cold
// against a single long-lived compose Postgres at 4.0s cold and 0.4s warm, and
// an adopter who inherits the first shape rarely revisits it. sqlb.md points at
// sqlbtest.Fresh instead, which takes a DSN and starts nothing, and at the build
// tag that keeps the default `go test ./...` green on a machine with no
// Postgres — rather than a skip, because a suite that passes quietly when it
// cannot reach a database reports coverage it does not have.
//
// The emitted source uses no raw string literals, so this template needs no
// backtick gymnastics and stays one plain literal to read.
const initPredicateTest = `package {{.Pkg}}

// Two tests that need no database.
//
// github.com/mind-vm/sqlb/sqlbtest is a scripted Executor: it answers whatever
// it is told to, parses no SQL and evaluates no predicate. Its value is in what
// it records — the statements your code produced and the values it bound.
//
// This file is a template. The hook below stands in for the boundary your
// application will have (a tenant id stamped from a verified token, a role
// check, a soft-delete filter), because the scaffolded schema has none yet.
// Replace it with your own registry and the assertions keep their shape.
//
// What this cannot answer is "does this query return the right rows", which
// needs a real Postgres. Both are worth having — see sqlb.md, "Testing", for
// sqlbtest.Fresh and the build tag that keeps go test ./... green without one.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/sqlbtest"
)

// callerKey is how these tests hand the hook an identity. A real application's
// arrives from a token the middleware verified, and stamping it from there
// rather than from the request body is a fact about $1 — which is exactly what
// a statement assertion can check and a round trip cannot.
type callerKey struct{}

// hooks is the registry the server mounts. Move it into your application once
// it has one: cmd/server would pass sqlb.New(pool).WithHooks(hooks()).
func hooks() *sqlb.Registry {
	reg := sqlb.NewRegistry()
	sqlb.On[Task](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Task]) error {
		if ctx.Value(callerKey{}) == nil {
			return errors.New("this endpoint needs a caller")
		}
		q.Where(TaskCols.Done.Eq(false))
		return nil
	})
	return reg
}

// A hook that adds a predicate is only doing its job if the predicate is in the
// SQL, and reading the statement is the only way to know.
func TestTheHookPredicateReachesTheStatement(t *testing.T) {
	exec := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id", "title", "done"}})
	db := sqlb.New(exec).WithHooks(hooks())

	ctx := context.WithValue(context.Background(), callerKey{}, "someone")
	if _, err := sqlb.Query[Task]().All(ctx, db); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(exec.LastStatement(), "\"done\" = $1") {
		t.Errorf("the hook's predicate did not reach the statement:\n%s", exec.LastStatement())
	}
	// And the bound value came from the hook rather than from anything a caller
	// sent, which after a round trip is invisible: both spellings return the
	// same rows for every caller who asks for what they are entitled to.
	if args := exec.LastArgs(); len(args) != 1 || args[0] != false {
		t.Errorf("bound %v, want the single value the hook chose", args)
	}
}

// A refused read must issue no statement at all.
//
// Zero rows and no query are indistinguishable from outside, and they are not
// the same thing: one is a boundary, the other is a filter that happened to
// match nothing today.
func TestARefusedReadIssuesNoStatement(t *testing.T) {
	exec := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id", "title", "done"}})
	db := sqlb.New(exec).WithHooks(hooks())

	if _, err := sqlb.Query[Task]().All(context.Background(), db); err == nil {
		t.Fatal("a query with no caller should have been refused")
	}
	if stmts := exec.Statements(); len(stmts) != 0 {
		t.Errorf("the refusal still reached the database:\n%s", strings.Join(stmts, "\n"))
	}
}
`
