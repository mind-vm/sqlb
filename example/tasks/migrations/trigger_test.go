// A column the database owns, and what the schema knows about it.
//
// The history this package replays installs seven triggers: six that keep
// updated_at true, and one that reconciles tasks.completed_at with the check
// constraint requiring it. None of them can be declared in the DSL, and this is
// the test that says what happens to them — because nothing else does, and
// "nothing says so" is the whole adoption question for any database older than
// a week.
//
// The answer is that they are *invisible* rather than dropped. migrate.Diff is
// a pure function over two registries and a registry has no concept of a
// trigger, so a diff neither creates one, drops one, nor mentions one. That is
// the right default — a tool that proposed dropping every construct it cannot
// describe would be unusable against a real database — and it has a
// consequence that belongs in a test rather than in somebody's memory: DDL
// rendered from an introspected registry rebuilds the tables without them.
//
// See docs/special-cases-subject-go.md §2. The suggestion there is `Managed()`,
// a column marker meaning *the database writes this* — implying ReadOnly,
// excluding the column from create and patch bodies, and making the write-back
// after a mutation the documented refresh path rather than an accident. This
// test is the half of that which needs no new feature: stating the current
// behaviour, and failing if it changes.

package migrations_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/migrate"
	"github.com/jryannel/sqlb/schema"
	"github.com/jryannel/sqlb/shadow"
	"github.com/jryannel/sqlb/sqlbtest"
)

// triggers are the ones the history installs and the DSL cannot describe.
// Listed rather than counted, so that a trigger disappearing from the history
// fails here instead of quietly making this test pass against nothing.
var triggers = []string{
	"workspaces_touch_updated_at",
	"users_touch_updated_at",
	"memberships_touch_updated_at",
	"lists_touch_updated_at",
	"tasks_touch_updated_at",
	"comments_touch_updated_at",
	"tasks_sync_completed_at",
}

func TestATriggerIsInvisibleToTheDiff(t *testing.T) {
	ctx := context.Background()
	// A database of this test's own: shadow.Build refuses one that already has
	// tables in it, and drift_test.go has already replayed into the shared one.
	db := freshDatabase(t)

	current, report, res, err := shadow.Build(ctx, db, shadow.Options{Dir: "."})
	if err != nil {
		t.Fatalf("replaying the migration history: %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatal("the replay applied no files, so everything below is checking an empty database")
	}

	// The premise. Without this the rest of the test passes against a database
	// that has no triggers in it, which is the failure mode a test like this
	// is most likely to rot into.
	installed := map[string]bool{}
	rows, err := db.Query(ctx, `
		SELECT tgname FROM pg_trigger WHERE NOT tgisinternal`)
	if err != nil {
		t.Fatalf("listing triggers: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning a trigger name: %v", err)
		}
		installed[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing triggers: %v", err)
	}
	for _, name := range triggers {
		if !installed[name] {
			t.Fatalf("the history no longer installs %s, so this test is about nothing", name)
		}
	}

	// Introspection does not read them, and does not report them either. The
	// distinction matters: introspect.Report is how a construct the DSL cannot
	// express announces itself — the two composite foreign keys in this history
	// are in there — and a trigger is not in that list. It is not skipped. It
	// is unseen.
	text := report.String()
	for _, name := range triggers {
		if strings.Contains(text, name) {
			t.Errorf("introspection now reports %s. That is an improvement and this test is "+
				"the wrong shape for it:\n%s", name, text)
		}
	}

	target := schema.DefaultRegistry()

	// The diff a `sqlb migrate` run would compute. It must not propose creating
	// a trigger, because it cannot; it must not propose dropping one, because
	// the registry it is diffing against does not know one exists.
	//
	// Only trigger statements are asserted on here. Whether the diff is *empty*
	// is drift_test.go's question, and answering it requires normalising the
	// declared check expressions into Postgres's spelling first — which mutates
	// the shared registry, and is exactly why that test is careful about it.
	changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("diffing the history against the declaration: %v", err)
	}
	assertNoTriggerStatements(t, "the diff against the declaration", changes)

	// And the direction that has the consequence. This is what `sqlb migrate`
	// renders for a database adopted by introspection: every table, from
	// nothing. It is complete DDL, it applies cleanly, and the triggers are not
	// in it — so a schema rebuilt from an introspected registry has lost the
	// only thing keeping updated_at true, with no diagnostic anywhere.
	fromNothing, err := migrate.Diff(schema.NewRegistry(), current, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("diffing an empty registry against the replayed schema: %v", err)
	}
	if len(fromNothing) == 0 {
		t.Fatal("creating the replayed schema from nothing produced no statements at all")
	}
	assertNoTriggerStatements(t, "the create-from-nothing diff", fromNothing)
}

// The other half of the same fact, from the application's side: the database
// writes the column, so a row read before a write is stale after it, and no
// declaration says so.
//
// updated_at is ReadOnly in taskschema — Timestamps() declares it — which keeps
// it out of the generated create and patch bodies and is the whole of what the
// schema knows. ReadOnly is a statement about the generated surface, not about
// the column: SQL can still write it, this test does, and the trigger overrules
// it on the next update. What is missing is the marker that says *the database
// maintains this*, which is what would make the write-back after a mutation the
// documented way to refresh rather than a convenience.
func TestTheDatabaseOverrulesAValueGoWrote(t *testing.T) {
	ctx := context.Background()
	db := freshDatabase(t)

	if _, _, _, err := shadow.Build(ctx, db, shadow.Options{Dir: "."}); err != nil {
		t.Fatalf("replaying the migration history: %v", err)
	}

	// An updated_at nobody would mistake for a default, so that the assertions
	// below cannot pass on clock resolution.
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var id string
	var written time.Time
	if err := db.QueryRow(ctx, `
		INSERT INTO workspaces (name, slug, updated_at) VALUES ('Acme', 'acme', $1)
		RETURNING id, updated_at`, stale).Scan(&id, &written); err != nil {
		t.Fatalf("inserting a workspace: %v", err)
	}
	if !written.UTC().Equal(stale) {
		t.Fatalf("the insert stored updated_at as %s, want the %s it was given — a BEFORE INSERT "+
			"trigger has appeared on workspaces and this test's premise has changed",
			written.UTC(), stale)
	}

	// A write that names one column. The row a caller is holding now has two
	// wrong fields in it, and only one of them was mentioned.
	if _, err := db.Exec(ctx,
		`UPDATE workspaces SET name = 'Acme Inc' WHERE id = $1`, id); err != nil {
		t.Fatalf("updating the workspace: %v", err)
	}

	var after time.Time
	if err := db.QueryRow(ctx,
		`SELECT updated_at FROM workspaces WHERE id = $1`, id).Scan(&after); err != nil {
		t.Fatalf("re-reading the workspace: %v", err)
	}
	if !after.After(written) {
		t.Fatalf("updated_at is still %s after an update that did not name it. The trigger is "+
			"what makes the column true, and it did not fire", after.UTC())
	}
	if time.Since(after) > time.Hour {
		t.Errorf("updated_at is %s, which is not a time this update happened at", after.UTC())
	}
}

func assertNoTriggerStatements(t *testing.T, what string, changes []migrate.Change) {
	t.Helper()
	for _, c := range changes {
		for _, sql := range []string{c.Up, c.Down} {
			upper := strings.ToUpper(sql)
			for _, word := range []string{"TRIGGER", "CREATE FUNCTION", "DROP FUNCTION"} {
				if strings.Contains(upper, word) {
					t.Errorf("%s renders %s:\n%s\n\n"+
						"A diff that can write triggers is a change to what migrate.Diff is, and "+
						"docs/special-cases-subject-go.md §2 argues it should not be one — a diff "+
						"comparing trigger bodies is a harder question than rendering them.",
						what, word, strings.TrimSpace(sql))
				}
			}
		}
	}
}

// freshDatabase creates an empty database in the running container and returns
// a connection to it, dropped when the test ends.
//
// drift_test.go opens the container's own database and replays into it once.
// Every test here needs an empty one of its own, because a replay is only
// meaningful against a database with nothing in it.
// freshDatabase returns a pool for a database of its own.
//
// The name parameter is gone with the eighty lines that needed it: sqlbtest
// derives one from the test, which is what kept these two tests from ever
// running in parallel with each other in the first place.
func freshDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sqlbtest.Fresh(t, sqlbtest.DSN(t, pgEnv, "run `mise run pg-up` first"))
}
