package rest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// The reported shape of #308, which is what makes the obligation worth its cost.
//
// Two resources on the same confined model. One exposes a PATCH and so has a
// BeforeUpdate registered for it; the other exposes no update at all — only a
// verb. Both verbs declare a write set, and before the fix both mounted.
//
// The first was safe by accident: a hook registered for a *different route*
// happened to cover it. The second was not, and the write landed. What follows
// asserts that the difference between the two is no longer luck — the resource
// with no write rule is refused, and the refusal names the hook to register.

// familyOptions is the resource with no update op: exactly the shape that had
// no reason to have a BeforeUpdate and therefore did not.
func familyOptions() rest.Options {
	return rest.Options{Path: "/family", Name: "family", Ops: rest.OpRead}
}

func setPinSpec() rest.ActionSpec {
	return rest.ActionSpec{
		Name: "set-parent-pin", Path: "/family/{id}/set-parent-pin",
		Field: "SetParentPin", Writes: []string{"title"},
	}
}

func TestAWritingActionIsRefusedWhenOnlyTheReadRuleExists(t *testing.T) {
	// The registry a resource exposing no update would plausibly carry: the
	// read is confined, and nothing else was ever needed.
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil })
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	err := rest.Action[Scoped, CompletePost](api, db, familyOptions(), setPinSpec(),
		func(context.Context, *Scoped, CompletePost) error { return nil })
	if err == nil {
		t.Fatal("a verb writing a confined row mounted with no write rule; this is the escalation in #308")
	}
	// The diagnostic has to name the hook and say why, because the author's
	// mental model is "the fetch is confined, so the route is covered" — which
	// is true of confinement and silent about authorisation.
	for _, want := range []string{
		"BeforeUpdate",
		"org_id is Scoped",
		"set-parent-pin",
		`writes "title"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q:\n%v", want, err)
		}
	}
	// And it should not describe a surface that is not mounted. This resource
	// exposes read only; the update obligation comes from the write set, not
	// from an exposed PATCH, so claiming it "exposes update" would send the
	// reader looking for one.
	if strings.Contains(err.Error(), "exposes read|update") {
		t.Errorf("the headline describes operations this resource does not expose:\n%v", err)
	}
}

// The accidental-safety case, now deliberate: the same verb mounts once the
// write rule exists, whatever else the resource exposes.
func TestAWritingActionMountsOnceTheWriteRuleExists(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[Scoped](reg).BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil })
	sqlb.On[Scoped](reg).BeforeUpdate(func(context.Context, *sqlb.Update[Scoped]) error { return nil })
	db := sqlb.New(newFakeDB(t).db).WithHooks(reg)

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Action[Scoped, CompletePost](api, db, familyOptions(), setPinSpec(),
		func(context.Context, *Scoped, CompletePost) error { return nil }); err != nil {
		t.Fatalf("mounting with both rules registered: %v", err)
	}
}

// An unconfined model owes nothing either way. The obligation is a consequence
// of the declaration, not a tax on actions — without this, adding a write set
// to a verb on an ordinary table would start demanding a hook that has nothing
// to enforce.
func TestAWritingActionOnAnUnconfinedModelObligesNothing(t *testing.T) {
	db := sqlb.New(newFakeDB(t).db).WithHooks(sqlb.NewRegistry())

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	spec := completeSpec()
	spec.Writes = []string{"status"}
	if err := rest.Action[Post, CompletePost](api, db, postOptions(), spec,
		func(context.Context, *Post, CompletePost) error { return nil }); err != nil {
		t.Fatalf("an unconfined model should owe no hook: %v", err)
	}
}
