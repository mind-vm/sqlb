package codegen

// #268: the DDL for a drop of an index sqlb built, a drop of one somebody built
// by hand, and the one-time drop the v0.15 upgrade proposes are byte-identical.
// These tests hold the two signals that tell them apart, and — as much as the
// notes themselves — the silence on the ordinary case, since a note that fires
// on every drop is a note nobody reads.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// refSchemas is the shape #259 left behind: a reference whose index the
// database has and the declaration does not claim. indexed says whether the
// side being built declares the index.
func refSchemas(t *testing.T, indexed bool) *schema.Registry {
	t.Helper()
	r := schema.NewRegistry()
	threads := r.Table("threads", schema.UUIDv7("id").PrimaryKey())
	ref := schema.Ref("thread", threads).Nullable()
	if indexed {
		ref = ref.Indexed()
	}
	r.Table("messages", schema.UUIDv7("id").PrimaryKey(), ref, schema.Text("body"))
	return r
}

// dropOf is the single index-drop change diffing current into declared
// produces, with the notes applied.
func dropOf(t *testing.T, current, declared *schema.Registry, dir string) migrate.Change {
	t.Helper()
	changes, err := migrate.Diff(current, declared)
	if err != nil {
		t.Fatal(err)
	}
	changes = noteIndexDrops(changes, current, declared, dir)
	var found []migrate.Change
	for _, c := range changes {
		if c.DroppedIndex() != "" {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one index drop, got %d: %v", len(found), changes)
	}
	return found[0]
}

// writeMigration puts a file in dir. header decides whether it claims to be
// sqlb's, which is the whole of the provenance test.
func writeMigration(t *testing.T, dir, name, body string, header bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if header {
		body = migrate.Header + "\n" + body
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The case #268 actually hit: the index was created by a migration written by
// hand to undo a phantom drop, so no sqlb-generated file ever created it.
func TestIndexDropNamesAnIndexNoSqlbMigrationCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	writeMigration(t, dir, "20260101000000_by_hand.sql",
		`CREATE INDEX CONCURRENTLY "messages_thread_id_idx" ON "messages" ("thread_id");`, false)

	c := dropOf(t, refSchemas(t, true), refSchemas(t, false), dir)
	if !strings.Contains(c.Comment, "no sqlb-generated migration") {
		t.Errorf("a drop of a hand-built index read exactly like a drop of one sqlb "+
			"built:\n%s", c.Comment)
	}
	if !strings.Contains(c.Comment, dir) {
		t.Errorf("the note did not name the directory it looked in:\n%s", c.Comment)
	}
}

// The upgrade artifact: sqlb v0.14 did create this index, in a generated
// migration, so provenance says nothing — and the shape still does, because
// the declaration calls the column a reference and covers it with nothing.
func TestIndexDropNamesTheReferenceItLeavesUncovered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	writeMigration(t, dir, "20260101000000_init.sql",
		`CREATE INDEX CONCURRENTLY "messages_thread_id_idx" ON "messages" ("thread_id");`, true)

	c := dropOf(t, refSchemas(t, true), refSchemas(t, false), dir)
	if strings.Contains(c.Comment, "no sqlb-generated migration") {
		t.Errorf("provenance fired on an index a header-bearing migration created:\n%s", c.Comment)
	}
	if !strings.Contains(c.Comment, ".Indexed()") {
		t.Errorf("the note did not name the one word that keeps the index:\n%s", c.Comment)
	}
	if !strings.Contains(c.Comment, "messages.thread_id") {
		t.Errorf("the note did not name the reference it is about:\n%s", c.Comment)
	}
}

// The ordinary case, and the one that decides whether any of this is worth
// having: the author dropped a declared index on an ordinary column, sqlb
// built it, and there is nothing to say.
func TestIndexDropOnADeclaredIndexSaysNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	writeMigration(t, dir, "20260101000000_init.sql",
		`CREATE INDEX CONCURRENTLY "messages_body_idx" ON "messages" ("body");`, true)

	current := refSchemas(t, false)
	current.Get("messages").Index("body")

	c := dropOf(t, current, refSchemas(t, false), dir)
	if strings.Contains(c.Comment, "note:") {
		t.Errorf("an intended drop of an index sqlb built was annotated, which is how a "+
			"note stops being read:\n%s", c.Comment)
	}
}

// A declaration that replaced the ref's index with one of its own has not lost
// its cover, so the .Indexed() advice would be wrong.
func TestIndexDropIsSilentWhenAnotherIndexStillCoversTheRef(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	writeMigration(t, dir, "20260101000000_init.sql",
		`CREATE INDEX CONCURRENTLY "messages_thread_id_idx" ON "messages" ("thread_id");`, true)

	declared := refSchemas(t, false)
	declared.Get("messages").IndexNamed("messages_thread_covering_idx", "thread_id", "body")

	c := dropOf(t, refSchemas(t, true), declared, dir)
	if strings.Contains(c.Comment, ".Indexed()") {
		t.Errorf("the note told a declaration that already covers the reference to cover "+
			"it again:\n%s", c.Comment)
	}
}

// A commented-out CREATE INDEX is a statement that did not run — most often
// the Down of a drop, rendered as prose above it.
func TestIndexProvenanceIgnoresACommentedOutCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	writeMigration(t, dir, "20260101000000_init.sql",
		`-- CREATE INDEX CONCURRENTLY "messages_thread_id_idx" ON "messages" ("thread_id");`, true)

	c := dropOf(t, refSchemas(t, true), refSchemas(t, false), dir)
	if !strings.Contains(c.Comment, "no sqlb-generated migration") {
		t.Errorf("a CREATE INDEX inside a comment counted as one that ran:\n%s", c.Comment)
	}
}

// No migration directory is not evidence of anything: a project generating its
// history elsewhere has one this cannot see, and a note claiming otherwise
// would be wrong rather than cautious.
func TestIndexProvenanceIsSilentWithoutAMigrationDirectory(t *testing.T) {
	c := dropOf(t, refSchemas(t, true), refSchemas(t, false), "")
	if strings.Contains(c.Comment, "no sqlb-generated migration") {
		t.Errorf("provenance claimed to have read a directory that was never named:\n%s", c.Comment)
	}
	// The shape half does not need the directory and should still fire.
	if !strings.Contains(c.Comment, ".Indexed()") {
		t.Errorf("the shape note needs no migration history and did not fire:\n%s", c.Comment)
	}
}
