package sqlb_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// ExpandOnly narrows an expanded row to the columns a caller is willing to
// carry. The whole property is that it narrows: it takes keys off a row and can
// put none on, so every refusal below is a name that would have widened the row
// or silently done nothing.

func TestExpandOnlyNarrowsAForwardExpansion(t *testing.T) {
	got, _, err := sqlb.Query[expTask]().ExpandOnly("list", "name").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `json_build_object('name', "__ex_list"."name")`) {
		t.Errorf("the expanded row was not narrowed to name: %s", got)
	}
	// The join is unchanged, and so is the NULL test that tells an absent
	// related row from a row of nulls — it reads the joined column rather than
	// the object, which is why leaving the key out is safe.
	for _, want := range []string{
		`LEFT JOIN "lists" AS "__ex_list" ON "__ex_list"."id" = "tasks"."list_id"`,
		`CASE WHEN "__ex_list"."id" IS NULL THEN NULL`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("narrowing changed more than the row object, missing %s: %s", want, got)
		}
	}
}

// The direction that proves it is narrowing rather than reordering: the columns
// left out are gone.
func TestTheColumnsNotNamedAreAbsent(t *testing.T) {
	got, _, err := sqlb.Query[expTask]().ExpandOnly("list", "name").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(got, `'id', "__ex_list"."id"`) {
		t.Errorf("the expanded row still carries id, which was not asked for: %s", got)
	}
	// And Expand on its own still carries the whole row, or the test above would
	// pass against a feature that had broken expansion generally.
	whole, _, err := sqlb.Query[expTask]().Expand("list").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(whole, `'id', "__ex_list"."id"`) {
		t.Errorf("an unnarrowed expansion lost a column: %s", whole)
	}
}

func TestExpandOnlyNarrowsACollection(t *testing.T) {
	got, _, err := sqlb.Query[expList]().ExpandOnly("tasks", "title").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `json_build_object('title', "__ex_tasks"."title")`) {
		t.Errorf("the collection's rows were not narrowed to title: %s", got)
	}
	// The cap and the ordering are the schema's and are untouched: they are what
	// stop a response's size being a function of data nobody bounded.
	for _, want := range []string{`ORDER BY "__ex_tasks"."id"`, `LIMIT`} {
		if !strings.Contains(got, want) {
			t.Errorf("narrowing disturbed the collection's %s: %s", want, got)
		}
	}
}

func TestExpandOnlyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func() *sqlb.Builder[expTask]
		wantErr string
	}{
		{
			name:    "a column the target does not have",
			build:   func() *sqlb.Builder[expTask] { return sqlb.Query[expTask]().ExpandOnly("list", "titel") },
			wantErr: "not a column of lists",
		},
		{
			// Skipping it quietly would read as "this expansion carries what I
			// asked for" right up until someone used the key that is not there.
			name:    "a hidden column",
			build:   func() *sqlb.Builder[expTask] { return sqlb.Query[expTask]().ExpandOnly("list", "secret") },
			wantErr: "never serves",
		},
		{
			name:    "no columns at all",
			build:   func() *sqlb.Builder[expTask] { return sqlb.Query[expTask]().ExpandOnly("list") },
			wantErr: "names no columns",
		},
		{
			name:    "a relation that does not exist",
			build:   func() *sqlb.Builder[expTask] { return sqlb.Query[expTask]().ExpandOnly("lst", "name") },
			wantErr: "no such relation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.build().SQL()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error does not say %q: %v", tc.wantErr, err)
			}
		})
	}
}

// Naming a relation twice replaces the selection rather than accumulating it,
// or a narrowing would widen back to the full row by accident.
func TestNarrowingTwiceReplacesRatherThanAccumulates(t *testing.T) {
	got, _, err := sqlb.Query[expTask]().
		ExpandOnly("list", "name").
		ExpandOnly("list", "id").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(got, `json_build_object('id', "__ex_list"."id")`) {
		t.Errorf("the second narrowing did not replace the first: %s", got)
	}
	if strings.Contains(got, `'name'`) {
		t.Errorf("the first narrowing survived the second: %s", got)
	}
}

// Clone copies the selection deeply, or narrowing a derived query would reach
// back into the one it came from.
func TestCloneCarriesTheNarrowingIndependently(t *testing.T) {
	base := sqlb.Query[expTask]().ExpandOnly("list", "name")
	derived := base.Clone().ExpandOnly("list", "id")

	baseSQL, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(baseSQL, `'name'`) || strings.Contains(baseSQL, `'id', "__ex_list"`) {
		t.Errorf("narrowing the clone reached the original: %s", baseSQL)
	}
	derivedSQL, _, err := derived.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(derivedSQL, `'id', "__ex_list"."id"`) {
		t.Errorf("the clone did not take its own narrowing: %s", derivedSQL)
	}
}
