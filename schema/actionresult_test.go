package schema_test

import (
	"testing"

	"github.com/jryannel/sqlb/schema"
)

// A verb's declared result (#312): the same vocabulary as its body, travelling
// the other way.

func TestAValidActionResultPasses(t *testing.T) {
	r := tasksWith(schema.Action{
		Name:    "grade",
		Body:    schema.Body(schema.JSON("answers")),
		Returns: schema.Result(schema.Int("score"), schema.Text("grade")),
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("a well-formed result was refused: %v", err)
	}
	if got := r.Get("tasks").Actions()[0].Returns; len(got) != 2 {
		t.Fatalf("the declaration did not survive AddAction: %+v", got)
	}
}

// A result is a value like a body is, so the capabilities that place a column
// in a table are refused there too — and the diagnostic says which of the two
// declarations it is about, since a verb can have both.
func TestActionResultCannotClaimColumnCapabilities(t *testing.T) {
	refusal(t, tasksWith(schema.Action{
		Name:    "grade",
		Returns: schema.Result(schema.Int("score").Filterable()),
	}), `action "grade": result`, "Filterable", "describes a column rather than a declared property")
}

func TestActionResultRefusesADuplicateProperty(t *testing.T) {
	refusal(t, tasksWith(schema.Action{
		Name:    "grade",
		Returns: schema.Result(schema.Int("score"), schema.Int("score")),
	}), "declared twice")
}

// A body and a result may share a property name: they are two objects
// travelling in opposite directions, and `answers` in and `answers` out is a
// verb that echoes what it was given rather than a collision.
func TestABodyAndAResultMayShareAName(t *testing.T) {
	r := tasksWith(schema.Action{
		Name:    "grade",
		Body:    schema.Body(schema.JSON("answers")),
		Returns: schema.Result(schema.JSON("answers")),
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("a body and a result sharing a name was refused: %v", err)
	}
}

// The manifest is what an agent reads before calling a verb, and what a verb
// answers with is the fact that decides what it does next.
func TestActionResultReachesTheManifest(t *testing.T) {
	r := tasksWith(schema.Action{
		Name:    "grade",
		Returns: schema.Result(schema.Int("score"), schema.Text("grade").Nullable()),
	})
	for _, tm := range r.BuildManifest().Tables {
		if tm.Name != "tasks" {
			continue
		}
		got := tm.REST.Actions[0].Returns
		if len(got) != 2 || got[0].Name != "score" || got[0].Type != string(schema.TypeInt) {
			t.Fatalf("the manifest does not describe the result: %+v", got)
		}
		if !got[1].Nullable {
			t.Errorf("the manifest lost the nullability of %+v", got[1])
		}
		return
	}
	t.Fatal("tasks is not in the manifest")
}
