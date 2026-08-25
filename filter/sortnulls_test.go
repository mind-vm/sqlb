package filter_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// Feed is the shape issue #88 was found on: a nullable timestamp whose NULL
// carries a meaning. A NULL published_at means "not published", so those rows
// belong at the bottom of the feed however it is sorted — and Postgres's own
// default puts them at the top the moment the sort goes descending.
type Feed struct {
	ID        string     `db:"id" sqlb:"pk"`
	Pinned    bool       `db:"pinned" sqlb:"sort"`
	Published *time.Time `db:"published_at" sqlb:"sort:nullslast"`
	Retracted *time.Time `db:"retracted_at" sqlb:"sort:nullsfirst"`
	CreatedAt time.Time  `db:"created_at" sqlb:"sort"`
}

func (Feed) TableName() string { return "feed" }

func feedSQL(t *testing.T, query string) string {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Feed]()})
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Feed]().Select(sqlb.F("id")), q).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql
}

// The placement is a property of the column, so it applies in both directions
// rather than only the one that motivated it. Descending is the case from the
// issue; ascending is here because a declaration that only bit on DESC would be
// a rule with an invisible exception.
func TestDeclaredNullPlacementAppliesInBothDirections(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"descending, the case from #88", "sort=-published_at", `ORDER BY "published_at" DESC NULLS LAST`},
		{"ascending", "sort=published_at", `ORDER BY "published_at" ASC NULLS LAST`},
		{"the other placement", "sort=-retracted_at", `ORDER BY "retracted_at" DESC NULLS FIRST`},
		{"dotted spelling", "sort=published_at.desc", `ORDER BY "published_at" DESC NULLS LAST`},
		{
			"beside an undeclared column, which keeps the default",
			"sort=-pinned,-published_at",
			`ORDER BY "pinned" DESC, "published_at" DESC NULLS LAST`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if sql := feedSQL(t, tc.query); !strings.Contains(sql, tc.want) {
				t.Errorf("?%s\n got: %s\nwant it to contain: %s", tc.query, sql, tc.want)
			}
		})
	}
}

// A column that declares nothing must render exactly as it did before #88 — no
// NULLS clause at all, rather than one spelling out the default. The two are
// the same ordering to Postgres and not the same statement to a golden test or
// a reader diffing a query plan.
func TestUndeclaredColumnsRenderNoNullsClause(t *testing.T) {
	for _, query := range []string{"sort=-created_at", "sort=created_at", "sort=-pinned"} {
		if sql := feedSQL(t, query); strings.Contains(sql, "NULLS") {
			t.Errorf("?%s rendered a NULLS clause on a column that declares no placement: %s", query, sql)
		}
	}
}

// The grammar has no spelling for the placement, and that is the design. A
// caller who tries one gets the ordinary unknown-direction refusal rather than
// a half-understood sort.
func TestTheSortGrammarStillRefusesANullsSpelling(t *testing.T) {
	values, _ := url.ParseQuery("sort=published_at.nullslast")
	_, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Feed]()})
	if err == nil {
		t.Fatal("?sort=published_at.nullslast was accepted; the placement is declared, not requested")
	}
	if !strings.Contains(err.Error(), "expected asc or desc") {
		t.Errorf("refusal does not explain what the sort grammar accepts: %v", err)
	}
}
