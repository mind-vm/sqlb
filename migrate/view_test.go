package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

func usersAndActiveView(r *schema.Registry) {
	r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email"),
		schema.Bool("active"),
	)
	r.View("active_users", `SELECT id, email FROM users WHERE active`,
		schema.UUID("id").PrimaryKey(),
		schema.Text("email"),
	)
}

func TestDiffCreateView(t *testing.T) {
	target := build(usersAndActiveView)
	changes := diff(t, build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"), schema.Bool("active"))
	}), target)

	c := find(t, changes, "CREATE VIEW")
	if !strings.Contains(c.Up, `DROP VIEW IF EXISTS "active_users"`) {
		t.Errorf("a new view's Up should still drop-if-exists first:\n%s", c.Up)
	}
	if !strings.Contains(c.Up, `CREATE VIEW "active_users" AS`) {
		t.Errorf("Up missing CREATE VIEW:\n%s", c.Up)
	}
	if !strings.Contains(c.Up, `SELECT id, email FROM users WHERE active`) {
		t.Errorf("Up missing the declared query:\n%s", c.Up)
	}
	if c.Destructive {
		t.Error("creating a view should not be marked Destructive")
	}
}

func TestDiffViewUnchangedProducesNothing(t *testing.T) {
	changes := diff(t, build(usersAndActiveView), build(usersAndActiveView))
	if len(changes) != 0 {
		t.Fatalf("identical view declarations should produce nothing, got:\n%s", render(changes))
	}
}

func TestDiffChangedViewIsDropThenCreate(t *testing.T) {
	current := build(usersAndActiveView)
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"), schema.Bool("active"))
		r.View("active_users", `SELECT id, email FROM users WHERE active AND deleted_at IS NULL`,
			schema.UUID("id").PrimaryKey(),
			schema.Text("email"),
		)
	})

	c := find(t, diff(t, current, target), "CREATE VIEW")
	if !strings.Contains(c.Comment, "definition changed") {
		t.Errorf("Comment should say the definition changed: %q", c.Comment)
	}
	if !strings.Contains(c.Down, "WHERE active") || strings.Contains(c.Down, "deleted_at") {
		t.Errorf("Down should recreate the *previous* definition:\n%s", c.Down)
	}
}

func TestDiffDropView(t *testing.T) {
	current := build(usersAndActiveView)
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"), schema.Bool("active"))
	})

	c := find(t, diff(t, current, target), "DROP VIEW")
	if !strings.Contains(c.Up, `DROP VIEW IF EXISTS "active_users";`) {
		t.Errorf("Up: %s", c.Up)
	}
	if !strings.Contains(c.Down, "CREATE VIEW") {
		t.Errorf("Down should recreate the dropped view:\n%s", c.Down)
	}
	if c.Destructive {
		t.Error("dropping a view should not be marked Destructive — a view holds no rows of its own")
	}
}
