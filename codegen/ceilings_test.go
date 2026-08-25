package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// ceilingFixture declares all five per-resource cost ceilings, which is the
// point: three of them were emitted and two were silently dropped, so a fixture
// that sets only the new pair would not show that the old three still travel
// (#151).
func ceilingFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("products",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
		schema.Text("sku").Filterable(),
	).Expose(schema.REST{
		Ops:             schema.OpRead | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
		MaxFilters:      12,
		MaxSortTerms:    3,
		MaxOffset:       10000,
		// Not a ceiling, and here anyway: it travels the same road as the five
		// above — declaration to mount to exit — and it is the one of the six
		// whose absence is invisible in a response (#165).
		DefaultSort: []string{"-name"},
	})
	return r
}

// All five ceilings reach the mount. Before this, MaxSortTerms and MaxOffset had
// no spelling in schema.REST at all, so a schema-first resource took the package
// default — MaxOffset = 100_000, two to four orders of magnitude above what any
// particular table wants.
func TestGeneratedRegisterCarriesEveryCostCeiling(t *testing.T) {
	src := generate(t, ceilingFixture())["rest_gen.go"]

	for _, want := range []string{
		"DefaultPageSize: 25",
		"MaxPageSize: 100",
		"MaxFilters: 12",
		"MaxSortTerms: 3",
		"MaxOffset: 10000",
		`DefaultSort: []string{"-name"}`,
	} {
		if !contains(src, want) {
			t.Errorf("registration missing %q:\n%s", want, src)
		}
	}
}

// A ceiling left at zero emits nothing, so the mount takes the package default
// rather than being pinned to a number the schema never said.
func TestUndeclaredCeilingsAreNotEmitted(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("products",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	).Expose(schema.REST{Ops: schema.OpList})

	src := generate(t, r)["rest_gen.go"]
	for _, unwanted := range []string{"MaxSortTerms:", "MaxOffset:"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("an undeclared ceiling should not be emitted, found %q:\n%s", unwanted, src)
		}
	}
}

// The exit carries them too, and this is where the old gap was widest: the
// emitter wrote a literal 0 for MaxSortTerms — so a declared value was dropped
// on the way out — and had no MaxOffset field at all, which meant the ejected
// handlers served ?page=50000000 while the API they replaced refused it.
func TestEjectedLimitsCarryEveryCostCeiling(t *testing.T) {
	files := eject(t, ceilingFixture())

	want := "var productLimits = Limits{DefaultPageSize: 25, MaxPageSize: 100, " +
		"MaxFilters: 12, MaxSortTerms: 3, MaxOffset: 10000, " +
		`DefaultSort: []Order{{Column: "name", Desc: true}}}`
	if !contains(files["handlers.go"], want) {
		t.Errorf("ejected limits missing %q:\n%s", want, files["handlers.go"])
	}
	// And the parser has to act on the fields, not merely hold them.
	if !contains(files["support.go"], "starts past the offset budget of") {
		t.Errorf("the ejected list parser does not enforce an offset budget:\n%s", files["support.go"])
	}
	if !contains(files["support.go"], "lim.DefaultSort...") {
		t.Errorf("the ejected list parser ignores the declared ordering:\n%s", files["support.go"])
	}
}
