// Package rooms_test settles docs/special-cases.md's "rooms" case: what
// sqlb does with a room-booking invariant that is neither a unique key nor a
// CHECK — no two confirmed bookings on one room may overlap in time.
//
// The census that proposed this example, written 2026-07-30, says EXCLUDE
// constraints are "not expressible" in sqlb. That is no longer true.
// schema.Exclusion and TableDef.AddExclude are a real, shipped feature
// (issue #121) — see roomsschema/schema.go's doc comment. This suite is the
// worked demonstration that it holds under real contention, not the
// discovery of a gap.
package rooms_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/example/rooms"
	"github.com/jryannel/sqlb/migrate"
	"github.com/jryannel/sqlb/schema"
	"github.com/jryannel/sqlb/sqlbtest"

	// Imported for its side effect: declaring Room and Booking registers them
	// in schema.DefaultRegistry(), which migrateSchema below diffs against an
	// empty one to get the DDL — the same baseline-migration path
	// example/tasks/cmd/migrate/main.go uses.
	_ "github.com/jryannel/sqlb/example/rooms/roomsschema"
)

// roomsDB migrates a fresh database from the declared schema and returns a
// *sqlb.DB over it.
//
// The database, its name, its cleanup and the DSN it came from are
// sqlbtest.Fresh's — this file used to sit beside a main_test.go carrying the
// eighty lines that did it by hand, which is now deleted.
func roomsDB(t *testing.T) *sqlb.DB {
	t.Helper()
	pool := sqlbtest.Fresh(t,
		sqlbtest.DSN(t, "SQLB_TEST_POSTGRES", "run `mise run pg-up` first"),
		// Eight, because the contention test below races eight goroutines
		// through this one pool: a pool smaller than the racers waits on itself
		// and demonstrates nothing about the constraint.
		sqlbtest.MaxConns(8),
		// The extension the exclusion needs comes with it: migrate.Diff detects
		// that pairing a scalar = with a range && needs btree_gist and emits the
		// CREATE EXTENSION itself.
		sqlbtest.Declared(schema.DefaultRegistry(), migrate.MinPostgres(18)),
	)
	return sqlb.New(pool)
}

func seedRoom(t *testing.T, ctx context.Context, db *sqlb.DB) rooms.Room {
	t.Helper()
	r := rooms.Room{Name: "Ada"}
	got, err := sqlb.InsertRows(&r).One(ctx, db)
	if err != nil {
		t.Fatalf("seeding a room: %v", err)
	}
	return got
}

// TestDoubleBookingUnderContentionIsRefusedExactlyOnce is the claim the
// census asks an example to make: that the constraint holds under real
// contention, not merely that it compiles. Eight goroutines race to confirm
// an overlapping booking on the same room; exactly one must win, and the
// other seven must fail with the exclusion's own name — not a generic
// "duplicate key", and not a deadlock.
func TestDoubleBookingUnderContentionIsRefusedExactlyOnce(t *testing.T) {
	ctx := context.Background()
	db := roomsDB(t)
	room := seedRoom(t, ctx, db)

	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	const racers = 8
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b := rooms.Booking{
				RoomID:   room.ID,
				StartsAt: start,
				EndsAt:   end,
				Status:   rooms.BookingStatusConfirmed,
			}
			_, err := sqlb.InsertRows(&b).One(ctx, db)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var wins, losses int
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		var ce *sqlb.ConstraintError
		if !errors.As(err, &ce) {
			t.Fatalf("a racer failed with a non-constraint error: %v", err)
		}
		if ce.Kind != sqlb.ConstraintExclusion {
			t.Errorf("racer failed with kind %q, want %q", ce.Kind, sqlb.ConstraintExclusion)
		}
		if ce.Constraint != "bookings_no_double_booking" {
			t.Errorf("racer failed naming constraint %q, want \"bookings_no_double_booking\"", ce.Constraint)
		}
		losses++
	}
	if wins != 1 {
		t.Errorf("%d of %d racers won, want exactly 1", wins, racers)
	}
	if losses != racers-1 {
		t.Errorf("%d of %d racers lost, want exactly %d", losses, racers, racers-1)
	}

	n, err := sqlb.Query[rooms.Booking]().Where(sqlb.F("room_id").Eq(room.ID)).Count(ctx, db)
	if err != nil {
		t.Fatalf("counting bookings: %v", err)
	}
	if n != 1 {
		t.Errorf("%d bookings landed, want exactly 1 — a double claim would still be visible as two rows", n)
	}
}

// TestTheWhereClauseNarrowsWhatCollides is the other half of AddExclude's
// Where: the exclusion applies only to confirmed bookings, so a pending
// booking overlapping a confirmed one is not a double booking yet, and two
// confirmed bookings that do not overlap in time are not either.
func TestTheWhereClauseNarrowsWhatCollides(t *testing.T) {
	ctx := context.Background()
	db := roomsDB(t)
	room := seedRoom(t, ctx, db)

	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	confirmed := rooms.Booking{RoomID: room.ID, StartsAt: start, EndsAt: end, Status: rooms.BookingStatusConfirmed}
	if _, err := sqlb.InsertRows(&confirmed).One(ctx, db); err != nil {
		t.Fatalf("seeding the confirmed booking: %v", err)
	}

	// Overlapping, but pending: the exclusion's WHERE does not cover it.
	pending := rooms.Booking{RoomID: room.ID, StartsAt: start, EndsAt: end, Status: rooms.BookingStatusPending}
	if _, err := sqlb.InsertRows(&pending).One(ctx, db); err != nil {
		t.Errorf("an overlapping pending booking was refused, want it accepted: %v", err)
	}

	// Confirmed, but disjoint in time: no overlap, so no collision either.
	later := rooms.Booking{RoomID: room.ID, StartsAt: end, EndsAt: end.Add(time.Hour), Status: rooms.BookingStatusConfirmed}
	if _, err := sqlb.InsertRows(&later).One(ctx, db); err != nil {
		t.Errorf("a non-overlapping confirmed booking was refused, want it accepted: %v", err)
	}
}

// TestADayFilterAgainstTimestamptzAnswersTheDay is what
// pgtest/census_test.go's test of the same shape became. It was written when
// the answer was wrong: a "what's on today" view compiled, ran, and answered
// zero rows for a day with bookings on it, because a bare date compares against
// midnight and a booking is almost never at midnight (#241).
//
// It now demonstrates the fix on the schema that makes the failure concrete —
// a booking calendar, whose whole purpose is answering "what is on this day".
func TestADayFilterAgainstTimestamptzAnswersTheDay(t *testing.T) {
	ctx := context.Background()
	db := roomsDB(t)
	room := seedRoom(t, ctx, db)

	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	b := rooms.Booking{RoomID: room.ID, StartsAt: start, EndsAt: start.Add(time.Hour), Status: rooms.BookingStatusConfirmed}
	if _, err := sqlb.InsertRows(&b).One(ctx, db); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// The day before, to prove the range has an upper bound as well as a lower
	// one: a predicate that only said ">= the date" would match this too.
	next := rooms.Booking{RoomID: room.ID, StartsAt: start.Add(24 * time.Hour),
		EndsAt: start.Add(25 * time.Hour), Status: rooms.BookingStatusConfirmed}
	if _, err := sqlb.InsertRows(&next).One(ctx, db); err != nil {
		t.Fatalf("seeding the next day: %v", err)
	}

	day := "2026-09-01"

	n, err := sqlb.Query[rooms.Booking]().Where(sqlb.F("starts_at").OnDay(day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("day filter: %v", err)
	}
	if n != 1 {
		t.Errorf("OnDay matched %d rows, want the one booking on that day", n)
	}

	// The same rows the cast spelling selects, which is the claim OnDay makes
	// about being a rewrite rather than a different question.
	cast, err := sqlb.Query[rooms.Booking]().
		Where(sqlb.RawPred(`"starts_at"::date = ?::date`, day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("cast day filter: %v", err)
	}
	if cast != n {
		t.Errorf("OnDay matched %d rows and the ::date cast matched %d; they are meant to be the same set", n, cast)
	}

	// And the spelling a reader used to reach for is now refused by the filter
	// grammar rather than answering nothing — see filter/day_test.go, which
	// holds that half. The builder still compiles it, because Eq on a timestamp
	// column is a legitimate exact comparison; what changed is that a request
	// cannot ask for it by accident.
	bare, err := sqlb.Query[rooms.Booking]().Where(sqlb.F("starts_at").Eq(day)).Count(ctx, db)
	if err != nil {
		t.Fatalf("bare day filter: %v", err)
	}
	if bare != 0 {
		t.Errorf("an equality against midnight matched %d rows, want 0 — the point of OnDay", bare)
	}
}
