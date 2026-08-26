package sqlb_test

import (
	"testing"

	"github.com/mind-vm/sqlb"
)

// TestRightFullCrossJoinRenderTheirKeyword pins the compiled SQL for the
// three join kinds LeftJoin didn't cover — RIGHT/FULL JOIN render exactly
// like LEFT JOIN but for the keyword, and CROSS JOIN is the one kind with no
// ON clause at all, which is the part worth pinning: join() refuses a zero
// predicate for every other kind precisely because "no ON" usually means "I
// forgot it," so CrossJoin has to be the one place that isn't refused.
func TestRightFullCrossJoinRenderTheirKeyword(t *testing.T) {
	tests := []struct {
		name string
		q    *sqlb.Builder[User]
		want string
	}{
		{
			"right",
			sqlb.Query[User]().Select(sqlb.F("id")).
				RightJoin("orgs", "o", sqlb.F("org_id").EqField(sqlb.F("id").Qualify("o"))),
			`SELECT "users"."id" FROM "users" RIGHT JOIN "orgs" AS "o" ON "users"."org_id" = "o"."id"`,
		},
		{
			"full",
			sqlb.Query[User]().Select(sqlb.F("id")).
				FullJoin("orgs", "o", sqlb.F("org_id").EqField(sqlb.F("id").Qualify("o"))),
			`SELECT "users"."id" FROM "users" FULL JOIN "orgs" AS "o" ON "users"."org_id" = "o"."id"`,
		},
		{
			"cross",
			sqlb.Query[User]().Select(sqlb.F("id")).CrossJoin("orgs", "o"),
			`SELECT "users"."id" FROM "users" CROSS JOIN "orgs" AS "o"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := tt.q.SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if got != tt.want {
				t.Errorf("SQL mismatch:\n got:  %s\n want: %s", got, tt.want)
			}
		})
	}
}

func TestCrossJoinNeedsNoOnCondition(t *testing.T) {
	_, _, err := sqlb.Query[User]().CrossJoin("orgs", "o").SQL()
	if err != nil {
		t.Fatalf("CrossJoin should not require an ON condition: %v", err)
	}
}
