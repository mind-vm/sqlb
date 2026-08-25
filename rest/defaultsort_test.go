package rest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// A resource's declared ordering is what silence means.
//
// The failure it closes is the quiet one: a list with no `?sort` is well-formed
// in any order, so a caller that forgets to restate the ordering gets a 200 and
// the wrong product, and nothing anywhere says so (#165).
func TestListUsesTheDeclaredDefaultOrdering(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	opts := postOptions()
	opts.DefaultSort = []string{"-status", "created_at"}
	api := mount(t, db.db, opts)

	if resp := api.Get("/posts"); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	if !strings.Contains(stmt, `ORDER BY "status" DESC, "created_at" ASC`) {
		t.Errorf("the declared ordering did not reach the statement:\n%s", stmt)
	}
	// The primary-key tiebreak is still appended, which is what keeps a cursor
	// able to name a position.
	if !strings.Contains(stmt, `"id" ASC`) {
		t.Errorf("the ordering was not made total:\n%s", stmt)
	}
}

// ?sort replaces the default rather than being added to it. The default decides
// what silence means and nothing else; a client that named an ordering said
// what it wanted.
func TestExplicitSortReplacesTheDefault(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: [][]any{postRow("p1", "Hello")}})
	opts := postOptions()
	opts.DefaultSort = []string{"-status"}
	api := mount(t, db.db, opts)

	if resp := api.Get("/posts?sort=title"); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	stmt := db.lastStatement()
	_, order, _ := strings.Cut(stmt, "ORDER BY ")
	if !strings.HasPrefix(order, `"title" ASC`) {
		t.Errorf("the request's ordering did not reach the statement:\n%s", stmt)
	}
	if strings.Contains(order, `"status"`) {
		t.Errorf("the default was appended to an explicit sort:\n%s", stmt)
	}
}

// A default naming something this resource cannot sort by is a startup failure.
//
// The alternative is the one thing a resource must not do: answer 400 to a
// client that sent nothing at all. Each rejection names the columns that would
// have been accepted, as every other refusal in this package does.
func TestDefaultSortIsCheckedAtMount(t *testing.T) {
	tests := []struct {
		name, term, want string
	}{
		{"unknown column", "-nonesuch", "has no such column"},
		{"not sortable", "excerpt", "does not declare that column Sortable"},
		{"hidden", "secret", "has no such column"},
		{"bad direction", "title.sideways", "unknown sort direction"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeDB(t)
			opts := postOptions()
			opts.DefaultSort = []string{tc.term}

			_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
			err := rest.Resource[Post, PostCreate, PostUpdate](api, sqlb.New(db.db), opts)
			if err == nil {
				t.Fatalf("DefaultSort %q was accepted", tc.term)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say what is wrong (%q missing): %v", tc.want, err)
			}
		})
	}
}

// A narrowed resource cannot order by a column it does not serve. Columns is a
// disclosure boundary (#148), so a default reaching through it would read the
// column on every request of a resource narrowed to keep it away.
func TestDefaultSortRespectsTheResourcesSurface(t *testing.T) {
	db := newFakeDB(t)
	opts := postOptions()
	opts.Columns = []string{"id", "title"}
	opts.DefaultSort = []string{"-status"}

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Resource[Post, PostCreate, PostUpdate](api, sqlb.New(db.db), opts)
	if err == nil {
		t.Fatal("a DefaultSort outside Columns was accepted")
	}
	if !strings.Contains(err.Error(), "has no such column") {
		t.Errorf("error = %v, want it to treat the column as absent", err)
	}
}
