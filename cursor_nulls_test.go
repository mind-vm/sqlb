package sqlb_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// dated declares a null placement on a nullable timestamp, the shape #88 is
// about. undated is the same table without the declaration, standing in for the
// deploy that came before it.
type dated struct {
	ID        string     `db:"id" sqlb:"pk"`
	Published *time.Time `db:"published_at" sqlb:"sort:nullslast"`
}

func (dated) TableName() string { return "posts" }

type undated struct {
	ID        string     `db:"id" sqlb:"pk"`
	Published *time.Time `db:"published_at" sqlb:"sort"`
}

func (undated) TableName() string { return "posts" }

// The declared placement has to reach the keyset predicate, not just the ORDER
// BY. A cursor that pages under NULLS LAST while the boundary comparison
// assumes the default would hand out overlapping or gapped pages, which is the
// one failure keyset paging exists to prevent.
func TestDeclaredPlacementReachesTheOrderByAndTheCursor(t *testing.T) {
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	c, err := sqlb.Query[dated]().
		OrderBy(sqlb.F("published_at").Desc().NullsLast()).
		Stable().
		CursorFor(dated{ID: "p1", Published: &at})
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}
	sql, _, err := sqlb.Query[dated]().
		OrderBy(sqlb.F("published_at").Desc().NullsLast()).
		Stable().
		After(c).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "published_at" DESC NULLS LAST`) {
		t.Errorf("the placement did not reach ORDER BY:\n%s", sql)
	}
	// Under DESC NULLS LAST the NULLs come after every real value, so a
	// boundary on a real value must not let them back in.
	if !strings.Contains(sql, "IS NOT NULL") && !strings.Contains(sql, "IS NULL") {
		t.Errorf("the keyset predicate ignores null placement entirely:\n%s", sql)
	}
}

// The cursor carries the *declared* placement, not the resolved one. That is
// what keeps cursors issued before #88 valid: a column with no declaration
// encodes nothing, so an old cursor and a new request agree.
func TestACursorIssuedBeforeTheFieldExistedStillDecodes(t *testing.T) {
	// A cursor in the pre-#88 wire format: column, direction, value, and no
	// placement field at all.
	old := base64.RawURLEncoding.EncodeToString([]byte(
		`{"k":[{"c":"published_at","d":true,"v":null},{"c":"id","d":true,"v":"p1"}]}`))

	_, _, err := sqlb.Query[undated]().
		OrderBy(sqlb.F("published_at").Desc()).
		Stable().
		After(sqlb.Cursor(old)).
		SQL()
	if err != nil {
		t.Fatalf("a cursor issued before the placement field existed was refused: %v", err)
	}
}

// And the refusal it does buy: a column that gains a declaration invalidates
// the cursors issued under the old ordering, rather than interpreting their
// boundary under a placement they were not built for.
func TestACursorLosesValidityWhenTheColumnGainsAPlacement(t *testing.T) {
	old := base64.RawURLEncoding.EncodeToString([]byte(
		`{"k":[{"c":"published_at","d":true,"v":null},{"c":"id","d":true,"v":"p1"}]}`))

	_, _, err := sqlb.Query[dated]().
		OrderBy(sqlb.F("published_at").Desc().NullsLast()).
		Stable().
		After(sqlb.Cursor(old)).
		SQL()
	if err == nil {
		t.Fatal("a cursor issued under the default placement was accepted under NULLS LAST")
	}
	if !errors.Is(err, sqlb.ErrBadCursor) {
		t.Errorf("a stale cursor is the client's to drop, so it should read as a bad cursor: %v", err)
	}
	// The message has to name the difference, or it reads as two identical
	// orderings and the caller has nothing to act on.
	if !strings.Contains(err.Error(), "nulls last") {
		t.Errorf("the mismatch does not say what differs:\n%v", err)
	}
}

// The encoded form is checked directly because it is a wire format: a cursor
// outlives the process that issued it, so the field being omitted when unset is
// the compatibility property, not an incidental encoding detail.
func TestAnUndeclaredPlacementIsOmittedFromTheCursor(t *testing.T) {
	c, err := sqlb.Query[undated]().
		OrderBy(sqlb.F("published_at").Desc()).
		Stable().
		CursorFor(undated{ID: "p1"})
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}
	buf, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(string(c), "="))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(buf, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.Contains(string(buf), `"n"`) {
		t.Errorf("an undeclared placement was written into the cursor: %s", buf)
	}
}
