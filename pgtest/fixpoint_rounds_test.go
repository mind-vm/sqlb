package pgtest

import (
	"context"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// The round trip converges, but not always on the first round — and this is the
// test that says on which one.
//
// Postgres normalises a CHECK over a *varchar* to a different spelling on the
// second application than on the first. The first stores a cast of the array:
//
//	CHECK (((status)::text = ANY ((ARRAY['open'::character varying, …])::text[])))
//
// and fed that back verbatim, it stores a cast of each element instead:
//
//	CHECK (((status)::text = ANY (ARRAY[('open'::character varying)::text, …])))
//
// which is then stable. So `written → A → B → B`: a fixpoint at two iterations
// rather than one. `sqlb survey`'s Phase C compared after one and reported a
// residual for every such constraint on schemas that were entirely clean, which
// reads as "this schema will never be stable under sqlb" about a schema that is
// (issue #136).
//
// The text spelling of the same constraint comes back as an enum column
// (ADR-0017) and settles on the first round, which is why awkwardSchema — whose
// orgs.plan is exactly that — never showed this. The type is the difference.
const varcharCheckSchema = `
CREATE TABLE tickets (
    id     uuid PRIMARY KEY,
    status varchar(20) NOT NULL,
    CONSTRAINT tickets_status_check CHECK (status IN ('open','done'))
);
`

func TestVarcharCheckIsAFixpointAtTwoRounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	source := freshDB(t)
	mustExec(t, source, varcharCheckSchema)

	reg, rep, err := introspect.Registry(ctx, sqlb.New(source), introspect.Options{})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("the fixture is meant to be fully describable, and this was skipped:\n%s", rep)
	}

	// Round 1 is what Phase C used to do and stop at.
	round1 := rebuildAndDiff(t, reg)
	if len(round1) == 0 {
		t.Fatal("a varchar CHECK now settles on the first round. That is good news " +
			"rather than a regression — but this fixture no longer exercises #136, so " +
			"either find a construct Postgres still renormalises on second application " +
			"or retire this test. The bounded loop in survey's Phase C is what depends " +
			"on it, and an assertion that cannot fail is not guarding it.")
	}
	t.Logf("round 1 left %d change(s) — the false residual #136 reported", len(round1))

	// Round 2 renders what round 1 produced. That is the whole fix: compare the
	// database against the declaration *it* yields, not against the one that
	// was rendered into it.
	rebuilt := freshDB(t)
	applyRegistry(t, rebuilt, reg)
	back, _, err := introspect.Registry(ctx, sqlb.New(rebuilt), introspect.Options{})
	if err != nil {
		t.Fatalf("re-introspect: %v", err)
	}
	round2 := rebuildAndDiff(t, back)
	if len(round2) != 0 {
		t.Errorf("the round trip did not settle on the second round either, so this is an "+
			"oscillating spelling rather than a renormalised one:\n%s",
			renderChanges("round 2", round2))
	}
}

// rebuildAndDiff renders a registry into a fresh database and returns what
// re-introspecting it does not account for: one round of the loop.
func rebuildAndDiff(t *testing.T, reg *schema.Registry) []migrate.Change {
	t.Helper()
	db := freshDB(t)
	applyRegistry(t, db, reg)
	back, _, err := introspect.Registry(context.Background(), sqlb.New(db), introspect.Options{})
	if err != nil {
		t.Fatalf("introspect the rebuilt database: %v", err)
	}
	changes, err := migrate.Diff(reg, back)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return changes
}
