package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// variants is the shape issue #154 was filed about: a variant is identified by
// the combination of its option values, which live in a child table, so the
// combination is denormalised into a signature column and the constraint has to
// hold over the committed state — the variant row is written before the option
// values that identify it.
func variants(deferrable schema.Deferrable) func(*schema.Registry) {
	return func(r *schema.Registry) {
		r.Table("variants",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("product_id"),
			schema.Text("option_signature").Default(schema.Value("")),
		).AddUnique(schema.Unique{
			Columns:    []string{"product_id", "option_signature"},
			Deferrable: deferrable,
		})
	}
}

func TestDeferredUniqueRendersItsClause(t *testing.T) {
	c := only(t, diff(t, nil, build(variants(schema.DeferredCheck))))

	want := `CONSTRAINT "variants_product_id_option_signature_key" ` +
		`UNIQUE ("product_id", "option_signature") DEFERRABLE INITIALLY DEFERRED`
	if !strings.Contains(c.Up, want) {
		t.Errorf("CREATE TABLE is missing the deferral:\n%s", c.Up)
	}
}

// The declaration and the database disagreeing about *when* a constraint is
// checked is drift, and reporting it is the half of #154 that matters most: a
// migration that recreated the constraint without its clause would break every
// multi-variant write, and the gate that exists to catch exactly that used to
// stay green because neither side could see the property.
func TestDeferralIsDiffed(t *testing.T) {
	notDeferred := build(variants(schema.NotDeferrable))
	deferred := build(variants(schema.DeferredCheck))

	changes := diff(t, notDeferred, deferred)
	if len(changes) == 0 {
		t.Fatal("adding a deferral to a constraint produced no migration")
	}
	stmts := strings.Join(ups(changes), "\n")
	if !strings.Contains(stmts, "DROP CONSTRAINT") ||
		!strings.Contains(stmts, "DEFERRABLE INITIALLY DEFERRED") {
		t.Errorf("the change does not replace the constraint with the deferred one:\n%s", stmts)
	}

	// And the other way, which is the direction a hand-written migration
	// produces: the database defers and the declaration does not.
	back := diff(t, deferred, notDeferred)
	if len(back) == 0 {
		t.Fatal("removing a deferral from a constraint produced no migration")
	}
}

// Two spellings of the same answer must compare equal, or every run proposes
// replacing a constraint that has not changed — the failure ordered indexes had
// in issue #63.
func TestUndeferredIsTheDefaultSpelling(t *testing.T) {
	explicit := build(variants(schema.NotDeferrable))
	implicit := build(func(r *schema.Registry) {
		r.Table("variants",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("product_id"),
			schema.Text("option_signature").Default(schema.Value("")),
		).Unique("product_id", "option_signature")
	})
	if changes := diff(t, explicit, implicit); len(changes) != 0 {
		t.Fatalf("NotDeferrable and the shorthand are the same constraint, got:\n%s", render(changes))
	}
}

// The lock-brief form renders the constraint from its parts rather than from
// the definition string, so it is a second place the clause can be lost — and
// losing it there produces a constraint the very next diff proposes replacing.
func TestConcurrentAdoptionKeepsTheDeferral(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("variants",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("product_id"),
			schema.Text("option_signature").Default(schema.Value("")),
		)
	})
	changes := migrate.Unblock(diff(t, current, build(variants(schema.DeferredCheck))))

	stmts := strings.Join(ups(changes), "\n")
	if !strings.Contains(stmts, "CREATE UNIQUE INDEX CONCURRENTLY") {
		t.Fatalf("expected the two-step form:\n%s", stmts)
	}
	if !strings.Contains(stmts, "USING INDEX \"variants_product_id_option_signature_key\" DEFERRABLE INITIALLY DEFERRED") {
		t.Errorf("the adopted constraint lost its deferral:\n%s", stmts)
	}
}

// A column's own unique constraint carries it too, which is the shape a list
// rewritten in one transaction needs: each intermediate state violates a rule
// the committed one satisfies.
func TestFieldDeferredRendersItsClause(t *testing.T) {
	c := only(t, diff(t, nil, build(func(r *schema.Registry) {
		r.Table("steps",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Int("position").Unique().Deferred(),
		)
	})))

	want := `CONSTRAINT "steps_position_key" UNIQUE ("position") DEFERRABLE INITIALLY DEFERRED`
	if !strings.Contains(c.Up, want) {
		t.Errorf("CREATE TABLE is missing the deferral:\n%s", c.Up)
	}
}
