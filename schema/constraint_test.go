package schema_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The format rules a value may declare (#311).
//
// The case is the reported one: a four-digit PIN. `Varchar("pin", 4)` carries
// maxLength and nothing else, so "exactly four, all digits" had to live as a
// regexp in the transition func — where it reaches no emitter, and a caller
// with no compile step cannot discover it without sending a bad request. Moving
// a verb onto the generated path therefore lost validation the hand-written
// version had, which is a regression in the direction the library is pulling.

func TestAValidPatternAndBoundsAreAccepted(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Varchar("pin", 4).Pattern(`^[0-9]{4}$`),
		schema.Int("age").Min(0).Max(150),
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("a well-formed pattern and bounds should validate: %v", err)
	}
}

// A pattern that does not compile is a schema that refuses every request the
// moment it mounts, with the document advertising a field that can be sent.
// Caught where it is fixable rather than at the first request.
func TestAPatternThatDoesNotCompileIsRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Varchar("pin", 4).Pattern("^[0-9"),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("an uncompilable pattern was accepted")
	}
	for _, want := range []string{"pin", "does not compile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

// Each constraint is refused where it would silently check nothing, rather than
// being ignored — the same rule the body validator already applies to a
// capability a property cannot claim.
func TestAConstraintOnATypeThatCannotCarryItIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field schema.FieldSpec
		want  string
	}{
		{"pattern on an int", schema.Int("count").Pattern("^[0-9]+$"), "requires a text value"},
		{"bounds on text", schema.Text("title").Min(1), "require a numeric value"},
		{"pattern on an enum", schema.Enum("status", "a", "b").Pattern("^a$"), "already the value set"},
		{"pattern on a text array", schema.Text("tags").Array().Pattern("^x$"), "rather than its elements"},
		{"min above max", schema.Int("count").Min(10).Max(1), "no value can satisfy both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := schema.NewRegistry()
			r.Table("things", schema.UUIDv7("id").PrimaryKey(), tc.field)
			err := r.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error should say %q: %v", tc.want, err)
			}
		})
	}
}

// One vocabulary, one validator. A rule that fired for a column and not for an
// action's body would be exactly the drift the shared vocabulary exists to
// prevent — and the action body is the declaration the issue is about.
func TestADeclaredBodyGetsTheSameConstraintRules(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.Reads}).
		AddAction(schema.Action{
			Name: "set-pin",
			Body: schema.Body(schema.Int("pin").Pattern("^[0-9]{4}$")),
		})
	err := r.Validate()
	if err == nil {
		t.Fatal("a body property carrying a pattern on an int was accepted")
	}
	for _, want := range []string{"set-pin", "pin", "requires a text value"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}
