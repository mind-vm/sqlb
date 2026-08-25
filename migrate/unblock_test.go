package migrate_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// stageOf renders a change's stage for a failure message.
func stageOf(c migrate.Change) string {
	switch c.Stage {
	case migrate.StageValidate:
		return "validate"
	case migrate.StageFinish:
		return "finish"
	case migrate.StageAdopt:
		return "adopt"
	case migrate.StageConcurrent:
		return "concurrent"
	}
	return "main"
}

func staged(changes []migrate.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = stageOf(c) + ": " + c.Up
	}
	return out
}

func TestUnblockCheckConstraint(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("views"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Int("views"),
		).Check("views_positive", `"views" >= 0`)
	})

	changes := migrate.Unblock(diff(t, current, target))
	want := []string{
		`main: ALTER TABLE "posts" ADD CONSTRAINT "views_positive" CHECK ("views" >= 0) NOT VALID;`,
		`validate: ALTER TABLE "posts" VALIDATE CONSTRAINT "views_positive";`,
	}
	if got := staged(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("check sequence\n got: %#v\nwant: %#v", got, want)
	}
	// The add is reversible on its own; the validation has nothing of its own
	// to reverse and says so rather than rendering as an unexplained gap.
	if changes[0].Down != `ALTER TABLE "posts" DROP CONSTRAINT "views_positive";` {
		t.Errorf("Down = %q", changes[0].Down)
	}
	if !strings.HasPrefix(changes[1].Down, "--") {
		t.Errorf("a validation's Down should explain itself, got %q", changes[1].Down)
	}
}

func TestUnblockForeignKey(t *testing.T) {
	decl := func(withRef bool) func(*schema.Registry) {
		return func(r *schema.Registry) {
			orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
			col := schema.UUID("org_id")
			if withRef {
				col = schema.Ref("org", orgs)
			}
			r.Table("users", schema.UUIDv7("id").PrimaryKey(), col)
		}
	}

	changes := migrate.Unblock(diff(t, build(decl(false)), build(decl(true))))
	want := []string{
		`main: ALTER TABLE "users" ADD CONSTRAINT "users_org_id_fkey" FOREIGN KEY ("org_id") REFERENCES "orgs" ("id") NOT VALID;`,
		`validate: ALTER TABLE "users" VALIDATE CONSTRAINT "users_org_id_fkey";`,
	}
	if got := staged(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("foreign key sequence\n got: %#v\nwant: %#v", got, want)
	}
}

func TestUnblockSetNotNull(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"))
	})

	changes := migrate.Unblock(diff(t, current, target))
	want := []string{
		`main: ALTER TABLE "users" ADD CONSTRAINT "users_email_notnull_check" CHECK ("email" IS NOT NULL) NOT VALID;`,
		`validate: ALTER TABLE "users" VALIDATE CONSTRAINT "users_email_notnull_check";`,
		`finish: ALTER TABLE "users" ALTER COLUMN "email" SET NOT NULL;`,
		`finish: ALTER TABLE "users" DROP CONSTRAINT "users_email_notnull_check";`,
	}
	if got := staged(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("set not null sequence\n got: %#v\nwant: %#v", got, want)
	}

	// The temporary check is this package's invention, so it has to be gone by
	// the end: a schema that does not declare it would otherwise see the next
	// diff propose dropping it.
	if !strings.Contains(changes[3].Up, "DROP CONSTRAINT") {
		t.Error("the sequence must leave no constraint of its own behind")
	}

	// Reversed, each file's Down restores the state the file before it left.
	wantDown := []string{
		`ALTER TABLE "users" ADD CONSTRAINT "users_email_notnull_check" CHECK ("email" IS NOT NULL) NOT VALID;`,
		`ALTER TABLE "users" ALTER COLUMN "email" DROP NOT NULL;`,
		"", // the validation, which explains itself in a comment
		`ALTER TABLE "users" DROP CONSTRAINT "users_email_notnull_check";`,
	}
	got := downs(changes)
	for i := range got {
		if wantDown[i] == "" {
			if !strings.HasPrefix(got[i], "--") {
				t.Errorf("Down[%d] = %q, want an explanation", i, got[i])
			}
			continue
		}
		if got[i] != wantDown[i] {
			t.Errorf("Down[%d] = %q, want %q", i, got[i], wantDown[i])
		}
	}
}

func TestUnblockCarriesTheReasonToEveryStep(t *testing.T) {
	// A sequence with half its statements commented out is worse than either
	// whole one, so the review the original needed applies to all of it.
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"))
	})

	for i, c := range migrate.Unblock(diff(t, current, target)) {
		if !c.Destructive || c.Reason == "" {
			t.Errorf("step %d lost the reason it needed review: %+v", i, c)
		}
	}
}

// TestUnblockCarriesADependencyToEveryStep is the same rule for the other
// reason a change is commented out. A unique constraint over a column whose ADD
// COLUMN is commented out expands into a concurrent index build and an
// adoption — and the build is the half that would actually fail, so a sequence
// that lost the dependency on the way through would be worse than the single
// statement it replaced.
func TestUnblockCarriesADependencyToEveryStep(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
	})
	target := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug").Unique())
	})

	unblocked := migrate.Unblock(diff(t, current, target))
	if len(unblocked) != 3 {
		t.Fatalf("want the add plus a two-step sequence, got:\n%s", render(unblocked))
	}
	for i, c := range unblocked {
		if c.Destructive || c.DependsOn != "" {
			continue
		}
		t.Errorf("step %d would run against a column that does not exist yet: %s", i, c.Up)
	}
}

func TestUnblockRemovesTheHazardItReplaces(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Nullable(),
			schema.Text("note"),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email"),
			schema.Text("note"),
		).Check("note_set", `"note" <> ''`)
	})

	changes := diff(t, current, target)
	before := migrate.Migration{Version: "1", Name: "x", Changes: changes}
	after := migrate.Migration{Version: "1", Name: "x", Changes: migrate.Unblock(changes)}
	if len(before.Blocking()) != 2 {
		t.Fatalf("want 2 blocking changes before, got %d:\n%s", len(before.Blocking()), render(changes))
	}
	if got := after.Blocking(); len(got) != 0 {
		t.Fatalf("want none after, got %d:\n%s", len(got), render(after.Changes))
	}
}

func TestUnblockLeavesWhatItCannotFix(t *testing.T) {
	// A type change rewrites every row and has no in-place form, concurrent
	// form, or any other form: the alternative is a second column, a batched
	// backfill and a cutover, and only the person doing it knows what a batch
	// costs or when the cutover can happen. So it keeps its hazard and Blocking
	// still reports it, which is the honest answer.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("views"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.BigInt("views"))
	})

	changes := migrate.Unblock(diff(t, current, target))
	m := migrate.Migration{Version: "1", Name: "x", Changes: changes}
	if len(m.Blocking()) != 1 {
		t.Fatalf("want it still blocking, got %d:\n%s", len(m.Blocking()), render(changes))
	}
	if c := only(t, changes); c.Up != `ALTER TABLE "posts" ALTER COLUMN "views" TYPE bigint;` {
		t.Fatalf("Unblock invented a sequence for a table rewrite: %q", c.Up)
	}
}

func TestUnblockUniqueConstraint(t *testing.T) {
	// A unique constraint has no NOT VALID form — Postgres has no way to build
	// an index without reading every row — but the index can be built
	// beforehand under a lock that lets writes through, and then adopted.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug").Unique())
	})

	changes := migrate.Unblock(diff(t, current, target))
	want := []string{
		`concurrent: CREATE UNIQUE INDEX CONCURRENTLY "posts_slug_key" ON "posts" ("slug");`,
		`adopt: ALTER TABLE "posts" ADD CONSTRAINT "posts_slug_key" UNIQUE USING INDEX "posts_slug_key";`,
	}
	if got := staged(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("unique sequence\n got: %#v\nwant: %#v", got, want)
	}

	// The index is built under the constraint's own name. ADD CONSTRAINT ...
	// USING INDEX renames the index to match the constraint, so naming it
	// correctly up front is what makes the end state identical to the plain
	// statement's rather than merely equivalent.
	if strings.Count(changes[1].Up, `"posts_slug_key"`) != 2 {
		t.Errorf("the index and the constraint should share a name: %q", changes[1].Up)
	}

	// Dropping a unique constraint drops the index enforcing it, so reversing
	// both files would otherwise try to drop an index that is already gone.
	if changes[0].Down != `DROP INDEX CONCURRENTLY IF EXISTS "posts_slug_key";` {
		t.Errorf("Down = %q", changes[0].Down)
	}
	if changes[1].Down != `ALTER TABLE "posts" DROP CONSTRAINT "posts_slug_key";` {
		t.Errorf("Down = %q", changes[1].Down)
	}

	if m := (migrate.Migration{Version: "1", Name: "x", Changes: changes}); len(m.Blocking()) != 0 {
		t.Errorf("the hazard should be gone, got %d", len(m.Blocking()))
	}
}

func TestUnblockPrimaryKeyIsSpelledDifferently(t *testing.T) {
	// A primary key is adopted the same way and named differently, and carries
	// a NOT NULL that a plain unique constraint does not — which is free here
	// only because a primary key column cannot be nullable in the first place.
	current := build(func(r *schema.Registry) {
		r.Table("events", schema.UUID("id"), schema.Text("kind"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("events", schema.UUID("id").PrimaryKey(), schema.Text("kind"))
	})

	changes := migrate.Unblock(diff(t, current, target))
	adopt := find(t, changes, "USING INDEX")
	if adopt.Up != `ALTER TABLE "events" ADD CONSTRAINT "events_pkey" PRIMARY KEY USING INDEX "events_pkey";` {
		t.Fatalf("Up = %q", adopt.Up)
	}
}

func TestUnblockIsANoOpWhenNothingBlocks(t *testing.T) {
	// Everything on a new table is free, so there is nothing to substitute and
	// the change list must come back untouched.
	target := build(func(r *schema.Registry) {
		orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique(),
			schema.Ref("org", orgs),
		).Check("email_set", `"email" <> ''`)
	})

	changes := diff(t, nil, target)
	if got := migrate.Unblock(changes); !reflect.DeepEqual(got, changes) {
		t.Fatalf("Unblock changed a migration with nothing to unblock:\n%s", render(got))
	}
}

func TestUnblockScansNeverFollowAStrongLock(t *testing.T) {
	// The reason StageFinish exists. A lock is held until the transaction
	// commits rather than until the statement ends, so a SET NOT NULL scheduled
	// before a VALIDATE would leave that validation scanning underneath an
	// ACCESS EXCLUSIVE — which is precisely what the sequence exists to avoid.
	current := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Nullable(),
			schema.Text("name").Nullable(),
			schema.Text("note"),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email"),
			schema.Text("name"),
			schema.Text("note"),
		).Check("note_set", `"note" <> ''`)
	})

	parts := migrate.Split(migrate.Migration{
		Version: "1", Name: "tighten",
		Changes: migrate.Unblock(diff(t, current, target)),
	})
	if len(parts) != 2 {
		t.Fatalf("want a main file and a validate file, got %d", len(parts))
	}

	seenStrongLock := false
	for _, c := range parts[1].Changes {
		switch {
		case strings.Contains(c.Up, "VALIDATE CONSTRAINT"):
			if seenStrongLock {
				t.Errorf("this scans under a lock taken earlier in the same transaction: %q", c.Up)
			}
		case strings.Contains(c.Up, "SET NOT NULL"), strings.Contains(c.Up, "DROP CONSTRAINT"):
			seenStrongLock = true
		}
	}
	if !seenStrongLock {
		t.Fatal("the fixture stopped covering the case it was written for")
	}
}

func TestUnblockRendersIntoTwoFiles(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email")).Index("email")
	})

	files, err := migrate.Render(migrate.Migration{
		Version: "20260727140000",
		Name:    "require_email",
		Changes: migrate.Unblock(diff(t, current, target)),
	}, migrate.Options{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	// The expensive work happens in the middle, under the weakest lock that
	// will carry it, and the short strong-lock statements come last.
	want := []string{
		"20260727140000_require_email.sql",
		"20260727140001_require_email_indexes.sql",
		"20260727140002_require_email_validate.sql",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files\n got: %#v\nwant: %#v", names, want)
	}

	// Only the index file gives up its transaction. The validate file needs a
	// transaction of its own, not the absence of one.
	if !strings.Contains(files[want[1]], "NO TRANSACTION") {
		t.Errorf("the index file still needs its directive:\n%s", files[want[1]])
	}
	if strings.Contains(files[want[2]], "NO TRANSACTION") {
		t.Errorf("the validate file must stay transactional:\n%s", files[want[2]])
	}
}

func TestUnblockAdoptionSharesTheValidateTransaction(t *testing.T) {
	// The adoption needs a transaction, so it cannot join the index file that
	// has none — and it takes ACCESS EXCLUSIVE, so within the file it shares it
	// has to come after every scan, for the same reason StageFinish does.
	current := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email"),
			schema.Text("note"),
		)
	})
	target := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique(),
			schema.Text("note"),
		).Check("note_set", `"note" <> ''`)
	})

	parts := migrate.Split(migrate.Migration{
		Version: "1", Name: "tighten",
		Changes: migrate.Unblock(diff(t, current, target)),
	})
	if len(parts) != 3 {
		t.Fatalf("want three files, got %d", len(parts))
	}

	last := parts[2].Changes
	if got := staged(last); len(got) != 2 ||
		!strings.Contains(got[0], "VALIDATE CONSTRAINT") ||
		!strings.Contains(got[1], "USING INDEX") {
		t.Fatalf("the scan must come before the adoption:\n%#v", got)
	}
}

func TestSplitKeepsAMigrationWholeWhenItCan(t *testing.T) {
	// The common case is one file, and it keeps the name it was given rather
	// than being renamed after whichever stage happened to fill it.
	m := migrate.Migration{
		Version: "1", Name: "x",
		Changes: []migrate.Change{{Up: "SELECT 1;"}, {Up: "SELECT 2;"}},
	}
	if got := migrate.Split(m); len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("Split split a single-stage migration: %+v", got)
	}

	// And a migration that is nothing but index changes is still one file.
	only := migrate.Migration{
		Version: "1", Name: "x",
		Changes: []migrate.Change{{Up: "CREATE INDEX CONCURRENTLY a;", Stage: migrate.StageConcurrent}},
	}
	if got := migrate.Split(only); len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("Split renamed a single-file migration: %+v", got)
	}
}
