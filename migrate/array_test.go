package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// An array column renders as its element type plus [], and the diff picks the
// change up because it compares rendered types (ADR-0033).
func TestArrayColumnDDL(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("tags").Array().Default(schema.Value("{}")),
			schema.Int("sizes").Array().Nullable(),
		)
	})
	change := only(t, diff(t, schema.NewRegistry(), target))
	for _, want := range []string{
		`"tags" text[] NOT NULL DEFAULT '{}'`,
		`"sizes" integer[]`,
	} {
		if !strings.Contains(change.Up, want) {
			t.Errorf("DDL is missing %q:\n%s", want, change.Up)
		}
	}
}

// An enum array is constrained by containment, not by IN. `col IN (...)`
// compares the whole array to each value, which admits any array at all — so
// the wrong spelling here is permissive rather than merely incorrect.
func TestEnumArrayCheckIsContainment(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Enum("labels", "red", "green").Array(),
		)
	})
	change := only(t, diff(t, schema.NewRegistry(), target))
	if !strings.Contains(change.Up, `"labels" <@ ARRAY['red', 'green']::text[]`) {
		t.Errorf("enum array check is not a containment test:\n%s", change.Up)
	}
	// And the scalar form is untouched.
	scalar := build(func(r *schema.Registry) {
		r.Table("notes",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Enum("state", "on", "off"),
		)
	})
	c := only(t, diff(t, schema.NewRegistry(), scalar))
	if !strings.Contains(c.Up, `"state" IN ('on', 'off')`) {
		t.Errorf("scalar enum check changed shape:\n%s", c.Up)
	}
}

// Gaining or losing the array rewrites every row and is never a widening, so
// the change is generated commented out rather than applied.
func TestArrayTypeChangeIsNotAWidening(t *testing.T) {
	scalar := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("tags"))
	})
	array := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("tags").Array())
	})

	change := find(t, diff(t, scalar, array), "TYPE text[]")
	if !change.Destructive {
		t.Error("text to text[] was not reported as destructive")
	}
	back := find(t, diff(t, array, scalar), "TYPE text")
	if !back.Destructive {
		t.Error("text[] to text was not reported as destructive")
	}
}

// smallint and real render at the width the database already has, scalar and
// array alike. Widening either to suit the DSL was a schema change an adopter
// could not justify, which is what issues #114 and #120 were about — so the
// declaration has to reach the DDL unwidened or the fix is cosmetic.
func TestNarrowNumericWidthsRenderUnwidened(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("events",
			schema.UUIDv7("id").PrimaryKey(),
			schema.SmallInt("pos_x"),
			schema.SmallInt("weekdays").Array().Nullable(),
			schema.Real("confidence").Nullable(),
		)
	})
	change := only(t, diff(t, schema.NewRegistry(), target))
	for _, want := range []string{
		`"pos_x" smallint NOT NULL`,
		`"weekdays" smallint[]`,
		`"confidence" real`,
	} {
		if !strings.Contains(change.Up, want) {
			t.Errorf("DDL is missing %q:\n%s", want, change.Up)
		}
	}
	// The point of the issue: nothing in the generated DDL widened.
	for _, unwanted := range []string{"integer", "double precision"} {
		if strings.Contains(change.Up, unwanted) {
			t.Errorf("a narrow width was widened to %q:\n%s", unwanted, change.Up)
		}
	}
}
