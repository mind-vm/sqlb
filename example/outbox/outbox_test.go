// Package outbox_test settles docs/special-cases.md's "outbox" case: a
// transactional outbox and a pool of competing consumers.
//
// pgtest/census_test.go's TestClaimHandsEachRowToExactlyOneWorker already
// proves the mechanism — ForUpdate().SkipLocked() inside a db.WithTx
// boundary hands each row to exactly one worker under real contention — on a
// bare claim-only table with no retry policy around it. This suite repeats
// that proof once, on this package's real schema, and then goes on to what
// that pgtest test explicitly does not cover: a retry policy with
// exponential backoff, and a dead-letter rule. See ../README.md for what is
// settled here and what is still one opinion among several reasonable ones.
package outbox_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/outbox"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/sqlbtest"

	// Imported for its side effect: declaring Event registers it in
	// schema.DefaultRegistry(), which outboxDB below diffs against an empty
	// registry to get the DDL — the same baseline-migration path
	// example/rooms and example/tasks/cmd/migrate/main.go use.
	_ "github.com/mind-vm/sqlb/example/outbox/outboxschema"
)

// outboxDB migrates a fresh database from the declared schema and returns a
// *sqlb.DB over it.
//
// The main_test.go that used to sit beside this file carried the bootstrap by
// hand, and said why: "every one of these lean examples is a standalone module
// by design, so the alternative is a module whose only purpose is a helper the
// others would import — more machinery than it would save". The helper turned
// out not to need a module of its own; it is in sqlb, beside the scripted
// Executor, so the file is gone.
func outboxDB(t *testing.T) *sqlb.DB {
	t.Helper()
	pool := sqlbtest.Fresh(t,
		sqlbtest.DSN(t, "SQLB_TEST_POSTGRES", "run `mise run pg-up` first"),
		// Eight, because the competing-consumers test races workers through
		// this pool.
		sqlbtest.MaxConns(8),
		sqlbtest.Declared(schema.DefaultRegistry(), migrate.MinPostgres(18)),
	)
	return sqlb.New(pool)
}

func seedEvent(t *testing.T, ctx context.Context, db *sqlb.DB, topic string) outbox.OutboxEvent {
	t.Helper()
	e := outbox.OutboxEvent{Topic: topic, Payload: []byte(`{}`)}
	got, err := sqlb.InsertRows(&e).One(ctx, db)
	if err != nil {
		t.Fatalf("seeding event %q: %v", topic, err)
	}
	return got
}

func forceAvailableNow(t *testing.T, ctx context.Context, db *sqlb.DB, id int64, at time.Time) {
	t.Helper()
	if _, err := outbox.UpdateOutboxEvent().
		SetAvailableAt(at).
		Where(outbox.OutboxEventCols.ID.Eq(id)).
		Stmt().One(ctx, db); err != nil {
		t.Fatalf("forcing available_at for event %d: %v", id, err)
	}
}

// TestConcurrentWorkersEachClaimExactlyOnce adapts
// pgtest/census_test.go's TestClaimHandsEachRowToExactlyOneWorker onto this
// package's real schema and its own Claim/Complete, rather than hand-rolled
// query calls. Four workers race over twelve immediately-available events;
// every event is claimed by exactly one worker, and none are left behind.
//
// The proof does not need an artificial delay between the claiming SELECT
// and the status UPDATE (the pgtest test uses one to widen the window for the
// *broken* variant — no WithTx — to reliably double-claim). Here the lock is
// real by construction, so SkipLocked's guarantee holds regardless of how
// wide or narrow the race window happens to be; the delay would only be
// needed to make a missing lock fail reliably, and that variant is not what
// this test is for.
func TestConcurrentWorkersEachClaimExactlyOnce(t *testing.T) {
	ctx := context.Background()
	db := outboxDB(t)

	const workers = 4
	const events = 12
	for i := 0; i < events; i++ {
		seedEvent(t, ctx, db, fmt.Sprintf("event-%d", i))
	}

	var mu sync.Mutex
	claimedBy := map[int64][]int{}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		worker := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Enough passes that every worker competes for the tail of the
			// queue, which is where a double claim would show up.
			for i := 0; i < events; i++ {
				got, err := outbox.Claim(ctx, db, 1)
				if err != nil {
					t.Errorf("worker %d: claim: %v", worker, err)
					return
				}
				if len(got) == 0 {
					continue
				}

				mu.Lock()
				claimedBy[got[0].ID] = append(claimedBy[got[0].ID], worker)
				mu.Unlock()

				if err := outbox.Complete(ctx, db, got[0].ID); err != nil {
					t.Errorf("worker %d: complete %d: %v", worker, got[0].ID, err)
				}
			}
		}()
	}
	wg.Wait()

	if len(claimedBy) != events {
		t.Errorf("%d of %d events were claimed; SkipLocked should skip a locked row, not lose it", len(claimedBy), events)
	}
	for id, by := range claimedBy {
		if len(by) != 1 {
			t.Errorf("event %d was claimed %d times, by workers %v", id, len(by), by)
		}
	}

	done, err := sqlb.Query[outbox.OutboxEvent]().
		Where(outbox.OutboxEventCols.Status.Eq(outbox.OutboxEventStatusDone)).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting done events: %v", err)
	}
	if done != events {
		t.Errorf("%d events done, want all %d", done, events)
	}

	pending, err := sqlb.Query[outbox.OutboxEvent]().
		Where(outbox.OutboxEventCols.Status.Eq(outbox.OutboxEventStatusPending)).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting pending events: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d events left pending, want 0", pending)
	}
}

// TestFailBacksOffBeforeBecomingClaimableAgain settles the retry half of the
// lifecycle. A failed event with attempts below max_attempts goes back to
// pending, but not to now — its available_at is pushed into the future by
// worker.go's exponential backoff, so an immediate re-Claim must not see it.
// There is no clock to fast-forward in a test, so the assertion that it
// becomes claimable again is made by moving available_at into the past by
// hand, exactly as a real clock eventually would on its own.
func TestFailBacksOffBeforeBecomingClaimableAgain(t *testing.T) {
	ctx := context.Background()
	db := outboxDB(t)

	seeded := seedEvent(t, ctx, db, "retry-me")

	claimed, err := outbox.Claim(ctx, db, 1)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != seeded.ID {
		t.Fatalf("claim = %+v, want exactly the seeded event", claimed)
	}

	if err := outbox.Fail(ctx, db, seeded.ID); err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Not immediately claimable: Fail pushed available_at into the future.
	got, err := outbox.Claim(ctx, db, 10)
	if err != nil {
		t.Fatalf("claim right after fail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Claim returned %+v right after Fail; want nothing until the backoff elapses", got)
	}

	row, err := sqlb.Query[outbox.OutboxEvent]().
		Where(outbox.OutboxEventCols.ID.Eq(seeded.ID)).One(ctx, db)
	if err != nil {
		t.Fatalf("reading back the event: %v", err)
	}
	if row.Status != outbox.OutboxEventStatusPending {
		t.Errorf("status = %q, want pending (attempts %d is below max_attempts %d)", row.Status, row.Attempts, row.MaxAttempts)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", row.Attempts)
	}
	if !row.AvailableAt.After(time.Now()) {
		t.Errorf("available_at = %v, want pushed into the future by the backoff", row.AvailableAt)
	}

	// Advance the clock the only way a test can: move the row's own
	// available_at into the past.
	forceAvailableNow(t, ctx, db, seeded.ID, time.Now().Add(-time.Second))

	got, err = outbox.Claim(ctx, db, 10)
	if err != nil {
		t.Fatalf("claim after advancing available_at: %v", err)
	}
	if len(got) != 1 || got[0].ID != seeded.ID {
		t.Fatalf("claim after advancing available_at = %+v, want exactly the retried event", got)
	}
}

// TestRepeatedFailureDeadLettersAndStaysUnclaimable drives an event's
// attempts up to max_attempts through repeated Claim/Fail cycles (forcing
// available_at into the past between cycles, standing in for the real wait a
// production poller would do). At max_attempts, Fail dead-letters the event
// instead of rescheduling it — and once dead, the event is never returned by
// Claim again, even with available_at back in the past, which is the
// property that makes "dead" a stopping point rather than just another
// state Claim might still hand out.
func TestRepeatedFailureDeadLettersAndStaysUnclaimable(t *testing.T) {
	ctx := context.Background()
	db := outboxDB(t)

	seeded := seedEvent(t, ctx, db, "doomed")

	const maxAttempts = 5 // outboxschema's declared default for max_attempts
	for i := 0; i < maxAttempts; i++ {
		got, err := outbox.Claim(ctx, db, 10)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(got) != 1 || got[0].ID != seeded.ID {
			t.Fatalf("claim %d = %+v, want exactly the seeded event", i, got)
		}
		if err := outbox.Fail(ctx, db, seeded.ID); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
		// Only matters for the iterations before the last: once dead, this
		// has no effect on whether Claim can see the row again.
		forceAvailableNow(t, ctx, db, seeded.ID, time.Now().Add(-time.Second))
	}

	row, err := sqlb.Query[outbox.OutboxEvent]().
		Where(outbox.OutboxEventCols.ID.Eq(seeded.ID)).One(ctx, db)
	if err != nil {
		t.Fatalf("reading back the event: %v", err)
	}
	if row.Status != outbox.OutboxEventStatusDead {
		t.Fatalf("status = %q after %d failures, want dead", row.Status, row.Attempts)
	}
	if row.Attempts != maxAttempts {
		t.Errorf("attempts = %d, want %d", row.Attempts, maxAttempts)
	}

	got, err := outbox.Claim(ctx, db, 10)
	if err != nil {
		t.Fatalf("claim after dead-lettering: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Claim returned a dead-lettered event: %+v", got)
	}
}
