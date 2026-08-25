package pgtest

import (
	"context"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// A day is a question about a calendar, and a calendar belongs to a time zone.
//
// OnDay binds the date as text and casts it in Postgres, so the day it means is
// the session's — the same day `at::date` reports, and the same day the caller
// asking "what happened on the first" is thinking of. Binding a Go time.Time
// instead would carry that value's own zone into the comparison and answer a
// different question either side of midnight, which is the trap one layer down
// from the one #241 reported.
//
// The instant below is chosen to fall on different dates in the two zones: half
// past eleven at night in UTC is half past one the next morning in Berlin.
func TestOnDayFollowsTheSessionTimeZone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := metersDB(t)

	at := time.Date(2026, 9, 1, 23, 30, 0, 0, time.UTC)
	m := Meter{Tenant: "acme", Kind: "api_call", At: at, Count: 1}
	if _, err := sqlb.InsertRows(&m).One(ctx, db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		zone      string
		day       string
		want      int64
		otherDay  string
		wantOther int64
	}{
		{zone: "UTC", day: "2026-09-01", want: 1, otherDay: "2026-09-02", wantOther: 0},
		{zone: "Europe/Berlin", day: "2026-09-02", want: 1, otherDay: "2026-09-01", wantOther: 0},
	}

	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			// SET LOCAL, so the zone is scoped to this transaction and the
			// pooled connection goes back as it came.
			err := db.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
				if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE `+quoteLiteral(c.zone)); err != nil {
					return err
				}
				n, err := sqlb.Query[Meter]().Where(sqlb.F("at").OnDay(c.day)).Count(ctx, tx)
				if err != nil {
					return err
				}
				if n != c.want {
					t.Errorf("in %s, OnDay(%s) matched %d rows, want %d", c.zone, c.day, n, c.want)
				}

				other, err := sqlb.Query[Meter]().Where(sqlb.F("at").OnDay(c.otherDay)).Count(ctx, tx)
				if err != nil {
					return err
				}
				if other != c.wantOther {
					t.Errorf("in %s, OnDay(%s) matched %d rows, want %d", c.zone, c.otherDay, other, c.wantOther)
				}

				// The claim that makes the half-open range a rewrite rather
				// than a different question, under a zone where the two could
				// disagree if either read the date differently.
				cast, err := sqlb.Query[Meter]().
					Where(sqlb.RawPred(`"at"::date = ?::date`, c.day)).Count(ctx, tx)
				if err != nil {
					return err
				}
				if cast != c.want {
					t.Errorf("in %s, the ::date cast matched %d rows and OnDay wants %d", c.zone, cast, c.want)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("in %s: %v", c.zone, err)
			}
		})
	}
}

// quoteLiteral is enough for a time zone name written in this file. SET does not
// take a bind parameter.
func quoteLiteral(s string) string { return "'" + s + "'" }
