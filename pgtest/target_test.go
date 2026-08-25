package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// The gap these tests close was found by writing the harness for the round-trip
// tests, not by a failing test: schema.GenUUIDv7 emits uuid_generate_v7(), the
// pg_uuidv7 extension's spelling, so generated DDL for a UUIDv7 primary key did
// not apply to a stock Postgres at all. The extension is documented on
// GenUUIDv7 and nothing warns at generation time.
//
// migrate.MinPostgres(18) emits the built-in uuidv7() instead. These run against
// freshStockDB — a Postgres exactly as it ships — because a database with the
// shim installed cannot tell the two spellings apart.

func TestMinPostgres18AppliesToAStockPostgres(t *testing.T) {
	t.Parallel()
	db := freshStockDB(t)

	target := schema.NewRegistry()
	declare(target)

	// No shim. If this needs an extension, it fails here.
	applySchema(t, db, target, migrate.MinPostgres(18))

	// And the column really does generate UUIDv7s, rather than merely parsing.
	// Version 7 is recorded in the 13th hex digit of the textual form.
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO orgs (name, slug) VALUES ('acme', 'acme') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("inserting a row that relies on the generated default: %v", err)
	}
	if got := strings.ReplaceAll(id, "-", "")[12]; got != '7' {
		t.Errorf("id %q is not a version 7 UUID (version nibble %q)", id, string(got))
	}
}

// TestTheDefaultTargetStillNeedsTheExtension is the other direction, and the
// reason the test above means anything.
//
// If generated DDL applied to a stock Postgres either way, MinPostgres(18) would
// be solving nothing and this whole option would be dead weight. Asserting the
// failure is what proves the gap is real — and it documents, executably, what a
// project that has not set the option is depending on.
func TestTheDefaultTargetStillNeedsTheExtension(t *testing.T) {
	t.Parallel()
	db := freshStockDB(t)

	target := schema.NewRegistry()
	declare(target)

	changes := diff(t, schema.NewRegistry(), target)
	var lastErr error
	for _, c := range changes {
		if strings.TrimSpace(c.Up) == "" {
			continue
		}
		if _, err := db.Exec(context.Background(), c.Up); err != nil {
			lastErr = err
			break
		}
	}

	if lastErr == nil {
		t.Fatal("the default target applied to a stock Postgres with no extension installed.\n" +
			"That would mean uuid_generate_v7() now exists without pg_uuidv7, and MinPostgres(18) " +
			"is solving a problem that no longer exists — check whether the option still earns its keep.")
	}
	if !strings.Contains(lastErr.Error(), "uuid_generate_v7") {
		t.Errorf("expected the failure to name the missing generator, got: %v", lastErr)
	}
}

// TestMinPostgres18RoundTripsAndIsAFixpoint is the regression this fix could
// most easily have introduced.
//
// A database written with uuidv7() reads back as that text. Unless introspect
// maps it onto schema.GenUUIDv7 — the same generator uuid_generate_v7() maps to
// — the imported registry holds a raw expression where the declared one holds a
// generator, and every subsequent diff proposes to change the default of every
// UUIDv7 column. Forever. That is exactly the failure ADR-0014 calls decisive,
// and it would not show up in any test that used only the default target.
func TestMinPostgres18RoundTripsAndIsAFixpoint(t *testing.T) {
	t.Parallel()
	first := freshStockDB(t)

	target := schema.NewRegistry()
	declare(target)
	applySchema(t, first, target, migrate.MinPostgres(18))

	imported := importRegistry(t, first)

	// The declared schema and the imported one must agree, check-constraint
	// normalisation aside — the same allowance the default-target round trip
	// makes, and nothing wider.
	for _, c := range diff(t, imported, target) {
		if _, isAdd := addedCheckConstraint(c); isAdd {
			continue
		}
		if _, isDrop := droppedConstraint(c); isDrop {
			continue
		}
		t.Errorf("MinPostgres(18) round trip is not clean:\n%s", describe([]migrate.Change{c}))
	}

	// And it settles: render the imported schema into a second stock database
	// and import that. An empty diff here is the claim that a project on
	// Postgres 18 does not carry a phantom default change in every migration.
	second := freshStockDB(t)
	applySchema(t, second, imported, migrate.MinPostgres(18))
	twice := importRegistry(t, second)

	if changes := diff(t, imported, twice); len(changes) > 0 {
		t.Errorf("MinPostgres(18) import is not a fixpoint — %d change(s) on the second pass:\n%s",
			len(changes), describe(changes))
	}
}
