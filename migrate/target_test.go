package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// withUUIDv7 declares the one construct MinPostgres currently affects.
func withUUIDv7(r *schema.Registry) {
	r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
}

func TestDefaultTargetEmitsTheExtensionSpelling(t *testing.T) {
	changes, err := migrate.Diff(schema.NewRegistry(), build(withUUIDv7))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	// Unstated means unchanged. Every migration generated before this option
	// existed reads uuid_generate_v7(), and a default that silently rewrote
	// emitted DDL is the one mistake ADR-0014 says regenerating cannot undo.
	sql := render(changes)
	if !strings.Contains(sql, "uuid_generate_v7()") {
		t.Errorf("want the extension spelling by default, got:\n%s", sql)
	}
	if strings.Contains(sql, "uuidv7()") {
		t.Errorf("emitted the Postgres 18 built-in without being asked:\n%s", sql)
	}
}

func TestMinPostgres18EmitsTheBuiltin(t *testing.T) {
	changes, err := migrate.Diff(schema.NewRegistry(), build(withUUIDv7), migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	sql := render(changes)
	if !strings.Contains(sql, "uuidv7()") {
		t.Errorf("want the built-in at MinPostgres(18), got:\n%s", sql)
	}
	// Substring care: "uuid_generate_v7()" does not contain "uuidv7()", but
	// the reverse check needs the underscore form spelled out or it would pass
	// on output that still requires the extension.
	if strings.Contains(sql, "uuid_generate_v7()") {
		t.Errorf("still requires the pg_uuidv7 extension at MinPostgres(18):\n%s", sql)
	}
}

func TestBelowTheVersionTheBuiltinArrivedInNothingChanges(t *testing.T) {
	changes, err := migrate.Diff(schema.NewRegistry(), build(withUUIDv7), migrate.MinPostgres(17))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if sql := render(changes); !strings.Contains(sql, "uuid_generate_v7()") {
		t.Errorf("MinPostgres(17) must not use a built-in that arrived in 18:\n%s", sql)
	}
}

// TestOnlyKnownGeneratorsAreRewritten guards the property that makes this safe
// to apply to a raw string at all: schema.Expr takes arbitrary SQL, and
// rewriting an expression the package does not understand is the guessing this
// project refuses everywhere else.
func TestOnlyKnownGeneratorsAreRewritten(t *testing.T) {
	decl := func(r *schema.Registry) {
		r.Table("orgs",
			schema.UUIDv7("id").PrimaryKey(),
			// Deliberately adjacent to the generator being rewritten: a
			// substring or prefix match would catch this too.
			schema.Text("note").Default(schema.Expr("my_uuid_generate_v7()")),
			schema.Text("other").Default(schema.Expr("coalesce(uuid_generate_v7()::text, '')")),
		)
	}

	changes, err := migrate.Diff(schema.NewRegistry(), build(decl), migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	sql := render(changes)
	for _, want := range []string{
		"my_uuid_generate_v7()",
		"coalesce(uuid_generate_v7()::text, '')",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("a raw expression was rewritten; want %q in:\n%s", want, sql)
		}
	}
}

// TestAChangedTargetIsNotAChangedSchema is the subtle one.
//
// renderDefault serves two callers: one writes DDL, the other compares the
// current and target renderings to decide whether the default changed. If those
// two resolved against different versions, moving a project onto MinPostgres(18)
// would produce a migration altering the default of every UUIDv7 column in the
// database — pure churn, and destructive-looking in review.
func TestAChangedTargetIsNotAChangedSchema(t *testing.T) {
	current, target := build(withUUIDv7), build(withUUIDv7)

	changes, err := migrate.Diff(current, target, migrate.MinPostgres(18))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("identical schemas produced %d change(s) under MinPostgres(18):\n%s",
			len(changes), render(changes))
	}
}
