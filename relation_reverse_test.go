package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

type oneToOneUser struct {
	ID      string           `db:"id" sqlb:"pk"`
	Profile *oneToOneProfile `db:"-" json:"profile,omitempty" sqlb:"expands=user_id,reverse"`
}

type oneToOneProfile struct {
	ID     string `db:"id" sqlb:"pk"`
	UserID string `db:"user_id"`
}

// The guard-proven-both-ways companion coverage lives in the broader
// regression suite, not beside this test: TestInversesReportsCollectionForNonUniqueFK
// (schema), TestNonUniqueInverseStillEmitsACollection (Go codegen),
// TestTSNonUniqueInverseStillUsesCollection (TypeScript codegen) and
// TestDartNonUniqueInverseStillUsesCollectionGetter (Dart codegen) all confirm
// a plain forward relation and a capped collection keep their existing shape,
// so this new branch is not the only path exercised.
func TestReverseTagJoinsOnTheTargetsForeignKey(t *testing.T) {
	q := sqlb.Query[oneToOneUser]().Expand("profile")
	got, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL() error: %v", err)
	}
	if !strings.Contains(got, `LEFT JOIN "one_to_one_profiles" AS "__ex_profile"`) {
		t.Errorf("missing the expected join:\n%s", got)
	}
	if !strings.Contains(got, `"__ex_profile"."user_id" = "one_to_one_users"."id"`) {
		t.Errorf("join condition should be target.FK = base.PK, got:\n%s", got)
	}
	if strings.Contains(got, "has_more") {
		t.Errorf("a one-to-one reverse relation must not use the capped-collection envelope:\n%s", got)
	}
}

// noPKBase has a reverse relation but no declared primary key, exercising
// the join-direction branch's dependency on the base model's PK — the
// sibling of compileCollection's own base-PK guard (~line 494).
type noPKBase struct {
	Name    string       `db:"name"`
	Profile *noPKProfile `db:"-" json:"profile,omitempty" sqlb:"expands=base_id,reverse"`
}

type noPKProfile struct {
	ID     string `db:"id" sqlb:"pk"`
	BaseID string `db:"base_id"`
}

func (noPKBase) TableName() string    { return "no_pk_bases" }
func (noPKProfile) TableName() string { return "no_pk_profiles" }

func TestReverseExpandOnABaseWithNoPrimaryKeyErrorsInsteadOfPanicking(t *testing.T) {
	q := sqlb.Query[noPKBase]().Expand("profile")
	_, _, err := q.SQL()
	if err == nil {
		t.Fatal("expected an error, got nil (and no panic, so the guard is missing)")
	}
	if !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected an actionable no-primary-key error, got: %v", err)
	}
}
