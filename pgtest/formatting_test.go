package pgtest

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
)

// What a diff says to the caller who did not normalise.
//
// shadow.Normalize is a call the caller makes, deliberately, so that
// migrate.Diff stays a pure function over two registries. A consumer diffing
// against introspect.Registry without making it still gets the diff #24 and #63
// were reported as: a statement identical to the one already in effect, with
// nothing to say the difference is whitespace. These two assert the clause that
// now says it — measured against what Postgres actually stores rather than
// against a hand-written guess at it, which is what the unit tests in migrate
// necessarily are.

func TestAnUnnormalisedCheckIsExplainedRatherThanJustProposed(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	applySchema(t, db, checked(declaredCheck))
	current := importRegistry(t, db)

	// Deliberately not normalised: this is the position all three reports came
	// from.
	changes, err := migrate.Diff(current, checked(declaredCheck))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("this test needs the un-normalised diff it is about, and there was none")
	}

	if !explains(changes, "ADD CONSTRAINT") {
		t.Fatalf("the re-add of an identical check explains nothing:\n%s", describe(changes))
	}
}

func TestAnUnnormalisedPredicateIsExplainedRatherThanJustProposed(t *testing.T) {
	t.Parallel()
	db := freshDB(t)
	applySchema(t, db, partial(declaredPredicate))
	current := importRegistry(t, db)

	changes, err := migrate.Diff(current, partial(declaredPredicate))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("this test needs the un-normalised diff it is about, and there was none")
	}

	if !explains(changes, "CREATE INDEX") {
		t.Fatalf("the rebuild of an identical index explains nothing:\n%s", describe(changes))
	}
}

// explains reports whether the change carrying the given statement says its
// difference is a matter of formatting.
func explains(changes []migrate.Change, statement string) bool {
	for _, c := range changes {
		if strings.Contains(c.Up, statement) && strings.Contains(c.Comment, "only in spacing") {
			return true
		}
	}
	return false
}
