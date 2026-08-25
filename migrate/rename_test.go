package migrate_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// downs returns the Down of every change in the order a migration applies
// them, which is the reverse of the Up order.
func downs(changes []migrate.Change) []string {
	out := make([]string, 0, len(changes))
	for i := len(changes) - 1; i >= 0; i-- {
		out = append(out, changes[i].Down)
	}
	return out
}

func TestDiffRenameColumn(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("headline").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Nullable().RenamedFrom("headline"),
		)
	})

	c := only(t, diff(t, current, target))
	if c.Up != `ALTER TABLE "posts" RENAME COLUMN "headline" TO "title";` {
		t.Fatalf("Up = %q", c.Up)
	}
	if c.Down != `ALTER TABLE "posts" RENAME COLUMN "title" TO "headline";` {
		t.Fatalf("Down = %q", c.Down)
	}
	// The whole point of the hint: no data moves, so nothing is destructive
	// and nothing needs uncommenting before it will run.
	if c.Destructive {
		t.Error("a rename loses nothing and must not be marked destructive")
	}
	if c.Stage != migrate.StageMain {
		t.Error("a rename is a catalog write and must not force a file split")
	}
}

func TestDiffRenameColumnCarriesItsIndexAndConstraint(t *testing.T) {
	// Without this, renaming a column rebuilds every index over it — which on
	// the tables where a rename is frightening is exactly the cost that makes
	// it frightening. Both the index and the unique constraint keep their
	// definitions and change only their derived names, so both are renamed.
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("headline").Unique(),
			schema.Text("body"),
		).Index("headline", "body")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Unique().RenamedFrom("headline"),
			schema.Text("body"),
		).Index("title", "body")
	})

	got := ups(diff(t, current, target))
	want := []string{
		`ALTER TABLE "posts" RENAME COLUMN "headline" TO "title";`,
		`ALTER TABLE "posts" RENAME CONSTRAINT "posts_headline_key" TO "posts_title_key";`,
		`ALTER INDEX "posts_headline_body_idx" RENAME TO "posts_title_body_idx";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("follow-on renames\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffRenameTable(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("organisations",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("name"),
		).RenamedFrom("orgs")
	})

	changes := diff(t, current, target)
	got := ups(changes)
	// Postgres does not rename a table's constraints along with it, so the
	// primary key would otherwise stay called orgs_pkey forever and show up as
	// a difference in every diff from here on.
	want := []string{
		`ALTER TABLE "orgs" RENAME TO "organisations";`,
		`ALTER TABLE "organisations" RENAME CONSTRAINT "orgs_pkey" TO "organisations_pkey";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("table rename\n got: %#v\nwant: %#v", got, want)
	}

	// The Down runs backwards, so the constraint reverts while the table still
	// answers to its new name, and the table reverts last.
	wantDown := []string{
		`ALTER TABLE "organisations" RENAME CONSTRAINT "organisations_pkey" TO "orgs_pkey";`,
		`ALTER TABLE "organisations" RENAME TO "orgs";`,
	}
	if got := downs(changes); !reflect.DeepEqual(got, wantDown) {
		t.Fatalf("table rename Down\n got: %#v\nwant: %#v", got, wantDown)
	}
}

func TestDiffRenameTableKeepsItsForeignKeys(t *testing.T) {
	// A foreign key follows the table it points at automatically, so the only
	// correct output here is nothing at all for the referencing table.
	decl := func(name string) func(*schema.Registry) {
		return func(r *schema.Registry) {
			orgs := r.Table(name, schema.UUIDv7("id").PrimaryKey())
			if name != "orgs" {
				orgs.RenamedFrom("orgs")
			}
			r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Ref("org", orgs))
		}
	}

	changes := diff(t, build(decl("orgs")), build(decl("organisations")))
	for _, c := range changes {
		if strings.Contains(c.Up, "users_org_id_fkey") && !strings.Contains(c.Up, "RENAME") {
			t.Fatalf("the foreign key must not be rebuilt:\n%s", render(changes))
		}
	}
	if got := ups(changes); !reflect.DeepEqual(got, []string{
		`ALTER TABLE "orgs" RENAME TO "organisations";`,
		`ALTER TABLE "organisations" RENAME CONSTRAINT "orgs_pkey" TO "organisations_pkey";`,
	}) {
		t.Fatalf("unexpected changes:\n%s", render(changes))
	}
}

func TestDiffRenameTableAndItsColumnTogether(t *testing.T) {
	// The table rename has to come first within the phase: everything after it
	// addresses the table by its new name, including the column rename. On the
	// way back the phase reverses too, so the table reverts last.
	current := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("title"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("organisations",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("name").RenamedFrom("title"),
		).RenamedFrom("orgs")
	})

	changes := diff(t, current, target)
	want := []string{
		`ALTER TABLE "orgs" RENAME TO "organisations";`,
		`ALTER TABLE "organisations" RENAME COLUMN "title" TO "name";`,
		`ALTER TABLE "organisations" RENAME CONSTRAINT "orgs_pkey" TO "organisations_pkey";`,
	}
	if got := ups(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("table and column\n got: %#v\nwant: %#v", got, want)
	}
	wantDown := []string{
		`ALTER TABLE "organisations" RENAME CONSTRAINT "organisations_pkey" TO "orgs_pkey";`,
		`ALTER TABLE "organisations" RENAME COLUMN "name" TO "title";`,
		`ALTER TABLE "organisations" RENAME TO "orgs";`,
	}
	if got := downs(changes); !reflect.DeepEqual(got, wantDown) {
		t.Fatalf("table and column, reversed\n got: %#v\nwant: %#v", got, wantDown)
	}
}

func TestDiffRenameReferenceColumn(t *testing.T) {
	// The foreign key's definition names the constrained column, so a rename
	// changes it — but the constraint itself is the same one, and rebuilding it
	// would revalidate every row in both tables.
	decl := func(column string) func(*schema.Registry) {
		return func(r *schema.Registry) {
			orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
			ref := schema.Ref("org", orgs).Named(column)
			if column != "org_id" {
				ref = ref.RenamedFrom("org_id")
			}
			r.Table("users", schema.UUIDv7("id").PrimaryKey(), ref)
		}
	}

	got := ups(diff(t, build(decl("org_id")), build(decl("owner_id"))))
	want := []string{
		`ALTER TABLE "users" RENAME COLUMN "org_id" TO "owner_id";`,
		`ALTER TABLE "users" RENAME CONSTRAINT "users_org_id_fkey" TO "users_owner_id_fkey";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reference rename\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffStaleRenameHintIsIgnored(t *testing.T) {
	// The hint is needed for one release. Once the migration it produced has
	// been applied, the old name is gone everywhere and the hint describes
	// nothing — so leaving it in the schema must not generate a second rename.
	decl := func(r *schema.Registry) {
		r.Table("organisations",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").RenamedFrom("headline"),
		).RenamedFrom("orgs")
	}
	if changes := diff(t, build(decl), build(decl)); len(changes) != 0 {
		t.Fatalf("a stale hint must produce nothing, got:\n%s", render(changes))
	}
}

func TestDiffRenameHintDoesNotSuppressARealAdd(t *testing.T) {
	// A hint whose old column was never there is not a rename of anything. The
	// column is new and has to be added, not silently skipped.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Nullable().RenamedFrom("headline"),
		)
	})

	c := only(t, diff(t, current, target))
	if c.Up != `ALTER TABLE "posts" ADD COLUMN "title" text;` {
		t.Fatalf("Up = %q", c.Up)
	}
}

func TestDiffRenameWithOtherChanges(t *testing.T) {
	// A column can be renamed and altered in the same release. The alter is
	// written against the new name, because it runs after the rename.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("hits").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.BigInt("views").RenamedFrom("hits").Comment("total reads"),
		)
	})

	got := ups(diff(t, current, target))
	want := []string{
		`ALTER TABLE "posts" RENAME COLUMN "hits" TO "views";`,
		`ALTER TABLE "posts" ALTER COLUMN "views" TYPE bigint;`,
		`ALTER TABLE "posts" ALTER COLUMN "views" SET NOT NULL;`,
		`COMMENT ON COLUMN "posts"."views" IS 'total reads';`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rename with other changes\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffRenamePhaseOrdering(t *testing.T) {
	// The phase order is what makes the Down work: everything before the
	// rename is written in the old names, everything after in the new ones,
	// and reversing the list reverses each half while the names it was written
	// against are the ones in effect.
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("headline").Nullable(),
			schema.Text("dead").Nullable(),
		).Check("headline_set", "headline <> ''")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Nullable().RenamedFrom("headline"),
		).Check("title_set", "title <> ''")
	})

	changes := diff(t, current, target)
	got := ups(changes)
	want := []string{
		`ALTER TABLE "posts" DROP CONSTRAINT "headline_set";`,
		`ALTER TABLE "posts" RENAME COLUMN "headline" TO "title";`,
		`ALTER TABLE "posts" DROP COLUMN "dead";`,
		`ALTER TABLE "posts" ADD CONSTRAINT "title_set" CHECK (title <> '');`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rename phase order\n got: %#v\nwant: %#v", got, want)
	}

	wantDown := []string{
		`ALTER TABLE "posts" DROP CONSTRAINT "title_set";`,
		`ALTER TABLE "posts" ADD COLUMN "dead" text;`,
		`ALTER TABLE "posts" RENAME COLUMN "title" TO "headline";`,
		`ALTER TABLE "posts" ADD CONSTRAINT "headline_set" CHECK (headline <> '');`,
	}
	if got := downs(changes); !reflect.DeepEqual(got, wantDown) {
		t.Fatalf("rename phase order, reversed\n got: %#v\nwant: %#v", got, wantDown)
	}
}

func TestDiffRenamedColumnInAHandWrittenCheck(t *testing.T) {
	// A check declared with a free-text expression cannot be rewritten — this
	// package did not author the SQL and will not guess at substituting inside
	// it. So the check is dropped and re-added, which is correct and cheap, and
	// the ordering keeps both halves reversible: the drop is written in the old
	// name and runs before the rename, the add in the new name and after it.
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Int("hits"),
		).Check("hits_positive", "hits >= 0")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Int("views").RenamedFrom("hits"),
		).Check("hits_positive", "views >= 0")
	})

	got := ups(diff(t, current, target))
	want := []string{
		`ALTER TABLE "posts" DROP CONSTRAINT "hits_positive";`,
		`ALTER TABLE "posts" RENAME COLUMN "hits" TO "views";`,
		`ALTER TABLE "posts" ADD CONSTRAINT "hits_positive" CHECK (views >= 0);`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hand-written check\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffRenamedColumnInAQuotedCheck(t *testing.T) {
	// A double-quoted token in Postgres is always an identifier — a literal is
	// single-quoted — so a check that spells its columns in quotes can be
	// rewritten exactly, and follows the rename instead of being rebuilt.
	table := func(col string) func(*schema.Registry) {
		return func(r *schema.Registry) {
			f := schema.Int(col)
			if col != "hits" {
				f = f.RenamedFrom("hits")
			}
			r.Table("posts", schema.UUIDv7("id").PrimaryKey(), f).
				Check(col+"_positive", `"`+col+`" >= 0`)
		}
	}

	got := ups(diff(t, build(table("hits")), build(table("views"))))
	want := []string{
		`ALTER TABLE "posts" RENAME COLUMN "hits" TO "views";`,
		`ALTER TABLE "posts" RENAME CONSTRAINT "hits_positive" TO "views_positive";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quoted check\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffConstraintRenamedByPinning(t *testing.T) {
	// Nothing here is renamed in the schema sense: the constraint's definition
	// is unchanged and only its name was pinned differently. Renaming it beats
	// dropping and re-adding it, which would revalidate every row.
	current := build(func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Unique())
	})
	target := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique().ConstraintNamed("uq_user_email"),
		)
	})

	c := only(t, diff(t, current, target))
	if c.Up != `ALTER TABLE "users" RENAME CONSTRAINT "users_email_key" TO "uq_user_email";` {
		t.Fatalf("Up = %q", c.Up)
	}
}

func TestDiffRenameInAModule(t *testing.T) {
	// RenamedFrom takes the local name and is qualified with the same prefix as
	// the current one, so a rename inside a module does not have to repeat it.
	current := func() *schema.Registry {
		r := schema.NewModule("billing")
		r.Table("invoices", schema.UUIDv7("id").PrimaryKey())
		return r
	}()
	target := func() *schema.Registry {
		r := schema.NewModule("billing")
		r.Table("bills", schema.UUIDv7("id").PrimaryKey()).RenamedFrom("invoices")
		return r
	}()

	got := ups(diff(t, current, target))
	want := []string{
		`ALTER TABLE "billing_invoices" RENAME TO "billing_bills";`,
		`ALTER TABLE "billing_bills" RENAME CONSTRAINT "billing_invoices_pkey" TO "billing_bills_pkey";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module rename\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffRenameRendersAsAMigration(t *testing.T) {
	// A rename is not concurrent, so the whole thing stays in one file and
	// inside one transaction — which is the difference between a rename that
	// can be rolled back and one that cannot.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("headline").Unique())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Unique().RenamedFrom("headline"),
		)
	})

	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000",
		Name:    "rename_headline",
		Changes: diff(t, current, target),
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want one file, got %d: %v", len(files), keysOf(files))
	}
	body := files["20260727120000_rename_headline.sql"]
	if strings.Contains(body, "NO TRANSACTION") {
		t.Errorf("a rename needs no NO TRANSACTION directive:\n%s", body)
	}
	if strings.Contains(body, "DESTRUCTIVE") {
		t.Errorf("a rename is not destructive and must not render commented out:\n%s", body)
	}
	for _, want := range []string{
		`ALTER TABLE "posts" RENAME COLUMN "headline" TO "title";`,
		`ALTER TABLE "posts" RENAME COLUMN "title" TO "headline";`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in:\n%s", want, body)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
