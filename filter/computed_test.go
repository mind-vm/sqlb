package filter_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// Report is a model with two derived columns: one the row can answer on its
// own, and one that depends on who is asking.
type Report struct {
	ID        string `db:"id" sqlb:"pk"`
	Title     string `db:"title" sqlb:"filter,search,sort"`
	DueDays   int32  `db:"due_days" sqlb:"filter"`
	IsOverdue bool   `db:"is_overdue" sqlb:"filter,sort,readonly"`
	IsMine    bool   `db:"is_mine" sqlb:"filter,readonly"`
}

func (Report) TableName() string { return "reports" }

func (Report) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{
		{Name: "is_overdue", Expr: "due_days < 0"},
		{Name: "is_mine", Expr: "owner_id = ?", Needs: []string{"viewer"}},
	}
}

func reportSQL(t *testing.T, query string) (string, []any) {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, filter.Options{
		Model:    sqlb.ModelOf[Report](),
		Computed: []string{"is_overdue", "is_mine"},
	})
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	b := filter.Apply(sqlb.Query[Report]().Bind("viewer", "member-1"), q)
	sql, args, err := b.SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql, args
}

// The filter grammar reaches a derived column because it reaches every column:
// it builds predicates as F(name), and the substitution happens below that.
func TestFilterOnAComputedColumn(t *testing.T) {
	sql, _ := reportSQL(t, "is_overdue=true")
	if !strings.Contains(sql, "WHERE (due_days < 0) = $") {
		t.Errorf("the expression should be in the predicate:\n%s", sql)
	}
}

func TestSortByAComputedColumn(t *testing.T) {
	sql, _ := reportSQL(t, "sort=-is_overdue")
	if !strings.Contains(sql, "ORDER BY (due_days < 0) DESC") {
		t.Errorf("the expression should be in the ordering:\n%s", sql)
	}
}

// ?select names it like any other column, and the projection aliases the
// expression back to the name — which is what lets the scan find it.
func TestSelectAComputedColumn(t *testing.T) {
	sql, _ := reportSQL(t, "select=id,is_overdue")
	if !strings.Contains(sql, `(due_days < 0) AS "is_overdue"`) {
		t.Errorf("the projection should alias the expression:\n%s", sql)
	}
	if strings.Contains(sql, `"title"`) {
		t.Errorf("?select should still narrow the projection:\n%s", sql)
	}
}

// A capability a computed column did not declare is refused exactly as it is
// for a stored one, and the rejection names what would have worked (ADR-0011).
func TestUndeclaredCapabilityOnAComputedColumnIsRefused(t *testing.T) {
	values, err := url.ParseQuery("sort=is_mine")
	if err != nil {
		t.Fatal(err)
	}
	_, err = filter.Parse(values, filter.Options{
		Model:    sqlb.ModelOf[Report](),
		Computed: []string{"is_overdue", "is_mine"},
	})
	if err == nil {
		t.Fatal("sorting on a column that is not Sortable should be refused")
	}
	if !strings.Contains(err.Error(), "is_overdue") {
		t.Errorf("the rejection should name the sortable columns: %v", err)
	}
}

// The per-viewer expression is one parameter however many places it appears.
func TestComputedBindIsSentOnce(t *testing.T) {
	sql, args := reportSQL(t, "is_mine=true")
	if n := strings.Count(sql, "$1"); n != 2 {
		t.Errorf("want the viewer bound once and referenced twice, got %d:\n%s", n, sql)
	}
	if len(args) == 0 || args[0] != "member-1" {
		t.Errorf("args = %v, want the viewer first", args)
	}
}

// Options.Columns is the same per-resource reachability, generalised to stored
// columns: one model, two surfaces, and the second one may not see everything
// the first does (#148).
//
// Tested here as well as in rest because Parse and Apply are the layer that
// decides it — a resource whose parser refused a column while Apply projected
// it anyway would read the value on every request and drop it on the way out,
// which is a narrowing of the response and not of the query.
func TestColumnsNarrowsTheSurfaceParseAndApplyAgreeOn(t *testing.T) {
	narrow := filter.Options{
		Model:   sqlb.ModelOf[Report](),
		Columns: []string{"id", "title"},
	}

	// Not projected. The default projection is built in Apply, so this is the
	// half a response-only narrowing would have missed.
	q, err := filter.Parse(url.Values{}, narrow)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Report](), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "due_days") {
		t.Errorf("the narrowed projection reads a column outside its surface:\n%s", sql)
	}
	for _, want := range []string{`"id"`, `"title"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("the narrowed projection dropped %s:\n%s", want, sql)
		}
	}

	// Not filterable, not sortable, not nameable — and not offered back in the
	// rejection, which for a resource narrowed to conceal a column is the
	// difference between a refusal and a disclosure.
	for _, query := range []string{"due_days=eq.3", "sort=due_days", "select=id,due_days"} {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		_, err = filter.Parse(values, narrow)
		if err == nil {
			t.Errorf("%s: accepted against a resource that does not serve the column", query)
			continue
		}
		if !strings.Contains(err.Error(), "unknown") {
			t.Errorf("%s: the refusal should read as unknown: %v", query, err)
		}
		// The message carries the allowed list, and due_days must not be in it.
		// The name the caller typed is echoed before it, which is not a
		// disclosure — the caller typed it.
		_, allowed, found := strings.Cut(err.Error(), "(allowed: ")
		if !found {
			t.Errorf("%s: the refusal names nothing that would have been accepted: %v", query, err)
			continue
		}
		if strings.Contains(allowed, "due_days") {
			t.Errorf("%s: the allowed list offers the column the resource does not serve: %v", query, err)
		}
	}

	// An empty Columns is no narrowing at all, which is what every existing
	// resource relies on.
	wide := filter.Options{Model: sqlb.ModelOf[Report]()}
	if _, err := filter.Parse(url.Values{"due_days": {"eq.3"}}, wide); err != nil {
		t.Errorf("an un-narrowed resource lost a filter: %v", err)
	}
}
