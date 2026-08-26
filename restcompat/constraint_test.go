package restcompat_test

import (
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// A declared format rule is part of the contract, and tightening one rejects
// input that worked (#311).
//
// It is the shape a type-keyed diff cannot see: the column stays `varchar(64)`
// and the pattern under it goes from absent to `^[a-z]+$`, so every check that
// compares types, sizes and enum sets reports nothing while every request
// carrying a capital letter starts failing. That is the class ADR-0016 exists
// for — a clean migration and a broken client.

// constrained builds a one-table registry whose title carries the given rules,
// so a test can state a before and an after differing in exactly one bound.
func constrained(pattern string, min, max *float64) *schema.Registry {
	r := schema.NewRegistry()
	title := schema.Varchar("title", 64)
	if pattern != "" {
		title = title.Pattern(pattern)
	}
	count := schema.Int("count")
	if min != nil {
		count = count.Min(*min)
	}
	if max != nil {
		count = count.Max(*max)
	}
	r.Table("posts", schema.UUIDv7("id").PrimaryKey(), title, count).
		Expose(schema.REST{Ops: schema.OpCreate | schema.OpRead | schema.OpList})
	return r
}

func f(v float64) *float64 { return &v }

func TestGainingAPatternIsAWriteBreak(t *testing.T) {
	breaks := restcompat.Diff(constrained("", nil, nil), constrained("^[a-z]+$", nil, nil))
	assertBreaking(t, breaks, restcompat.FacetCreate, "title")
	if !mentions(breaks, "now 422s") {
		t.Errorf("the break should say what happens to a request that worked:\n%s", render(breaks))
	}
}

// Dropping one is not a break but is not neutral either: a generated client
// still enforcing the old rule refuses input the server now accepts, which is a
// stale client rather than a broken contract. Reported as unknown rather than
// guessed either way, which is ADR-0016's rule.
func TestDroppingAPatternIsReportedRatherThanIgnored(t *testing.T) {
	breaks := restcompat.Diff(constrained("^[a-z]+$", nil, nil), constrained("", nil, nil))
	if len(breaks) == 0 {
		t.Fatal("dropping a declared pattern was reported as no change at all")
	}
	if !mentions(breaks, "still enforcing the old one") {
		t.Errorf("the report should say what a stale client does:\n%s", render(breaks))
	}
}

// A bound moving in the rejecting direction is the same break as a pattern
// arriving; moving the other way is the same stale-client report. Both bounds
// share one classifier, so this also pins that they cannot disagree about
// which direction is which.
func TestTighteningABoundIsAWriteBreakAndLooseningIsNot(t *testing.T) {
	tightMin := restcompat.Diff(constrained("", f(0), nil), constrained("", f(10), nil))
	assertBreaking(t, tightMin, restcompat.FacetCreate, "count")
	if !mentions(tightMin, "tightened its minimum") {
		t.Errorf("a raised minimum should be named as a tightening:\n%s", render(tightMin))
	}

	tightMax := restcompat.Diff(constrained("", nil, f(100)), constrained("", nil, f(10)))
	assertBreaking(t, tightMax, restcompat.FacetCreate, "count")
	if !mentions(tightMax, "tightened its maximum") {
		t.Errorf("a lowered maximum should be named as a tightening:\n%s", render(tightMax))
	}

	// The opposite directions, which must not be breaking — otherwise every
	// relaxation would fail the gate and the gate would stop being believed.
	for _, loosened := range []struct {
		name  string
		diffs []restcompat.Break
	}{
		{"lowered minimum", restcompat.Diff(constrained("", f(10), nil), constrained("", f(0), nil))},
		{"raised maximum", restcompat.Diff(constrained("", nil, f(10)), constrained("", nil, f(100)))},
	} {
		for _, b := range loosened.diffs {
			if b.Level == restcompat.LevelBreaking {
				t.Errorf("%s should not be breaking: %s", loosened.name, render(loosened.diffs))
			}
		}
	}
}

// A rule merely restated is silent. Without this every regeneration of an
// unchanged schema would report a delta, which is the fastest way to make a
// contract gate ignored.
func TestAnUnchangedConstraintIsNotADiff(t *testing.T) {
	same := restcompat.Diff(
		constrained("^[a-z]+$", f(0), f(10)),
		constrained("^[a-z]+$", f(0), f(10)),
	)
	if len(same) != 0 {
		t.Errorf("an unchanged constraint should produce no diff:\n%s", render(same))
	}
}
