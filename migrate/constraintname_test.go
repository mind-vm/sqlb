package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// Two constraints on one table cannot share a name: Postgres refuses the
// CREATE TABLE, and the diff path keys constraints by name and loses one
// without saying so.
//
// The collision is easy to write because half of it is invisible. An Enum
// column's CHECK is named <table>_<column>_check with nothing in the Go source
// saying so, and that is the name somebody writing a second constraint about
// the same column reaches for — so the mistake is between something written
// and something that does not appear in the declaration at all (#303).
//
// The shadow database cannot catch this: it replays the committed history and
// diffs the result, and the file about to be written is not itself applied. So
// generation was clean and the failure surfaced on the next migrate run, in
// CI, or at deploy, depending on which came first.

func bookings(check func(*schema.TableDef)) *schema.Registry {
	return build(func(r *schema.Registry) {
		t := r.Table("offering_bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Enum("status", "pending", "confirmed", "cancelled").Default(schema.Value("pending")),
			schema.Timestamp("confirmed_at").Nullable(),
		)
		check(t)
	})
}

func TestDiffRefusesTwoConstraintsWithOneName(t *testing.T) {
	// The name the Enum column already took, and the obvious name for a second
	// constraint about status.
	target := bookings(func(td *schema.TableDef) {
		td.Check("offering_bookings_status_check", "status <> 'confirmed' OR confirmed_at IS NOT NULL")
	})

	_, err := migrate.Diff(schema.NewRegistry(), target)
	if err == nil {
		t.Fatal("Diff wrote DDL carrying a duplicate constraint name; Postgres refuses it")
	}
	// The diagnostic has to name both sources, because one of them is not in
	// the source being read. An error saying only "duplicate name" leaves the
	// author looking for a second Check that does not exist.
	for _, want := range []string{
		"offering_bookings",
		`"offering_bookings_status_check"`,
		"Enum column's generated check",
		"explicit Check",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

// The other direction, which is what makes the refusal above mean something:
// a second constraint about the same column is perfectly legal, and is the
// documented workaround. Naming it for what it asserts rather than for its
// column is all that was needed.
func TestDiffAcceptsASecondConstraintUnderItsOwnName(t *testing.T) {
	target := bookings(func(td *schema.TableDef) {
		td.Check("offering_bookings_timestamps_check", "status <> 'confirmed' OR confirmed_at IS NOT NULL")
	})

	changes := diff(t, schema.NewRegistry(), target)
	create := find(t, changes, "CREATE TABLE")
	for _, want := range []string{
		`CONSTRAINT "offering_bookings_status_check"`,
		`CONSTRAINT "offering_bookings_timestamps_check"`,
	} {
		if !strings.Contains(create.Up, want) {
			t.Errorf("both constraints should be emitted; missing %s:\n%s", want, create.Up)
		}
	}
}

// The alter path is where the loss was silent rather than loud: the differ
// keys constraints by name, so the second one was neither added nor reported.
// It reaches the same refusal, before any change is rendered.
func TestDiffRefusesADuplicateNameOnAnExistingTable(t *testing.T) {
	current := bookings(func(*schema.TableDef) {})
	target := bookings(func(td *schema.TableDef) {
		td.Check("offering_bookings_status_check", "status <> 'confirmed' OR confirmed_at IS NOT NULL")
	})

	if _, err := migrate.Diff(current, target); err == nil {
		t.Fatal("an ALTER adding a constraint under a taken name must be refused, not dropped")
	}
}

// The same name and the same definition is one constraint declared twice.
// Postgres refuses it just the same, but the advice differs — there is nothing
// to rename, only something to delete — so the message says which it is.
func TestDiffNamesTheDuplicateWhenBothHalvesAgree(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("offering_bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Timestamp("confirmed_at").Nullable(),
		).Check("bookings_confirmed_check", "confirmed_at IS NOT NULL").
			Check("bookings_confirmed_check", "confirmed_at IS NOT NULL")
	})

	_, err := migrate.Diff(schema.NewRegistry(), target)
	if err == nil {
		t.Fatal("the same constraint declared twice must be refused")
	}
	if !strings.Contains(err.Error(), "twice") || !strings.Contains(err.Error(), "remove one") {
		t.Errorf("the error should say the declaration is duplicated, not that a name clashes: %v", err)
	}
}
