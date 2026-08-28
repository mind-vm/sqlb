package codegen_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A declared read's parameters bind off the query string, which is a narrower
// place than a request body and refuses two things the body vocabulary allows.
//
// This was not a design question answered here — it was a panic. huma rejects
// a pointer on a query parameter at Register, so a schema whose declared read
// carried one optional parameter brought the server down at mount rather than
// serving a route. The generator had copied the body's rule, where a pointer
// is what distinguishes omitted from zero and is right.
func queryParamFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	).
		Expose(schema.REST{Ops: schema.OpList}).
		AddQuery(schema.Query{
			Name: "overdue",
			Params: schema.Body(
				schema.Timestamp("as_of"),
				schema.UUID("list_id").Nullable(),
				schema.Int("limit").Default(schema.Value(20)),
			),
		})
	return r
}

func TestAQueryParameterIsNeverAPointer(t *testing.T) {
	src := generate(t, queryParamFixture())["rest_gen.go"]

	for _, unwanted := range []string{"*string `query", "*int `query", "*time.Time `query"} {
		if contains(src, unwanted) {
			t.Errorf("a query parameter is a pointer, which huma refuses at Register (%q):\n%s", unwanted, src)
		}
	}
}

// The distinction the pointer used to carry has to go somewhere, and `required`
// is where: a parameter the read cannot answer without earns a 422 naming it,
// and one that may be omitted arrives as its zero value.
func TestOnlyAnUnomittableQueryParameterIsRequired(t *testing.T) {
	src := generate(t, queryParamFixture())["rest_gen.go"]

	for _, want := range []string{
		"AsOf   time.Time `query:\"as_of\" required:\"true\"`",
		// Nullable, so it may be omitted and arrives as "".
		"ListID string    `query:\"list_id\"`",
		// Defaulted, so it may be omitted too — the same rule a create body
		// applies, reached through optionalOnCreate.
		"Limit  int32     `query:\"limit\"`",
	} {
		if !contains(src, want) {
			t.Errorf("query parameters missing %q:\n%s", want, src)
		}
	}
}
