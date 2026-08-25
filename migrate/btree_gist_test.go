package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The extension is ordered ahead of the table it constrains, in the same
// migration — not merely before the app starts. Following the pattern that
// held for the extension diff *does* emit (vector, in vector_test.go) but
// hand-splitting a CREATE EXTENSION into a later migration fails outright on
// a fresh database, because AddExclude's EXCLUDE clause is inline in whatever
// migration first creates the table (issue #194).
func TestBtreeGistExtensionComesFirst(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("coach_id"),
			schema.Timestamp("starts_at"),
			schema.Timestamp("ends_at"),
		).AddExclude(schema.Exclusion{
			Name:     "bookings_no_double_booking",
			Using:    "gist",
			Elements: "coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&",
		})
	})
	changes := diff(t, schema.NewRegistry(), target)
	if len(changes) < 2 {
		t.Fatalf("expected an extension and a table, got %d change(s):\n%s", len(changes), render(changes))
	}
	// Exact, not Contains: goose splits a migration file's ungrouped
	// statements on ";", so a missing one is not cosmetic — the extension
	// statement runs together with whatever text follows it. A prior version
	// of this test used Contains and passed against exactly that bug.
	if want := `CREATE EXTENSION IF NOT EXISTS "btree_gist";`; changes[0].Up != want {
		t.Errorf("the first statement is not the btree_gist extension:\n got:  %q\n want: %q", changes[0].Up, want)
	}
	if !strings.Contains(changes[1].Up, "CREATE TABLE") {
		t.Errorf("the table does not follow the extension:\n%s", changes[1].Up)
	}
	if changes[0].Down != "" {
		t.Errorf("the extension should not be automatically reversible, got Down = %q", changes[0].Down)
	}
}

// A gist exclusion over ranges and geometric types alone — no scalar equality
// — needs no extension, so nothing should be proposed for one. This is also
// the boundary the heuristic misses on the other side (documented on
// usesBtreeGist): it looks for the literal substring "WITH =", so it is a
// false negative here only by coincidence of there being no "=" to find, not
// because it understands operator classes.
func TestBtreeGistExtensionNotTriggeredByRangeOnly(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Timestamp("starts_at"),
			schema.Timestamp("ends_at"),
		).AddExclude(schema.Exclusion{
			Name:     "bookings_no_overlap",
			Using:    "gist",
			Elements: "tstzrange(starts_at, ends_at) WITH &&",
		})
	})
	for _, c := range diff(t, schema.NewRegistry(), target) {
		if strings.Contains(c.Up, "CREATE EXTENSION") {
			t.Errorf("btree_gist was proposed for an exclusion with no scalar equality:\n%s", c.Up)
		}
	}
}

// A schema with no gist-over-equality exclusion never asks for the extension,
// and one that already has it is not asked to repeat the idempotent
// statement on the next migration.
func TestBtreeGistExtensionIsNotRepeated(t *testing.T) {
	plain := build(func(r *schema.Registry) {
		r.Table("notes", schema.UUIDv7("id").PrimaryKey(), schema.Text("body"))
	})
	for _, c := range diff(t, schema.NewRegistry(), plain) {
		if strings.Contains(c.Up, "CREATE EXTENSION") {
			t.Errorf("the extension was proposed for a schema with no exclusion needing it:\n%s", c.Up)
		}
	}

	withExcl := build(func(r *schema.Registry) {
		r.Table("rooms", schema.UUIDv7("id").PrimaryKey(), schema.UUID("room_id")).
			AddExclude(schema.Exclusion{Name: "rooms_excl", Using: "gist", Elements: "room_id WITH ="})
	})
	andAColumn := build(func(r *schema.Registry) {
		r.Table("rooms", schema.UUIDv7("id").PrimaryKey(), schema.UUID("room_id"), schema.Text("note").Nullable()).
			AddExclude(schema.Exclusion{Name: "rooms_excl", Using: "gist", Elements: "room_id WITH ="})
	})
	for _, c := range diff(t, withExcl, andAColumn) {
		if strings.Contains(c.Up, "CREATE EXTENSION") {
			t.Errorf("the extension was proposed again for a schema that already has one:\n%s", c.Up)
		}
	}
}
