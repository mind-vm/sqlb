package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A vector column renders with its dimension in the type name, because that is
// what it is: Postgres will not store a vector(768) value in a vector(1536)
// column and the two are different types in the catalog (ADR-0026).
//
// The dimension being a Go expression is the whole point of the declaration —
// it is what removes the substitution sentinel a migration file needs when the
// width lives in configuration, and with it the hand-maintained mirror of the
// schema that sentinel forces.
func TestVectorColumnCarriesItsDimension(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("chunks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("body"),
			schema.Vector("embedding", 1536),
		)
	})
	changes := diff(t, schema.NewRegistry(), target)
	create := changes[len(changes)-1]
	if !strings.Contains(create.Up, `"embedding" vector(1536) NOT NULL`) {
		t.Errorf("DDL does not carry the dimension:\n%s", create.Up)
	}
}

// The extension is ordered ahead of every table, since a table declaring a
// column of its type cannot be created until it exists.
func TestVectorExtensionComesFirst(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("chunks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Vector("embedding", 8),
		)
	})
	changes := diff(t, schema.NewRegistry(), target)
	if len(changes) < 2 {
		t.Fatalf("expected an extension and a table, got %d change(s)", len(changes))
	}
	// Exact, not Contains: goose splits a migration file's ungrouped
	// statements on ";", so a missing one is not cosmetic — the extension
	// statement runs together with whatever text follows it. A prior version
	// of this test used Contains and passed against exactly that bug.
	if want := `CREATE EXTENSION IF NOT EXISTS "vector";`; changes[0].Up != want {
		t.Errorf("the first statement is not the extension:\n got:  %q\n want: %q", changes[0].Up, want)
	}
	if !strings.Contains(changes[1].Up, "CREATE TABLE") {
		t.Errorf("the table does not follow the extension:\n%s", changes[1].Up)
	}

	// It has no Down. Dropping the extension would drop every vector column in
	// the database including ones this schema has never heard of, and an
	// unused extension costs nothing to leave behind.
	if changes[0].Down != "" {
		t.Errorf("the extension should not be automatically reversible, got Down = %q", changes[0].Down)
	}
	// And it says what will go wrong, because it is the statement most likely
	// to fail and the privilege it needs is not one the schema can grant.
	if !strings.Contains(changes[0].Hazard, "privileges") {
		t.Errorf("the extension change does not warn about privileges: %q", changes[0].Hazard)
	}
}

// It is a change rather than a preamble: emitted when a vector column first
// appears, and not in every migration after that. The statement is idempotent,
// so repeating it would be harmless — and would be noise in every file forever.
func TestVectorExtensionIsNotRepeated(t *testing.T) {
	withVector := build(func(r *schema.Registry) {
		r.Table("chunks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Vector("embedding", 8),
		)
	})
	andAColumn := build(func(r *schema.Registry) {
		r.Table("chunks",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Vector("embedding", 8),
			schema.Text("body").Nullable(),
		)
	})
	for _, c := range diff(t, withVector, andAColumn) {
		if strings.Contains(c.Up, "CREATE EXTENSION") {
			t.Errorf("the extension was emitted again for a schema that already has a vector column:\n%s", c.Up)
		}
	}

	// A schema with no vector column never asks for it at all.
	plain := build(func(r *schema.Registry) {
		r.Table("notes", schema.UUIDv7("id").PrimaryKey(), schema.Text("body"))
	})
	for _, c := range diff(t, schema.NewRegistry(), plain) {
		if strings.Contains(c.Up, "CREATE EXTENSION") {
			t.Errorf("the extension was emitted for a schema with no vector column:\n%s", c.Up)
		}
	}
}

// Changing the dimension is the change this declaration exists to notice, and
// the one whose consequence is worst understated by the generic message.
//
// Postgres refuses the cast outright, and underneath that refusal is the real
// problem: the stored vectors were produced by an embedder of the old width and
// mean nothing at the new one. They cannot be converted, only recomputed — so
// the migration is not a rewrite but a re-embedding, which is not a thing a
// migration file should try to contain.
func TestVectorDimensionChangeIsRefusedAndSaysWhy(t *testing.T) {
	from := build(func(r *schema.Registry) {
		r.Table("chunks", schema.UUIDv7("id").PrimaryKey(), schema.Vector("embedding", 1536))
	})
	to := build(func(r *schema.Registry) {
		r.Table("chunks", schema.UUIDv7("id").PrimaryKey(), schema.Vector("embedding", 768))
	})

	change := only(t, diff(t, from, to))
	if !change.Destructive {
		t.Fatal("a dimension change is not a widening and must be generated commented out")
	}
	for _, want := range []string{"invalidates every stored embedding", "recomputed"} {
		if !strings.Contains(change.Reason, want) {
			t.Errorf("the reason does not mention %q: %s", want, change.Reason)
		}
	}
	if !strings.Contains(change.Up, "vector(768)") {
		t.Errorf("the change does not alter to the new dimension:\n%s", change.Up)
	}
}

// A vector column with no dimension is refused before it reaches DDL. The
// declaration cannot render at all without one, and a zero would otherwise
// arrive as `vector(0)` for Postgres to reject halfway through a migration.
func TestVectorWithoutADimensionIsRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Vector("embedding", 0),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("expected a vector column with no dimension to be refused")
	}
	if !strings.Contains(err.Error(), "needs a dimension") {
		t.Errorf("error does not say what is missing: %v", err)
	}
}
