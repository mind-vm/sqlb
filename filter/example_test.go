package filter_test

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// These examples parse against the Article model declared in filter_test.go.
// Only columns whose `sqlb` tag declares a capability can be reached: title is
// filterable, searchable and sortable; body is searchable only; internal_note is
// hidden, so it is absent from responses and unusable as a filter.
func exampleOptions() filter.Options {
	return filter.Options{Model: sqlb.ModelOf[Article]()}
}

// A parsed request compiles into the same predicates hand-written Go produces,
// so it goes through the same builder, the same bind-parameter discipline and
// the same query hooks. This is the whole design: one AST, two producers.
func ExampleParse() {
	values, err := url.ParseQuery("status=eq.published&views=gte.100&sort=-views&per_page=10")
	if err != nil {
		panic(err)
	}

	q, err := filter.Parse(values, exampleOptions())
	if err != nil {
		panic(err)
	}

	sql, args, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "id", "title", "body", "status", "views", "author_id", "draft", "published_at", "created_at" FROM "articles" WHERE ("status" = $1) AND ("views" >= $2) ORDER BY "views" DESC, "id" DESC LIMIT 10 OFFSET 0
	// published 100
}

// A column that does not declare a capability cannot be reached through it, and
// the rejection reports what would have been accepted. Every problem in the
// request is reported at once rather than one per round trip.
func ExampleParse_rejection() {
	values, err := url.ParseQuery("sort=body&internal_note=eq.hunter2")
	if err != nil {
		panic(err)
	}

	_, err = filter.Parse(values, exampleOptions())

	// AsErrors unwraps to the structured form, which carries the allowed
	// alternatives — the difference between a dead end and a fix. Prefer it to a
	// type assertion, which panics the moment a caller wraps the error.
	errs, ok := filter.AsErrors(err)
	if !ok {
		panic("expected parse errors")
	}
	for _, e := range errs {
		fmt.Printf("%s: %s\n", e.Param, e.Reason)
		fmt.Println("  allowed:", strings.Join(e.Allowed, ", "))
	}
	// Output:
	// internal_note: unknown parameter
	//   allowed: id, title, body, status, views, author_id, draft, published_at
	// sort: column is not sortable
	//   allowed: title, views, published_at, created_at
}

// A hidden column stays out of the projection even when the request names no
// columns, because Apply owns the projection rather than falling back to the
// builder's default of every mapped column. Forgetting to project cannot leak a
// password hash.
//
// The trailing `ORDER BY "id"` is Apply making the ordering total. Nothing asked
// for it, and without it an unsorted list has no stable page boundary — page two
// may repeat a row from page one or skip one, whether it is reached by offset or
// by cursor.
func ExampleApply() {
	q, err := filter.Parse(url.Values{}, exampleOptions())
	if err != nil {
		panic(err)
	}

	sql, _, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql)
	// Output:
	// SELECT "id", "title", "body", "status", "views", "author_id", "draft", "published_at", "created_at" FROM "articles" ORDER BY "id" ASC LIMIT 25 OFFSET 0
}

// Search fans out over every searchable column as a disjunction, and the user's
// input is escaped, so typing "50%" searches for that literal string rather than
// matching every row.
func ExampleParse_search() {
	values, err := url.ParseQuery("search=50%25&select=id,title")
	if err != nil {
		panic(err)
	}

	q, err := filter.Parse(values, exampleOptions())
	if err != nil {
		panic(err)
	}

	sql, args, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "id", "title" FROM "articles" WHERE ("title" ILIKE $1) OR ("body" ILIKE $2) ORDER BY "id" ASC LIMIT 25 OFFSET 0
	// %50\%% %50\%%
}

// Cursor pagination reads the URL the same way, and the boundary it produces is
// a predicate like any other — so it goes through the same builder, the same
// bind parameters and the same hooks as everything else.
//
// The cursor names the position of the last row of the previous page. Because
// `?sort=-views` is not a total order on its own, Apply has already appended the
// primary key, and the cursor carries both terms.
//
// Both are descending and neither column is nullable, so the boundary compiles
// to a row comparison rather than the lexicographic OR-chain. That is the form
// Postgres can answer with a single index seek on `(views DESC, id DESC)`, which
// is the entire reason to page this way instead of with OFFSET.
func ExampleParse_cursor() {
	values, err := url.ParseQuery(
		"sort=-views&per_page=10&cursor=" +
			"eyJrIjpbeyJjIjoidmlld3MiLCJkIjp0cnVlLCJ2IjoxMDB9LHsiYyI6ImlkIiwiZCI6dHJ1ZSwidiI6ImE3In1dfQ")
	if err != nil {
		panic(err)
	}

	q, err := filter.Parse(values, exampleOptions())
	if err != nil {
		panic(err)
	}

	sql, args, err := filter.Apply(sqlb.Query[Article](), q).SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql[strings.Index(sql, "WHERE"):])
	fmt.Println(args...)
	// Output:
	// WHERE ("views", "id") < ($1, $2) ORDER BY "views" DESC, "id" DESC LIMIT 10 OFFSET 0
	// 100 a7
}

// A cursor and a page number are two answers to "where does this page start", so
// a request carrying both is refused rather than having one of them silently
// ignored.
func ExampleParse_cursorConflict() {
	values, err := url.ParseQuery("cursor=abc&page=3")
	if err != nil {
		panic(err)
	}

	if _, err := filter.Parse(values, exampleOptions()); err != nil {
		fmt.Println(err)
	}
	// Output:
	// filter: cursor=abc: a cursor and page both say where the page starts; send one or the other
}
