package sqlb_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// The shape from #90: a table whose updated_at should come from the database
// clock and whose hits accumulate, neither of which `col = EXCLUDED.col` can
// say.
type secret struct {
	ID        int64     `db:"id" sqlb:"type:bigint,pk,default"`
	Key       string    `db:"key" sqlb:"type:text"`
	Payload   string    `db:"payload" sqlb:"type:text"`
	Note      *string   `db:"note" sqlb:"type:text"`
	Hits      int64     `db:"hits" sqlb:"type:bigint"`
	UpdatedAt time.Time `db:"updated_at" sqlb:"type:timestamptz,default"`
}

func (secret) TableName() string { return "secrets" }

func TestConflictSetAssignsAnExpression(t *testing.T) {
	sql, args, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}, "payload").
		OnConflictSet("updated_at", sqlb.Now()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `DO UPDATE SET "payload" = EXCLUDED."payload", "updated_at" = now()`
	if !strings.Contains(sql, want) {
		t.Errorf("want it to contain:\n%s\ngot:\n%s", want, sql)
	}
	// now() takes no bind, so the VALUES args are all there is.
	for _, a := range args {
		if _, isTime := a.(time.Time); isTime {
			t.Error("the timestamp was bound from the application clock, which is the thing being fixed")
		}
	}
}

// The accumulate case. Current qualifies to the target table so the statement
// says which row it means rather than relying on nothing else being in scope.
func TestConflictSetAccumulates(t *testing.T) {
	sql, _, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}).
		OnConflictSet("hits", sqlb.Add(sqlb.Current("hits"), sqlb.Val(1))).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if want := `DO UPDATE SET "hits" = "secrets"."hits" + $`; !strings.Contains(sql, want) {
		t.Errorf("want it to contain:\n%s\ngot:\n%s", want, sql)
	}
}

// Keep-on-null, spelled with the Coalesce that already existed — its Expr()
// makes a Selection usable here, so this needed no new vocabulary.
func TestConflictSetKeepsOnNull(t *testing.T) {
	sql, _, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}).
		OnConflictSet("note", sqlb.Coalesce(sqlb.Excluded("note"), sqlb.Current("note")).Expr()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if want := `"note" = coalesce(EXCLUDED."note", "secrets"."note")`; !strings.Contains(sql, want) {
		t.Errorf("want it to contain:\n%s\ngot:\n%s", want, sql)
	}
}

// The bind-numbering property the issue asked for: an assignment's parameter is
// numbered in the same sequence as the VALUES list, not a separate one that
// happens to line up.
func TestConflictSetSharesTheBindNumbering(t *testing.T) {
	sql, args, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}).
		OnConflictSet("payload", sqlb.Val("replacement")).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	last := len(args)
	if args[last-1] != "replacement" {
		t.Fatalf("the assignment's value is not the last argument: %v", args)
	}
	if want := `"payload" = $` + strconv.Itoa(last); !strings.Contains(sql, want) {
		t.Errorf("the assignment did not continue the VALUES numbering; want %s in:\n%s", want, sql)
	}
}

// Both rows are in scope inside DO UPDATE, so a bare reference has two readings
// and SQL silently picks one. Refused rather than resolved.
func TestABareReferenceInAConflictSetIsRefused(t *testing.T) {
	_, _, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}).
		OnConflictSet("hits", sqlb.Add(sqlb.F("hits"), sqlb.Val(1))).
		SQL()
	if err == nil {
		t.Fatal("a bare column reference inside DO UPDATE was accepted")
	}
	for _, want := range []string{"does not say which row", "Excluded", "Current"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not point at the fix (%q missing): %v", want, err)
		}
	}
}

// The same validation the bare-column form already had, extended to the two
// places an assignment can name a column.
func TestConflictSetValidatesColumnNames(t *testing.T) {
	tests := []struct {
		name string
		ins  func() *sqlb.Insert[secret]
		want string
	}{
		{
			"the assigned column",
			func() *sqlb.Insert[secret] {
				return sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
					OnConflictUpdate([]string{"key"}).
					OnConflictSet("hitz", sqlb.Now())
			},
			`OnConflictSet("hitz")`,
		},
		{
			"a column inside the expression",
			func() *sqlb.Insert[secret] {
				return sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
					OnConflictUpdate([]string{"key"}).
					OnConflictSet("hits", sqlb.Current("hitz"))
			},
			`Current("hitz")`,
		},
		{
			"no conflict clause at all",
			func() *sqlb.Insert[secret] {
				return sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
					OnConflictSet("hits", sqlb.Now())
			},
			"needs a conflict clause",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := tc.ins().SQL()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the problem (%q missing): %v", tc.want, err)
			}
		})
	}
}

// An assignment alone is a DO UPDATE, not a DO NOTHING. Without this the
// updated_at-only upsert — which is the case the issue opened with — would
// compile to a statement that writes nothing.
func TestAnAssignmentAloneStillUpdates(t *testing.T) {
	sql, _, err := sqlb.InsertRows(&secret{Key: "k", Payload: "p"}).
		OnConflictUpdate([]string{"key"}).
		OnConflictSet("updated_at", sqlb.Now()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "DO NOTHING") {
		t.Errorf("an upsert carrying only an assignment compiled to DO NOTHING:\n%s", sql)
	}
}
