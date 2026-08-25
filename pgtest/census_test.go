package pgtest

// The shapes docs/special-cases.md counts in its census, reduced to the part
// that can be settled without building an application.
//
// The document proposes six worked examples. Each of them is mostly
// scaffolding — a schema, a server, a client — wrapped around one or two
// questions about what sqlb actually does. These are those questions, asked
// directly. They do not replace the examples: nothing here says what a
// generated REST surface looks like over an aggregate, or what a second year of
// migrations does to a schema. They do mean the examples would be written
// against measured behaviour rather than against the census's reading of the
// source.
//
// Several of these assert that something does *not* work. That is deliberate:
// an absence nobody has written down is rediscovered by every reader, and an
// absence with a test is a decision. Each such test says what the workaround is
// and what it costs, so that closing the gap makes the test fail loudly rather
// than silently going stale.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// Meter is the metering table of the proposed `meter` example: one row per
// (tenant, day, kind), many concurrent producers, read as a chart.
//
// The key is a surrogate with a unique index beside it, because that is the
// workaround ADR-0034's refusal names — see
// TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround.
type Meter struct {
	ID      int64     `db:"id" sqlb:"pk,default"`
	Tenant  string    `db:"tenant" sqlb:"filter,sort"`
	Kind    string    `db:"kind" sqlb:"filter,sort"`
	At      time.Time `db:"at" sqlb:"filter,sort,default"`
	Count   int64     `db:"count" sqlb:"filter,sort"`
	Payload []byte    `db:"payload" sqlb:"filter,default"`
}

func (Meter) TableName() string { return "meters" }

func metersDB(t *testing.T) *sqlb.DB {
	t.Helper()
	db := freshStockDB(t)
	mustExec(t, db, `
		CREATE TABLE meters (
			id      bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant  text        NOT NULL,
			kind    text        NOT NULL,
			at      timestamptz NOT NULL DEFAULT now(),
			count   bigint      NOT NULL DEFAULT 0,
			payload jsonb       NOT NULL DEFAULT '{}'
		);
		CREATE UNIQUE INDEX meters_tenant_kind_uniq ON meters (tenant, kind);
	`)
	return sqlb.New(db)
}

// TestUpsertIncrementNeedsAnExpression was the census's first standout finding
// and is now its demonstration — which is what the assertion below said should
// happen if the expression form ever landed. It did, in #90.
//
// The default is unchanged and still the right one: OnConflictUpdate copies
// EXCLUDED.<col>, so a second write of 3 over an existing 5 leaves 3. For every
// non-arithmetic upsert — a profile, a cache entry, a settings row — that is
// exactly right, which is why it stays the default and why the arithmetic form
// is something a declaration asks for.
//
// What has changed is what a counter has to do about it. OnConflictSet now
// expresses the increment in one statement, inside the builder, with the model
// and the hooks and RETURNING intact:
//
//	OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count")))
//
// SetExpr is kept below because it is still a different thing rather than a
// worse one: it increments a row that is already there, and says nothing about
// the row that is not. The raw-SQL workaround this test used to carry is gone,
// because leaving the builder is no longer the price of an atomic counter.
func TestUpsertIncrementNeedsAnExpression(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	first := Meter{Tenant: "acme", Kind: "api_call", Count: 5}
	if _, err := sqlb.InsertRows(&first).
		OnConflictUpdate([]string{"tenant", "kind"}, "count").One(ctx, db); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := Meter{Tenant: "acme", Kind: "api_call", Count: 3}
	got, err := sqlb.InsertRows(&second).
		OnConflictUpdate([]string{"tenant", "kind"}, "count").One(ctx, db)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got.Count != 3 {
		t.Fatalf("count = %d after upserting 3 over 5; OnConflictUpdate copies EXCLUDED, so 3 is the contract "+
			"and the arithmetic form is opt-in", got.Count)
	}

	// Workaround one: SetExpr, which does express the arithmetic — but only for
	// a row that already exists.
	updated, err := sqlb.UpdateRows[Meter]().
		SetExpr("count", sqlb.Binary{Op: "+", Left: sqlb.F("count").Column(), Right: sqlb.Param{Value: 5}}).
		Where(sqlb.F("tenant").Eq("acme"), sqlb.F("kind").Eq("api_call")).
		One(ctx, db)
	if err != nil {
		t.Fatalf("SetExpr increment: %v", err)
	}
	if updated.Count != 8 {
		t.Errorf("count = %d after SetExpr(+5) over 3, want 8", updated.Count)
	}

	// And the thing the census said was missing: one statement, atomic, and it
	// does not leave the builder. 4 over the 8 SetExpr just wrote is 12.
	incremented, err := sqlb.InsertRows(&Meter{Tenant: "acme", Kind: "api_call", Count: 4}).
		OnConflictUpdate([]string{"tenant", "kind"}).
		OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count"))).
		One(ctx, db)
	if err != nil {
		t.Fatalf("arithmetic upsert: %v", err)
	}
	if incremented.Count != 12 {
		t.Errorf("count = %d after incrementing by 4 over 8, want 12", incremented.Count)
	}

	// The row it lands on when there is none: the insert's own value, not an
	// increment of a row that was never there. Current reads the stored row, and
	// on the insert branch there is nothing to read, so the branch never runs.
	fresh, err := sqlb.InsertRows(&Meter{Tenant: "acme", Kind: "webhook", Count: 4}).
		OnConflictUpdate([]string{"tenant", "kind"}).
		OnConflictSet("count", sqlb.Add(sqlb.Current("count"), sqlb.Excluded("count"))).
		One(ctx, db)
	if err != nil {
		t.Fatalf("arithmetic upsert on a fresh key: %v", err)
	}
	if fresh.Count != 4 {
		t.Errorf("count = %d on a key that did not exist, want the inserted 4", fresh.Count)
	}
}

// TestDateTruncBucketNeedsALiteralUnit settles half of the census's GROUP BY
// row, and finds something the census did not.
//
// "A date_trunc bucket needs Sel(Call{…})" is true, and the obvious way to
// write it is broken. GroupByExpr has to be handed the *same* expression as the
// projection, and an expression carrying a Param is not the same expression
// twice: the compiler numbers the two occurrences $1 and $2, and Postgres —
// which matches GROUP BY entries structurally — sees two different expressions
// and refuses the query for not grouping by `at`.
//
// The fix is to write the unit as a literal through Raw rather than as a bind
// parameter. That is safe here because the unit is a developer-chosen constant
// and never user input; a unit that did come from a request would have to be
// mapped through an allow-list before it reached this, exactly as Call.Name and
// Field.Cast already require.
//
// Deliberately not: the REST surface over the result. The census's real
// complaint about rollups is that there is no aggregate response shape at all,
// and that is an example's job, not a test's.
func TestDateTruncBucketNeedsALiteralUnit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	for i, m := range []Meter{
		{Tenant: "acme", Kind: "a", Count: 1},
		{Tenant: "acme", Kind: "b", Count: 2},
		{Tenant: "acme", Kind: "c", Count: 4},
	} {
		row := m
		if _, err := sqlb.InsertRows(&row).One(ctx, db); err != nil {
			t.Fatalf("seed %d: %v", i, err)
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

// TestRelativeTimeWindowNeedsRawOrAGoComputedInstant settles the census row
// worth 34 lines: `now() - interval …` has no spelling.
//
// There are two ways to write it and they are not equivalent. RawPred puts the
// arithmetic in the database, which is where a retry backoff or a token expiry
// wants it: one clock, and the boundary is evaluated at execution. Computing
// the instant in Go and binding it works through the ordinary typed API, and
// moves the clock to the application — so two application hosts disagreeing by
// a few seconds disagree about which rows are expired.
//
// Both are recorded because the second is the one a reader will reach for
// without noticing it changed which clock decides.
//
// Deliberately not: an interval literal or a now() helper. Whether those belong
// in the builder is open; what is settled here is that neither exists and what
// the absence costs.
func TestRelativeTimeWindowNeedsRawOrAGoComputedInstant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	recent := Meter{Tenant: "acme", Kind: "recent", Count: 1}
	if _, err := sqlb.InsertRows(&recent).One(ctx, db); err != nil {
		t.Fatalf("seed recent: %v", err)
	}
	// A row deliberately outside every window below.
	if _, err := db.Exec(ctx,
		`INSERT INTO meters (tenant, kind, at) VALUES ($1, $2, now() - interval '2 days')`,
		"acme", "old"); err != nil {
		t.Fatalf("seed old: %v", err)
	}

	// The database's clock, through the escape hatch.
	n, err := sqlb.Query[Meter]().
		Where(sqlb.RawPred(`"at" > now() - interval '1 hour'`)).Count(ctx, db)
	if err != nil {
		t.Fatalf("RawPred window: %v", err)
	}
	if n != 1 {
		t.Errorf("RawPred window matched %d rows, want 1", n)
	}

	// The application's clock, through the typed API. Same answer here, and a
	// different clock — which is the finding.
	n, err = sqlb.Query[Meter]().
		Where(sqlb.F("at").Gt(time.Now().Add(-time.Hour))).Count(ctx, db)
	if err != nil {
		t.Fatalf("Go-computed window: %v", err)
	}
	if n != 1 {
		t.Errorf("Go-computed window matched %d rows, want 1", n)
	}
}

// TestADayFilterAgainstTimestamptzAnswersTheDay is the census row that was a
// gap: `?day=eq.2026-07-30` against a timestamptz column became `at = $1`,
// Postgres parsed the date as midnight in the session time zone, and the
// comparison against a timestamp that is almost never exactly midnight returned
// zero rows and no error — the worst combination, because there is nothing to
// notice.
//
// This test used to assert that failure and say what would fix it: a cast the
// builder could not express, because Field.Cast returns an Expr and every
// comparison hangs off Field, so `at::date = $1::date` was writable in a SELECT
// list and not in a WHERE clause. Both halves are now built (#241).
//
// The design question it deliberately left open — half-open range or ::date
// equality — is settled here as the range, and the two are asserted to select
// the same rows. The range is what an index on the column can serve; the
// equality is what reads more obviously, and it is the one this compares
// against so that "the same set" is a claim under test rather than an argument.
func TestADayFilterAgainstTimestamptzAnswersTheDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	m := Meter{Tenant: "acme", Kind: "api_call", Count: 1}
	if _, err := sqlb.InsertRows(&m).One(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The day the row was written, as a client would spell it in a query
	// string. Read from the database rather than from Go, so that a host clock
	// on the other side of midnight cannot make this test flaky for a reason
	// that has nothing to do with what it measures.
	type today struct {
		Day string `db:"day"`
	}
	days, err := sqlb.Collect[today](ctx, db, sqlb.Query[Meter]().ClearSelect().
		Select(sqlb.RawSel(`(now() AT TIME ZONE 'UTC')::date::text`).As("day")).Limit(1))
	if err != nil || len(days) != 1 {
		t.Fatalf("reading today: %v (%d rows)", err, len(days))
	}
	day := days[0].Day

	n, err := sqlb.Query[Meter]().Where(sqlb.F("at").OnDay(day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("day filter: %v", err)
	}
	if n != 1 {
		t.Errorf("OnDay matched %d rows, want the row written today", n)
	}

	// The spelling this test was written to recommend, now that both are
	// available: the same set, by a plan an index cannot serve.
	cast, err := sqlb.Query[Meter]().
		Where(sqlb.RawPred(`"at"::date = ?::date`, day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("cast day filter: %v", err)
	}
	if cast != n {
		t.Errorf("OnDay matched %d rows and ::date equality matched %d; they are meant to be the same set", n, cast)
	}

	// The bare comparison still means what it says — an instant, at midnight —
	// and still matches nothing. That is correct for a builder: what changed is
	// that the URL grammar refuses to compile it from a date, naming `day.`
	// instead (filter/day_test.go).
	bare, err := sqlb.Query[Meter]().Where(sqlb.F("at").Eq(day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("bare day filter: %v", err)
	}
	if bare != 0 {
		t.Errorf("equality against midnight matched %d rows, want 0", bare)
	}
}

// TestJSONBIsStoredAndOnlyFilteredThroughRaw records the census's third
// standout finding: 96 lines declare jsonb and zero filter into it.
//
// Storing and reading back is ordinary — a []byte column, no ceremony. What has
// no operator is reaching *into* the document, and RawPred does it fine. The
// census reads the corpus's silence as permission not to build jsonb operators;
// this pins the behaviour that permission is granted against, so that a future
// argument for building them starts from what is there rather than from
// intuition.
//
// Deliberately not: a GIN index, or an argument about jsonb containment
// performance. The question here is expressibility.
func TestJSONBIsStoredAndOnlyFilteredThroughRaw(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	m := Meter{Tenant: "acme", Kind: "api_call", Payload: []byte(`{"tier":"gold","region":"eu"}`)}
	stored, err := sqlb.InsertRows(&m).One(ctx, db)
	if err != nil {
		t.Fatalf("insert with a document: %v", err)
	}
	if !strings.Contains(string(stored.Payload), `"tier"`) {
		t.Errorf("payload came back as %q, want the stored document", stored.Payload)
	}

	// Reaching into the document, both spellings the corpus would use.
	n, err := sqlb.Query[Meter]().
		Where(sqlb.RawPred(`"payload" ->> 'tier' = ?`, "gold")).Count(ctx, db)
	if err != nil {
		t.Fatalf("->> filter: %v", err)
	}
	if n != 1 {
		t.Errorf("->> filter matched %d rows, want 1", n)
	}

	n, err = sqlb.Query[Meter]().
		Where(sqlb.RawPred(`"payload" @> ?::jsonb`, `{"region":"eu"}`)).Count(ctx, db)
	if err != nil {
		t.Fatalf("@> filter: %v", err)
	}
	if n != 1 {
		t.Errorf("@> filter matched %d rows, want 1", n)
	}
}

// Job is the outbox row of the proposed `outbox` example: claimed by exactly
// one worker out of a pool of competing consumers.
type Job struct {
	ID        int64  `db:"id" sqlb:"pk,default"`
	Topic     string `db:"topic" sqlb:"filter"`
	Status    string `db:"status" sqlb:"filter,sort,default"`
	ClaimedBy string `db:"claimed_by" sqlb:"filter,default"`
}

func (Job) TableName() string { return "jobs" }

// TestClaimHandsEachRowToExactlyOneWorker settles the `outbox` example's first
// question: that ForUpdate and SkipLocked work under contention rather than
// merely compile.
//
// The census records them as working and undocumented, with no example walking
// them. Compiling is not the claim worth making — `SELECT … FOR UPDATE SKIP
// LOCKED` is four words of SQL. The claim worth making is that a pool of
// workers running the loop concurrently never double-claims a row, and that
// needs real connections holding real locks, which is why it lives here and not
// in the root package's fake driver.
//
// The lock is held by the transaction, so the claim has to be inside WithTx —
// under autocommit the row unlocks the instant the SELECT returns and two
// workers claim it. That is the part an example would exist to show.
//
// Deliberately not: retry, backoff, dead-lettering, or LISTEN/NOTIFY. Those are
// the rest of the proposed example and they are about a design for a change
// feed, not about whether the lock holds.
func TestClaimHandsEachRowToExactlyOneWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshStockDB(t)
	mustExec(t, raw, `
		CREATE TABLE jobs (
			id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			topic      text   NOT NULL,
			status     text   NOT NULL DEFAULT 'pending',
			claimed_by text   NOT NULL DEFAULT ''
		)`)
	db := sqlb.New(raw)

	const jobs = 12
	for i := range jobs {
		j := Job{Topic: fmt.Sprintf("event-%d", i)}
		if _, err := sqlb.InsertRows(&j).One(ctx, db); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	// The seed relies on the database defaults, so a mistagged column would
	// leave every row outside the claim predicate and the assertions below
	// would then be measuring nothing.
	if n, err := sqlb.Query[Job]().Where(sqlb.F("status").Eq("pending")).Count(ctx, db); err != nil || n != jobs {
		t.Fatalf("seeded %d pending jobs (err %v), want %d", n, err, jobs)
	}

	var mu sync.Mutex
	claims := map[int64][]string{}

	var wg sync.WaitGroup
	for w := range 4 {
		worker := fmt.Sprintf("worker-%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Enough passes that every worker competes for the tail of the
			// queue, which is where a double claim would show up.
			for range jobs {
				err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
					pending, err := sqlb.Query[Job]().
						Where(sqlb.F("status").Eq("pending")).
						OrderBy(sqlb.F("id").Asc()).
						Limit(1).
						ForUpdate().SkipLocked().
						All(ctx, tx)
					if err != nil || len(pending) == 0 {
						return err
					}
					job := pending[0]

					// Hold the lock long enough that the other workers are
					// certainly inside their own SELECT while this one is open.
					time.Sleep(5 * time.Millisecond)

					if _, err := sqlb.UpdateRows[Job]().
						Set("status", "claimed").
						Set("claimed_by", worker).
						Where(sqlb.F("id").Eq(job.ID)).
						One(ctx, tx); err != nil {
						return err
					}

					mu.Lock()
					claims[job.ID] = append(claims[job.ID], worker)
					mu.Unlock()
					return nil
				})
				if err != nil {
					t.Errorf("%s: %v", worker, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(claims) != jobs {
		t.Errorf("%d of %d jobs were claimed; SkipLocked should skip a locked row, not lose it", len(claims), jobs)
	}
	for id, by := range claims {
		if len(by) != 1 {
			t.Errorf("job %d was claimed %d times, by %v", id, len(by), by)
		}
	}

	remaining, err := sqlb.Query[Job]().Where(sqlb.F("status").Eq("pending")).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting leftovers: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d jobs left pending, want 0", remaining)
	}
}

// Invitation carries the invariant that lives in a partial index.
type Invitation struct {
	ID     int64  `db:"id" sqlb:"pk,default"`
	Email  string `db:"email" sqlb:"filter"`
	Status string `db:"status" sqlb:"filter"`
}

func (Invitation) TableName() string { return "invitations" }

// TestAnInvariantInAPartialIndexArrivesAsANamedConstraint settles the first of
// the census's four "shapes worth arguing about".
//
// `CREATE UNIQUE INDEX … WHERE status = 'pending'` is how a corpus says *one
// pending invitation per email*. The document's complaint is that nothing in
// the schema tells a reader a business rule is in there, and that the REST
// layer cannot turn its violation into a 409 that says which rule was hit.
//
// Half of that turns out to be already true. The violation arrives as a
// *ConstraintError carrying the index name, which is a name the schema chose —
// so a handler can match on `one_pending_invitation_per_email` and answer with
// the rule rather than with "duplicate key".
//
// The other half used to stand: reaching the name needed a driver-aware
// SetErrorClassifier, and this test was the repository's only worked example of
// one. ADR-0040 retired that — sqlb reads *pgconn.PgError itself now — so the
// registration is gone from this test and what it asserts is stronger for it:
// the fields below are filled with nothing registered at all.
//
// Deliberately not: the schema-side declaration the census argues for. Whether
// a partial unique index should be a named declaration like Check is the open
// design question; this settles what the runtime already gives that design to
// build on.
func TestAnInvariantInAPartialIndexArrivesAsANamedConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshStockDB(t)
	mustExec(t, raw, `
		CREATE TABLE invitations (
			id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email  text   NOT NULL,
			status text   NOT NULL
		);
		CREATE UNIQUE INDEX one_pending_invitation_per_email
			ON invitations (email) WHERE status = 'pending';
	`)
	db := sqlb.New(raw)

	pending := Invitation{Email: "ada@example.com", Status: "pending"}
	if _, err := sqlb.InsertRows(&pending).One(ctx, db); err != nil {
		t.Fatalf("first pending invitation: %v", err)
	}

	// The predicate is what makes the index an invariant rather than a
	// uniqueness rule: a second invitation for the same address is fine once
	// the first is no longer pending.
	accepted := Invitation{Email: "ada@example.com", Status: "accepted"}
	if _, err := sqlb.InsertRows(&accepted).One(ctx, db); err != nil {
		t.Fatalf("an accepted invitation is outside the index and must be allowed: %v", err)
	}

	second := Invitation{Email: "ada@example.com", Status: "pending"}
	_, err := sqlb.InsertRows(&second).One(ctx, db)
	if !errors.Is(err, sqlb.ErrConstraint) {
		t.Fatalf("second pending invitation: err = %v, want ErrConstraint", err)
	}

	var ce *sqlb.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("second pending invitation: %v does not unwrap to *ConstraintError", err)
	}
	if ce.Kind != sqlb.ConstraintUnique {
		t.Errorf("kind = %q, want %q", ce.Kind, sqlb.ConstraintUnique)
	}
	// This is the assertion the 409 would be built on, and since ADR-0040 it
	// holds with no classifier registered.
	if ce.Constraint != "one_pending_invitation_per_email" {
		t.Errorf("constraint = %q, want the index name the schema chose", ce.Constraint)
	}
	if ce.Table != "invitations" {
		t.Errorf("table = %q, want %q", ce.Table, "invitations")
	}
}

// TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround settles the census's
// second standout finding: composite keys are not an edge case — 26 lines and
// roughly 15 tables, every m2m link table among them.
//
// ADR-0034 refuses them, and the value of the refusal is entirely in whether it
// tells the reader what to do instead. It does: the error names UniqueIndex and
// the surrogate key, which is the shape `Meter` above is declared in. That is
// worth a test, because an error message is the only documentation a caller
// hitting this will read, and nothing else guards its text.
//
// Deliberately not: an argument about whether the refusal is right. The census
// says ADR-0034 already concedes the refusal is wider than its argument; this
// pins the behaviour so a decision to narrow it fails here first.
func TestCompositePrimaryKeyIsRefusedAndNamesItsWorkaround(t *testing.T) {
	t.Parallel()
	r := schema.NewRegistry()
	r.Table("memberships",
		schema.UUID("user_id").PrimaryKey(),
		schema.UUID("team_id").PrimaryKey(),
		schema.Timestamps(),
	)

	_, err := migrate.Diff(schema.NewRegistry(), r)
	if err == nil {
		t.Fatal("two primary-key columns were accepted; ADR-0034 refuses them, so this test should record the new behaviour instead")
	}
	for _, want := range []string{"memberships", "primary keys", "UniqueIndex", "surrogate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q — a caller reading only this error would not know what to do instead:\n%v", want, err)
		}
	}

	// The workaround the error names, applied: a surrogate key, and the pair
	// carried by a unique index.
	ok := schema.NewRegistry()
	ok.Table("memberships",
		schema.UUIDv7("id").PrimaryKey(),
		schema.UUID("user_id"),
		schema.UUID("team_id"),
		schema.Timestamps(),
	).UniqueIndex("user_id", "team_id")

	changes, err := migrate.Diff(schema.NewRegistry(), ok)
	if err != nil {
		t.Fatalf("the workaround the error recommends does not compile: %v", err)
	}
	if !strings.Contains(describe(changes), "CREATE UNIQUE INDEX") {
		t.Errorf("the workaround rendered no unique index:\n%s", describe(changes))
	}
}

// TestSelfReferenceIsAPlainColumnWithoutAForeignKey settles the census row the
// document calls universal and counts zero times: a parent_id pointing at its
// own table.
//
// Ref(name, target *TableDef) needs the target value, which does not exist yet
// inside its own Table(…) call, and there is no AddField — so the relation
// cannot be declared at all in the direct form. This test does not demonstrate
// that: it does not compile, which is the strongest possible demonstration and
// an unwritable test.
//
// What it does demonstrate is the fallback and its cost. ExternalRef names the
// target by string and renders the column and its index, so a tree is
// *storable*. What it does not render is the foreign key — so a parent_id
// pointing at a row that was deleted is a state the database will accept, and
// the cycle protection a self-referencing tree needs is not there either.
//
// Deliberately not: recursive traversal. WITH RECURSIVE is a documented
// non-goal, and a tree that cannot be declared has a more basic problem than
// one that cannot be walked.
func TestSelfReferenceIsAPlainColumnWithoutAForeignKey(t *testing.T) {
	t.Parallel()
	db := freshDB(t)

	r := schema.NewRegistry()
	r.Table("categories",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable(),
		schema.ExternalRef("parent", "categories").Nullable().Filterable(),
		schema.Timestamps(),
	)

	applySchema(t, db, r)

	// The column is there.
	var columns int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'categories' AND column_name = 'parent_id'
	`).Scan(&columns); err != nil {
		t.Fatalf("looking for parent_id: %v", err)
	}
	if columns != 1 {
		t.Fatalf("categories has %d parent_id columns, want 1", columns)
	}

	// The foreign key is not.
	var fks int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'categories' AND c.contype = 'f'
	`).Scan(&fks); err != nil {
		t.Fatalf("counting foreign keys: %v", err)
	}
	if fks != 0 {
		t.Fatalf("categories has %d foreign key(s); ExternalRef is documented to give one up, "+
			"so this test should record the new behaviour instead", fks)
	}

	// Which is not a theoretical loss: the database accepts a parent that is
	// not there, and a real FK would have refused it.
	var id string
	if err := db.QueryRow(context.Background(), `
		INSERT INTO categories (name, parent_id)
		VALUES ('orphan', '00000000-0000-7000-8000-000000000000')
		RETURNING id
	`).Scan(&id); err != nil {
		t.Fatalf("without a foreign key this insert is expected to succeed: %v", err)
	}
}
