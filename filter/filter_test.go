package filter_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

type Article struct {
	ID        string     `db:"id" sqlb:"pk,default"`
	Title     string     `db:"title" sqlb:"filter,search,sort"`
	Body      string     `db:"body" sqlb:"search"`
	Status    string     `db:"status" sqlb:"filter"`
	Views     int64      `db:"views" sqlb:"filter,sort"`
	AuthorID  string     `db:"author_id" json:"author_id" sqlb:"filter,expand"`
	Draft     bool       `db:"draft" sqlb:"filter"`
	Secret    string     `db:"internal_note" sqlb:"hidden"`
	Published *time.Time `db:"published_at" sqlb:"filter,sort"`
	CreatedAt time.Time  `db:"created_at" sqlb:"sort,readonly,default"`

	Author *Author `db:"-" json:"author,omitempty" sqlb:"expands=author_id"`
}

func (Article) TableName() string { return "articles" }

// Author is the expansion target. Its hidden column is here on purpose: a
// hidden column must stay hidden when the row is reached through a join.
type Author struct {
	ID    string `db:"id" json:"id" sqlb:"pk"`
	Name  string `db:"name" json:"name"`
	Email string `db:"email" json:"-" sqlb:"hidden"`
}

func (Author) TableName() string { return "authors" }

func opts() filter.Options {
	return filter.Options{Model: sqlb.ModelOf[Article](), Expandable: []string{"author"}}
}

// Doc carries the document column. It is a model of its own rather than two
// more fields on Article so that the package examples keep documenting a
// resource with an ordinary column set.
//
// Blob is here deliberately: []byte and json.RawMessage are the same reflect
// kind, and only one of them may collect the jsonb operators.
type Doc struct {
	ID       string          `db:"id" sqlb:"pk"`
	Title    string          `db:"title" sqlb:"filter,sort"`
	Metadata json.RawMessage `db:"metadata" sqlb:"filter"`
	Blob     []byte          `db:"blob" sqlb:"filter"`
}

func (Doc) TableName() string { return "docs" }

func docOpts() filter.Options { return filter.Options{Model: sqlb.ModelOf[Doc]()} }

// compileDoc is compile against the Doc model.
func compileDoc(t *testing.T, query string) (string, []any) {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, docOpts())
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	b := filter.Apply(sqlb.Query[Doc]().Select(sqlb.F("id")), q)
	sql, args, err := b.SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql, args
}

// compile parses a query string and renders the resulting SQL, which is the
// only way to be sure a filter reached the statement it claimed to.
func compile(t *testing.T, query string) (string, []any) {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, opts())
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	b := filter.Apply(sqlb.Query[Article]().Select(sqlb.F("id")), q)
	sql, args, err := b.SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql, args
}

func TestFilterCompilesToSQL(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
		args  []any
	}{
		{
			name:  "operator form",
			query: "status=eq.published",
			want:  `WHERE "status" = $1`,
			args:  []any{"published"},
		},
		{
			name:  "shorthand equality",
			query: "status=published",
			want:  `WHERE "status" = $1`,
			args:  []any{"published"},
		},
		{
			name:  "a dotted value is not mistaken for an operator",
			query: "author_id=alice.smith@example.com",
			want:  `WHERE "author_id" = $1`,
			args:  []any{"alice.smith@example.com"},
		},
		{
			name:  "repeated parameters conjoin into a range",
			query: "views=gte.10&views=lt.100",
			want:  `WHERE ("views" >= $1) AND ("views" < $2)`,
			args:  []any{int64(10), int64(100)},
		},
		{
			name:  "value list",
			query: "status=in.draft,published",
			want:  `WHERE "status" IN ($1, $2)`,
			args:  []any{"draft", "published"},
		},
		{
			name:  "null test",
			query: "published_at=isnull",
			want:  `WHERE "published_at" IS NULL`,
		},
		{
			name:  "between",
			query: "views=between.10,20",
			want:  `WHERE "views" BETWEEN $1 AND $2`,
			args:  []any{int64(10), int64(20)},
		},
		{
			name:  "explicit disjunction",
			query: "or=(status.eq.draft,views.lt.5)",
			want:  `WHERE ("status" = $1) OR ("views" < $2)`,
			args:  []any{"draft", int64(5)},
		},
		{
			name:  "search fans out over searchable columns only",
			query: "search=ada",
			want:  `WHERE ("title" ILIKE $1) OR ("body" ILIKE $2)`,
			args:  []any{"%ada%", "%ada%"},
		},
		{
			name:  "contains escapes wildcards",
			query: "title=contains.50%25",
			want:  `WHERE "title" ILIKE $1`,
			args:  []any{`%50\%%`},
		},
		{
			name:  "boolean coercion",
			query: "draft=true",
			want:  `WHERE "draft" = $1`,
			args:  []any{true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args := compile(t, tt.query)
			if !strings.Contains(sql, tt.want) {
				t.Errorf("SQL\n got: %s\nwant it to contain: %s", sql, tt.want)
			}
			if len(args) != len(tt.args) {
				t.Fatalf("args = %#v, want %#v", args, tt.args)
			}
			for i := range tt.args {
				if args[i] != tt.args[i] {
					t.Errorf("arg %d = %#v (%T), want %#v (%T)", i, args[i], args[i], tt.args[i], tt.args[i])
				}
			}
		})
	}
}

func TestSortAndPagination(t *testing.T) {
	sql, _ := compile(t, "sort=-views,title&page=3&per_page=10")
	if !strings.Contains(sql, `ORDER BY "views" DESC, "title" ASC`) {
		t.Errorf("ordering missing from: %s", sql)
	}
	if !strings.Contains(sql, "LIMIT 10 OFFSET 20") {
		t.Errorf("pagination missing from: %s", sql)
	}
}

func TestPostgRESTSortSpelling(t *testing.T) {
	sql, _ := compile(t, "sort=views.desc")
	if !strings.Contains(sql, `ORDER BY "views" DESC`) {
		t.Errorf("the `column.desc` spelling should work too, got: %s", sql)
	}
}

func TestSelectAlwaysKeepsThePrimaryKey(t *testing.T) {
	sql, _ := compile(t, "select=title")
	if !strings.Contains(sql, `SELECT "id", "title"`) {
		t.Errorf("a projection must keep the key that addresses the row, got: %s", sql)
	}
}

// Capability enforcement is the security boundary: a column without a
// capability must be unreachable through it, and the rejection must say what
// is reachable instead.
func TestCapabilitiesAreEnforced(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantReason string
		wantAllows string
	}{
		{"undeclared column", "created_at=eq.x", "not filterable", "title"},
		{"unknown column", "nonsense=eq.x", "unknown parameter", "title"},
		{"unsortable column", "sort=body", "not sortable", "title"},
		{"hidden column is invisible", "internal_note=eq.x", "unknown parameter", ""},
		{"hidden column cannot be selected", "select=internal_note", "unknown column", ""},
		{"unexpandable relation", "expand=secrets", "not expandable", "author"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, _ := url.ParseQuery(tt.query)
			_, err := filter.Parse(values, opts())
			if err == nil {
				t.Fatalf("Parse(%q) should have been rejected", tt.query)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantReason)
			}
			if tt.wantAllows != "" && !strings.Contains(err.Error(), tt.wantAllows) {
				t.Errorf("error = %q, want it to list %q among the allowed values", err, tt.wantAllows)
			}
		})
	}
}

// A hidden column must not be probeable: neither its name nor its existence
// should show up in a rejection.
func TestHiddenColumnsDoNotLeak(t *testing.T) {
	values, _ := url.ParseQuery("internal_note=eq.x")
	_, err := filter.Parse(values, opts())
	if err == nil {
		t.Fatal("filtering a hidden column should be rejected")
	}
	if strings.Contains(err.Error(), "internal_note") && strings.Contains(err.Error(), "allowed") {
		// The parameter name is echoed, which is fine; it must not appear in
		// the allow-list.
		allowed := err.Error()[strings.Index(err.Error(), "allowed"):]
		if strings.Contains(allowed, "internal_note") {
			t.Errorf("hidden column leaked into the allow-list: %s", err)
		}
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	values, _ := url.ParseQuery("nope=eq.1&sort=body&select=internal_note")
	_, err := filter.Parse(values, opts())
	if err == nil {
		t.Fatal("expected errors")
	}
	errs, ok := filter.AsErrors(err)
	if !ok {
		t.Fatalf("error type = %T, want filter.Errors", err)
	}
	if len(errs) != 3 {
		t.Errorf("reported %d problems, want 3: %v", len(errs), errs)
	}
}

func TestTypeCoercionFailures(t *testing.T) {
	for _, query := range []string{"views=eq.notanumber", "draft=eq.maybe", "published_at=gt.yesterday"} {
		values, _ := url.ParseQuery(query)
		if _, err := filter.Parse(values, opts()); err == nil {
			t.Errorf("Parse(%q) should have rejected the value", query)
		}
	}
}

func TestTimestampCoercion(t *testing.T) {
	sql, args := compile(t, "published_at=gte.2024-01-02")
	if !strings.Contains(sql, `"published_at" >= $1`) {
		t.Fatalf("SQL = %s", sql)
	}
	if _, ok := args[0].(time.Time); !ok {
		t.Errorf("arg type = %T, want time.Time", args[0])
	}
}

func TestPageSizeIsCapped(t *testing.T) {
	values, _ := url.ParseQuery("per_page=100000")
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article](), MaxPageSize: 50})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Limit != 50 {
		t.Errorf("limit = %d, want it capped to 50", q.Limit)
	}
}

func TestUnpaginatedRequestGetsADefaultLimit(t *testing.T) {
	sql, _ := compile(t, "")
	if !strings.Contains(sql, "LIMIT") {
		t.Errorf("a list query must always be bounded, got: %s", sql)
	}
}

func TestFilterCountIsBounded(t *testing.T) {
	var parts []string
	for i := 0; i < 30; i++ {
		parts = append(parts, "status=eq.a")
	}
	values, _ := url.ParseQuery(strings.Join(parts, "&"))
	if _, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article](), MaxFilters: 5}); err == nil {
		t.Error("an unbounded number of filters should be rejected")
	}
}

func TestGroupNestingIsBounded(t *testing.T) {
	deep := "or=(" + strings.Repeat("or(", 6) + "status.eq.a" + strings.Repeat(")", 6) + ")"
	values, _ := url.ParseQuery(deep)
	if _, err := filter.Parse(values, opts()); err == nil {
		t.Error("deeply nested groups should be rejected")
	}
}

// A group is one entry in Query.Where and any number of conditions, so a
// budget counting entries bounds the wrong thing: nesting was capped and width
// was not, and one `or=` could carry as many conditions as a client cared to
// write while the count of filters stayed at one.
func TestGroupWidthIsBounded(t *testing.T) {
	var conds []string
	for i := 0; i < 200; i++ {
		conds = append(conds, "status.eq.a")
	}
	values, _ := url.ParseQuery("or=(" + strings.Join(conds, ",") + ")")
	_, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article](), MaxFilters: 5})
	if err == nil {
		t.Fatal("a group wider than the filter budget should be rejected")
	}
	// One error, not one per condition over the limit: a pathological request
	// must not be answered with a pathological document.
	var errs filter.Errors
	if errors.As(err, &errs) && len(errs) != 1 {
		t.Errorf("reported %d errors, want 1", len(errs))
	}
}

// An `in` list is one condition however long it is, so the filter budget never
// bounded it. Long enough and it exhausts the driver's parameter limit, which
// arrives as a 500 for a request that should have been refused.
func TestListLengthIsBounded(t *testing.T) {
	var vals []string
	for i := 0; i < 500; i++ {
		vals = append(vals, "a")
	}
	values, _ := url.ParseQuery("status=in." + strings.Join(vals, ","))
	if _, err := filter.Parse(values, opts()); err == nil {
		t.Error("an unbounded IN list should be rejected")
	}

	// A list within the limit still parses, so the guard is proven both ways.
	values, _ = url.ParseQuery("status=in.a,b,c")
	if _, err := filter.Parse(values, opts()); err != nil {
		t.Errorf("a short list should still be accepted: %v", err)
	}
}

// The pattern operators pass their operand through unescaped on purpose, so
// value length is a lever on how much work a scan does.
func TestValueLengthIsBounded(t *testing.T) {
	long := strings.Repeat("a", 10000)
	for _, q := range []string{
		"title=like." + long,
		"title=eq." + long,
		"search=" + long,
		"status=in.a," + long,
	} {
		values, _ := url.ParseQuery(q)
		if _, err := filter.Parse(values, opts()); err == nil {
			t.Errorf("an oversized value should be rejected: %s", q[:20])
		}
	}
	// A list of many short values is not an oversized value, and must not be
	// caught by measuring the list in aggregate.
	values, _ := url.ParseQuery("status=in." + strings.Repeat("ab,", 60) + "z")
	if _, err := filter.Parse(values, opts()); err != nil {
		t.Errorf("a long list of short values should be accepted: %v", err)
	}
}

func TestQuotedValuesKeepTheirCommas(t *testing.T) {
	sql, args := compile(t, `status=in."a,b",c`)
	if !strings.Contains(sql, `"status" IN ($1, $2)`) {
		t.Fatalf("SQL = %s", sql)
	}
	if args[0] != "a,b" {
		t.Errorf("arg 0 = %#v, want %q", args[0], "a,b")
	}
}

// Containment is what a document column is for: the point of `metadata` is
// that a caller attaches keys nobody declared and narrows by them later, which
// is the one filter that cannot be expressed as a column capability.
func TestJSONContainment(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
		arg   string
	}{
		{
			name:  "object",
			query: `metadata=hasdoc.{"lang":"de"}`,
			want:  `WHERE "metadata" @> $1::jsonb`,
			arg:   `{"lang":"de"}`,
		},
		{
			// The comma inside the object must not be read as a value
			// separator, and the nesting must survive intact.
			name:  "nested object with commas",
			query: `metadata=hasdoc.{"a":{"b":1,"c":2},"d":[1,2]}`,
			want:  `WHERE "metadata" @> $1::jsonb`,
			arg:   `{"a":{"b":1,"c":2},"d":[1,2]}`,
		},
		{
			name:  "array",
			query: `metadata=hasdoc.["urgent"]`,
			want:  `WHERE "metadata" @> $1::jsonb`,
			arg:   `["urgent"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args := compileDoc(t, tt.query)
			if !strings.Contains(sql, tt.want) {
				t.Fatalf("SQL = %s\nwant it to contain %s", sql, tt.want)
			}
			if len(args) != 1 || args[0] != tt.arg {
				t.Errorf("args = %#v, want [%q]", args, tt.arg)
			}
		})
	}
}

// The `,` inside a JSON object sits at the same nesting level as the one
// separating conditions, so a group has to count braces to tell them apart.
func TestJSONContainmentInsideAGroup(t *testing.T) {
	sql, args := compileDoc(t, `or=(metadata.hasdoc.{"a":1,"b":2},title.eq.draft)`)
	if !strings.Contains(sql, `("metadata" @> $1::jsonb) OR ("title" = $2)`) {
		t.Fatalf("SQL = %s", sql)
	}
	if args[0] != `{"a":1,"b":2}` {
		t.Errorf("arg 0 = %#v, want the whole object", args[0])
	}
}

func TestJSONColumnRejections(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantReason string
		wantAllows string
	}{
		{
			name:       "malformed document",
			query:      `metadata=hasdoc.{"lang":`,
			wantReason: "not valid JSON",
		},
		{
			name:       "empty document",
			query:      "metadata=hasdoc.",
			wantReason: "needs a JSON document",
		},
		{
			// The request named no operator, so the rejection must not quote
			// back the "eq" that the shorthand rule inferred.
			name:       "shorthand has no meaning here",
			query:      `metadata={"lang":"de"}`,
			wantReason: "no shorthand form",
			wantAllows: "hasdoc",
		},
		{
			name:       "ordering operator",
			query:      "metadata=gt.1",
			wantReason: "does not apply to the JSON document column metadata",
			wantAllows: "hasdoc",
		},
		{
			name:       "pattern operator",
			query:      "metadata=startswith.x",
			wantReason: "does not apply to the JSON document column metadata",
			wantAllows: "hasdoc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, _ := url.ParseQuery(tt.query)
			_, err := filter.Parse(values, docOpts())
			if err == nil {
				t.Fatalf("Parse(%q) should have been rejected", tt.query)
			}
			if !strings.Contains(err.Error(), tt.wantReason) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantReason)
			}
			if tt.wantAllows != "" && !strings.Contains(err.Error(), tt.wantAllows) {
				t.Errorf("error = %q, want it to offer %q", err, tt.wantAllows)
			}
			if strings.Contains(err.Error(), "shorthand") && strings.Contains(err.Error(), `"eq"`) {
				t.Errorf("the rejection quotes an operator the request never wrote: %s", err)
			}
		})
	}
}

// Null tests keep working on a document column: "no metadata at all" is a
// different question from "metadata containing nothing", and both are askable.
func TestJSONColumnStillTakesNullTests(t *testing.T) {
	sql, _ := compileDoc(t, "metadata=isnull")
	if !strings.Contains(sql, `"metadata" IS NULL`) {
		t.Fatalf("SQL = %s", sql)
	}
}

// json.RawMessage and []byte are both slices of bytes. Only the first is a
// document, and a bytea column must keep the ordinary operators rather than be
// offered containment it cannot answer.
func TestByteaIsNotTreatedAsJSON(t *testing.T) {
	values, _ := url.ParseQuery(`blob=contains.{"a":1}`)
	_, err := filter.Parse(values, docOpts())
	if err == nil {
		t.Fatal("contains on a bytea column should have been rejected")
	}
	if !strings.Contains(err.Error(), "needs a text column") {
		t.Errorf("error = %q, want the text-column rejection, not the jsonb one", err)
	}
}

// TestApplyNeverProjectsHiddenColumns is the last line of defence: a handler
// that forgets to project must still not leak a hidden column into a response.
func TestApplyNeverProjectsHiddenColumns(t *testing.T) {
	values, _ := url.ParseQuery("")
	q, err := filter.Parse(values, opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if strings.Contains(sql, "internal_note") {
		t.Errorf("hidden column reached the projection: %s", sql)
	}
	if !strings.Contains(sql, `"title"`) {
		t.Errorf("visible columns should still be projected: %s", sql)
	}
}

// The package contract is that a parameter is never silently ignored. Apply now
// performs the join rather than refusing it, so the assertion is that the
// relation reaches the SQL — an accepted ?expand that compiled to a statement
// without the join would be the same silent drop, wearing a 200.
func TestApplyPerformsAnExpand(t *testing.T) {
	values, _ := url.ParseQuery("expand=author")
	q, err := filter.Parse(values, opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(q.Expand) != 1 || q.Expand[0] != "author" {
		t.Fatalf("Expand = %v, want [author]", q.Expand)
	}

	b := filter.Apply(sqlb.Query[Article](), q)
	if err := b.Err(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	sql, _, err := b.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{
		`LEFT JOIN "authors" AS "__ex_author"`,
		`AS "__expand_author"`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("statement missing %q:\n%s", want, sql)
		}
	}
	// Hidden survives the join: the target's email must not be built into the
	// JSON object, or expansion becomes a way around the capability.
	if strings.Contains(sql, "email") {
		t.Errorf("a hidden column of the expanded target reached the statement:\n%s", sql)
	}
}

// Not asking for an expansion must not pay for one.
func TestApplyWithoutExpandDoesNotJoin(t *testing.T) {
	q, err := filter.Parse(url.Values{}, opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sql, _, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "LEFT JOIN") {
		t.Errorf("an unexpanded query joined anyway:\n%s", sql)
	}
}

// A cursor is read off the last row of the page, so the ordering columns have
// to be fetched even when ?select leaves them out. Otherwise the cursor would
// encode a zero value and the next page would start from the beginning.
func TestApplyProjectsTheOrderingColumns(t *testing.T) {
	values, err := url.ParseQuery("select=id,title&sort=-views")
	if err != nil {
		t.Fatal(err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article]()})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	sql, _, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, `SELECT "id", "title", "views" FROM`) {
		t.Errorf("SQL = %s\nwant views fetched alongside the requested columns", sql)
	}
	// The request's own ?select is what the response is built from, and it is
	// untouched — rest marshals from q.Select, not from what the statement read.
	if got := strings.Join(q.Select, ","); got != "id,title" {
		t.Errorf("q.Select = %q, want the request's own selection unchanged", got)
	}
}

// A cursor names a position in an ordering, so it is only meaningful against
// the ordering it was issued for. Changing ?sort= and keeping the cursor is the
// ordinary way a client reaches this, so the message says so.
func TestCursorAgainstADifferentSortIsRejected(t *testing.T) {
	issued, err := sqlb.Query[Article]().OrderBy(sqlb.F("views").Desc()).
		CursorFor(Article{ID: "a7", Views: 100})
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}

	values, err := url.ParseQuery("sort=title&cursor=" + string(issued))
	if err != nil {
		t.Fatal(err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article]()})
	if err != nil {
		t.Fatalf("Parse should accept the cursor and leave the mismatch to Apply: %v", err)
	}

	_, _, err = filter.Apply(sqlb.Query[Article](), q).SQL()
	if err == nil {
		t.Fatal("expected the cursor to be refused against a different sort")
	}
	if !errors.Is(err, sqlb.ErrBadCursor) {
		t.Errorf("error %v does not wrap ErrBadCursor, so rest cannot map it to 400", err)
	}
}

// Offset paging is bounded like every other untrusted-input dimension. A deep
// offset is the cheapest per-request scan-cost lever the grammar has, and the
// silly end of it — page 2^63-1 — used to overflow (page-1)*size into a negative
// offset that failed at the database rather than at validation.
func TestOffsetPagingIsBounded(t *testing.T) {
	for _, tc := range []struct{ query, param string }{
		{"page=50000000", "page"},
		{"page=9223372036854775807", "page"},
		{"offset=1000000", "offset"},
	} {
		values, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		_, err = filter.Parse(values, opts())
		errs, ok := filter.AsErrors(err)
		if !ok {
			t.Errorf("Parse(%q) = %v, want a refusal", tc.query, err)
			continue
		}
		if len(errs) != 1 || errs[0].Param != tc.param {
			t.Errorf("Parse(%q) errors = %v, want one about %s", tc.query, errs, tc.param)
			continue
		}
		// The refusal is also where cursor paging gets discovered.
		if !strings.Contains(errs[0].Reason, "cursor") {
			t.Errorf("reason = %q, want it to point at cursor paging", errs[0].Reason)
		}
	}
}

// And the budget is a ceiling, not a ban: an ordinary page still parses, and the
// bound is overridable per resource like the others.
func TestOffsetPagingWithinTheBudgetStillParses(t *testing.T) {
	values, _ := url.ParseQuery("page=40&per_page=25")
	q, err := filter.Parse(values, opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if q.Offset != 975 {
		t.Errorf("Offset = %d, want 975", q.Offset)
	}

	o := opts()
	o.MaxOffset = 10
	values, _ = url.ParseQuery("offset=11")
	if _, err := filter.Parse(values, o); err == nil {
		t.Error("a per-resource MaxOffset of 10 accepted offset=11")
	}
}

// A reserved parameter that means one thing per request must say so when it is
// sent twice. The parser reads these with url.Values.Get, so a second occurrence
// used to vanish: `?sort=title&sort=views` sorted by title and reported nothing,
// while a repeated per-column filter parameter conjoins — an asymmetry no caller
// could see.
func TestRepeatedSingleValuedParametersAreRefused(t *testing.T) {
	for _, query := range []string{
		"sort=title&sort=views",
		"search=a&search=b",
		"select=title&select=body",
		"page=1&page=2",
		"limit=1&limit=2",
		"cursor=a&cursor=b",
		"filter=%7B%7D&filter=%7B%7D",
	} {
		values, err := url.ParseQuery(query)
		if err != nil {
			t.Fatalf("bad test query %q: %v", query, err)
		}
		_, err = filter.Parse(values, opts())
		errs, ok := filter.AsErrors(err)
		if !ok {
			t.Errorf("Parse(%q) = %v, want a refusal naming the repeated parameter", query, err)
			continue
		}
		want := strings.SplitN(query, "=", 2)[0]
		var found bool
		for _, e := range errs {
			if e.Param == want && strings.Contains(e.Reason, "one value per request") {
				found = true
			}
		}
		if !found {
			t.Errorf("Parse(%q) errors = %v, want one about %q", query, errs, want)
		}
	}
}

// The other direction: parameters that conjoin by design still take repeats, and
// so does a per-column filter. Without this the fix above would be a regression
// dressed as a refusal.
func TestRepeatedGroupAndColumnParametersStillConjoin(t *testing.T) {
	values, err := url.ParseQuery("or=(views.gte.10,draft.eq.true)&or=(status.eq.a,status.eq.b)&views=gte.1&views=lte.9")
	if err != nil {
		t.Fatal(err)
	}
	q, err := filter.Parse(values, opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(q.Where) != 4 {
		t.Errorf("Where has %d predicates, want 4 (two groups and two column filters)", len(q.Where))
	}
}

func TestCursorConflictsWithOffset(t *testing.T) {
	values, err := url.ParseQuery("cursor=abc&offset=20")
	if err != nil {
		t.Fatal(err)
	}
	_, err = filter.Parse(values, filter.Options{Model: sqlb.ModelOf[Article]()})
	errs, ok := filter.AsErrors(err)
	if !ok {
		t.Fatalf("expected a parse error, got %v", err)
	}
	if len(errs) != 1 || errs[0].Param != "cursor" {
		t.Fatalf("errors = %v, want one about the cursor", errs)
	}
	if !strings.Contains(errs[0].Reason, "offset") {
		t.Errorf("reason = %q, want it to name the conflicting parameter", errs[0].Reason)
	}
}

// The query-parameter grammar gained `not=(…)` in #98, completing the triple
// `or`/`and`/`not` that the JSON tree already had. These pin the four things
// that decision rests on.
func TestNotGroup(t *testing.T) {
	t.Run("negates a group, so a nested De Morgan is the parser's job", func(t *testing.T) {
		sql, args := compile(t, "not=(or(status.eq.draft,views.lt.5))")
		want := `WHERE NOT (("status" = $1) OR ("views" < $2))`
		if !strings.Contains(sql, want) {
			t.Errorf("sql = %q\nwant it to contain %q", sql, want)
		}
		if len(args) != 2 || args[0] != "draft" {
			t.Errorf("args = %v", args)
		}
	})

	// A group is variadic by syntax, so the bare list has to mean something.
	t.Run("a bare list reads as NOT (a AND b), inverting and=(…) exactly", func(t *testing.T) {
		not, _ := compile(t, "not=(status.eq.draft,views.lt.5)")
		and, _ := compile(t, "and=(status.eq.draft,views.lt.5)")
		// The conjunctive form, wrapped in NOT, verbatim — that is what makes
		// `?not=(…)` the exact inverse of `?and=(…)` rather than merely similar.
		conj := whereClause(t, and)
		if want := "NOT (" + conj + ")"; whereClause(t, not) != want {
			t.Errorf("not WHERE = %q\nwant %q", whereClause(t, not), want)
		}
	})

	t.Run("nests inside another group", func(t *testing.T) {
		sql, _ := compile(t, "or=(status.eq.draft,not(views.lt.5))")
		want := `WHERE ("status" = $1) OR (NOT ("views" < $2))`
		if !strings.Contains(sql, want) {
			t.Errorf("sql = %q\nwant it to contain %q", sql, want)
		}
	})

	// Group parameters conjoin, and `not` is one: several of them are
	// NOT A AND NOT B, which is what a reader expects and what `or`/`and`
	// already do.
	t.Run("repeats conjoin rather than being refused", func(t *testing.T) {
		values, err := url.ParseQuery("not=(status.eq.draft)&not=(views.lt.5)")
		if err != nil {
			t.Fatal(err)
		}
		q, err := filter.Parse(values, opts())
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(q.Where) != 2 {
			t.Errorf("Where has %d predicates, want 2", len(q.Where))
		}
	})

	// The bounds are the group bounds; nothing new threads through.
	t.Run("is bounded by the same nesting limit", func(t *testing.T) {
		deep := "not=(" + strings.Repeat("not(", 6) + "status.eq.a" + strings.Repeat(")", 6) + ")"
		values, err := url.ParseQuery(deep)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := filter.Parse(values, opts()); err == nil {
			t.Fatal("expected the nesting limit to refuse this")
		}
	})

	// The nested form requires the parenthesis, so an item that merely starts
	// with "not" is still read as a column reference. Without the guard it
	// would be swallowed as a malformed group and the caller would be told
	// about parentheses rather than about their column.
	t.Run("an item starting with not is still read as a column", func(t *testing.T) {
		values, err := url.ParseQuery("or=(notreally.eq.x,status.eq.draft)")
		if err != nil {
			t.Fatal(err)
		}
		_, err = filter.Parse(values, opts())
		if err == nil {
			t.Fatal("expected an unknown-column error")
		}
		if !strings.Contains(err.Error(), "notreally") {
			t.Errorf("error = %v\nwant it to name the column rather than the group syntax", err)
		}
	})
}

// The two grammars have to agree: that is the argument that settled `not`
// compiling to a bare SQL NOT rather than IS NOT TRUE, so it is worth a test
// rather than a convention. Each case is one logical filter written both ways.
func TestGrammarsAgree(t *testing.T) {
	cases := []struct {
		name   string
		params string
		tree   string
	}{
		{
			name:   "negated disjunction",
			params: "not=(or(status.eq.draft,views.lt.5))",
			tree:   `{"op":"not","children":[{"op":"or","children":[{"op":"eq","field":"status","value":"draft"},{"op":"lt","field":"views","value":5}]}]}`,
		},
		{
			name:   "bare list is the tree's not over an explicit and",
			params: "not=(status.eq.draft,views.lt.5)",
			tree:   `{"op":"not","children":[{"op":"and","children":[{"op":"eq","field":"status","value":"draft"},{"op":"lt","field":"views","value":5}]}]}`,
		},
		{
			name:   "negation nested inside a disjunction",
			params: "or=(status.eq.draft,not(views.lt.5))",
			tree:   `{"op":"or","children":[{"op":"eq","field":"status","value":"draft"},{"op":"not","children":[{"op":"lt","field":"views","value":5}]}]}`,
		},
		{
			// The leaf complement and the group negation are different
			// spellings that must not be different filters.
			name:   "leaf complement equals the negated leaf",
			params: "not=(status.eq.draft)",
			tree:   `{"op":"not","children":[{"op":"eq","field":"status","value":"draft"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fromParams, argsA := compile(t, tc.params)
			fromTree, argsB := compile(t, "filter="+url.QueryEscape(tc.tree))
			if fromParams != fromTree {
				t.Errorf("the two grammars compiled differently:\n  params: %s\n  tree:   %s", fromParams, fromTree)
			}
			if len(argsA) != len(argsB) {
				t.Errorf("args differ: %v vs %v", argsA, argsB)
			}
		})
	}
}

// whereClause returns everything between WHERE and ORDER BY, so a test can
// compare two statements' predicates without restating the projection.
func whereClause(t *testing.T, sql string) string {
	t.Helper()
	start := strings.Index(sql, "WHERE ")
	if start < 0 {
		t.Fatalf("no WHERE in %q", sql)
	}
	rest := sql[start+len("WHERE "):]
	if end := strings.Index(rest, " ORDER BY "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}
