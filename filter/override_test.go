package filter_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// The half of ADR-0035's boundary that points the other way.
//
// An override does not reach the wire, and it *does* reach filter coercion —
// because coercion reads the model's Go type, and the whole point of the
// override is that the model's Go type changed. A request naming an overridden
// column has to parse into that type rather than into a string.
//
// key stands in for uuid.UUID: the root module is stdlib-only, so the test
// declares a type with the shape that matters — encoding.TextUnmarshaler, which
// is the hook Coerce has always used for wrapper types.
type key struct{ v string }

func (k *key) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		return fmt.Errorf("a key cannot be empty")
	}
	if !strings.HasPrefix(string(b), "k_") {
		return fmt.Errorf("a key is prefixed k_, got %q", b)
	}
	k.v = string(b)
	return nil
}

type keyed struct {
	ID    key    `db:"id" sqlb:"pk"`
	Title string `db:"title" sqlb:"filter"`
}

func (keyed) TableName() string { return "keyed" }

func TestCoercionFollowsAnOverriddenType(t *testing.T) {
	values, err := url.ParseQuery("id=eq.k_019")
	if err != nil {
		t.Fatal(err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[keyed]()})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(q.Where) != 1 {
		t.Fatalf("got %d predicates, want 1", len(q.Where))
	}

	_, args, err := filter.Apply(sqlb.Query[keyed]().Select(sqlb.F("id")), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// The bound value is the overridden type, not the string the URL carried.
	// Binding a string here would reach Postgres as text and fail to compare
	// against a uuid column — which is the failure this coercion exists to
	// prevent, and the reason an override has to be visible to it.
	if len(args) != 1 {
		t.Fatalf("args = %v, want one", args)
	}
	if got, ok := args[0].(key); !ok || got.v != "k_019" {
		t.Errorf("bound %#v, want the column's own Go type", args[0])
	}
}

// And the type's own validation is the request's validation, so a malformed
// value is a 400 naming the reason rather than a database error later.
func TestAnOverriddenTypeRejectsItsOwnBadValues(t *testing.T) {
	values, _ := url.ParseQuery("id=eq.nope")
	_, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[keyed]()})
	if err == nil {
		t.Fatal("a value the type refuses was accepted")
	}
	if !strings.Contains(err.Error(), "prefixed k_") {
		t.Errorf("the type's own error should reach the caller, got: %v", err)
	}
}
