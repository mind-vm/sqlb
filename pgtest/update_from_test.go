package pgtest

// The round-trip half of #174's test plan: sqlb.Update.From renders
// `WITH <name> AS (<select>) UPDATE … FROM <name> …`, and the engine's own
// suite can only check the rendered text. What it cannot check is that the
// text is what it looks like: that the CTE's own bind (the claim predicate)
// and the outer statement's binds (the SET values) land at the positions
// their placeholders name, and that FOR UPDATE SKIP LOCKED still does its job
// once it is sitting inside a CTE instead of a bare SELECT. Both need a real
// server and, for the second one, real contention — a fake driver has nothing
// to lock.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// ClaimJob is the queue row the issue's worked example claims: a batch
// selected with ForUpdate().SkipLocked(), marked and returned in the same
// statement.
type ClaimJob struct {
	ID          int64     `db:"id" sqlb:"pk,default"`
	Topic       string    `db:"topic" sqlb:"filter"`
	Attempts    int64     `db:"attempts" sqlb:"default"`
	ClaimedBy   string    `db:"claimed_by" sqlb:"default"`
	AvailableAt time.Time `db:"available_at" sqlb:"sort,default"`
}

func (ClaimJob) TableName() string { return "claim_jobs" }

func claimJobDB(t *testing.T) *sqlb.DB {
	t.Helper()
	raw := freshStockDB(t)
	mustExec(t, raw, `
		CREATE TABLE claim_jobs (
			id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			topic        text        NOT NULL,
			attempts     bigint      NOT NULL DEFAULT 0,
			claimed_by   text        NOT NULL DEFAULT '',
			available_at timestamptz NOT NULL DEFAULT now()
		)`)
	return sqlb.New(raw)
}

// claimBatch is the statement under test: one round trip that locks a batch,
// marks it as this worker's and hands the marked rows back. available_at is
// pushed into the future on claim, which is what makes a second claim not see
// these rows again — the backoff a real dispatcher would use between
// attempts, stood in for here with a fixed offset since computing it from a
// duration is #173's raw-SQL gap, deliberately not this one's problem.
func claimBatch(ctx context.Context, db sqlb.Executor, worker string, n int) ([]ClaimJob, error) {
	claimed := sqlb.Query[ClaimJob]().
		Select(sqlb.F("id")).
		Where(sqlb.F("available_at").Lte(time.Now())).
		OrderBy(sqlb.F("available_at").Asc()).
		Limit(n).
		ForUpdate().SkipLocked()

	return sqlb.UpdateRows[ClaimJob]().
		From("claimed", claimed).
		Set("claimed_by", worker).
		Set("available_at", time.Now().Add(time.Hour)).
		SetExpr("attempts", sqlb.Add(sqlb.F("attempts"), sqlb.Val(1))).
		Where(sqlb.F("id").EqField(sqlb.F("id").Qualify("claimed"))).
		Exec(ctx, db)
}

// TestUpdateFromClaimsExactlyTheLockedBatch seeds more rows than one claim
// asks for, claims a batch smaller than the seed, and checks both sides of the
// line: the claimed rows carry the write, the rest do not, and a second claim
// call — the one a drain loop makes on its next pass — picks up exactly what
// the first left rather than anything it already took.
func TestUpdateFromClaimsExactlyTheLockedBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := claimJobDB(t)

	const seeded = 10
	for i := range seeded {
		row := ClaimJob{Topic: fmt.Sprintf("event-%d", i)}
		if _, err := sqlb.InsertRows(&row).One(ctx, db); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
	}

	const firstBatch = 4
	first, err := claimBatch(ctx, db, "worker-1", firstBatch)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != firstBatch {
		t.Fatalf("first claim returned %d rows, want %d", len(first), firstBatch)
	}
	firstIDs := map[int64]bool{}
	for _, row := range first {
		firstIDs[row.ID] = true
		if row.Attempts != 1 {
			t.Errorf("claimed row %d: attempts = %d, want 1", row.ID, row.Attempts)
		}
		if row.ClaimedBy != "worker-1" {
			t.Errorf("claimed row %d: claimed_by = %q, want worker-1", row.ID, row.ClaimedBy)
		}
		if !row.AvailableAt.After(time.Now()) {
			t.Errorf("claimed row %d: available_at = %s, want pushed into the future", row.ID, row.AvailableAt)
		}
	}

	// Unclaimed rows must be untouched by the first call: this is what proves
	// the CTE's WHERE (inside the SELECT) and the outer WHERE (o.id =
	// claimed.id) are both doing their job rather than one of them silently
	// matching everything.
	untouched, err := sqlb.Query[ClaimJob]().Where(sqlb.F("claimed_by").Eq("")).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting untouched rows: %v", err)
	}
	if untouched != seeded-firstBatch {
		t.Fatalf("%d rows still unclaimed, want %d", untouched, seeded-firstBatch)
	}

	// A second claim, asking for more than remains: it must return exactly the
	// rest, none of it overlapping the first batch — the check that a bind
	// bound to the wrong position (the risk the issue calls out) would surface
	// as a wrong set of rows rather than as a compile error.
	second, err := claimBatch(ctx, db, "worker-2", seeded)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != seeded-firstBatch {
		t.Fatalf("second claim returned %d rows, want %d", len(second), seeded-firstBatch)
	}
	for _, row := range second {
		if firstIDs[row.ID] {
			t.Errorf("second claim re-claimed row %d, which the first claim already took", row.ID)
		}
		if row.ClaimedBy != "worker-2" {
			t.Errorf("row %d: claimed_by = %q, want worker-2", row.ID, row.ClaimedBy)
		}
	}

	remaining, err := sqlb.Query[ClaimJob]().Where(sqlb.F("claimed_by").Eq("")).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting leftovers: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d rows left unclaimed after both calls, want 0", remaining)
	}
}

// TestUpdateFromSkipLockedHoldsUnderContention is census_test.go's
// TestClaimHandsEachRowToExactlyOneWorker, replayed against the single-
// statement CTE form instead of the two-statement SELECT-then-UPDATE it
// proved. That test settled that ForUpdate/SkipLocked work under contention at
// all; this one settles that folding the SELECT into a WITH and joining the
// UPDATE against it did not quietly reopen the race — which is exactly the
// kind of thing a rewrite that only changes SQL shape can get wrong without
// any single-connection test noticing, because a single connection never
// contends with itself.
func TestUpdateFromSkipLockedHoldsUnderContention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := claimJobDB(t)

	const jobs = 12
	for i := range jobs {
		row := ClaimJob{Topic: fmt.Sprintf("event-%d", i)}
		if _, err := sqlb.InsertRows(&row).One(ctx, db); err != nil {
			t.Fatalf("seeding row %d: %v", i, err)
		}
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
			// queue, the same margin census_test.go's version uses and for the
			// same reason: that is where a double claim would show up.
			for range jobs {
				err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
					got, err := claimBatch(ctx, tx, worker, 1)
					if err != nil || len(got) == 0 {
						return err
					}
					// Hold the row lock long enough that the other workers are
					// certainly inside their own claim while this one is open —
					// the lock is what a bare two-statement rewrite would still
					// need and the CTE form must not have lost.
					time.Sleep(5 * time.Millisecond)

					mu.Lock()
					claims[got[0].ID] = append(claims[got[0].ID], worker)
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
		t.Errorf("%d of %d jobs were claimed; SkipLocked inside the CTE should skip a locked row, not lose it", len(claims), jobs)
	}
	for id, by := range claims {
		if len(by) != 1 {
			t.Errorf("job %d was claimed %d times, by %v — SkipLocked did not survive the CTE rewrite", id, len(by), by)
		}
	}
}
