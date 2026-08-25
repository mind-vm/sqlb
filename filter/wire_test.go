package filter_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// A model as codegen emits it for a schema declaring WireCase(Camel): the db
// tag keeps the database's name, the json tag and the sqlb `wire:` entry carry
// the spelling the outside world uses.
type CamelArticle struct {
	ID        string `db:"id" json:"id" sqlb:"pk"`
	Title     string `db:"title" json:"title" sqlb:"filter,search,sort"`
	CreatedAt string `db:"created_at" json:"createdAt" sqlb:"type:timestamptz,filter,sort,wire:createdAt"`
	AuthorID  string `db:"author_id" json:"authorId" sqlb:"filter,wire:authorId"`
	Secret    string `db:"internal_note" json:"-" sqlb:"hidden,wire:internalNote"`
}

func (CamelArticle) TableName() string { return "articles" }

func camelOpts(t *testing.T) filter.Options {
	t.Helper()
	return filter.Options{Model: sqlb.ModelOf[CamelArticle]()}
}

// The request names a column the way the wire does, and the SQL names it the
// way the database does. Both, from one declaration (ADR-0036's amendment).
func TestFilterResolvesTheWireSpelling(t *testing.T) {
	q, err := filter.Parse(url.Values{"createdAt": {"gte.2026-01-01"}}, camelOpts(t))
	if err != nil {
		t.Fatalf("?createdAt should be accepted: %v", err)
	}
	sql, _, err := filter.Apply(sqlb.Query[CamelArticle](), q).SQL()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// The database's own name reaches Postgres. The wire spelling must not.
	if !strings.Contains(sql, `"created_at"`) {
		t.Errorf("the SQL does not name the column: %s", sql)
	}
	if strings.Contains(sql, "createdAt") {
		t.Errorf("the wire spelling leaked into SQL: %s", sql)
	}
}

// The other half: once a schema has chosen camelCase, the column's own name is
// not a parameter. Two spellings answering is the failure ADR-0036 forbids.
func TestFilterRefusesTheColumnSpelling(t *testing.T) {
	_, err := filter.Parse(url.Values{"created_at": {"gte.2026-01-01"}}, camelOpts(t))
	if err == nil {
		t.Fatal("?created_at must not be accepted alongside ?createdAt")
	}
	// And the rejection lists what the caller *can* type, which is the whole
	// point of listing anything. The message echoes the offending parameter as
	// well, so the check is scoped to the allowed set rather than to the whole
	// string — echoing what was sent is correct and is not the thing under test.
	_, allowed, found := strings.Cut(err.Error(), "allowed: ")
	if !found {
		t.Fatalf("the rejection lists nothing a caller could use instead:\n%v", err)
	}
	if !strings.Contains(allowed, "createdAt") {
		t.Errorf("the allowed set omits the accepted spelling: %q", allowed)
	}
	if strings.Contains(allowed, "created_at") {
		t.Errorf("the allowed set offers a spelling that does not work: %q", allowed)
	}
}

// ?sort and ?select speak the same vocabulary as the filter, because they name
// columns too.
func TestSortAndSelectUseTheWireSpelling(t *testing.T) {
	q, err := filter.Parse(url.Values{
		"sort":   {"-createdAt"},
		"select": {"id,createdAt"},
	}, camelOpts(t))
	if err != nil {
		t.Fatalf("sort and select should take the wire spelling: %v", err)
	}
	if len(q.Order) != 1 {
		t.Fatalf("Order = %v", q.Order)
	}
	if !containsString(q.Select, "created_at") {
		t.Errorf("Select should carry the database's name, got %v", q.Select)
	}

	if _, err := filter.Parse(url.Values{"sort": {"-created_at"}}, camelOpts(t)); err == nil {
		t.Error("?sort must not accept the column spelling")
	}
}

// A hidden column has no spelling at all, in either case, and the tag it
// carries must not turn into one.
func TestHiddenColumnHasNoWireSpelling(t *testing.T) {
	for _, name := range []string{"internalNote", "internal_note"} {
		if _, err := filter.Parse(url.Values{name: {"eq.x"}}, camelOpts(t)); err == nil {
			t.Errorf("?%s reached a hidden column", name)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
