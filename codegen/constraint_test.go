package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A declared format rule reaches the generated request bodies as the struct
// tags Huma already reads (#311).
//
// The point is not the tag but where the tag goes: Huma turns it into
// validation on the server and into the OpenAPI document, so a caller with no
// compile step can discover the rule instead of learning it from a rejected
// request. A regexp in a transition func reaches neither.

func constraintFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`),
		schema.Int("age").Min(0).Max(150),
		schema.Numeric("score", 5, 2).Min(0.5),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddAction(schema.Action{
			Name: "set-pin",
			Body: schema.Body(schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`)),
		}).
		AddQuery(schema.Query{
			Name:   "by-age",
			Params: schema.Body(schema.Int("at_least").Min(0)),
		})
	return r
}

func TestConstraintsReachEveryRequestBody(t *testing.T) {
	src := generate(t, constraintFixture())["rest_gen.go"]

	for _, want := range []string{
		// The create body: required, so the value is the field itself.
		`json:"pin" pattern:"^[0-9]{4}$"`,
		// The patch body, where every field is a pointer and the rule still
		// applies to the value when one is sent.
		`json:"pin,omitempty" pattern:"^[0-9]{4}$"`,
		// Bounds, rendered short rather than as 0e+00.
		`minimum:"0" maximum:"150"`,
		`minimum:"0.5"`,
		// A query parameter, which Huma validates the same way. Neither
		// nullable nor defaulted, so it is required — huma treats a query
		// parameter as optional otherwise, and a read that cannot answer
		// without one should say so rather than receive a zero.
		`query:"at_least" required:"true" minimum:"0"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated bodies missing %q:\n%s", want, src)
		}
	}
}

// A bound is written the way it was declared. Without this, 0 renders as
// 0e+00 and 150 as 1.5e+02 — valid tags Huma parses, and a diff on every
// regeneration for anyone reading the file.
func TestABoundIsRenderedInItsShortestForm(t *testing.T) {
	src := generate(t, constraintFixture())["rest_gen.go"]
	for _, unwanted := range []string{"e+0", "0.00000", "150.0"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("a bound is rendered in exponent or padded form (%q):\n%s", unwanted, src)
		}
	}
}

// A column that declares no rule carries no tag, so the change is invisible to
// every schema that does not use it. Without this the assertions above are
// satisfied by tagging everything, which would put `minimum:"0"` on every
// integer in every body.
func TestAValueWithNoDeclaredRuleCarriesNoConstraintTag(t *testing.T) {
	src := generate(t, constraintFixture())["rest_gen.go"]
	body := src[strings.Index(src, "type ChildrenCreate struct {"):strings.Index(src, "func (c ChildrenCreate) Row()")]
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, `json:"name"`) {
			continue
		}
		for _, unwanted := range []string{"pattern:", "minimum:", "maximum:"} {
			if strings.Contains(line, unwanted) {
				t.Errorf("an unconstrained column carries %s: %s", unwanted, line)
			}
		}
	}
}
