package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

type NearChunk struct {
	ID        string      `db:"id" sqlb:"pk"`
	Body      string      `db:"body"`
	Embedding sqlb.Vector `db:"embedding" sqlb:"hidden"`
}

func (NearChunk) TableName() string { return "chunks" }

// The three expressions a similarity search needs come from one handle, so the
// distance is written once and cannot disagree with itself.
func TestNearYieldsProjectionPredicateAndOrder(t *testing.T) {
	near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1, 2, 3})
	sql, _, err := sqlb.Query[NearChunk]().
		Select(sqlb.F("id"), near.Similarity()).
		Where(near.AtLeast(0.75)).
		OrderBy(near.Nearest()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		// Similarity, not distance: larger is closer.
		`(1) - ("embedding" <=> $1::vector) AS "similarity"`,
		// The threshold compares the same score.
		`((1) - ("embedding" <=> $1::vector)) >= $2`,
		// The ordering is by distance, which is the shape an ANN index serves —
		// so adding one later changes the plan and not the statement.
		`ORDER BY "embedding" <=> $1::vector ASC`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL is missing %q:\n%s", want, sql)
		}
	}
}

// The vector is one bind parameter however many times it appears.
//
// Not a micro-optimisation: an embedding is about twenty kilobytes, and a
// search names it in the projection, the threshold and the ordering. Binding it
// per mention would treble the payload of every search for no reason a caller
// could see.
func TestNearBindsTheVectorOnce(t *testing.T) {
	near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1, 2, 3})
	q := sqlb.Query[NearChunk]().
		Select(sqlb.F("id"), near.Similarity()).
		Where(sqlb.F("body").Eq("x"), near.AtLeast(0.5)).
		OrderBy(near.Nearest())

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 3 {
		t.Fatalf("bound %d parameters, want 3 (the vector, the filter, the threshold): %#v", len(args), args)
	}
	if v, ok := args[0].(sqlb.Vector); !ok || len(v) != 3 {
		t.Errorf("the first parameter is %#v, want the vector itself", args[0])
	}
	if n := strings.Count(sql, "$1"); n != 3 {
		t.Errorf("the vector's placeholder appears %d times, want 3 mentions of one parameter:\n%s", n, sql)
	}

	// Compiling twice must produce the same statement. The placeholder reuse is
	// per-compilation state, and state that survived one would renumber the
	// second — which is the bug this assertion exists for rather than a
	// hypothetical.
	sql2, args2, err := q.SQL()
	if err != nil {
		t.Fatalf("second SQL: %v", err)
	}
	if sql2 != sql || len(args2) != len(args) {
		t.Errorf("recompiling changed the statement:\n%s\n%s", sql, sql2)
	}
}

// Two separate handles over equal vectors are two parameters. Sharing is by
// identity, because deduplicating by value would be a much larger promise —
// every equal string in a statement collapsing into one placeholder — and
// nothing here needs it.
func TestTwoNearHandlesAreTwoParameters(t *testing.T) {
	a := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1, 2, 3})
	b := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1, 2, 3})
	_, args, err := sqlb.Query[NearChunk]().
		Select(sqlb.F("id"), a.Similarity(), b.Distance()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 2 {
		t.Errorf("bound %d parameters, want 2: %#v", len(args), args)
	}
}

// Distance is offered beside Similarity for a caller re-ranking against another
// system's numbers, where the comparable quantity is the one Postgres computed.
func TestNearDistanceSelectsTheRawNumber(t *testing.T) {
	near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1})
	sql, _, err := sqlb.Query[NearChunk]().Select(near.Distance()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `"embedding" <=> $1::vector AS "distance"`) {
		t.Errorf("Distance did not select the raw distance:\n%s", sql)
	}
	if strings.Contains(sql, "(1) -") {
		t.Errorf("Distance selected the similarity instead:\n%s", sql)
	}
}
