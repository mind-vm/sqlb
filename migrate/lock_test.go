package migrate_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// blocking asserts that the one change whose Up contains substr holds the given
// lock, and returns it.
func blocking(t *testing.T, changes []migrate.Change, substr, lock string) migrate.Change {
	t.Helper()
	c := find(t, changes, substr)
	if c.Lock != lock {
		t.Fatalf("%s: Lock = %q, want %q", substr, c.Lock, lock)
	}
	if c.Hazard == "" {
		t.Fatalf("%s: a lock with no hazard says nothing about what it costs", substr)
	}
	return c
}

// free asserts the opposite: that the change is a catalog write and carries no
// lock hazard. Half of these tests are this assertion, because a warning on
// every statement is the same as no warning at all.
func free(t *testing.T, changes []migrate.Change, substr string) {
	t.Helper()
	if c := find(t, changes, substr); c.Lock != "" {
		t.Fatalf("%s: Lock = %q, want none — %s", substr, c.Lock, c.Hazard)
	}
}

func TestDiffSetNotNullIsBlocking(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"))
	})

	c := blocking(t, diff(t, current, target), "SET NOT NULL", "ACCESS EXCLUSIVE")
	// The remedy has to be the actual sequence, not the advice to be careful.
	for _, want := range []string{"NOT VALID", "VALIDATE CONSTRAINT"} {
		if !strings.Contains(c.Hazard, want) {
			t.Errorf("the hazard should name %s:\n%s", want, c.Hazard)
		}
	}
	// Two different problems with the same statement, each with its own fix:
	// backfill the NULLs, and avoid the scan.
	if !c.Destructive {
		t.Error("rows holding NULL still make this fail")
	}
}

func TestDiffDropNotNullIsFreeButItsReversalIsNot(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Nullable())
	})

	c := only(t, diff(t, current, target))
	free(t, []migrate.Change{c}, "DROP NOT NULL")
	if !strings.Contains(c.Comment, "scans the table") {
		t.Errorf("the rollback scans the table and the comment should say so: %q", c.Comment)
	}
}

func TestDiffTypeChangeIsBlocking(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("views"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("views"))
	})

	// A widening is not destructive — nothing is lost — and is still a full
	// rewrite. The two flags are independent and this is the case that shows it.
	c := blocking(t, diff(t, current, target), "TYPE bigint", "ACCESS EXCLUSIVE")
	if c.Destructive {
		t.Error("int to bigint loses nothing")
	}
	if !strings.Contains(c.Hazard, "backfill") {
		t.Errorf("the hazard should name the expand/contract alternative:\n%s", c.Hazard)
	}
}

func TestDiffVarcharWideningIsFree(t *testing.T) {
	// Postgres accepts the new type over the bytes already stored when they are
	// binary coercible, so relaxing a length limit is a catalog write. This is
	// the one exemption, and getting it wrong in the other direction would mean
	// a warning on a statement that costs nothing.
	cases := []struct {
		name         string
		from, to     *schema.Field
		wantUpLock   string
		wantDownFree bool
	}{
		{"longer limit", schema.Varchar("bio", 100), schema.Varchar("bio", 300), "", false},
		{"limit removed", schema.Varchar("bio", 100), schema.Text("bio"), "", false},
		{"shorter limit", schema.Varchar("bio", 300), schema.Varchar("bio", 100), "ACCESS EXCLUSIVE", false},
		{"limit added", schema.Text("bio"), schema.Varchar("bio", 100), "ACCESS EXCLUSIVE", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			current := build(func(r *schema.Registry) {
				r.Table("posts", schema.UUIDv7("id").PrimaryKey(), tt.from)
			})
			target := build(func(r *schema.Registry) {
				r.Table("posts", schema.UUIDv7("id").PrimaryKey(), tt.to)
			})

			c := only(t, diff(t, current, target))
			if c.Lock != tt.wantUpLock {
				t.Fatalf("Lock = %q, want %q", c.Lock, tt.wantUpLock)
			}
			// When the Up is free the Down is the narrowing, and finding that
			// out during a rollback is the worst time to find it out.
			if c.Lock == "" && !strings.Contains(c.Comment, "reversing it rewrites") {
				t.Errorf("the comment should warn that the reverse rewrites: %q", c.Comment)
			}
		})
	}
}

func TestDiffAddedConstraintsAreBlocking(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email"),
			schema.Text("state"),
			schema.UUID("org_id"),
		)
	})
	target := build(func(r *schema.Registry) {
		orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique(),
			schema.Enum("state", "draft", "live"),
			schema.Ref("org", orgs),
		).Check("email_set", `"email" <> ''`)
	})

	changes := diff(t, current, target)

	// A unique constraint is enforced by an index, and building it is the
	// expensive part — which is why the remedy is a concurrent index rather
	// than the NOT VALID dance that fits the other two.
	u := blocking(t, changes, `ADD CONSTRAINT "users_email_key"`, "ACCESS EXCLUSIVE")
	if !strings.Contains(u.Hazard, "CREATE UNIQUE INDEX CONCURRENTLY") {
		t.Errorf("a unique constraint is adopted from a concurrent index:\n%s", u.Hazard)
	}

	for _, name := range []string{`ADD CONSTRAINT "email_set"`, `ADD CONSTRAINT "users_state_check"`} {
		c := blocking(t, changes, name, "ACCESS EXCLUSIVE")
		if !strings.Contains(c.Hazard, "NOT VALID") {
			t.Errorf("%s: the hazard should name the NOT VALID sequence:\n%s", name, c.Hazard)
		}
	}

	// A foreign key locks both tables, and against writes rather than against
	// everything — a different lock, so it is named differently.
	fk := blocking(t, changes, `ADD CONSTRAINT "users_org_id_fkey"`, "SHARE ROW EXCLUSIVE")
	if !strings.Contains(fk.Hazard, "orgs") {
		t.Errorf("the hazard should name the other table locked:\n%s", fk.Hazard)
	}
}

func TestDiffNewTableConstraintsAreFree(t *testing.T) {
	// The counterweight to the test above: the same constraints on a table
	// created by the same migration cost nothing, because the table is empty by
	// construction. Warning about them would be the fastest way to teach
	// everyone to ignore the warnings.
	target := build(func(r *schema.Registry) {
		orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique(),
			schema.Enum("state", "draft", "live"),
			schema.Ref("org", orgs),
		).Check("email_set", `"email" <> ''`).Index("state")
	})

	changes := diff(t, nil, target)
	for _, c := range changes {
		if c.Lock != "" {
			t.Errorf("nothing on a new table is blocking, but %q is: %s", c.Comment, c.Hazard)
		}
	}
	if m := (migrate.Migration{Version: "1", Name: "init", Changes: changes}); len(m.Blocking()) != 0 {
		t.Errorf("Blocking() = %d changes, want 0", len(m.Blocking()))
	}
}

func TestDiffCatalogChangesAreFree(t *testing.T) {
	// Everything here takes ACCESS EXCLUSIVE too, and holds it for as long as a
	// catalog row takes to write. If any of these ever grew a hazard, the flag
	// would stop meaning "this can take the table down".
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("headline").Unique(),
			schema.Text("body"),
			schema.Text("dead").Nullable(),
			schema.Int("views").Default(schema.Value(0)),
		).Describe("old").Check("body_set", `"body" <> ''`)
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Unique().RenamedFrom("headline"),
			schema.Text("body").Nullable(),
			schema.Int("views"),
			schema.Text("added").Nullable(),
		).Describe("new").Index("title")
	})

	changes := diff(t, current, target)
	for _, substr := range []string{
		`RENAME COLUMN`,
		`RENAME CONSTRAINT`,
		`DROP CONSTRAINT`,
		`ADD COLUMN "added"`,
		`DROP COLUMN "dead"`,
		`DROP DEFAULT`,
		`DROP NOT NULL`,
		`COMMENT ON TABLE`,
		`CREATE INDEX CONCURRENTLY`,
	} {
		free(t, changes, substr)
	}
}

func TestBlockingListsTheChangesInOrder(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Int("views"),
			schema.Text("slug").Nullable(),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.BigInt("views"),
			schema.Text("slug").Unique(),
		)
	})

	changes := diff(t, current, target)
	m := migrate.Migration{Version: "1", Name: "tighten", Changes: changes}
	got := m.Blocking()
	if len(got) != 3 {
		t.Fatalf("want 3 blocking changes, got %d:\n%s", len(got), render(changes))
	}
	// A project that knows which of its tables are big gates on this. The order
	// is the order they are applied, so the first one to hurt is the first one
	// listed.
	for i, want := range []string{"TYPE bigint", "SET NOT NULL", `ADD CONSTRAINT "posts_slug_key"`} {
		if !strings.Contains(got[i].Up, want) {
			t.Errorf("Blocking()[%d] = %q, want it to contain %q", i, got[i].Up, want)
		}
	}
}

func TestMigrationRejectsALockWithNoHazard(t *testing.T) {
	// The same rule as Destructive without a Reason: a flag with nothing behind
	// it tells a reviewer there is a problem and not what it is.
	_, err := migrate.Render(migrate.Migration{
		Version: "1",
		Name:    "x",
		Changes: []migrate.Change{{Up: "SELECT 1;", Lock: "ACCESS EXCLUSIVE"}},
	}, migrate.Options{})
	if err == nil || !strings.Contains(err.Error(), "what the lock costs") {
		t.Fatalf("err = %v, want it to reject a lock with no hazard", err)
	}
}

func TestBlockingChangeRendersLiveWithTheLockNamed(t *testing.T) {
	// Unlike a destructive change, this is not commented out. Whether a scan
	// matters depends on how many rows the table holds, which is not in the
	// schema — and a migration nobody can apply without editing teaches people
	// to edit without reading.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("views"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("views"))
	})

	files, err := migrate.Render(migrate.Migration{
		Version: "20260727130000",
		Name:    "widen_views",
		Changes: diff(t, current, target),
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := files["20260727130000_widen_views.sql"]

	if !strings.Contains(body, "\nALTER TABLE \"posts\" ALTER COLUMN \"views\" TYPE bigint;") {
		t.Errorf("the statement must be live, not commented out:\n%s", body)
	}
	if !strings.Contains(body, "-- LOCK ACCESS EXCLUSIVE:") {
		t.Errorf("the lock must be named above the statement:\n%s", body)
	}
	// The note is there to be read, so it has to fit on a screen.
	for _, line := range strings.Split(body, "\n") {
		if len(line) > 80 {
			t.Errorf("line runs off the screen (%d chars): %q", len(line), line)
		}
	}
}
