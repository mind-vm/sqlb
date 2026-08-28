package sqlb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// Reading a grouped result (#306).
//
// The reporter wanted "how many threads are open, per company, for a handful of
// companies" — one statement SQL answers in a pass — and concluded the builder
// could express GROUP BY and not read what it returned. They shipped an N+1
// loop and a paragraph explaining why, because the alternative on offer looked
// like dropping to raw SQL and leaving the confinement hooks behind.
//
// Collect had been the answer for a month. What made it invisible is that All
// *appeared to work*: a grouped query scanned into the model type came back the
// right length, with the grouped column set and the aggregate silently
// discarded, and a nil error. Nothing pointed at the count being gone.

// perOrg is the shape a grouped projection actually has: the column grouped by,
// and the aggregate the query exists for.
type perOrg struct {
	OrgID string `db:"org_id"`
	Open  int64  `db:"open"`
}

func TestAGroupedResultIsReadWithCollect(t *testing.T) {
	h := newHarness(t, []string{"org_id", "open"},
		[][]any{{"acme", int64(7)}, {"globex", int64(3)}})
	defer h.close()

	rows, err := sqlb.Collect[perOrg](context.Background(), h.db,
		sqlb.Query[User]().
			GroupBy(sqlb.F("org_id")).
			Select(sqlb.F("org_id"), sqlb.Count().As("open")))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(rows), rows)
	}
	// The aggregate is the point. A test asserting only the group keys would
	// pass against the old silent-discard behaviour.
	if rows[0].OrgID != "acme" || rows[0].Open != 7 {
		t.Errorf("first group = %+v, want {acme 7}", rows[0])
	}
	if rows[1].OrgID != "globex" || rows[1].Open != 3 {
		t.Errorf("second group = %+v, want {globex 3}", rows[1])
	}
}

// The same query read with All is refused rather than answered with rows whose
// numbers are missing. This is the failure the issue is really about: it did
// not error, so there was nothing to search for.
func TestAGroupedQueryReadWithAllIsRefused(t *testing.T) {
	h := newHarness(t, []string{"org_id", "open"},
		[][]any{{"acme", int64(7)}, {"globex", int64(3)}})
	defer h.close()

	_, err := sqlb.Query[User]().
		GroupBy(sqlb.F("org_id")).
		Select(sqlb.F("org_id"), sqlb.Count().As("open")).
		All(context.Background(), h.db)
	if err == nil {
		t.Fatal("All discarded a grouped query's aggregate and reported success")
	}
	// The refusal has to name the column that would have been dropped and the
	// call that reads it. Without the second half this is a dead end: the
	// reader already believes no such call exists, which is why they wrote the
	// loop.
	for _, want := range []string{"open", "Collect", "grouped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
}

// The other direction, and the reason the check is keyed on the projection
// rather than on GROUP BY alone: grouping by the primary key and selecting the
// model's own columns is legal Postgres and scans into the model exactly. A
// blanket refusal would break it.
func TestAGroupedQueryProjectingOnlyModelColumnsStillScans(t *testing.T) {
	h := newHarness(t, []string{"id", "email"},
		[][]any{{"u1", "ada@example.com"}})
	defer h.close()

	rows, err := sqlb.Query[User]().
		GroupBy(sqlb.F("id")).
		Select(sqlb.F("id"), sqlb.F("email")).
		All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("a grouped query the model can hold should still scan: %v", err)
	}
	if len(rows) != 1 || rows[0].Email != "ada@example.com" {
		t.Errorf("rows = %+v", rows)
	}
}

// And an ungrouped partial select is untouched. ?select=id,name is a
// projection, and leaving the rest zero is what it means — so the refusal must
// not reach it.
func TestAnUngroupedPartialSelectIsUnaffected(t *testing.T) {
	h := newHarness(t, []string{"id", "surprise"},
		[][]any{{"u1", "ignored"}})
	defer h.close()

	rows, err := sqlb.Query[User]().Select(sqlb.F("id")).All(context.Background(), h.db)
	if err != nil {
		t.Fatalf("an ungrouped select must keep discarding unmatched columns: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "u1" {
		t.Errorf("rows = %+v", rows)
	}
}
