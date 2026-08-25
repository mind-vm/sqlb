package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// Adopting a database somebody else's tool built.
//
// Every test here is a diff between a *declared* registry and one shaped like
// what introspect reads back, because that is the comparison an adoption lives
// in: the database is right, the declaration is trying to describe it, and any
// difference the diff reports that is not real is a migration proposed against
// a production schema for no reason.

// A jsonb default has two spellings and each side of the comparison picks a
// different one. They are the same default, so no migration is generated
// (issue #56).
func TestJSONDefaultRoundTripsWhicheverWayItIsSpelled(t *testing.T) {
	// What introspect produces: the literal, normalised into a value.
	current := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.JSON("team_member_ids").Nullable().Default(schema.Value("[]")),
			schema.JSON("settings").Nullable().Default(schema.Value(`{"a": 1, "b": 2}`)),
		)
	})
	// What a hand-written declaration says, having read the raw DDL.
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.JSON("team_member_ids").Nullable().Default(schema.Expr("'[]'::jsonb")),
			// Key order and whitespace are not part of a jsonb value, because
			// Postgres stores a parsed document rather than the text it arrived
			// as.
			schema.JSON("settings").Nullable().Default(schema.Expr(`'{"b":2,"a":1}'::jsonb`)),
		)
	})

	if changes := diff(t, current, target); len(changes) != 0 {
		t.Errorf("the same default was proposed as a change:\n%s", render(changes))
	}
}

// The comparison is semantic, not blind: a default that really did change is
// still a change.
func TestJSONDefaultThatActuallyChanged(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.JSON("tags").Nullable().Default(schema.Value("[]")),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.JSON("tags").Nullable().Default(schema.Expr(`'{}'::jsonb`)),
		)
	})

	c := only(t, diff(t, current, target))
	if !strings.Contains(c.Up, "SET DEFAULT '{}'::jsonb") {
		t.Errorf("want the new default:\n%s", c.Up)
	}
}

// The unwrapping is for jsonb only. On a text column the two strings are two
// different strings, and pretending otherwise would silently keep the wrong
// default.
func TestTextDefaultIsComparedAsText(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("shape").Default(schema.Value(`{"a":1}`)),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("shape").Default(schema.Value(`{ "a" : 1 }`)),
		)
	})
	if len(diff(t, current, target)) != 1 {
		t.Error("two different text defaults should be a change")
	}
}

// An index can be declared under the name the database already gave it, so
// adopting a table does not propose renaming its indexes (issue #57).
func TestIndexNamedDescribesAnExistingIndex(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("org_id"),
			schema.Text("code"),
		).
			AddIndex(schema.Index{Name: "idx_projects_org_id", Columns: []string{"org_id"}}).
			AddIndex(schema.Index{Name: "idx_projects_org_code", Columns: []string{"org_id", "code"}, Unique: true})
	})
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("org_id"),
			schema.Text("code"),
		).
			IndexNamed("idx_projects_org_id", "org_id").
			UniqueIndexNamed("idx_projects_org_code", "org_id", "code")
	})

	if changes := diff(t, current, target); len(changes) != 0 {
		t.Errorf("declaring the indexes the database has should propose nothing:\n%s", render(changes))
	}
}

// Without the name the diff still renames — that is correct, and the comment
// says what a rename can cost, because Postgres reports a violated unique
// constraint by name and applications match those names.
func TestIndexRenameSaysWhatItCosts(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("org_id"),
			schema.Text("code"),
		).
			AddIndex(schema.Index{Name: "idx_projects_org_code", Columns: []string{"org_id", "code"}, Unique: true})
	})
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("org_id"),
			schema.Text("code"),
		).UniqueIndex("org_id", "code")
	})

	c := only(t, diff(t, current, target))
	if !strings.Contains(c.Up, "ALTER INDEX") {
		t.Fatalf("want a rename, got:\n%s", c.Up)
	}
	for _, want := range []string{"23505", "idx_projects_org_code", "UniqueIndexNamed"} {
		if !strings.Contains(c.Comment, want) {
			t.Errorf("the rename comment does not mention %q:\n%s", want, c.Comment)
		}
	}
}

// A plain index rename carries no such warning: nothing reports a
// non-unique index's name back to an application.
func TestPlainIndexRenameIsQuiet(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("projects", schema.UUIDv7("id").PrimaryKey(), schema.UUID("org_id")).
			AddIndex(schema.Index{Name: "idx_projects_org_id", Columns: []string{"org_id"}})
	})
	target := build(func(r *schema.Registry) {
		r.Table("projects", schema.UUIDv7("id").PrimaryKey(), schema.UUID("org_id")).Index("org_id")
	})

	c := only(t, diff(t, current, target))
	if strings.Contains(c.Comment, "23505") {
		t.Errorf("a non-unique index rename should not carry the unique-violation warning:\n%s", c.Comment)
	}
}

// The migration that motivated the issue, end to end: three indexes named by
// another convention, and a declaration that describes them exactly.
func TestAdoptingATableProposesNothing(t *testing.T) {
	declare := func(named bool) func(*schema.Registry) {
		return func(r *schema.Registry) {
			t := r.Table("projects",
				schema.UUIDv7("id").PrimaryKey(),
				schema.UUID("org_id"),
				schema.UUID("manager_id").Nullable(),
				schema.Text("code"),
				schema.JSON("team_member_ids").Nullable().Default(schema.Value("[]")),
			)
			if !named {
				t.Index("manager_id").Index("org_id").UniqueIndex("org_id", "code")
				return
			}
			t.IndexNamed("idx_projects_manager_id", "manager_id").
				IndexNamed("idx_projects_org_id", "org_id").
				UniqueIndexNamed("idx_projects_org_code", "org_id", "code")
		}
	}

	// The database as introspect reads it: named indexes, a normalised default.
	current := build(declare(true))

	// Declared the naive way, every index is a rename.
	if n := len(diff(t, current, build(declare(false)))); n != 3 {
		t.Errorf("want 3 renames from the naive declaration, got %d", n)
	}
	// Declared as the database actually is, nothing is proposed at all.
	if changes := diff(t, current, build(declare(true))); len(changes) != 0 {
		t.Errorf("an accurate declaration should propose nothing:\n%s", render(changes))
	}
}

// A partial declaration can describe a live foreign key without declaring the
// table it points at, which is what an incremental adoption needs and neither
// existing spelling gave it (issue #55).
func TestEnforcedExternalRefEmitsAForeignKey(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("org", "organizations.id").Enforced().OnDelete(schema.Cascade),
		)
	})

	changes := diff(t, nil, target)
	c := find(t, changes, "FOREIGN KEY")
	for _, want := range []string{
		`ADD CONSTRAINT "projects_org_id_fkey"`,
		`FOREIGN KEY ("org_id") REFERENCES "organizations" ("id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(c.Up, want) {
			t.Errorf("the constraint is missing %q:\n%s", want, c.Up)
		}
	}
}

// A bare table name means its id, which is the convention the rest of the DSL
// already assumes.
func TestEnforcedExternalRefDefaultsToTheIdColumn(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("org", "organizations").Enforced(),
		)
	})
	c := find(t, diff(t, nil, target), "FOREIGN KEY")
	if !strings.Contains(c.Up, `REFERENCES "organizations" ("id")`) {
		t.Errorf("want the id column:\n%s", c.Up)
	}
}

// Unenforced stays unenforced: module isolation is the default and this changes
// nothing about it (ADR-0015).
func TestExternalRefWithoutEnforcedStillEmitsNoForeignKey(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("org", "organizations.id"),
		)
	})
	for _, c := range diff(t, nil, target) {
		if strings.Contains(c.Up, "FOREIGN KEY") {
			t.Errorf("an unenforced external reference emitted a constraint:\n%s", c.Up)
		}
	}
}

// The declaration a drift gate compares against: the database has the
// constraint, the schema says so, and nothing is proposed.
func TestEnforcedExternalRefDiffsCleanAgainstTheDatabase(t *testing.T) {
	declare := func(r *schema.Registry) {
		r.Table("projects",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("org", "organizations.id").Enforced().OnDelete(schema.Cascade),
		)
	}
	if changes := diff(t, build(declare), build(declare)); len(changes) != 0 {
		t.Errorf("an accurate declaration should propose nothing:\n%s", render(changes))
	}
}

// A target that cannot name a table in this database is refused at validation,
// naming the two forms that work.
func TestEnforcedExternalRefRefusesAModuleQualifiedTarget(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.ExternalRef("user", "platform/users.users.id").Enforced(),
	)
	err := r.Validate()
	if err == nil {
		t.Fatal("a module-qualified target cannot carry a foreign key")
	}
	for _, want := range []string{"organizations.id", "module boundary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
