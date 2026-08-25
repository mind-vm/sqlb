package restcompat_test

import (
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// What a verb answers with is part of its contract (#312), and the reader side
// of a diff is the mirror image of the writer side: a property leaving a
// response breaks the client that reads it, and a value set that grows breaks
// the client that switched on it.

func grade(returns ...schema.FieldSpec) schema.Action {
	return schema.Action{Name: "grade", Returns: schema.Result(returns...)}
}

// Acquiring a result changes the response type, not one of its properties: a
// deployed client holds a function typed to return the row.
func TestAcquiringAResultIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(withActions(schema.Action{Name: "grade"}), withActions(grade(schema.Int("score"))))

	b := find(t, breaks, "now answers with a declared result")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Facet != restcompat.FacetAction || b.Field != "grade" {
		t.Errorf("break is at %s.%s, want the action itself", b.Facet, b.Field)
	}
}

func TestLosingAResultIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(withActions(grade(schema.Int("score"))), withActions(schema.Action{Name: "grade"}))

	if b := find(t, breaks, "no longer answers with a declared result"); b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
}

// The direction that separates a response from a request: losing a property
// breaks the reader, and gaining one does not.
func TestAResultPropertyRemovedIsBreakingAndAddedIsAdditive(t *testing.T) {
	removed := restcompat.Diff(
		withActions(grade(schema.Int("score"), schema.Text("grade"))),
		withActions(grade(schema.Int("score"))),
	)
	b := find(t, removed, "result property removed")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Field != "grade.result.grade" {
		t.Errorf("field = %q, want the verb and the property", b.Field)
	}

	added := restcompat.Diff(
		withActions(grade(schema.Int("score"))),
		withActions(grade(schema.Int("score"), schema.Text("grade"))),
	)
	if b := find(t, added, "result property added"); b.Level != restcompat.LevelAdditive {
		t.Errorf("level = %s, want additive", b.Level)
	}
}

// A property that may now be null breaks a client whose type says it cannot.
func TestAResultPropertyBecomingNullableIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(
		withActions(grade(schema.Text("grade"))),
		withActions(grade(schema.Text("grade").Nullable())),
	)
	if b := find(t, breaks, "may now be null"); b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
}

// The inversion that is easiest to get wrong. On a request, adding an accepted
// value is safe and removing one is not; on a response it is the other way
// round, because the client is the side with the closed type.
func TestAResultEnumWideningIsBreakingAndNarrowingIsNot(t *testing.T) {
	widened := restcompat.Diff(
		withActions(grade(schema.Enum("outcome", "pass", "fail"))),
		withActions(grade(schema.Enum("outcome", "pass", "fail", "pending"))),
	)
	if b := find(t, widened, "result property values changed"); b.Level != restcompat.LevelBreaking {
		t.Errorf("widening a returned value set: level = %s, want breaking", b.Level)
	}

	narrowed := restcompat.Diff(
		withActions(grade(schema.Enum("outcome", "pass", "fail", "pending"))),
		withActions(grade(schema.Enum("outcome", "pass", "fail"))),
	)
	if b := find(t, narrowed, "result property values changed"); b.Level != restcompat.LevelAdditive {
		t.Errorf("narrowing a returned value set: level = %s, want additive", b.Level)
	}
}

// A verb that declares no result records none, so a baseline taken before this
// existed reads as what it was.
func TestNoResultIsNoContract(t *testing.T) {
	snap := restcompat.Capture(withActions(schema.Action{Name: "grade"}))
	if got := snap.Resources[0].Actions[0].Returns; len(got) != 0 {
		t.Errorf("recorded %v for a verb that declares nothing", got)
	}
	if breaks := restcompat.Diff(withActions(grade(schema.Int("score"))), withActions(grade(schema.Int("score")))); len(breaks) != 0 {
		t.Errorf("a schema compared with itself reports %v", breaks)
	}
}
