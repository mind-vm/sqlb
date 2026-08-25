package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// What a diff says about a difference the reader cannot see.
//
// Issues #24, #56 and #63 were all reported from the same position: a
// declaration is compared with the database as text, Postgres does not hand
// back the text it was given, and so the diff proposes a statement
// indistinguishable from the one already in effect. The fix is shadow.Normalize
// on the declared side, which needs a database and is therefore tested in
// pgtest. What is testable here is the second half — that a caller who skipped
// that step is told what the difference is rather than left to measure both
// sides.

// partial declares the issue's index with the predicate spelled as given.
func partial(where string) *schema.Registry {
	return build(func(r *schema.Registry) {
		r.Table("work_packages",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUIDv7("project_id"),
			schema.Float("latitude").Nullable(),
		).AddIndex(schema.Index{
			Name:    "idx_work_packages_location_by_project",
			Columns: []string{"project_id"},
			Where:   where,
		})
	})
}

const formattingNote = "differs from the existing one only in spacing"

// The report, exactly: the database's spelling on one side, the author's on the
// other. The rebuild is still proposed — the two strings are not equal and this
// package does not guess — but it now carries the reason.
func TestAPredicateDifferingOnlyInFormattingSaysSo(t *testing.T) {
	current := partial("(latitude IS NOT NULL)")
	target := partial("latitude IS NOT NULL")

	changes := diff(t, current, target)
	if len(changes) != 2 {
		t.Fatalf("want a drop and a create, got:\n%s", render(changes))
	}
	create := find(t, changes, "CREATE INDEX")
	if !strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("the create does not explain itself: %q", create.Comment)
	}
	if !strings.Contains(create.Comment, "shadow.Normalize") {
		t.Fatalf("the comment names no way out: %q", create.Comment)
	}
}

// The hint explains a diff; it never decides one. If it started suppressing the
// change it annotates, it would be the paren-stripping fix shadow/normalize.go
// rejects, wearing a comment.
func TestTheFormattingHintDoesNotSuppressTheChange(t *testing.T) {
	changes := diff(t, partial("(latitude IS NOT NULL)"), partial("latitude IS NOT NULL"))
	if len(changes) == 0 {
		t.Fatal("the index was left as it is, and the diff reported nothing")
	}
}

// The case that makes the wording matter. `(a OR b) AND c` and
// `a OR (b AND c)` reduce alike, so the hint fires on two predicates that are
// genuinely different — which is why it claims they differ in parenthesisation
// rather than claiming they are the same predicate. Both statements are true
// here; only the first one stays true.
func TestTheHintIsWordedToSurviveARegroupedPredicate(t *testing.T) {
	current := partial("(latitude IS NOT NULL OR archived) AND active")
	target := partial("latitude IS NOT NULL OR (archived AND active)")

	changes := diff(t, current, target)
	create := find(t, changes, "CREATE INDEX")
	if strings.Contains(create.Comment, "same predicate") {
		t.Fatalf("the comment asserts an equality it cannot know: %q", create.Comment)
	}
	if !strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("the comment should still report what was compared: %q", create.Comment)
	}
}

// A predicate that really changed is not formatting, and saying it might be
// would send the reader looking for a normalisation bug instead of at their own
// edit.
func TestAChangedPredicateGetsNoFormattingHint(t *testing.T) {
	current := partial("(latitude IS NOT NULL)")
	target := partial("longitude IS NOT NULL")

	create := find(t, diff(t, current, target), "CREATE INDEX")
	if strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("a real change was excused as formatting: %q", create.Comment)
	}
}

// Nor is it formatting when the predicate is identical and something else
// moved. The hint is about the one difference a reader cannot see; a column
// list they can.
func TestARebuildForAnotherReasonGetsNoFormattingHint(t *testing.T) {
	current := partial("(latitude IS NOT NULL)")
	target := build(func(r *schema.Registry) {
		r.Table("work_packages",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUIDv7("project_id"),
			schema.Float("latitude").Nullable(),
		).AddIndex(schema.Index{
			Name:    "idx_work_packages_location_by_project",
			Columns: []string{"project_id", "latitude"},
			Where:   "latitude IS NOT NULL",
		})
	})

	create := find(t, diff(t, current, target), "CREATE INDEX")
	if strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("a changed column list was excused as formatting: %q", create.Comment)
	}
}

// An index that is simply new has nothing to be a formatting difference from.
func TestANewPartialIndexGetsNoFormattingHint(t *testing.T) {
	create := find(t, diff(t, nil, partial("latitude IS NOT NULL")), "CREATE INDEX")
	if strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("a new index was annotated as a formatting difference: %q", create.Comment)
	}
}

// The cast Postgres renders onto a literal is the other half of what it adds,
// and it reaches an index predicate the same way it reaches a check.
func TestACastOnlyDifferenceIsAlsoFormatting(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("work_packages",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUIDv7("project_id"),
			schema.Text("status"),
		).AddIndex(schema.Index{
			Name:    "idx_open_work_packages",
			Columns: []string{"project_id"},
			Where:   "(status <> 'done'::text)",
		})
	})
	target := build(func(r *schema.Registry) {
		r.Table("work_packages",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUIDv7("project_id"),
			schema.Text("status"),
		).AddIndex(schema.Index{
			Name:    "idx_open_work_packages",
			Columns: []string{"project_id"},
			Where:   "status <> 'done'",
		})
	})

	create := find(t, diff(t, current, target), "CREATE INDEX")
	if !strings.Contains(create.Comment, formattingNote) {
		t.Fatalf("a cast-only difference was not recognised: %q", create.Comment)
	}
}

// And the whole point of the exercise: once both sides spell the predicate the
// same way — which is what shadow.Normalize arranges — there is no diff at all.
func TestANormalisedPredicateDiffsToNothing(t *testing.T) {
	changes := diff(t, partial("(latitude IS NOT NULL)"), partial("(latitude IS NOT NULL)"))
	if len(changes) != 0 {
		t.Fatalf("a schema diffed against itself produced %d change(s):\n%s",
			len(changes), render(changes))
	}
}

// The same clause on the constraint the family started with. Issue #24 is
// fixed, so a caller of `sqlb migrate` never sees this; a consumer diffing
// against introspect.Registry without normalising still can, and that is who
// filed all three reports.

// guarded declares one hand-written CHECK, spelled as given.
func guarded(expr string) *schema.Registry {
	return build(func(r *schema.Registry) {
		r.Table("tasks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("status"),
			schema.Timestamp("completed_at").Nullable(),
		).Check("done_tasks_have_a_completion_time", expr)
	})
}

const declaredExpr = "status <> 'done' OR completed_at IS NOT NULL"
const storedExpr = "((status <> 'done'::text) OR (completed_at IS NOT NULL))"

func TestACheckDifferingOnlyInFormattingSaysSo(t *testing.T) {
	changes := diff(t, guarded(storedExpr), guarded(declaredExpr))
	add := find(t, changes, "ADD CONSTRAINT")
	if !strings.Contains(add.Comment, formattingNote) {
		t.Fatalf("the add does not explain itself: %q", add.Comment)
	}
	if !strings.Contains(add.Comment, "shadow.Normalize") {
		t.Fatalf("the comment names no way out: %q", add.Comment)
	}
}

// And it still drops and re-adds, for the same reason the index is still
// rebuilt: the comment explains the change, it does not cancel it.
func TestTheCheckHintDoesNotSuppressTheChange(t *testing.T) {
	changes := diff(t, guarded(storedExpr), guarded(declaredExpr))
	if len(changes) == 0 {
		t.Fatal("the check was left as it is, and the diff reported nothing")
	}
}

func TestAChangedCheckGetsNoFormattingHint(t *testing.T) {
	edited := "status <> 'done' AND completed_at IS NOT NULL"
	add := find(t, diff(t, guarded(storedExpr), guarded(edited)), "ADD CONSTRAINT")
	if strings.Contains(add.Comment, formattingNote) {
		t.Fatalf("a real change was excused as formatting: %q", add.Comment)
	}
}

// A check that is simply new has nothing to be a formatting difference from,
// and must keep the clause it had — which the new branch sits directly above in
// the same switch, so this is what says the ordering did not disturb it.
func TestANewCheckKeepsItsOwnClause(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("tasks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("status"),
			schema.Timestamp("completed_at").Nullable(),
		)
	})

	add := find(t, diff(t, current, guarded(declaredExpr)), "ADD CONSTRAINT")
	if strings.Contains(add.Comment, formattingNote) {
		t.Fatalf("a new check was annotated as a formatting difference: %q", add.Comment)
	}
	if !strings.Contains(add.Comment, "existing rows must already satisfy") {
		t.Fatalf("a new check lost the warning about the rows it has to hold for: %q", add.Comment)
	}
}

// A constraint that is not a hand-written CHECK is built from column names on
// both sides rather than carried through as text, so a difference between two
// of those is never formatting — and pointing at shadow.Normalize, which does
// not touch them, would be advice that cannot help.
func TestANonCheckConstraintGetsNoFormattingHint(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("tasks", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("tasks", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug").Unique())
	})

	for _, c := range diff(t, current, target) {
		if strings.Contains(c.Comment, formattingNote) {
			t.Fatalf("a unique constraint was annotated as a formatting difference: %q", c.Comment)
		}
	}
}

// The enum CHECK is the case that looks like it belongs and does not. Its
// expression is rendered from the declared values on both sides, and introspect
// reads the values back out rather than comparing the text (ADR-0017), so a
// difference means the values changed — which has its own, much more useful
// clause.
func TestAnEnumCheckGetsItsOwnClauseRatherThanTheFormattingOne(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("tasks", schema.UUIDv7("id").PrimaryKey(),
			schema.Enum("status", "todo", "doing", "done"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("tasks", schema.UUIDv7("id").PrimaryKey(),
			schema.Enum("status", "todo", "done"))
	})

	add := find(t, diff(t, current, target), "ADD CONSTRAINT")
	if strings.Contains(add.Comment, formattingNote) {
		t.Fatalf("a narrowed enum was excused as formatting: %q", add.Comment)
	}
	if !strings.Contains(add.Comment, "no longer permits") {
		t.Fatalf("the enum clause was lost: %q", add.Comment)
	}
}

func TestANormalisedCheckDiffsToNothing(t *testing.T) {
	changes := diff(t, guarded(storedExpr), guarded(storedExpr))
	if len(changes) != 0 {
		t.Fatalf("a schema diffed against itself produced %d change(s):\n%s",
			len(changes), render(changes))
	}
}
