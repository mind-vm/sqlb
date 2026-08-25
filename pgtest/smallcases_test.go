package pgtest

// The five shapes docs/special-cases.md lists under "Smaller cases — a test,
// not an example": one shape each, adequately covered by features that already
// exist, and demonstrated nowhere until now.
//
// They live here rather than in the root package because what is in question is
// not what sqlb renders — the root tests already pin that — but what Postgres
// does with it. Three of the five turn on a behaviour no golden test can see: a
// conflicting insert returning nothing, a bind-parameter ceiling in the wire
// protocol, and an aggregate over no rows being NULL rather than zero.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// Payment carries the idempotency key its callers retry against.
type Payment struct {
	ID     int64  `db:"id" sqlb:"pk,default"`
	Key    string `db:"key" sqlb:"filter"`
	Amount int64  `db:"amount" sqlb:"filter,sort"`
}

func (Payment) TableName() string { return "payments" }

func paymentsDB(t *testing.T) *sqlb.DB {
	t.Helper()
	db := freshStockDB(t)
	mustExec(t, db, `
		CREATE TABLE payments (
			id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			key    text   NOT NULL,
			amount bigint NOT NULL DEFAULT 0
		);
		CREATE UNIQUE INDEX payments_key_uniq ON payments (key);
	`)
	return sqlb.New(db)
}

// TestIdempotencyKeyMakesASecondCallReturnTheFirstCallsRow settles the census
// row worth 28 lines in the corpus: a unique index, a conflicting insert, and
// RETURNING.
//
// The finding is that the obvious spelling does not do it. OnConflictDoNothing
// skips the row, and a skipped row is *absent* from RETURNING — so One used to
// report ErrNotFound with the caller's struct at its zero value, which is a
// retried payment arriving as "not found". Since #146 the pairing is refused
// outright, at the terminal, before the statement runs; this test now pins the
// refusal, because what the caller needs is a message rather than a sentinel
// that reads as a real database answer.
//
// What does do it is OnConflictUpdate with the conflict target as its own
// update column. `DO UPDATE SET key = EXCLUDED.key` is a write that changes
// nothing, and a written row is a returned row. That is the whole trick, and it
// is what the refusal now names.
//
// Deliberately not: a claim about concurrent retries of the *same* key. Two
// simultaneous inserts on one index serialise, and the loser takes the update
// branch — but proving that needs the contention harness in census_test.go, and
// it is a different question from the one this test answers.
func TestIdempotencyKeyMakesASecondCallReturnTheFirstCallsRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := paymentsDB(t)

	first := Payment{Key: "charge-7", Amount: 250}
	stored, err := sqlb.InsertRows(&first).One(ctx, db)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if stored.ID == 0 {
		t.Fatal("first call returned no generated id")
	}

	// The spelling that looks right and is not — now refused rather than
	// answered.
	retry := Payment{Key: "charge-7", Amount: 250}
	_, err = sqlb.InsertRows(&retry).OnConflictDoNothing("key").One(ctx, db)
	switch {
	case err == nil:
		t.Error("do-nothing retry was accepted; it answers ErrNotFound on the idempotent path")
	case errors.Is(err, sqlb.ErrNotFound):
		t.Errorf("do-nothing retry still reports a missing row: %v", err)
	case !strings.Contains(err.Error(), "OnConflictUpdate"):
		t.Errorf("the refusal should name the spelling that works, got: %v", err)
	}
	if retry.ID != 0 {
		t.Errorf("do-nothing retry wrote back id %d; a refused statement must not run", retry.ID)
	}

	// And it is refused before the statement runs, so the row count is
	// untouched — the assertion at the end of this test would pass either way,
	// since DO NOTHING inserts nothing anyway.
	if _, _, err := sqlb.InsertRows(&retry).OnConflictDoNothing("key").SQL(); err != nil {
		t.Errorf("the clause itself is fine and only the pairing is refused; SQL() = %v", err)
	}

	// The spelling that is.
	retry2 := Payment{Key: "charge-7", Amount: 250}
	got, err := sqlb.InsertRows(&retry2).OnConflictUpdate([]string{"key"}, "key").One(ctx, db)
	if err != nil {
		t.Fatalf("update-target-to-itself retry: %v", err)
	}
	if got.ID != stored.ID {
		t.Errorf("retry returned id %d, want the first call's id %d", got.ID, stored.ID)
	}
	if retry2.ID != stored.ID {
		t.Errorf("retry wrote back id %d, want %d", retry2.ID, stored.ID)
	}

	// And exactly one row exists either way, which is the property the whole
	// construct is for.
	n, err := sqlb.Query[Payment]().Where(sqlb.F("key").Eq("charge-7")).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("%d rows for one idempotency key, want 1", n)
	}
}

// Doc carries a version column, which is the whole subject.
type Doc struct {
	ID      int64  `db:"id" sqlb:"pk,default"`
	Body    string `db:"body" sqlb:"filter"`
	Version int64  `db:"version" sqlb:"filter,default"`
}

func (Doc) TableName() string { return "docs" }

// TestOptimisticConcurrencyRefusesTheStaleWriter settles the second smaller
// case: a version column, an update predicated on it, and the zero-rows path.
//
// The mechanism is entirely there — Where on the version, SetExpr for the
// bump, and One returning ErrNotFound when nothing matched, which is the
// sentinel rest already maps. What is missing is one level up: ErrNotFound is
// the same sentinel a genuinely absent row produces, so a handler cannot tell
// "no such document" (404) from "somebody else got there first" (409) without
// a second query. This test does that second query, because it is what an
// If-Match story would have to do too.
//
// Deliberately not: If-Match or ETag headers. The REST layer has no story for
// them, and inventing one in a test would be proposing a design rather than
// recording a behaviour.
func TestOptimisticConcurrencyRefusesTheStaleWriter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshStockDB(t)
	mustExec(t, raw, `
		CREATE TABLE docs (
			id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			body    text   NOT NULL,
			version bigint NOT NULL DEFAULT 1
		)`)
	db := sqlb.New(raw)

	d := Doc{Body: "first draft"}
	stored, err := sqlb.InsertRows(&d).One(ctx, db)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Both writers read version 1.
	read := stored.Version

	bump := func(body string) (Doc, error) {
		return sqlb.UpdateRows[Doc]().
			Set("body", body).
			SetExpr("version", sqlb.Binary{Op: "+", Left: sqlb.F("version").Column(), Right: sqlb.Param{Value: 1}}).
			Where(sqlb.F("id").Eq(stored.ID), sqlb.F("version").Eq(read)).
			One(ctx, db)
	}

	winner, err := bump("winner's text")
	if err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if winner.Version != read+1 {
		t.Errorf("version = %d after the winning write, want %d", winner.Version, read+1)
	}

	_, err = bump("loser's text")
	if !errors.Is(err, sqlb.ErrNotFound) {
		t.Fatalf("second writer: err = %v, want ErrNotFound", err)
	}

	// The distinction a 409 needs and the sentinel does not carry: the row is
	// there, so this is a conflict rather than a miss.
	exists, err := sqlb.Query[Doc]().Where(sqlb.F("id").Eq(stored.ID)).Exists(ctx, db)
	if err != nil {
		t.Fatalf("existence check: %v", err)
	}
	if !exists {
		t.Fatal("the row vanished; the ErrNotFound was a miss, not a conflict")
	}

	current, err := sqlb.Query[Doc]().Where(sqlb.F("id").Eq(stored.ID)).One(ctx, db)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if current.Body != "winner's text" {
		t.Errorf("body = %q, want the winner's; the stale write was applied", current.Body)
	}
}

// Sample is a (bucket, value) pair — the smallest shape that can be inserted in
// bulk, grouped, and aggregated over nothing.
type Sample struct {
	ID     int64  `db:"id" sqlb:"pk,default"`
	Bucket string `db:"bucket" sqlb:"filter,sort"`
	Value  int64  `db:"value" sqlb:"filter,sort"`
}

func (Sample) TableName() string { return "samples" }

func samplesDB(t *testing.T) *sqlb.DB {
	t.Helper()
	db := freshStockDB(t)
	mustExec(t, db, `
		CREATE TABLE samples (
			id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			bucket text   NOT NULL,
			value  bigint NOT NULL
		)`)
	return sqlb.New(db)
}

// TestBulkInsertIsOneStatementWithAParameterCeiling measures what the census
// says nothing measures: what InsertRows does at a thousand rows, and where it
// stops.
//
// InsertRows renders one INSERT with one VALUES tuple per row, so the cost is
// linear and the limit is not sqlb's at all — it is the extended query
// protocol's 65535 bind parameters, divided by the number of columns actually
// written. Two written columns here, so 32767 rows pass and 32768 do not.
//
// The number is worth pinning because the failure is not graceful and not
// obviously about size: it surfaces as a driver error, from a call that
// succeeded with a slightly smaller slice, in a message that names neither the
// row count nor the column count that produced it. A caller batching a large
// import needs to know the arithmetic before it hits the wall.
//
// Deliberately not: a benchmark. What matters here is the cliff and its
// arithmetic, and a timing number would rot against hardware without saying
// anything the cliff does not.
func TestBulkInsertIsOneStatementWithAParameterCeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := samplesDB(t)

	// The ordinary case first: a thousand rows in one statement, all of them
	// stored, all of them written back with their generated ids.
	rows := make([]*Sample, 1000)
	for i := range rows {
		rows[i] = &Sample{Bucket: "import", Value: int64(i)}
	}
	stored, err := sqlb.InsertRows(rows...).Exec(ctx, db)
	if err != nil {
		t.Fatalf("1000 rows: %v", err)
	}
	if len(stored) != 1000 {
		t.Errorf("stored %d rows, want 1000", len(stored))
	}
	for i, r := range rows {
		if r.ID == 0 {
			t.Fatalf("row %d was not written back with its generated id", i)
		}
	}

	// The ceiling. `id` carries a default and holds its zero value, so it is
	// omitted and two columns per row reach the wire.
	const perRow = 2
	const limit = 65535 / perRow // 32767

	under := make([]*Sample, limit)
	for i := range under {
		under[i] = &Sample{Bucket: "edge", Value: int64(i)}
	}
	if _, err := sqlb.InsertRows(under...).Exec(ctx, db); err != nil {
		t.Fatalf("%d rows (%d parameters) should be the largest that fits: %v", limit, limit*perRow, err)
	}

	over := make([]*Sample, limit+1)
	for i := range over {
		over[i] = &Sample{Bucket: "over", Value: int64(i)}
	}
	_, err = sqlb.InsertRows(over...).Exec(ctx, db)
	if err == nil {
		t.Fatalf("%d rows (%d parameters) exceeded the protocol limit and was accepted anyway", len(over), len(over)*perRow)
	}
	// The message is the driver's, not sqlb's, and not one a caller would guess
	// from the call site. Asserting on it is asserting that the failure is at
	// least legible.
	if !strings.Contains(err.Error(), "65535") {
		t.Errorf("over-limit insert failed with %v;\nwant a message naming the 65535-parameter limit", err)
	}
}

// TestDistinctOnIsReachableOnlyThroughRawSel settles the census row: Distinct
// exists, DISTINCT ON does not, and "latest row per group" is how DISTINCT ON
// is used.
//
// The workaround is positional rather than structural: RawSel as the *first*
// projection item lands the fragment where Postgres expects the modifier, and
// the ordering has to be built with OrderBy over a Raw so that its leading
// terms match the distinct expression. Both of those are conventions a caller
// has to know and nothing enforces — put the RawSel second and the statement
// is a syntax error at the database rather than a build error in Go.
//
// So this records a workaround that works and says why it is not a feature.
//
// Deliberately not: a proposal for the spelling. Whether DISTINCT ON belongs in
// the builder at all is the open question; this only shows what the escape
// hatch costs today.
func TestDistinctOnIsReachableOnlyThroughRawSel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := samplesDB(t)

	for _, s := range []Sample{
		{Bucket: "a", Value: 1}, {Bucket: "a", Value: 9}, {Bucket: "a", Value: 4},
		{Bucket: "b", Value: 2}, {Bucket: "b", Value: 7},
	} {
		row := s
		if _, err := sqlb.InsertRows(&row).One(ctx, db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// The builder's own Distinct is whole-row, which is a different operator
	// and cannot answer "the highest value per bucket".
	q := sqlb.Query[Sample]().Distinct()
	text, _, err := q.SQL()
	if err != nil {
		t.Fatalf("Distinct().SQL(): %v", err)
	}
	if !strings.Contains(text, "SELECT DISTINCT ") || strings.Contains(text, "DISTINCT ON") {
		t.Errorf("Distinct rendered %q; expected plain SELECT DISTINCT", text)
	}

	type top struct {
		Bucket string `db:"bucket"`
		Value  int64  `db:"value"`
	}
	got, err := sqlb.Collect[top](ctx, db, sqlb.Query[Sample]().ClearSelect().
		Select(sqlb.RawSel(`DISTINCT ON ("bucket") "bucket"`), sqlb.F("value")).
		OrderBy(sqlb.F("bucket").Asc(), sqlb.F("value").Desc()))
	if err != nil {
		t.Fatalf("DISTINCT ON through RawSel: %v", err)
	}

	want := []top{{Bucket: "a", Value: 9}, {Bucket: "b", Value: 7}}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAggregateOverAnEmptyRangeNeedsCoalesce is the trap the exchange report
// names, next to the fix.
//
// SQL says an aggregate over no rows is NULL, and database/sql says NULL does
// not convert to int64. So a revenue chart with a date filter matching nothing
// does not return zero — it fails to scan, with an error naming a column index
// rather than the empty range that caused it. Count is the exception, which is
// what makes the trap easy to miss: the shape works until the first quiet week.
//
// Coalesce is the fix, and it is a documented method nobody has connected to
// this failure. That connection is the point of the test.
//
// Deliberately not: a change to Sum. Making the builder coalesce by default
// would make "no rows" and "rows summing to zero" indistinguishable, which is
// a real distinction in a ledger.
func TestAggregateOverAnEmptyRangeNeedsCoalesce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := samplesDB(t)

	s := Sample{Bucket: "a", Value: 5}
	if _, err := sqlb.InsertRows(&s).One(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	type total struct {
		Total int64 `db:"total"`
	}
	// A filter that matches nothing, which is what an empty reporting window is.
	empty := func() *sqlb.Builder[Sample] {
		return sqlb.Query[Sample]().Where(sqlb.F("bucket").Eq("no-such-bucket")).ClearSelect()
	}

	_, err := sqlb.Collect[total](ctx, db, empty().Select(sqlb.Sum(sqlb.F("value")).As("total")))
	if err == nil {
		t.Fatal("sum over an empty range scanned into int64 without error; the trap is gone and this test should be rewritten")
	}
	if !strings.Contains(err.Error(), "NULL") {
		t.Errorf("sum over an empty range failed with %v;\nwant a scan error naming NULL", err)
	}

	got, err := sqlb.Collect[total](ctx, db, empty().
		Select(sqlb.Coalesce(sqlb.Sum(sqlb.F("value")).Expr(), sqlb.Raw{SQL: "0"}).As("total")))
	if err != nil {
		t.Fatalf("coalesced sum over an empty range: %v", err)
	}
	if len(got) != 1 || got[0].Total != 0 {
		t.Errorf("coalesced sum = %#v, want one row totalling 0", got)
	}

	// And it still reports the real total when there are rows, which is the
	// half a Coalesce could plausibly break.
	got, err = sqlb.Collect[total](ctx, db, sqlb.Query[Sample]().ClearSelect().
		Select(sqlb.Coalesce(sqlb.Sum(sqlb.F("value")).Expr(), sqlb.Raw{SQL: "0"}).As("total")))
	if err != nil {
		t.Fatalf("coalesced sum over a populated range: %v", err)
	}
	if len(got) != 1 || got[0].Total != 5 {
		t.Errorf("coalesced sum = %#v, want one row totalling 5", got)
	}

	// Count is the exception, and the reason the trap survives review: it
	// answers zero rather than NULL, so the shape looks safe.
	n, err := sqlb.Query[Sample]().Where(sqlb.F("bucket").Eq("no-such-bucket")).Count(ctx, db)
	if err != nil {
		t.Fatalf("count over an empty range: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// Batched is a row whose defaulted column has a default worth telling apart
// from a zero: the whole question is whether a zero-valued row in a mixed batch
// takes the default or writes its zero.
type Batched struct {
	ID    int64  `db:"id" sqlb:"pk,default"`
	Name  string `db:"name" sqlb:"filter"`
	Tier  string `db:"tier" sqlb:"filter,default"`
	Quota int64  `db:"quota" sqlb:"filter,default"`
}

func (Batched) TableName() string { return "batched" }

// A defaulted column left zero takes its default whether or not a batch-mate
// filled it in.
//
// InsertRows omits a defaulted column only when *every* row leaves it zero, so
// in a mixed batch the column stays and the zero row used to bind an explicit
// zero: the same row got the database's default when inserted alone and a zero
// when inserted beside a neighbour (#73). The fix emits the DEFAULT keyword in
// that row's own tuple, and this is where the claim that Postgres accepts a
// per-position DEFAULT in a multi-row VALUES stops being something read in a
// manual.
func TestMixedBatchTakesTheDefaultPerRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := freshStockDB(t)
	mustExec(t, db, `
		CREATE TABLE batched (
			id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name  text   NOT NULL,
			tier  text   NOT NULL DEFAULT 'free',
			quota bigint NOT NULL DEFAULT 100
		)`)
	h := sqlb.New(db)

	set := &Batched{Name: "set", Tier: "pro", Quota: 5}
	unset := &Batched{Name: "unset"}

	stored, err := sqlb.InsertRows(set, unset).Exec(ctx, h)
	if err != nil {
		t.Fatalf("mixed batch: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored %d rows, want 2", len(stored))
	}

	if stored[0].Tier != "pro" || stored[0].Quota != 5 {
		t.Errorf("the row that set its columns stored %+v, want tier=pro quota=5", stored[0])
	}
	// The whole point. Before the fix this was tier="" — which this table's
	// NOT NULL would have caught — and quota=0, which it would not.
	if stored[1].Tier != "free" || stored[1].Quota != 100 {
		t.Errorf("the row that left its defaulted columns zero stored %+v, "+
			"want the table's defaults tier=free quota=100", stored[1])
	}

	// And the same row alone stores the same thing, which is the property that
	// was violated: a row's semantics must not depend on its batch-mates.
	solo := &Batched{Name: "solo"}
	one, err := sqlb.InsertRows(solo).Exec(ctx, h)
	if err != nil {
		t.Fatalf("solo insert: %v", err)
	}
	if one[0].Tier != stored[1].Tier || one[0].Quota != stored[1].Quota {
		t.Errorf("solo stored %+v but the same row in a batch stored %+v", one[0], stored[1])
	}
}
