// Package outbox is example/outbox's worker: the claim, complete and
// dead-letter lifecycle around outboxschema's outbox_events table.
//
// Read ../README.md before treating anything here as more than one worked
// answer to the shape ADR-0012 leaves open.
package outbox

import (
	"context"
	"math"
	"time"

	"github.com/mind-vm/sqlb"
)

// maxBackoff caps Fail's exponential retry delay. Without a cap, 2^attempts
// seconds outgrows a sane retry window long before max_attempts is reached
// for any table that raises its own threshold above the default — and
// math.Pow(2, big) overflowing into an unrepresentable time.Duration is a
// worse failure than a delay that stopped growing.
const maxBackoff = 5 * time.Minute

// Claim selects up to n pending, due events and marks them processing, all
// inside one transaction, then hands them to the caller to work on.
//
// The transaction boundary is load-bearing, not incidental.
// `FOR UPDATE SKIP LOCKED` holds its lock only until commit — under
// autocommit the row unlocks the instant the SELECT returns, and a second
// worker whose own SELECT starts before this one's UPDATE would still see
// the row as pending and claim it too. pgtest/census_test.go's
// TestClaimHandsEachRowToExactlyOneWorker proves the lock holds when it is
// held this way, against a bare claim-only table; this package's own
// TestConcurrentWorkersEachClaimExactlyOnce repeats the same proof on this
// schema, with a retry policy and a dead-letter rule sitting around the
// claim.
//
// The `available_at <= now` boundary compares against an instant computed in
// Go and bound as an ordinary parameter, not against Postgres's own now() at
// execution time. pgtest/census_test.go's
// TestRelativeTimeWindowNeedsRawOrAGoComputedInstant is why: the builder has
// no interval literal, so a relative-time window has to pick a clock, and
// Fail below computes its backoff with time.Now() too — the two call sites
// have to agree on which clock decides, or a claim boundary and a backoff
// computation could disagree by however far the two evaluations' clocks
// drift.
func Claim(ctx context.Context, db *sqlb.DB, n int) ([]OutboxEvent, error) {
	var claimed []OutboxEvent
	err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		now := time.Now()
		pending, err := sqlb.Query[OutboxEvent]().
			Where(
				OutboxEventCols.Status.Eq(OutboxEventStatusPending),
				OutboxEventCols.AvailableAt.Lte(now),
			).
			OrderBy(sqlb.F("id").Asc()).
			Limit(n).
			ForUpdate().SkipLocked().
			All(ctx, tx)
		if err != nil || len(pending) == 0 {
			return err
		}

		ids := make([]int64, len(pending))
		for i, e := range pending {
			ids[i] = e.ID
		}
		if _, err := UpdateOutboxEvent().
			SetStatus(OutboxEventStatusProcessing).
			Where(OutboxEventCols.ID.OneOf(ids...)).
			Stmt().Exec(ctx, tx); err != nil {
			return err
		}

		for i := range pending {
			pending[i].Status = OutboxEventStatusProcessing
		}
		claimed = pending
		return nil
	})
	return claimed, err
}

// Complete marks a claimed event done. It does not check the event's current
// status — a worker calling Complete holds the only claim on the row, by
// construction of Claim, so there is nothing to race against.
func Complete(ctx context.Context, db *sqlb.DB, id int64) error {
	_, err := UpdateOutboxEvent().
		SetStatus(OutboxEventStatusDone).
		Where(OutboxEventCols.ID.Eq(id)).
		Stmt().One(ctx, db)
	return err
}

// Fail records a failed attempt at a claimed event. Below max_attempts it
// goes back to pending with available_at pushed out by an exponential
// backoff (2^attempts seconds, capped at maxBackoff — see ../README.md's
// "one opinion" section); at or past max_attempts it is dead-lettered
// instead, and Claim will never return it again regardless of available_at.
//
// The row is read with ForUpdate inside the same transaction that writes it
// back, so a Fail racing a concurrent Fail on the same id (which should not
// happen under Claim's exclusivity, but costs nothing to close off) computes
// its backoff from a value nobody else is also about to overwrite.
func Fail(ctx context.Context, db *sqlb.DB, id int64) error {
	return db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		event, err := sqlb.Query[OutboxEvent]().
			Where(OutboxEventCols.ID.Eq(id)).
			ForUpdate().
			One(ctx, tx)
		if err != nil {
			return err
		}

		attempts := event.Attempts + 1
		if attempts >= event.MaxAttempts {
			_, err := UpdateOutboxEvent().
				SetAttempts(attempts).
				SetStatus(OutboxEventStatusDead).
				Where(OutboxEventCols.ID.Eq(id)).
				Stmt().One(ctx, tx)
			return err
		}

		// Same clock as Claim's boundary, and for the same reason: the
		// instant this row becomes claimable again has to be computed
		// against the clock Claim compares it to.
		backoff := time.Duration(math.Pow(2, float64(attempts))) * time.Second
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		_, err = UpdateOutboxEvent().
			SetAttempts(attempts).
			SetStatus(OutboxEventStatusPending).
			SetAvailableAt(time.Now().Add(backoff)).
			Where(OutboxEventCols.ID.Eq(id)).
			Stmt().One(ctx, tx)
		return err
	})
}
