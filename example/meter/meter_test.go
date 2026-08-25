package meter_test

// The four things docs/special-cases.md's "meter" case asks a metering table
// to settle, run against real Postgres:
//
//  1. TestArithmeticUpsertUnderConcurrency — the write is an increment, and
//     the concurrency is real. OnConflictSet (#90) is what makes this one
//     statement rather than a read-modify-write race.
//  2. TestDateTruncBucket — the obvious Sel(Call{...}) date_trunc bucket does
//     not run; the Raw-literal form does.
//  3. TestEmptyRangeAggregateNeedsCoalesce — a bare Sum over a filter
//     matching nothing fails to scan rather than reporting zero.
//  4. TestUniqueIndexHoldsTheCompositeKey — the surrogate-plus-UniqueIndex
//     workaround for the composite key this table actually wants, proven by
//     the write it refuses.
//
// The bootstrap below (freshDatabase, TestMain) is example/fxapp's
// main_test.go pattern: an admin pool against SQLB_TEST_POSTGRES, a fresh
// CREATE DATABASE per test, and the schema built from
// migrate.Diff(nil, schema.DefaultRegistry(), migrate.MinPostgres(18)) rather
// than a hand-written CREATE TABLE — so what these tests run against is the
// DDL meterschema.Meter actually declares, not a paraphrase of it.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	_ "github.com/mind-vm/sqlb/example/meter/meterschema"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/sqlbtest"
)

// freshDatabase returns a handle to a database of its own, built from what the
// registry declares.
//
// The eighty lines this used to be — an admin pool behind a sync.Once, a name
// derived from the test, create, drop, apply — were the same eighty lines as in
// eight other suites in this repository. They are sqlbtest.Fresh now.
func freshDatabase(t *testing.T) *sqlb.DB {
	t.Helper()
	pool := sqlbtest.Fresh(t,
		sqlbtest.DSN(t, "SQLB_TEST_POSTGRES", "run `mise run pg-up` first"),
		sqlbtest.Declared(schema.DefaultRegistry(), migrate.MinPostgres(18)),
	)
	return sqlb.New(pool)
}

// Meter is the row struct these tests query and write through — the runtime
// half of ADR-0010: the schema DSL in meterschema declares the DDL, but
// nothing here imports it for that. sqlb reflects over this struct's tags at
// request time, so the columns below have to name what meterschema.Meter
// declares, and nothing ties the two together except a reader checking both.
type Meter struct {
	ID     int64     `db:"id" sqlb:"pk,default"`
	Tenant string    `db:"tenant" sqlb:"filter"`
	Kind   string    `db:"kind" sqlb:"filter"`
	At     time.Time `db:"at" sqlb:"filter,sort,default"`
	Count  int64     `db:"count" sqlb:"filter,default"`
}

func (Meter) TableName() string { return "meters" }

// TestArithmeticUpsertUnderConcurrency is the example's reason to exist: the
// arithmetic upsert the census called missing (#90's OnConflictSet closed it)
// under the concurrency a metering table actually has, not merely run twice in
// sequence.
//
// Twenty goroutines race to write the same (tenant, kind) key, each
// incrementing by 1 through
//
//	OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count")))
//
// The first writer's INSERT is not a conflict — its count comes straight from
// the VALUES list, so Current never runs on that branch — and every later
// writer's conflict resolves through the read-current-add-excluded expression,
// atomically, inside Postgres, with no read-modify-write gap on the Go side
// for another goroutine to land in. Twenty concurrent writers of +1 should
// leave exactly 20, and zero errors: a lost update here would be silent in
// exactly the way a metering system cannot afford.
func TestArithmeticUpsertUnderConcurrency(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()

	const workers = 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := sqlb.InsertRows(&Meter{Tenant: "acme", Kind: "api_call", Count: 1}).
				OnConflictUpdate([]string{"tenant", "kind"}).
				OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count"))).
				One(ctx, db)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) != 0 {
		t.Fatalf("%d of %d concurrent upserts failed, want 0: %v", len(failures), workers, failures)
	}

	got, err := sqlb.Query[Meter]().
		Where(sqlb.F("tenant").Eq("acme"), sqlb.F("kind").Eq("api_call")).
		One(ctx, db)
	if err != nil {
		t.Fatalf("reading back the counter: %v", err)
	}
	if got.Count != workers {
		t.Errorf("count = %d after %d concurrent +1 upserts, want %d — a lost update would show up here",
			got.Count, workers, workers)
	}
}

// TestDateTruncBucket adapts pgtest/census_test.go's
// TestDateTruncBucketNeedsALiteralUnit to this table: the obvious
// Sel(Call{...}) bucket, handed the same expression twice, fails; the
// Raw-literal form of the same bucket runs and totals correctly.
//
// Why it fails: GroupByExpr has to receive the *same* expression as the
// projection, and a Param inside that expression is not the same expression
// twice — the compiler numbers bind parameters per occurrence, so
// Param{"day"} becomes $1 in the projection and $2 in the GROUP BY. Postgres
// matches GROUP BY entries structurally, sees two different expressions, and
// refuses the query for not grouping by `at`. The fix is Raw{SQL: "'day'"}: a
// literal is textually identical both times because there is no bind position
// to number, which is safe because the unit here is a developer-chosen
// constant and never user input.
func TestDateTruncBucket(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()

	for _, m := range []Meter{
		{Tenant: "acme", Kind: "a", Count: 1},
		{Tenant: "acme", Kind: "b", Count: 2},
		{Tenant: "acme", Kind: "c", Count: 4},
	} {
		row := m
		if _, err := sqlb.InsertRows(&row).One(ctx, db); err != nil {
			t.Fatalf("seeding %+v: %v", row, err)
		}
	}

	type bucketRow struct {
		Bucket time.Time `db:"bucket"`
		Total  int64     `db:"total"`
	}

	// The spelling that reads correctly and does not run.
	parameterised := func() sqlb.Expr {
		return sqlb.Call{Name: "date_trunc", Args: []sqlb.Expr{
			sqlb.Param{Value: "day"}, sqlb.F("at").Column(),
		}}
	}
	_, err := sqlb.Collect[bucketRow](ctx, db, sqlb.Query[Meter]().ClearSelect().
		Select(sqlb.Sel(parameterised()).As("bucket"), sqlb.Sum(sqlb.F("count")).As("total")).
		GroupByExpr(parameterised()))
	if err == nil {
		t.Fatal("a parameterised date_trunc unit grouped successfully; " +
			"the compiler now shares one placeholder across the projection and the GROUP BY, and this test should record that instead")
	}
	if !strings.Contains(err.Error(), "GROUP BY") {
		t.Errorf("parameterised bucket failed with %v;\nwant Postgres complaining about the GROUP BY clause", err)
	}

	// The spelling that runs.
	literal := func() sqlb.Expr {
		return sqlb.Call{Name: "date_trunc", Args: []sqlb.Expr{
			sqlb.Raw{SQL: "'day'"}, sqlb.F("at").Column(),
		}}
	}
	got, err := sqlb.Collect[bucketRow](ctx, db, sqlb.Query[Meter]().ClearSelect().
		Select(sqlb.Sel(literal()).As("bucket"), sqlb.Sum(sqlb.F("count")).As("total")).
		GroupByExpr(literal()).
		OrderBy(sqlb.OrderBy(literal())))
	if err != nil {
		t.Fatalf("literal-unit bucket: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d buckets, want 1 (all three rows land on today): %#v", len(got), got)
	}
	if got[0].Total != 7 {
		t.Errorf("bucket total = %d, want 7", got[0].Total)
	}
}

// TestEmptyRangeAggregateNeedsCoalesce adapts
// pgtest/smallcases_test.go's TestAggregateOverAnEmptyRangeNeedsCoalesce to
// this table: exactly the shape a chart over a metering table hits the first
// time somebody asks for a tenant, or a day, with no rows in it.
//
// SQL says an aggregate over zero rows is NULL. database/sql (and pgx) says
// NULL does not convert to int64, so a bare Sum does not report zero — it
// fails to scan, with an error naming NULL rather than the empty range that
// produced it. Coalescing the sum against a Raw 0 fixes it and still reports
// the true total once there are rows. Count is the exception: COUNT(*) over
// no rows is 0, not NULL, so it needs no Coalesce and is why this trap is easy
// to miss until the first quiet day.
func TestEmptyRangeAggregateNeedsCoalesce(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()

	seed := Meter{Tenant: "acme", Kind: "api_call", Count: 5}
	if _, err := sqlb.InsertRows(&seed).One(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type total struct {
		Total int64 `db:"total"`
	}
	empty := func() *sqlb.Builder[Meter] {
		return sqlb.Query[Meter]().Where(sqlb.F("tenant").Eq("no-such-tenant")).ClearSelect()
	}

	_, err := sqlb.Collect[total](ctx, db, empty().Select(sqlb.Sum(sqlb.F("count")).As("total")))
	if err == nil {
		t.Fatal("sum over an empty range scanned into int64 without error; the trap is gone and this test should be rewritten")
	}
	if !strings.Contains(err.Error(), "NULL") {
		t.Errorf("sum over an empty range failed with %v;\nwant a scan error naming NULL", err)
	}

	got, err := sqlb.Collect[total](ctx, db, empty().
		Select(sqlb.Coalesce(sqlb.Sum(sqlb.F("count")).Expr(), sqlb.Raw{SQL: "0"}).As("total")))
	if err != nil {
		t.Fatalf("coalesced sum over an empty range: %v", err)
	}
	if len(got) != 1 || got[0].Total != 0 {
		t.Errorf("coalesced sum = %#v, want one row totalling 0", got)
	}

	// The real total still comes through when there are rows — the failure
	// mode Coalesce could plausibly introduce, and does not.
	nonEmpty := func() *sqlb.Builder[Meter] {
		return sqlb.Query[Meter]().Where(sqlb.F("tenant").Eq("acme")).ClearSelect()
	}
	got, err = sqlb.Collect[total](ctx, db, nonEmpty().
		Select(sqlb.Coalesce(sqlb.Sum(sqlb.F("count")).Expr(), sqlb.Raw{SQL: "0"}).As("total")))
	if err != nil {
		t.Fatalf("coalesced sum over a non-empty range: %v", err)
	}
	if len(got) != 1 || got[0].Total != 5 {
		t.Errorf("coalesced sum = %#v, want one row totalling 5", got)
	}

	// Count is the exception: it needs no Coalesce because COUNT(*) over zero
	// rows is 0, not NULL.
	type countTotal struct {
		Count int64 `db:"count"`
	}
	counted, err := sqlb.Collect[countTotal](ctx, db, empty().Select(sqlb.Count()))
	if err != nil {
		t.Fatalf("count over an empty range: %v", err)
	}
	if len(counted) != 1 || counted[0].Count != 0 {
		t.Errorf("count = %#v, want one row totalling 0 with no Coalesce needed", counted)
	}
}

// TestUniqueIndexHoldsTheCompositeKey checks the invariant the surrogate key
// exists to carry: (tenant, kind) is unique, enforced by the UniqueIndex
// meterschema.Meter declares because a composite PRIMARY KEY is refused
// (ADR-0034; pgtest/census_test.go's
// TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround proves the refusal and
// the workaround in general — this proves the workaround actually holds for
// this table).
//
// A plain second InsertRows at an existing (tenant, kind), with no conflict
// clause, is rejected — not silently accepted as a second row with a new
// surrogate id, which is exactly the failure a reader would expect the
// surrogate key to invite if nothing else were guarding it.
func TestUniqueIndexHoldsTheCompositeKey(t *testing.T) {
	db := freshDatabase(t)
	ctx := context.Background()

	first := Meter{Tenant: "acme", Kind: "api_call", Count: 1}
	if _, err := sqlb.InsertRows(&first).One(ctx, db); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	second := Meter{Tenant: "acme", Kind: "api_call", Count: 1}
	_, err := sqlb.InsertRows(&second).One(ctx, db)
	if err == nil {
		t.Fatal("a second unconditional insert at an existing (tenant, kind) was accepted; " +
			"the surrogate id let two rows claim one counter")
	}
	if !errors.Is(err, sqlb.ErrConstraint) {
		t.Fatalf("second insert: err = %v, want ErrConstraint", err)
	}

	var ce *sqlb.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("second insert: %v does not unwrap to *sqlb.ConstraintError", err)
	}
	if ce.Kind != sqlb.ConstraintUnique {
		t.Errorf("kind = %q, want %q", ce.Kind, sqlb.ConstraintUnique)
	}
	if ce.Constraint != "meters_tenant_kind_uniq" {
		t.Errorf("constraint = %q, want the index name UniqueIndex(\"tenant\", \"kind\") derives", ce.Constraint)
	}
}
