package sqlb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// TestWithRendersANamedCTE is the SELECT-side counterpart to
// TestUpdateFromRendersAQueueClaimCTE (mutate_from_test.go): With gives
// Builder the same single-named-CTE capability Update.From already has,
// reusing exactly the same compileSub/bind-numbering machinery — a bind on
// each side of the CTE boundary, so a bind landing at the wrong position
// would show up as a wrong value in the wrong $n rather than as a passing
// test that never exercised the seam.
func TestWithRendersANamedCTE(t *testing.T) {
	active := sqlb.Query[subPost]().
		Select(sqlb.F("id"), sqlb.F("author_id")).
		Where(sqlb.F("title").Eq("go"))

	q := sqlb.Query[User]().
		Select(sqlb.F("id")).
		With("active", active).
		Join("active", "a", sqlb.F("id").EqField(sqlb.F("author_id").Qualify("a"))).
		Where(sqlb.F("org_id").Eq("org1"))

	got, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	want := `WITH "active" AS (SELECT "sub_posts"."id", "sub_posts"."author_id" FROM "sub_posts" WHERE "sub_posts"."title" = $1) ` +
		`SELECT "users"."id" FROM "users" JOIN "active" AS "a" ON "users"."id" = "a"."author_id" ` +
		`WHERE "users"."org_id" = $2`
	if got != want {
		t.Errorf("SQL mismatch:\n got:  %s\n want: %s", got, want)
	}

	wantArgs := []any{"go", "org1"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range args {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d] = %v, want %v — a bind landed at the wrong position", i, args[i], wantArgs[i])
		}
	}
}

func TestWithRequiresAName(t *testing.T) {
	sub := sqlb.Query[subPost]().Select(sqlb.F("id"))
	_, _, err := sqlb.Query[User]().With("", sub).SQL()
	if err == nil {
		t.Fatal(`With("", ...) should fail the statement`)
	}
}

func TestWithRequiresAQuery(t *testing.T) {
	_, _, err := sqlb.Query[User]().With("active", nil).SQL()
	if err == nil {
		t.Fatal("With(name, nil) should fail the statement")
	}
}

// TestWithRefusesAnUnresolvedHookedQuery and
// TestWithAcceptsAResolvedHookedQuery replay
// TestUpdateFromRefusesAnUnresolvedHookedQuery/
// TestUpdateFromAcceptsAResolvedHookedQuery (mutate_from_test.go) for the
// read side: With's query is compiled straight into the statement rather
// than run, so a model confined by a BeforeQuery hook must not reach the
// wire with that scope silently absent.
func TestWithRefusesAnUnresolvedHookedQuery(t *testing.T) {
	h := subHarness(t)
	reg := sqlb.NewRegistry()
	sqlb.On[subPost](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[subPost]) error {
		q.Where(sqlb.F("org_id").Eq("org1"))
		return nil
	})
	db := h.handle(reg)
	ctx := context.Background()

	sub := sqlb.Query[subPost]().Select(sqlb.F("author_id"))

	_, err := sqlb.Query[User]().With("active", sub).Resolved(ctx, db)
	if err == nil {
		t.Fatal("With nested a confined model without its scope")
	}
	if !strings.Contains(err.Error(), "subPost") {
		t.Errorf("error does not name the model: %v", err)
	}
}

func TestWithAcceptsAResolvedHookedQuery(t *testing.T) {
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

	if _, err := sqlb.Query[User]().With("active", sub).Resolved(ctx, db); err != nil {
		t.Fatalf("a resolved With should not be refused: %v", err)
	}
}
