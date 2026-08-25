package recipes_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
	"github.com/mind-vm/sqlb/filter"
)

// The REST filter grammar compiles a query string into the same predicate AST
// that hand-written Go produces. One compiler, one bind-parameter discipline,
// one set of hooks — two producers.
//
//	?status=eq.published         operator form
//	?status=published            shorthand for eq
//	?view_count=gte.100          repeated params conjoin
//	?status=in.draft,review      value lists
//	?published_at=isnull         null tests
//	?tags=has.go                 array containment
//	?metadata=hasdoc.{"a":1}     jsonb containment
//	?or=(status.eq.draft,view_count.lt.10)
//	?sort=-created_at,title      "-" for descending
//	?select=id,title             projection
//	?search=ada                  fan-out over searchable columns
//	?page=2&per_page=50          pagination
//	?cursor=…                    keyset pagination, instead of page
func Example_filterFromAQueryString() {
	opts := filter.Options{Model: sqlb.ModelOf[recipes.Post]()}

	values, err := url.ParseQuery("status=in.published,review&view_count=gte.100&sort=-view_count&per_page=10")
	if err != nil {
		panic(err)
	}
	q, err := filter.Parse(values, opts)
	if err != nil {
		panic(err)
	}

	show(filter.Apply(sqlb.Query[recipes.Post](), q))
	// Output:
	// SELECT "id", "org_id", "author_id", "title", "body", "status", "view_count", "tags", "metadata", "published_at", "deleted_at", "created_at" FROM "posts" WHERE ("status" IN ($1, $2)) AND ("view_count" >= $3) ORDER BY "view_count" DESC, "id" DESC LIMIT 10 OFFSET 0
	// args: [published review 100]
}

// Apply owns the projection, and the default is every *non-hidden* column —
// not the builder's "every mapped column". The difference is the reason: a
// handler that forgot to project would otherwise put a hidden column into a
// response, and a default that is safe only when remembered is not a default.
//
// Author.PasswordHash is what that buys. It is absent from both statements
// below, and there is no ?select that names it back in.
func Example_filterProjection() {
	opts := filter.Options{Model: sqlb.ModelOf[recipes.Author]()}

	values, err := url.ParseQuery("select=id,name")
	if err != nil {
		panic(err)
	}
	q, err := filter.Parse(values, opts)
	if err != nil {
		panic(err)
	}
	show(filter.Apply(sqlb.Query[recipes.Author](), q))

	all, err := filter.Parse(url.Values{}, opts)
	if err != nil {
		panic(err)
	}
	show(filter.Apply(sqlb.Query[recipes.Author](), all))
	// Output:
	// SELECT "id", "name" FROM "authors" ORDER BY "id" ASC LIMIT 25 OFFSET 0
	// SELECT "id", "org_id", "name", "email" FROM "authors" ORDER BY "id" ASC LIMIT 25 OFFSET 0
}

// ?search fans out over every column that declared the capability, joined with
// OR. A column is searchable only if it said so, which is what keeps the fan-out
// from turning into a sequential scan of the whole table.
func Example_filterSearch() {
	opts := filter.Options{Model: sqlb.ModelOf[recipes.Post]()}

	values, err := url.ParseQuery("search=postgres")
	if err != nil {
		panic(err)
	}
	q, err := filter.Parse(values, opts)
	if err != nil {
		panic(err)
	}
	showWhere(filter.Apply(sqlb.Query[recipes.Post](), q))
	// Output:
	// WHERE ("title" ILIKE $1) OR ("body" ILIKE $2) ORDER BY "id" ASC LIMIT 25 OFFSET 0
	// args: [%postgres% %postgres%]
}

// Options is where a resource's limits live. They are not advisory: an
// unbounded list endpoint is a denial of service waiting for a client that
// forgets to paginate, so the defaults are conservative and a request over
// budget is refused rather than silently trimmed.
//
// MaxFilters counts leaf conditions, including the ones inside or= groups —
// counting top-level parameters instead would leave the budget open to one
// group holding as many conditions as the client cared to write.
func Example_filterOptionsBoundARequest() {
	opts := filter.Options{
		Model:           sqlb.ModelOf[recipes.Post](),
		DefaultPageSize: 20,
		MaxPageSize:     100,
		MaxFilters:      2,
		MaxSortTerms:    2,
		Expandable:      []string{"author"},
	}

	values, err := url.ParseQuery("or=(status.eq.draft,status.eq.review,view_count.lt.10)")
	if err != nil {
		panic(err)
	}
	_, err = filter.Parse(values, opts)
	// The prefix is the package; the word after it is the parameter at fault,
	// which for a budget overrun is `filter` itself.
	showError(err)
	// Output:
	// filter: filter: 3 filter conditions requested, the limit is 2
}

// ?expand is validated against the Expandable list at parse time and performed
// by Apply, so a parsed expansion is never silently dropped: a name that is not
// there is a 400 that lists the ones that are.
func Example_filterExpand() {
	opts := filter.Options{
		Model:      sqlb.ModelOf[recipes.Post](),
		Expandable: []string{"author"},
	}

	values, err := url.ParseQuery("expand=author&select=id,title")
	if err != nil {
		panic(err)
	}
	q, err := filter.Parse(values, opts)
	if err != nil {
		panic(err)
	}
	fmt.Println("expand:", q.Expand)

	values, err = url.ParseQuery("expand=publisher")
	if err != nil {
		panic(err)
	}
	_, err = filter.Parse(values, opts)
	showError(err)
	// Output:
	// expand: [author]
	// filter: expand=publisher: relation is not expandable (allowed: author)
}

// The whole HTTP-to-SQL layer for a dynamic list endpoint, in one handler:
// parse, apply, run. Everything the request may ask for is decided by the
// column capabilities and the Options; everything the *caller* may see is
// decided by the hooks. Neither is written here.
//
// This is the payoff the rest of the design is aimed at. It is also what
// rest.Resource generates, for callers who would rather not write even this.
func Example_filterAsAnHTTPHandler() {
	opts := filter.Options{
		Model:           sqlb.ModelOf[recipes.Post](),
		DefaultPageSize: 20,
		MaxPageSize:     100,
	}
	db := recordingDB()

	list := func(w http.ResponseWriter, r *http.Request) {
		q, err := filter.Parse(r.URL.Query(), opts)
		if err != nil {
			// WriteError renders every rejected parameter, with the allowed
			// alternatives where there are any.
			filter.WriteError(w, err)
			return
		}

		posts, err := filter.Apply(sqlb.Query[recipes.Post](), q).All(r.Context(), db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": posts})
	}

	rec := httptest.NewRecorder()
	list(rec, httptest.NewRequest(http.MethodGet, "/posts?status=eq.published&sort=-view_count", nil))
	fmt.Println(rec.Code, firstWords(statements()[0], 4))

	rec = httptest.NewRecorder()
	list(rec, httptest.NewRequest(http.MethodGet, "/posts?sort=body", nil))
	fmt.Println(rec.Code, rec.Body.String())
	// Output:
	// 200 SELECT "id", "org_id", "author_id",
	// 400 {"details":[{"param":"sort","value":"body","reason":"column is not sortable","allowed":["title","status","view_count","published_at","created_at"]}],"error":"invalid_query","message":"one or more query parameters were rejected"}
}
