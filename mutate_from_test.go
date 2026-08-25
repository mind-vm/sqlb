package sqlb_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// QJob is the worked example issue #174 opened with: a queue-style outbox
// row, claimed by selecting a batch with FOR UPDATE SKIP LOCKED and marking
// it in the same statement via a CTE, rather than two statements inside a
// transaction whose atomicity a future edit could quietly break.
type QJob struct {
	ID            int64      `db:"id" sqlb:"pk,default"`
	Topic         string     `db:"topic" sqlb:"filter"`
	Payload       string     `db:"payload"`
	CorrelationID string     `db:"correlation_id" sqlb:"default"`
	Attempts      int64      `db:"attempts" sqlb:"default"`
	PublishedAt   *time.Time `db:"published_at" sqlb:"filter"`
	AvailableAt   time.Time  `db:"available_at" sqlb:"sort,default"`
}

func (QJob) TableName() string { return "qjobs" }

// TestUpdateFromRendersAQueueClaimCTE is the builder-level half of #174's
// test plan: the compiled SQL text and bind order for the worked example,
// with a bind on each side of the CTE boundary so that a bind landing at the
// wrong position — the risk the issue calls out by name — would show up as a
// wrong value in the wrong $n rather than as a passing test that never
// exercised the seam.
func TestUpdateFromRendersAQueueClaimCTE(t *testing.T) {
	claimed := sqlb.Query[QJob]().
		Select(sqlb.F("id")).
		Where(sqlb.F("topic").Eq("email")).
		OrderBy(sqlb.F("available_at").Asc()).
		Limit(5).
		ForUpdate().SkipLocked()

	upd := sqlb.UpdateRows[QJob]().
		From("claimed", claimed).
		Set("payload", "processing").
		SetExpr("attempts", sqlb.Add(sqlb.F("attempts"), sqlb.Val(1))).
		Where(sqlb.F("id").EqField(sqlb.F("id").Qualify("claimed")))

	sql, args, err := upd.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	want := `WITH "claimed" AS (SELECT "qjobs"."id" FROM "qjobs" WHERE "qjobs"."topic" = $1 ORDER BY "qjobs"."available_at" ASC LIMIT 5 FOR UPDATE SKIP LOCKED) ` +
		`UPDATE "qjobs" SET "payload" = $2, "attempts" = "qjobs"."attempts" + $3 FROM "claimed" ` +
		`WHERE "qjobs"."id" = "claimed"."id" ` +
		`RETURNING "qjobs"."id", "qjobs"."topic", "qjobs"."payload", "qjobs"."correlation_id", "qjobs"."attempts", "qjobs"."published_at", "qjobs"."available_at"`
	if sql != want {
		t.Errorf("SQL mismatch:\n got:  %s\n want: %s", sql, want)
	}

	wantArgs := []any{"email", "processing", 1}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range args {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d] = %v (%T), want %v (%T) — a bind landed at the wrong position",
				i, args[i], args[i], wantArgs[i], wantArgs[i])
		}
	}
}

// TestUpdateFromRequiresAName and TestUpdateFromRequiresAQuery are the
// argument-checking half: From fails the statement immediately, the same as
// every other builder setter, rather than compiling something nonsensical.
func TestUpdateFromRequiresAName(t *testing.T) {
	q := sqlb.Query[QJob]().Select(sqlb.F("id"))
	_, _, err := sqlb.UpdateRows[QJob]().
		From("", q).
		Set("payload", "x").
		Where(sqlb.F("id").Eq(int64(1))).
		SQL()
	if err == nil {
		t.Fatal("From(\"\", ...) should fail the statement")
	}
}

func TestUpdateFromRequiresAQuery(t *testing.T) {
	_, _, err := sqlb.UpdateRows[QJob]().
		From("claimed", nil).
		Set("payload", "x").
		Where(sqlb.F("id").Eq(int64(1))).
		SQL()
	if err == nil {
		t.Fatal("From(name, nil) should fail the statement")
	}
}

// From compiles its query straight into the surrounding statement rather than
// running it — the same reason a nested [sqlb.Subquery] does, and subject to
// the same refusal: a model confined by a BeforeQuery hook must not reach the
// wire with that scope silently absent. These two are
// TestAWriteRefusesAnUnresolvedNestedQuery and
// TestAResolvedNestedQueryCarriesItsScope, replayed for From instead of
// InQuery, over subPost and subHarness from subquery_test.go.
func TestUpdateFromRefusesAnUnresolvedHookedQuery(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)
	ctx := context.Background()

	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))

	_, err := sqlb.UpdateRows[User]().
		From("claimed", sub).
		Set("name", "x").
		Where(sqlb.F("id").Eq("u1")).
		Exec(ctx, db)
	if err == nil {
		t.Fatal("From nested a confined model without its scope")
	}
	if !strings.Contains(err.Error(), "subPost") {
		t.Errorf("error does not name the model: %v", err)
	}
}

func TestUpdateFromAcceptsAResolvedHookedQuery(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)
	ctx := context.Background()

	sub, err := sqlb.Query[subPost]().Select(sqlb.F("author_id")).Resolved(ctx, db)
	if err != nil {
		t.Fatalf("resolving the CTE source: %v", err)
	}

	if _, err := sqlb.UpdateRows[User]().
		From("claimed", sub).
		Set("name", "x").
		Where(sqlb.F("id").Eq("u1")).
		Exec(ctx, db); err != nil {
		t.Fatalf("a resolved From should not be refused: %v", err)
	}
}
