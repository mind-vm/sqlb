package filter_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// FuzzParse drives the designated untrusted-input parser with whatever a
// client can put in a query string.
//
// The properties asserted are the ones that have to hold no matter what the
// parser is handed: it returns rather than panicking or looping, a query it
// accepts compiles to SQL, a hidden column appears nowhere in that SQL, and a
// column that opted into search but not filtering is reachable only through
// ?search=. The last two are the ones worth fuzzing — capability enforcement
// is checked against a table of known-bad inputs elsewhere, and a table covers
// only what its author thought of.
//
// Note that a column with no capability at all is still *readable*: capabilities
// govern how a column can be reached, not whether it is returned. So the
// projection is not what these assertions look at.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"status=eq.published",
		"status=in.a,b,c",
		`status=in."a,b",c`,
		"or=(status.eq.draft,views.lt.5)",
		"and=(or(status.eq.a,status.eq.b),views.gt.1)",
		"views=between.10,20",
		"published_at=isnull",
		"search=ada&sort=-views&select=title&page=2&per_page=10",
		"title=contains.50%25",
		"expand=author",
		"cursor=abc",
		// The shapes most likely to break a hand-written splitter.
		`status=in."`,
		`status=in.a\`,
		"or=(",
		"or=()",
		"or=(((((((",
		"title=eq.\x00\x01",
		"=",
		"&&&",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	model := sqlb.ModelOf[Article]()

	f.Fuzz(func(t *testing.T, query string) {
		values, err := url.ParseQuery(query)
		if err != nil {
			return
		}
		q, err := filter.Parse(values, filter.Options{Model: model, Expandable: []string{"author"}})
		if err != nil {
			return
		}
		sql, _, err := filter.Apply(sqlb.Query[Article]().Select(sqlb.F("id")), q).SQL()
		if err != nil {
			// A query that parsed must compile, with one deliberate exception.
			// A cursor is judged against the ordering it is used with, which
			// Parse does not know — Apply chooses it. So ErrBadCursor is a
			// rejection that legitimately arrives late, and rest answers it
			// with a 400 like any other bad parameter. Anything else reaching
			// here is a failure the client was given no way to act on.
			if errors.Is(err, sqlb.ErrBadCursor) {
				return
			}
			t.Fatalf("parsed but did not compile: %q: %v", query, err)
		}

		// A hidden column has no spelling anywhere a request can reach —
		// not in the projection, not in a predicate, not in an ordering.
		if strings.Contains(sql, "internal_note") {
			t.Fatalf("query %q put a hidden column in the statement:\n%s", query, sql)
		}

		// body declared search and nothing else, so it may appear below the
		// FROM clause only when ?search= asked for it.
		_, below, found := strings.Cut(sql, " FROM ")
		if found && q.Search == "" && strings.Contains(below, `"body"`) {
			t.Fatalf("query %q reached a search-only column without ?search=:\n%s", query, sql)
		}
	})
}
