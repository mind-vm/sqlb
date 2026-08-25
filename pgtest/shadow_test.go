package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"
)

// TestTheHistoryProducesTheSchemaItClaimsTo is the property that makes a shadow
// database worth building at all.
//
// migrate.Diff needs a current state, and reading production gives the wrong
// one: it reports what the database looks like, not whether the checked-in
// migrations produce it. This replays the history into an empty database and
// asserts the result is the schema those migrations were generated from — so a
// migration that was edited after it ran, or one that never applied cleanly,
// shows up here rather than in the next generated migration.
func TestTheHistoryProducesTheSchemaItClaimsTo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A history with two migrations, because one proves nothing about order.
	v1 := schema.NewRegistry()
	v1.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
	writeMigration(t, dir, "1", "init", diff(t, schema.NewRegistry(), v1))

	v2 := schema.NewRegistry()
	orgs := v2.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		// Added by the second migration; if the two replay out of order the
		// ALTER lands before the CREATE and the whole thing fails.
		//
		// Nullable on purpose. Adding a NOT NULL column with no default is
		// destructive, so it renders commented out — which is a different
		// property, tested separately below.
		schema.Text("slug").Unique().Nullable(),
	)
	v2.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
		schema.Text("title"),
	).Index("org_id")
	writeMigration(t, dir, "2", "posts", diff(t, v1, v2))

	db := freshDB(t)

	replayed, report, res, err := shadow.Build(context.Background(), db, shadow.Options{Dir: dir})
	if err != nil {
		t.Fatalf("shadow.Build: %v", err)
	}
	if !report.Empty() {
		t.Fatalf("the replayed schema uses only constructs the DSL can express, but:\n%s", report)
	}
	t.Logf("replayed %d file(s), %d statement(s): %s",
		len(res.Files), res.Statements, strings.Join(res.Files, ", "))

	// The claim: nothing needs to change to get from what the history built to
	// what the schema declares.
	if changes := diff(t, replayed, v2); len(changes) > 0 {
		t.Errorf("the migration history does not produce the schema it was generated from — "+
			"%d change(s) still outstanding:\n%s", len(changes), describe(changes))
	}
}

// TestShadowCatchesDrift is the second thing a shadow database buys, and it
// needs no API of its own: the drift is the diff between what the history
// builds and what the database actually holds.
func TestShadowCatchesDrift(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	declared := schema.NewRegistry()
	declared.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
	writeMigration(t, dir, "1", "init", diff(t, schema.NewRegistry(), declared))

	// Production: the history, plus a column somebody added by hand.
	live := freshDB(t)
	applySchema(t, live, declared)
	mustExec(t, live, `ALTER TABLE orgs ADD COLUMN patched text`)

	// The shadow: the history alone, in a database of its own.
	shadowDB := freshDB(t)
	replayed, _, _, err := shadow.Build(context.Background(), shadowDB, shadow.Options{Dir: dir})
	if err != nil {
		t.Fatalf("shadow.Build: %v", err)
	}

	drift := diff(t, replayed, importRegistry(t, live))
	if len(drift) == 0 {
		t.Fatal("a column added by hand to the live database produced no drift; " +
			"the comparison cannot see a difference, so an empty result elsewhere means nothing")
	}
	if !strings.Contains(describe(drift), "patched") {
		t.Errorf("drift detected but the hand-added column is not named:\n%s", describe(drift))
	}
}

// TestReplayRefusesANonEmptyDatabase. Replaying a history onto tables that
// already exist produces a registry describing neither the history nor the
// database, and every migration generated from it afterwards is computed
// against a state that never existed. Nothing downstream can detect that, which
// is why it is refused here.
func TestReplayRefusesANonEmptyDatabase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	reg := schema.NewRegistry()
	reg.Table("orgs", schema.UUIDv7("id").PrimaryKey())
	writeMigration(t, dir, "1", "init", diff(t, schema.NewRegistry(), reg))

	db := freshDB(t)
	applySchema(t, db, reg) // already has the tables

	_, _, _, err := shadow.Build(context.Background(), db, shadow.Options{Dir: dir})
	if err == nil {
		t.Fatal("replaying onto a populated database was allowed")
	}
	if !strings.Contains(err.Error(), "orgs") {
		t.Errorf("the refusal should name what it found, got: %v", err)
	}
}

// TestReplayHandlesTheConcurrentIndexSplit.
//
// migrate.Unblock produces a file marked `-- +goose NO TRANSACTION`, holding a
// CREATE INDEX CONCURRENTLY that cannot run inside one. Replay has to notice the
// directive: Postgres wraps a multi-statement request in an implicit
// transaction, so a shadow that ignored it would fail on exactly the files the
// directive exists for.
func TestReplayHandlesTheConcurrentIndexSplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	base := schema.NewRegistry()
	base.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
	writeMigration(t, dir, "1", "init", diff(t, schema.NewRegistry(), base))

	// Adding a unique constraint is one of the changes Unblock rewrites into a
	// concurrent index build plus an adopting ADD CONSTRAINT.
	target := schema.NewRegistry()
	target.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Unique(),
	)
	changes := migrate.Unblock(diff(t, base, target))
	writeMigration(t, dir, "2", "unique_name", changes)

	db := freshDB(t)
	replayed, _, res, err := shadow.Build(context.Background(), db, shadow.Options{Dir: dir})
	if err != nil {
		t.Fatalf("replaying an Unblock-generated history: %v", err)
	}
	t.Logf("replayed %d file(s): %s", len(res.Files), strings.Join(res.Files, ", "))

	if changes := diff(t, replayed, target); len(changes) > 0 {
		t.Errorf("the unblocked history does not produce its target schema:\n%s", describe(changes))
	}
}

// TestADestructiveChangeIsNotReplayed records the sharpest limit on what a
// shadow database can tell you, found by this suite rather than reasoned about.
//
// A destructive change renders commented out, to be reviewed and uncommented
// before it is applied (ADR-0014). So the file in the repository is not the SQL
// that ran: production has the column, and a replay of the checked-in history
// does not. Every such change is permanent drift between the shadow and the
// database, and it looks exactly like the drift this is supposed to detect.
//
// What the file *does* do is apply. It once did not — the unique constraint over
// the commented-out column was emitted live and failed with `column "slug" named
// in key does not exist` — and this test pinned that as a defect in the diff
// engine. The engine now comments out what depends on a commented-out change, so
// the whole migration is the reviewable no-op it was meant to be, and what is
// left here is the limit that cannot be engineered away: replay reproduces the
// history as written, not the database somebody uncommented it into.
func TestADestructiveChangeIsNotReplayed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	base := schema.NewRegistry()
	base.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
	writeMigration(t, dir, "1", "init", diff(t, schema.NewRegistry(), base))

	// Adding a NOT NULL column with no default is destructive: it fails on any
	// table that already has rows. The unique constraint over it is not, and is
	// commented out anyway, because it cannot run until the column exists.
	target := schema.NewRegistry()
	target.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		schema.Text("slug").Unique(),
	)
	writeMigration(t, dir, "2", "slug", diff(t, base, target))

	db := freshDB(t)
	replayed, _, res, err := shadow.Build(context.Background(), db, shadow.Options{Dir: dir})
	if err != nil {
		t.Fatalf("a history whose second migration is entirely commented out should "+
			"still replay; if this names a statement from 2_slug.sql, the diff engine "+
			"has emitted something that depends on the commented-out column: %v", err)
	}
	t.Logf("replayed %d file(s), %d statement(s): %s",
		len(res.Files), res.Statements, strings.Join(res.Files, ", "))

	// The column is not there, and nothing in the history could have put it
	// there. That is the permanent drift.
	if orgs := replayed.Get("orgs"); orgs == nil {
		t.Fatal("the first migration did not replay")
	} else if orgs.Field("slug") != nil {
		t.Fatal("a commented-out ADD COLUMN was applied, which means it was not commented out")
	}

	// So the shadow proposes the destructive change again, forever, against a
	// production database that has already had it applied by hand. A caller
	// reading drift has to recognise this rather than act on it.
	drift := diff(t, replayed, target)
	if len(drift) == 0 {
		t.Fatal("the replayed schema should still differ from the one the history was generated from")
	}
	if !strings.Contains(describe(drift), "slug") {
		t.Errorf("the outstanding change should be the commented-out column:\n%s", describe(drift))
	}
}

// writeMigration renders changes into dir as goose files, the way a project's
// generator would.
func writeMigration(t *testing.T, dir, version, name string, changes []migrate.Change) {
	t.Helper()
	files, err := migrate.Write(dir, migrate.Migration{
		Version: version,
		Name:    name,
		Changes: changes,
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("migrate.Write: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("migrate.Write wrote nothing for %s_%s", version, name)
	}
}
