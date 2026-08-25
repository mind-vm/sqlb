package migrate_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// build makes a registry from a declaration function, so a test can state a
// before and an after state side by side.
func build(decl func(r *schema.Registry)) *schema.Registry {
	r := schema.NewRegistry()
	decl(r)
	return r
}

func diff(t *testing.T, current, target *schema.Registry) []migrate.Change {
	t.Helper()
	changes, err := migrate.Diff(current, target)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return changes
}

// only returns the single change a test expects, failing with the whole set if
// there is not exactly one — which is more useful than an index out of range.
func only(t *testing.T, changes []migrate.Change) migrate.Change {
	t.Helper()
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d:\n%s", len(changes), render(changes))
	}
	return changes[0]
}

// find returns the one change whose Up contains substr.
func find(t *testing.T, changes []migrate.Change, substr string) migrate.Change {
	t.Helper()
	var hits []migrate.Change
	for _, c := range changes {
		if strings.Contains(c.Up, substr) {
			hits = append(hits, c)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 change containing %q, got %d:\n%s", substr, len(hits), render(changes))
	}
	return hits[0]
}

func render(changes []migrate.Change) string {
	var b strings.Builder
	for i, c := range changes {
		b.WriteString(strings.Repeat(" ", 2))
		b.WriteString(strings.TrimSpace(c.Up))
		if c.Destructive {
			b.WriteString("   [destructive: " + c.Reason + "]")
		}
		switch c.Stage {
		case migrate.StageValidate:
			b.WriteString("   [validate]")
		case migrate.StageConcurrent:
			b.WriteString("   [concurrent]")
		}
		if i < len(changes)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// renderFiles renders changes the way a project would, so that a test can ask
// what the generated files actually execute rather than what the changes say.
func renderFiles(t *testing.T, changes []migrate.Change) map[string]string {
	t.Helper()
	files, err := migrate.Render(migrate.Migration{
		Version: "20260727120000",
		Name:    "generated",
		Changes: changes,
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return files
}

// liveSQL returns the lines of a rendered file that a runner would execute:
// everything neither blank nor commented out, which includes goose's own
// annotations.
func liveSQL(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func ups(changes []migrate.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Up
	}
	return out
}

// Common fixtures.

func orgsAndUsers(r *schema.Registry) {
	orgs := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
	)
	r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
	)
}

func TestDiffEmpty(t *testing.T) {
	if changes := diff(t, nil, nil); len(changes) != 0 {
		t.Fatalf("empty to empty should produce nothing, got:\n%s", render(changes))
	}
}

func TestDiffUnchanged(t *testing.T) {
	// Two registries built from the same declaration are structurally equal,
	// so a diff between them must be empty. This is the property that makes a
	// generator safe to run repeatedly: regenerating an unchanged schema
	// produces no migration at all.
	changes := diff(t, build(orgsAndUsers), build(orgsAndUsers))
	if len(changes) != 0 {
		t.Fatalf("identical schemas should produce nothing, got:\n%s", render(changes))
	}
}

func TestDiffCreateTable(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("slug").Unique(),
			schema.Varchar("title", 200),
			schema.Int("views").Default(schema.Value(0)),
			schema.Timestamp("published_at").Nullable(),
			schema.Enum("status", "draft", "live"),
		).Check("views_non_negative", `"views" >= 0`)
	})

	c := only(t, diff(t, nil, target))

	want := `CREATE TABLE "posts" (
    "id" uuid NOT NULL DEFAULT uuid_generate_v7(),
    "slug" text NOT NULL,
    "title" varchar(200) NOT NULL,
    "views" integer NOT NULL DEFAULT 0,
    "published_at" timestamptz,
    "status" text NOT NULL,
    CONSTRAINT "posts_pkey" PRIMARY KEY ("id"),
    CONSTRAINT "posts_slug_key" UNIQUE ("slug"),
    CONSTRAINT "posts_status_check" CHECK ("status" IN ('draft', 'live')),
    CONSTRAINT "views_non_negative" CHECK ("views" >= 0)
);`
	if c.Up != want {
		t.Fatalf("CREATE TABLE mismatch\n got:\n%s\nwant:\n%s", c.Up, want)
	}
	if c.Down != `DROP TABLE "posts";` {
		t.Fatalf("Down = %q", c.Down)
	}
	if c.Destructive {
		t.Fatal("creating a table is not destructive")
	}
}

func TestDiffCreateTableComments(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("body").Comment("markdown source"),
		).Describe("published articles")
	})

	c := only(t, diff(t, nil, target))
	for _, want := range []string{
		`COMMENT ON TABLE "posts" IS 'published articles';`,
		`COMMENT ON COLUMN "posts"."body" IS 'markdown source';`,
	} {
		if !strings.Contains(c.Up, want) {
			t.Errorf("missing %s in:\n%s", want, c.Up)
		}
	}
}

func TestDiffCreateTableModulePrefix(t *testing.T) {
	// The storage name is what reaches SQL, so the diff must compare and
	// render prefixed names rather than the local ones.
	target := schema.NewModule("billing")
	target.Table("invoices", schema.UUIDv7("id").PrimaryKey())

	c := only(t, diff(t, nil, target))
	if !strings.Contains(c.Up, `CREATE TABLE "billing_invoices"`) {
		t.Fatalf("want prefixed table name, got:\n%s", c.Up)
	}
	if !strings.Contains(c.Up, `CONSTRAINT "billing_invoices_pkey"`) {
		t.Fatalf("want prefixed constraint name, got:\n%s", c.Up)
	}
}

func TestDiffCreateTableForeignKeyIsSeparate(t *testing.T) {
	// A foreign key is never inlined into CREATE TABLE. That is what lets
	// tables be created in any order without a dependency sort, and it means
	// one code path adds a reference whether the table is new or not.
	changes := diff(t, nil, build(orgsAndUsers))

	fk := find(t, changes, "FOREIGN KEY")
	want := `ALTER TABLE "users" ADD CONSTRAINT "users_org_id_fkey" ` +
		`FOREIGN KEY ("org_id") REFERENCES "orgs" ("id") ON DELETE CASCADE;`
	if fk.Up != want {
		t.Fatalf("foreign key SQL\n got: %s\nwant: %s", fk.Up, want)
	}

	// It must land after both tables exist.
	order := ups(changes)
	fkAt, usersAt := indexOf(order, "FOREIGN KEY"), indexOf(order, `CREATE TABLE "users"`)
	orgsAt := indexOf(order, `CREATE TABLE "orgs"`)
	if fkAt < usersAt || fkAt < orgsAt {
		t.Fatalf("foreign key added before its tables exist:\n%s", render(changes))
	}
}

func TestDiffForeignKeyDefaultActionsOmitted(t *testing.T) {
	// NO ACTION is the Postgres default. Emitting it would make an imported
	// schema differ from the database it was imported from.
	target := build(func(r *schema.Registry) {
		orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Ref("org", orgs))
	})
	fk := find(t, diff(t, nil, target), "FOREIGN KEY")
	if strings.Contains(fk.Up, "NO ACTION") {
		t.Fatalf("NO ACTION should not be rendered: %s", fk.Up)
	}
}

func TestDiffExternalRefHasNoForeignKey(t *testing.T) {
	// An external reference is a column, never a constraint: module isolation
	// depends on there being nothing to migrate across the boundary (ADR-0015).
	target := build(func(r *schema.Registry) {
		r.Table("invoices",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("tenant", "tenants.id"),
		)
	})
	changes := diff(t, nil, target)
	for _, c := range changes {
		if strings.Contains(c.Up, "FOREIGN KEY") {
			t.Fatalf("external reference produced a foreign key:\n%s", c.Up)
		}
		if strings.Contains(c.Up, "CREATE INDEX") {
			t.Fatalf("an index nobody declared:\n%s", c.Up)
		}
	}
}

// The index it does get is the one it asked for. Indexed is a declaration, and
// this is the whole of what it changes in a migration.
func TestDiffIndexedColumnCreatesItsIndex(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("invoices",
			schema.UUIDv7("id").PrimaryKey(),
			schema.ExternalRef("tenant", "tenants.id").Indexed(),
		)
	})
	find(t, diff(t, nil, target), `CREATE INDEX "invoices_tenant_id_idx"`)
}

func TestDiffNewTableIndexIsNotConcurrent(t *testing.T) {
	// A table created in the same migration is empty, so its indexes cannot
	// contend with anything. Requiring CONCURRENTLY would force the migration
	// into a second file for no benefit.
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("slug"),
		).Index("slug")
	})

	idx := find(t, diff(t, nil, target), "CREATE INDEX")
	if idx.Stage == migrate.StageConcurrent {
		t.Fatal("index on a newly created table should not be concurrent")
	}
	if strings.Contains(idx.Up, "CONCURRENTLY") {
		t.Fatalf("unexpected CONCURRENTLY: %s", idx.Up)
	}
	if idx.Up != `CREATE INDEX "posts_slug_idx" ON "posts" ("slug");` {
		t.Fatalf("index SQL: %s", idx.Up)
	}
}

func TestDiffExistingTableIndexIsConcurrent(t *testing.T) {
	// The table already holds rows, so building the index without
	// CONCURRENTLY would lock it against writes for the duration.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("slug")).Index("slug")
	})

	idx := only(t, diff(t, current, target))
	if idx.Stage != migrate.StageConcurrent {
		t.Fatal("index on an existing table must be concurrent")
	}
	if idx.Up != `CREATE INDEX CONCURRENTLY "posts_slug_idx" ON "posts" ("slug");` {
		t.Fatalf("index SQL: %s", idx.Up)
	}
	if idx.Down != `DROP INDEX CONCURRENTLY "posts_slug_idx";` {
		t.Fatalf("index Down: %s", idx.Down)
	}
	// A CONCURRENTLY build that fails against existing data leaves an invalid
	// index behind instead of rolling back — issue #242 — so the comment has
	// to say so, and say what to run (this change's own Down) before an
	// operator reissues the identical Up and gets "already exists" instead of
	// the real error.
	if !strings.Contains(idx.Comment, "leaves an invalid index behind") {
		t.Fatalf("comment should warn that a failed CONCURRENTLY build leaves an invalid index behind: %q", idx.Comment)
	}
	if !strings.Contains(idx.Comment, "Down before retrying") {
		t.Fatalf("comment should point at this change's Down as the fix: %q", idx.Comment)
	}
}

func TestDiffIndexRedefinedIsDropAndCreate(t *testing.T) {
	// Changing an index under the same name has to be a drop and a create;
	// recognising it as one change rather than two unrelated ones is what
	// keeps the drop ordered before the create.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(),
			schema.Text("slug"), schema.Text("author")).
			AddIndex(schema.Index{Name: "posts_lookup", Columns: []string{"slug"}})
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(),
			schema.Text("slug"), schema.Text("author")).
			AddIndex(schema.Index{Name: "posts_lookup", Columns: []string{"slug", "author"}, Unique: true})
	})

	changes := diff(t, current, target)
	if len(changes) != 2 {
		t.Fatalf("want a drop and a create, got:\n%s", render(changes))
	}
	if !strings.HasPrefix(changes[0].Up, "DROP INDEX") {
		t.Fatalf("drop must come first:\n%s", render(changes))
	}
	if changes[1].Up != `CREATE UNIQUE INDEX CONCURRENTLY "posts_lookup" ON "posts" ("slug", "author");` {
		t.Fatalf("create SQL: %s", changes[1].Up)
	}
}

func TestDiffPartialAndMethodIndex(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.JSON("meta")).
			AddIndex(schema.Index{
				Name:    "posts_meta_gin",
				Columns: []string{"meta"},
				Method:  "gin",
				Where:   `"meta" IS NOT NULL`,
			})
	})
	idx := find(t, diff(t, nil, target), "posts_meta_gin")
	want := `CREATE INDEX "posts_meta_gin" ON "posts" USING gin ("meta") WHERE "meta" IS NOT NULL;`
	if idx.Up != want {
		t.Fatalf("\n got: %s\nwant: %s", idx.Up, want)
	}
}

// An index column can carry its own ordering, because for some indexes the
// ordering *is* the index: one backing `ORDER BY position ASC NULLS FIRST,
// created_at DESC` is unusable in any other order. Before this the DDL got the
// columns right and the rest wrong, so the diff proposed dropping the live
// index (issue #64).
func TestDiffOrderedIndex(t *testing.T) {
	ordered := func(orders map[string]schema.IndexOrder) *schema.Registry {
		return build(func(r *schema.Registry) {
			r.Table("tasks",
				schema.UUIDv7("id").PrimaryKey(),
				schema.UUIDv7("project_id"),
				schema.Int("position").Nullable(),
				schema.Timestamp("created_at"),
			).AddIndex(schema.Index{
				Name:    "tasks_feed_idx",
				Columns: []string{"project_id", "position", "created_at"},
				Orders:  orders,
			})
		})
	}

	target := ordered(map[string]schema.IndexOrder{
		"position":   {Nulls: schema.NullsFirst},
		"created_at": {Desc: true},
	})
	idx := find(t, diff(t, nil, target), "tasks_feed_idx")
	want := `CREATE INDEX "tasks_feed_idx" ON "tasks" ` +
		`("project_id", "position" NULLS FIRST, "created_at" DESC);`
	if idx.Up != want {
		t.Fatalf("\n got: %s\nwant: %s", idx.Up, want)
	}

	// The ordering is part of the index's identity: changing it is a replace,
	// not a no-op. Without this in the fingerprint a reordered index compared
	// equal and stayed as it was.
	changed := ordered(map[string]schema.IndexOrder{
		"position":   {Nulls: schema.NullsFirst},
		"created_at": {},
	})
	if got := diff(t, target, changed); len(got) == 0 {
		t.Error("changing a column's sort order produced no change")
	}

	// And the normalisation, in both directions. Postgres omits a null
	// placement that follows from the direction, so a declaration spelling it
	// out and one leaving it implicit are the same index — proposing to replace
	// one with the other is the cosmetic-diff failure issue #63 is about.
	explicit := ordered(map[string]schema.IndexOrder{
		"position":   {Nulls: schema.NullsFirst},
		"created_at": {Desc: true, Nulls: schema.NullsFirst},
	})
	if got := diff(t, target, explicit); len(got) != 0 {
		t.Errorf("two spellings of the same ordering proposed a change:\n%s", render(got))
	}
}

func TestDiffDropTable(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("legacy", schema.UUIDv7("id").PrimaryKey(), schema.Text("note"))
	})

	c := only(t, diff(t, current, nil))
	if c.Up != `DROP TABLE "legacy";` {
		t.Fatalf("Up = %q", c.Up)
	}
	if !c.Destructive {
		t.Fatal("dropping a table must be destructive")
	}
	if c.Reason == "" {
		t.Fatal("a destructive change must give a reason")
	}
	// The Down restores the structure. It cannot restore the rows, and the
	// Reason says so rather than the Down pretending otherwise.
	if !strings.HasPrefix(c.Down, `CREATE TABLE "legacy"`) {
		t.Fatalf("Down = %q", c.Down)
	}
}

func TestDiffAddColumn(t *testing.T) {
	base := func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey())
	}

	cases := []struct {
		name        string
		column      *schema.Field
		wantUp      string
		destructive bool
	}{{
		name:   "nullable",
		column: schema.Text("subtitle").Nullable(),
		wantUp: `ALTER TABLE "posts" ADD COLUMN "subtitle" text;`,
	}, {
		name:   "not null with default",
		column: schema.BigInt("views").Default(schema.Value(0)),
		wantUp: `ALTER TABLE "posts" ADD COLUMN "views" bigint NOT NULL DEFAULT 0;`,
	}, {
		// Postgres 11+ adds a NOT NULL column with a default without a table
		// rewrite, but one without a default is simply rejected on any table
		// that has rows.
		name:        "not null without default",
		column:      schema.Text("title"),
		wantUp:      `ALTER TABLE "posts" ADD COLUMN "title" text NOT NULL;`,
		destructive: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := build(func(r *schema.Registry) {
				r.Table("posts", schema.UUIDv7("id").PrimaryKey(), tc.column)
			})
			c := only(t, diff(t, build(base), target))
			if c.Up != tc.wantUp {
				t.Fatalf("\n got: %s\nwant: %s", c.Up, tc.wantUp)
			}
			if c.Destructive != tc.destructive {
				t.Fatalf("Destructive = %v, want %v", c.Destructive, tc.destructive)
			}
			if c.Destructive && c.Reason == "" {
				t.Fatal("destructive change without a reason")
			}
		})
	}
}

// slugAndItsDependents is the schema shape that produced the bug these three
// tests cover: a column added NOT NULL with no default, carrying a unique
// constraint, an index and a hand-written CHECK. The add renders commented out,
// so none of the other three can run until somebody uncomments it.
func slugAndItsDependents(slug *schema.Field) func(r *schema.Registry) {
	return func(r *schema.Registry) {
		r.Table("orgs",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("name"),
			slug,
		).Index("slug").Check("slug_not_blank", `"slug" <> ''`)
	}
}

func TestDiffDefersWhatACommentedOutColumnCarries(t *testing.T) {
	base := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	})
	changes := diff(t, base, build(slugAndItsDependents(schema.Text("slug").Unique())))

	add := find(t, changes, `ADD COLUMN "slug"`)
	if !add.Destructive {
		t.Fatal("adding a NOT NULL column with no default must be destructive")
	}
	if add.DependsOn != "" {
		t.Errorf("the add itself waits for nothing: %q", add.DependsOn)
	}

	// Each of these names the column the commented-out statement would have
	// added, so each has to be commented out with it. The note says how well
	// that is known: the column list of a UNIQUE or an index is built here and
	// is exact, while a hand-written CHECK expression is not read at all, so it
	// waits on the possibility rather than on a match.
	for _, tc := range []struct{ substr, prefix string }{
		{`ADD CONSTRAINT "orgs_slug_key"`, `"orgs"."slug"`},
		{`CREATE INDEX CONCURRENTLY "orgs_slug_idx"`, `"orgs"."slug"`},
		{`ADD CONSTRAINT "slug_not_blank"`, `possibly "orgs"."slug"`},
	} {
		c := find(t, changes, tc.substr)
		if c.DependsOn == "" {
			t.Errorf("%s must wait for the commented-out column add", tc.substr)
			continue
		}
		if !strings.HasPrefix(c.DependsOn, tc.prefix) {
			t.Errorf("%s should say it waits for %s, got: %s", tc.substr, tc.prefix, c.DependsOn)
		}
	}

	// The whole point: the migration is a no-op until reviewed, rather than a
	// file that fails partway through.
	for name, body := range renderFiles(t, changes) {
		if live := liveSQL(body); len(live) > 0 {
			t.Errorf("%s still executes %d statement(s) that depend on a commented-out "+
				"column:\n%s", name, len(live), strings.Join(live, "\n"))
		}
	}
}

// TestDiffDefersNothingWhenTheColumnIsLive is the other direction, without
// which the test above proves only that something is commented out (ADR-0016).
// The same schema with a nullable column has no commented-out add, so nothing
// waits for one and every statement is emitted live.
func TestDiffDefersNothingWhenTheColumnIsLive(t *testing.T) {
	base := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	})
	changes := diff(t, base, build(slugAndItsDependents(schema.Text("slug").Unique().Nullable())))

	for _, c := range changes {
		if c.DependsOn != "" {
			t.Errorf("nothing is commented out here, so nothing waits: %s\n%s", c.Up, c.DependsOn)
		}
	}
	for name, body := range renderFiles(t, changes) {
		if len(liveSQL(body)) == 0 {
			t.Errorf("%s should hold live SQL:\n%s", name, body)
		}
	}
}

// TestDiffDefersOnlyWhatNamesTheColumn. The dependency is on a column, not on a
// table: a change made in the same migration that does not name the pending one
// stays live, or a single destructive add would freeze everything around it.
func TestDiffDefersOnlyWhatNamesTheColumn(t *testing.T) {
	base := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	})
	target := build(func(r *schema.Registry) {
		r.Table("orgs",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("name"),
			schema.Text("slug").Unique(),     // NOT NULL, no default: commented out
			schema.Text("region").Nullable(), // ordinary, and indexed below
		).Index("name").Index("region")
	})
	changes := diff(t, base, target)

	if c := find(t, changes, `"orgs_slug_key"`); c.DependsOn == "" {
		t.Error("the constraint over the pending column must wait for it")
	}
	for _, substr := range []string{`"orgs_name_idx"`, `"orgs_region_idx"`, `ADD COLUMN "region"`} {
		if c := find(t, changes, substr); c.DependsOn != "" {
			t.Errorf("%s does not name the pending column and must stay live: %s", substr, c.DependsOn)
		}
	}
}

func TestDiffDropColumn(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("legacy_slug").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey())
	})

	c := only(t, diff(t, current, target))
	if c.Up != `ALTER TABLE "posts" DROP COLUMN "legacy_slug";` {
		t.Fatalf("Up = %q", c.Up)
	}
	if !c.Destructive || c.Reason == "" {
		t.Fatalf("dropping a column must be destructive with a reason: %+v", c)
	}
	if c.Down != `ALTER TABLE "posts" ADD COLUMN "legacy_slug" text;` {
		t.Fatalf("Down = %q", c.Down)
	}
}

func TestDiffRenameIsDropAndAdd(t *testing.T) {
	// A rename cannot be told from a drop and an add when only the before and
	// after states are known. Emitting drop-and-add is lossy but never
	// silently wrong, and the destructive guard keeps it commented out.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("headline").Nullable())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Text("title").Nullable())
	})

	changes := diff(t, current, target)
	add := find(t, changes, `ADD COLUMN "title"`)
	drop := find(t, changes, `DROP COLUMN "headline"`)
	if add.Destructive {
		t.Error("the add half is not destructive")
	}
	if !drop.Destructive {
		t.Error("the drop half must be destructive")
	}
	if indexOf(ups(changes), `ADD COLUMN "title"`) > indexOf(ups(changes), `DROP COLUMN "headline"`) {
		t.Fatalf("the add must precede the drop:\n%s", render(changes))
	}
}

func TestDiffColumnType(t *testing.T) {
	cases := []struct {
		name        string
		from, to    *schema.Field
		wantType    string
		destructive bool
	}{
		{"smallint to int widens", schema.SmallInt("n"), schema.Int("n"), "integer", false},
		{"smallint to bigint widens", schema.SmallInt("n"), schema.BigInt("n"), "bigint", false},
		{"int to smallint narrows", schema.Int("n"), schema.SmallInt("n"), "smallint", true},
		{"int to bigint widens", schema.Int("n"), schema.BigInt("n"), "bigint", false},
		{"bigint to int narrows", schema.BigInt("n"), schema.Int("n"), "integer", true},
		{"int to numeric widens", schema.Int("n"), schema.Numeric("n"), "numeric", false},
		{"real to double widens", schema.Real("n"), schema.Float("n"), "double precision", false},
		{"double to real narrows", schema.Float("n"), schema.Real("n"), "real", true},
		// real to numeric is a cast Postgres will make and a conversion nobody
		// asked for: an approximate binary float becomes an exact decimal, so
		// the value that comes back is the rounded expansion of the stored
		// approximation. Destructive, so the migration is generated commented
		// out and a person decides.
		{"real to numeric is not a widening", schema.Real("n"), schema.Numeric("n"), "numeric", true},
		{"varchar to text widens", schema.Varchar("n", 50), schema.Text("n"), "text", false},
		{"text to varchar narrows", schema.Text("n"), schema.Varchar("n", 50), "varchar(50)", true},
		{"longer varchar widens", schema.Varchar("n", 50), schema.Varchar("n", 100), "varchar(100)", false},
		{"shorter varchar narrows", schema.Varchar("n", 100), schema.Varchar("n", 50), "varchar(50)", true},
		{"unrelated type narrows", schema.Text("n"), schema.JSON("n"), "jsonb", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := func(f *schema.Field) func(*schema.Registry) {
				return func(r *schema.Registry) {
					r.Table("t", schema.UUIDv7("id").PrimaryKey(), f)
				}
			}
			c := only(t, diff(t, build(table(tc.from)), build(table(tc.to))))
			want := `ALTER TABLE "t" ALTER COLUMN "n" TYPE ` + tc.wantType + `;`
			if c.Up != want {
				t.Fatalf("\n got: %s\nwant: %s", c.Up, want)
			}
			if c.Destructive != tc.destructive {
				t.Fatalf("Destructive = %v, want %v", c.Destructive, tc.destructive)
			}
			// No USING clause is generated: Postgres refusing a cast it cannot
			// make implicitly is the correct outcome, and a generated USING
			// would silently truncate on the narrowing cases.
			if strings.Contains(c.Up, "USING") {
				t.Fatalf("USING should never be generated: %s", c.Up)
			}
		})
	}
}

func TestDiffNullability(t *testing.T) {
	table := func(f *schema.Field) func(*schema.Registry) {
		return func(r *schema.Registry) {
			r.Table("t", schema.UUIDv7("id").PrimaryKey(), f)
		}
	}

	t.Run("set not null", func(t *testing.T) {
		c := only(t, diff(t,
			build(table(schema.Text("n").Nullable())),
			build(table(schema.Text("n")))))
		if c.Up != `ALTER TABLE "t" ALTER COLUMN "n" SET NOT NULL;` {
			t.Fatalf("Up = %q", c.Up)
		}
		// The fix for a failure here is a backfill, not a retry, so it is
		// worth stopping a reviewer on.
		if !c.Destructive || c.Reason == "" {
			t.Fatalf("SET NOT NULL must be flagged with a reason: %+v", c)
		}
	})

	t.Run("drop not null", func(t *testing.T) {
		c := only(t, diff(t,
			build(table(schema.Text("n"))),
			build(table(schema.Text("n").Nullable()))))
		if c.Up != `ALTER TABLE "t" ALTER COLUMN "n" DROP NOT NULL;` {
			t.Fatalf("Up = %q", c.Up)
		}
		if c.Destructive {
			t.Fatal("relaxing a constraint loses nothing")
		}
	})
}

func TestDiffDefault(t *testing.T) {
	table := func(f *schema.Field) func(*schema.Registry) {
		return func(r *schema.Registry) {
			r.Table("t", schema.UUIDv7("id").PrimaryKey(), f)
		}
	}

	cases := []struct {
		name     string
		from, to *schema.Field
		wantUp   string
		wantDown string
	}{{
		name:     "added",
		from:     schema.Int("n").Nullable(),
		to:       schema.Int("n").Nullable().Default(schema.Value(7)),
		wantUp:   `ALTER TABLE "t" ALTER COLUMN "n" SET DEFAULT 7;`,
		wantDown: `ALTER TABLE "t" ALTER COLUMN "n" DROP DEFAULT;`,
	}, {
		name:     "removed",
		from:     schema.Int("n").Nullable().Default(schema.Value(7)),
		to:       schema.Int("n").Nullable(),
		wantUp:   `ALTER TABLE "t" ALTER COLUMN "n" DROP DEFAULT;`,
		wantDown: `ALTER TABLE "t" ALTER COLUMN "n" SET DEFAULT 7;`,
	}, {
		name:     "changed to an expression",
		from:     schema.Timestamp("n").Nullable(),
		to:       schema.Timestamp("n").Nullable().Default(schema.Now()),
		wantUp:   `ALTER TABLE "t" ALTER COLUMN "n" SET DEFAULT now();`,
		wantDown: `ALTER TABLE "t" ALTER COLUMN "n" DROP DEFAULT;`,
	}, {
		name:     "string literal is escaped",
		from:     schema.Text("n").Nullable(),
		to:       schema.Text("n").Nullable().Default(schema.Value("it's")),
		wantUp:   `ALTER TABLE "t" ALTER COLUMN "n" SET DEFAULT 'it''s';`,
		wantDown: `ALTER TABLE "t" ALTER COLUMN "n" DROP DEFAULT;`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := only(t, diff(t, build(table(tc.from)), build(table(tc.to))))
			if c.Up != tc.wantUp {
				t.Fatalf("Up\n got: %s\nwant: %s", c.Up, tc.wantUp)
			}
			if c.Down != tc.wantDown {
				t.Fatalf("Down\n got: %s\nwant: %s", c.Down, tc.wantDown)
			}
			if c.Destructive {
				t.Fatal("changing a default touches no existing row")
			}
			// Which is exactly what a reviewer needs told.
			if !strings.Contains(c.Comment, "not backfilled") {
				t.Fatalf("comment should warn that existing rows keep their values: %q", c.Comment)
			}
		})
	}
}

func TestDiffUniqueConstraint(t *testing.T) {
	plain := func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email"))
	}
	unique := func(r *schema.Registry) {
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Unique())
	}

	t.Run("added", func(t *testing.T) {
		c := only(t, diff(t, build(plain), build(unique)))
		want := `ALTER TABLE "users" ADD CONSTRAINT "users_email_key" UNIQUE ("email");`
		if c.Up != want {
			t.Fatalf("\n got: %s\nwant: %s", c.Up, want)
		}
		if c.Destructive {
			t.Fatal("adding a unique constraint fails loudly, it does not lose data")
		}
		if !strings.Contains(c.Comment, "existing rows") {
			t.Fatalf("comment should warn about existing rows: %q", c.Comment)
		}
	})

	t.Run("dropped", func(t *testing.T) {
		c := only(t, diff(t, build(unique), build(plain)))
		if c.Up != `ALTER TABLE "users" DROP CONSTRAINT "users_email_key";` {
			t.Fatalf("Up = %q", c.Up)
		}
		if c.Down != `ALTER TABLE "users" ADD CONSTRAINT "users_email_key" UNIQUE ("email");` {
			t.Fatalf("Down = %q", c.Down)
		}
	})
}

func TestDiffPinnedConstraintNames(t *testing.T) {
	// Adopting an existing database depends on this: a schema whose
	// constraint names do not match the ones already there would produce a
	// diff that drops and recreates every constraint on the first run.
	current := build(func(r *schema.Registry) {
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email").Unique().ConstraintNamed("uq_user_email"),
		).PrimaryKeyNamed("users_id_pk")
	})
	if changes := diff(t, current, current); len(changes) != 0 {
		t.Fatalf("a schema against itself must be empty, got:\n%s", render(changes))
	}

	c := only(t, diff(t, nil, current))
	for _, want := range []string{`CONSTRAINT "users_id_pk" PRIMARY KEY`, `CONSTRAINT "uq_user_email" UNIQUE`} {
		if !strings.Contains(c.Up, want) {
			t.Errorf("missing %s in:\n%s", want, c.Up)
		}
	}
}

func TestDiffEnumValues(t *testing.T) {
	table := func(values ...string) func(*schema.Registry) {
		return func(r *schema.Registry) {
			r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Enum("status", values...))
		}
	}

	t.Run("value added", func(t *testing.T) {
		changes := diff(t, build(table("draft", "live")), build(table("draft", "live", "archived")))
		if len(changes) != 2 {
			t.Fatalf("want a drop and an add, got:\n%s", render(changes))
		}
		if changes[0].Up != `ALTER TABLE "posts" DROP CONSTRAINT "posts_status_check";` {
			t.Fatalf("drop first: %s", changes[0].Up)
		}
		want := `ALTER TABLE "posts" ADD CONSTRAINT "posts_status_check" ` +
			`CHECK ("status" IN ('draft', 'live', 'archived'));`
		if changes[1].Up != want {
			t.Fatalf("\n got: %s\nwant: %s", changes[1].Up, want)
		}
		if strings.Contains(changes[1].Comment, "no longer permits") {
			t.Fatalf("nothing was removed: %q", changes[1].Comment)
		}
	})

	t.Run("value removed", func(t *testing.T) {
		changes := diff(t, build(table("draft", "live", "archived")), build(table("draft", "live")))
		add := find(t, changes, "ADD CONSTRAINT")
		// Removing a value cannot lose data — Postgres rejects the statement —
		// but the fix is in the rows, not in the schema, so it is named.
		if add.Destructive {
			t.Fatal("a rejected statement is not data loss")
		}
		if !strings.Contains(add.Comment, `no longer permits 'archived'`) {
			t.Fatalf("comment should name the removed value: %q", add.Comment)
		}
	})
}

func TestDiffForeignKeyOrdering(t *testing.T) {
	// A foreign key depends on the unique or primary key it points at, so its
	// drop must come before that constraint's, and its add after.
	current := build(orgsAndUsers)
	target := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
		r.Table("users", schema.UUIDv7("id").PrimaryKey(), schema.Text("email").Unique())
	})

	changes := diff(t, current, target)
	order := ups(changes)
	fkDrop := indexOf(order, `DROP CONSTRAINT "users_org_id_fkey"`)
	colDrop := indexOf(order, `DROP COLUMN "org_id"`)
	if fkDrop == -1 || colDrop == -1 {
		t.Fatalf("expected both a constraint drop and a column drop:\n%s", render(changes))
	}
	if fkDrop > colDrop {
		t.Fatalf("the foreign key must be dropped before its column:\n%s", render(changes))
	}
}

func TestDiffPhaseOrdering(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("legacy").Nullable(),
			schema.Text("kept").Nullable(),
		).Index("kept")
		r.Table("tags", schema.UUIDv7("id").PrimaryKey())
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Nullable(),
			schema.Text("kept").Nullable(),
		)
		r.Table("authors", schema.UUIDv7("id").PrimaryKey())
	})

	got := ups(diff(t, current, target))
	want := []string{
		`CREATE TABLE "authors" (` + "\n" +
			`    "id" uuid NOT NULL DEFAULT uuid_generate_v7(),` + "\n" +
			`    CONSTRAINT "authors_pkey" PRIMARY KEY ("id")` + "\n" + `);`,
		`DROP INDEX CONCURRENTLY "posts_kept_idx";`,
		`ALTER TABLE "posts" ADD COLUMN "title" text;`,
		`ALTER TABLE "posts" DROP COLUMN "legacy";`,
		`DROP TABLE "tags";`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phase order\n got: %#v\nwant: %#v", got, want)
	}
}

func TestDiffIndexOverADroppedColumnStaysInTheSameFile(t *testing.T) {
	// A concurrent index change is split into a file that runs after the one
	// holding the column drop — by which time Postgres has already dropped the
	// index along with the column, and DROP INDEX fails. So this one gives up
	// CONCURRENTLY, which costs nothing: DROP COLUMN takes an ACCESS EXCLUSIVE
	// lock on the same table moments later regardless.
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("legacy").Nullable(),
			schema.Text("kept").Nullable(),
		).Index("legacy").Index("kept")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("kept").Nullable(),
		)
	})

	changes := diff(t, current, target)

	legacy := find(t, changes, `"posts_legacy_idx"`)
	if legacy.Stage == migrate.StageConcurrent {
		t.Fatalf("this drop must stay with the column drop:\n%s", render(changes))
	}
	if legacy.Up != `DROP INDEX "posts_legacy_idx";` {
		t.Fatalf("Up = %q", legacy.Up)
	}
	// It must still be reversible: the Down recreates it after the column
	// comes back.
	if legacy.Down != `CREATE INDEX "posts_legacy_idx" ON "posts" ("legacy");` {
		t.Fatalf("Down = %q", legacy.Down)
	}

	// The other direction: an index whose columns all survive keeps
	// CONCURRENTLY, because nothing else is locking that table.
	kept := find(t, changes, `"posts_kept_idx"`)
	if kept.Stage != migrate.StageConcurrent {
		t.Fatalf("an ordinary index drop should stay concurrent:\n%s", render(changes))
	}

	// Both must precede the column drop within the change list.
	order := ups(changes)
	if indexOf(order, `DROP INDEX "posts_legacy_idx"`) > indexOf(order, `DROP COLUMN "legacy"`) {
		t.Fatalf("the index drop must precede the column drop:\n%s", render(changes))
	}

	// And the whole thing must render: the non-concurrent drop in the main
	// file, ahead of the column drop it depends on.
	files, err := migrate.Render(migrate.Migration{
		Version: "1", Name: "drop_legacy", Changes: changes,
	}, migrate.Options{AllowDestructive: true})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	main := files["1_drop_legacy.sql"]
	if !strings.Contains(main, `DROP INDEX "posts_legacy_idx";`) {
		t.Fatalf("index drop belongs in the main file:\n%s", main)
	}
	if strings.Index(main, `DROP INDEX "posts_legacy_idx"`) > strings.Index(main, `DROP COLUMN "legacy"`) {
		t.Fatalf("wrong order in the rendered file:\n%s", main)
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	// A migration that reorders itself between runs is a diff nobody can
	// review, and map iteration is the obvious way to get one by accident.
	current := build(orgsAndUsers)
	target := build(func(r *schema.Registry) {
		orgs := r.Table("orgs",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("name").Unique(),
			schema.Text("slug").Nullable(),
		).Index("slug")
		r.Table("users",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("email"),
			schema.Ref("org", orgs),
		)
	})

	first := ups(diff(t, current, target))
	if len(first) < 5 {
		t.Fatalf("expected a change set worth checking, got %d", len(first))
	}
	for i := 0; i < 10; i++ {
		if got := ups(diff(t, current, target)); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs\n got: %#v\nwant: %#v", i, got, first)
		}
	}
}

func TestDiffRendersAsAMigration(t *testing.T) {
	// Render validates what Diff must guarantee: every change has Up SQL, and
	// every destructive one gives a reason. Running the two together is the
	// cheapest check that the engine cannot emit a file the renderer rejects.
	current := build(orgsAndUsers)
	target := build(func(r *schema.Registry) {
		r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name")).Index("name")
	})

	files, err := migrate.Render(migrate.Migration{
		Version: "20260727150000",
		Name:    "drop_users",
		Changes: diff(t, current, target),
	}, migrate.Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The concurrent index change is split into its own file, because
	// NO TRANSACTION is file-scoped in goose.
	if len(files) != 2 {
		t.Fatalf("want 2 files (ordinary + indexes), got %d: %v", len(files), keys(files))
	}
	main := files["20260727150000_drop_users.sql"]
	if !strings.Contains(main, `-- DROP TABLE "users";`) {
		t.Errorf("the table drop should render commented out by default:\n%s", main)
	}
	// DROP TABLE takes the table's own constraints and indexes with it, so
	// emitting them separately would be noise that also fails on replay.
	if strings.Contains(main, "DROP CONSTRAINT") {
		t.Errorf("a dropped table needs no constraint drops:\n%s", main)
	}
}

func TestDiffRejectsInvalidSchema(t *testing.T) {
	// A schema that does not validate would produce DDL for a table that
	// cannot exist. Failing here beats failing halfway through a migration.
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey(), schema.Int("views").Searchable())
	})
	if _, err := migrate.Diff(nil, target); err == nil {
		t.Fatal("want an error for an invalid target schema")
	} else if !strings.Contains(err.Error(), "target schema is not valid") {
		t.Fatalf("error should say which side is invalid: %v", err)
	}

	if _, err := migrate.Diff(target, nil); err == nil {
		t.Fatal("want an error for an invalid current schema")
	} else if !strings.Contains(err.Error(), "current schema is not valid") {
		t.Fatalf("error should say which side is invalid: %v", err)
	}
}

func TestDiffComments(t *testing.T) {
	current := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("body").Comment("the text"),
		).Describe("articles")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("body").Comment("markdown source"),
		).Describe("published articles")
	})

	changes := diff(t, current, target)
	col := find(t, changes, "COMMENT ON COLUMN")
	if col.Up != `COMMENT ON COLUMN "posts"."body" IS 'markdown source';` {
		t.Fatalf("Up = %q", col.Up)
	}
	if col.Down != `COMMENT ON COLUMN "posts"."body" IS 'the text';` {
		t.Fatalf("Down = %q", col.Down)
	}

	tbl := find(t, changes, "COMMENT ON TABLE")
	if tbl.Up != `COMMENT ON TABLE "posts" IS 'published articles';` {
		t.Fatalf("Up = %q", tbl.Up)
	}
}

func TestDiffCommentRemoved(t *testing.T) {
	// Postgres removes a comment by setting it to NULL, which is not the same
	// as setting it to the empty string.
	current := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey()).Describe("articles")
	})
	target := build(func(r *schema.Registry) {
		r.Table("posts", schema.UUIDv7("id").PrimaryKey())
	})

	c := only(t, diff(t, current, target))
	if c.Up != `COMMENT ON TABLE "posts" IS NULL;` {
		t.Fatalf("Up = %q", c.Up)
	}
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if strings.Contains(s, needle) {
			return i
		}
	}
	return -1
}

// A table-level UNIQUE renders as a constraint, not as an index, and keeps the
// name it was declared under.
//
// The distinction is the whole of issue #108: a composite unique *index* was the
// nearest thing the DSL could declare, and it is a different object — it cannot
// be the target of REFERENCES t (a, b), cannot be named in ON CONFLICT ON
// CONSTRAINT, and diffs as an index, so adopting a table that had a constraint
// meant proposing to drop it and build an index in its place.
func TestCompositeUniqueConstraint(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("secrets",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("tenant_kind"),
			schema.UUID("tenant_id"),
			schema.Text("name"),
		).Unique("tenant_kind", "tenant_id", "name")
	})
	c := only(t, diff(t, schema.NewRegistry(), target))
	want := `CONSTRAINT "secrets_tenant_kind_tenant_id_name_key" UNIQUE ("tenant_kind", "tenant_id", "name")`
	if !strings.Contains(c.Up, want) {
		t.Fatalf("DDL is missing %q:\n%s", want, c.Up)
	}
	// Not an index. A CREATE UNIQUE INDEX here would be the approximation this
	// replaces, and it would look correct in every registry-level comparison.
	if strings.Contains(c.Up, "CREATE UNIQUE INDEX") {
		t.Errorf("the constraint was rendered as an index:\n%s", c.Up)
	}
}

// The name is pinnable, because Postgres reports a violated constraint by name
// and applications match on it — so a bootstrap that could not reproduce the
// live name would propose a rename that turns a handled 23505 into a 500.
func TestCompositeUniqueKeepsItsName(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("secrets",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("a"),
			schema.Text("b"),
		).UniqueNamed("secrets_natural_key", "a", "b")
	})
	c := only(t, diff(t, schema.NewRegistry(), target))
	if !strings.Contains(c.Up, `CONSTRAINT "secrets_natural_key" UNIQUE ("a", "b")`) {
		t.Fatalf("the declared name was not used:\n%s", c.Up)
	}
}

// Adding one to a table that exists is an ALTER, and dropping it is destructive
// — it is an invariant, and a migration that removes one silently is the case
// ADR-0014 is about.
func TestCompositeUniqueAddedAndDropped(t *testing.T) {
	plain := func(r *schema.Registry) {
		r.Table("secrets", schema.UUIDv7("id").PrimaryKey(), schema.Text("a"), schema.Text("b"))
	}
	withUnique := func(r *schema.Registry) {
		r.Table("secrets", schema.UUIDv7("id").PrimaryKey(), schema.Text("a"), schema.Text("b")).
			Unique("a", "b")
	}

	add := only(t, diff(t, build(plain), build(withUnique)))
	if !strings.Contains(add.Up, `ADD CONSTRAINT "secrets_a_b_key" UNIQUE ("a", "b")`) {
		t.Fatalf("adding is not an ALTER ... ADD CONSTRAINT:\n%s", add.Up)
	}
	if add.Destructive {
		t.Error("adding a constraint destroys nothing")
	}

	drop := only(t, diff(t, build(withUnique), build(plain)))
	if !strings.Contains(drop.Up, `DROP CONSTRAINT "secrets_a_b_key"`) {
		t.Fatalf("dropping is not an ALTER ... DROP CONSTRAINT:\n%s", drop.Up)
	}
	// Not destructive, and that is the convention every other constraint here
	// follows rather than a judgement about this one: dropping a constraint
	// removes no data, and the Down restores it exactly. A composite unique
	// behaving differently from a single-column one would be the surprise.
	if drop.Destructive {
		t.Error("dropping a constraint loses no data, and reverses cleanly")
	}
	if !strings.Contains(drop.Down, `ADD CONSTRAINT "secrets_a_b_key" UNIQUE ("a", "b")`) {
		t.Fatalf("the drop does not reverse to the constraint it removed:\n%s", drop.Down)
	}
}

// A composite primary key renders as one PRIMARY KEY over its columns, in the
// order declared, and never as a surrogate plus a unique index — which is the
// workaround it replaces and a schema change the adopter could not justify
// (issue #109).
func TestCompositePrimaryKeyDDL(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("llmcatalog_models",
			schema.Text("provider"),
			schema.Text("model_id"),
			schema.Text("display_name"),
		).PrimaryKeyColumns("provider", "model_id")
	})
	c := only(t, diff(t, schema.NewRegistry(), target))
	want := `CONSTRAINT "llmcatalog_models_pkey" PRIMARY KEY ("provider", "model_id")`
	if !strings.Contains(c.Up, want) {
		t.Fatalf("DDL is missing %q:\n%s", want, c.Up)
	}
	// No surrogate was invented, and no unique index stood in for the key.
	for _, unwanted := range []string{"gen_random_uuid", "uuid_generate", "CREATE UNIQUE INDEX"} {
		if strings.Contains(c.Up, unwanted) {
			t.Errorf("the workaround leaked into the DDL (%s):\n%s", unwanted, c.Up)
		}
	}
}

// An EXCLUDE constraint renders as a constraint on the table, with its index
// method, its element list and its predicate intact (issue #121).
func TestExclusionDDL(t *testing.T) {
	target := build(func(r *schema.Registry) {
		r.Table("bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("coach_id"),
			schema.Text("status"),
			schema.Timestamp("starts_at"),
			schema.Timestamp("ends_at"),
		).AddExclude(schema.Exclusion{
			Name:     "bookings_no_double_booking",
			Using:    "gist",
			Elements: "coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&",
			Where:    "status = 'confirmed'",
		})
	})
	// Two changes, not one: the scalar `=` inside a gist exclusion needs
	// btree_gist, and it is emitted alongside the table rather than left for
	// an adopter to order by hand (#194) — see btree_gist_test.go for that
	// behaviour on its own.
	c := find(t, diff(t, schema.NewRegistry(), target), "CREATE TABLE")
	want := `CONSTRAINT "bookings_no_double_booking" EXCLUDE USING gist ` +
		`(coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&) WHERE (status = 'confirmed')`
	if !strings.Contains(c.Up, want) {
		t.Fatalf("DDL is missing the constraint:\n got:\n%s\nwant to contain:\n%s", c.Up, want)
	}
}

// Adding one to a table that exists is an ALTER, and it reverses to a drop.
func TestExclusionAddedAndDropped(t *testing.T) {
	plain := func(r *schema.Registry) {
		r.Table("rooms", schema.UUIDv7("id").PrimaryKey(), schema.UUID("room_id"))
	}
	withExcl := func(r *schema.Registry) {
		r.Table("rooms", schema.UUIDv7("id").PrimaryKey(), schema.UUID("room_id")).
			AddExclude(schema.Exclusion{Name: "rooms_excl", Using: "gist", Elements: "room_id WITH ="})
	}

	// Two changes here too: rooms already exists, but this is still the first
	// exclusion the schema declares, so btree_gist still has to arrive with it.
	add := find(t, diff(t, build(plain), build(withExcl)), "ADD CONSTRAINT")
	if !strings.Contains(add.Up, `ADD CONSTRAINT "rooms_excl" EXCLUDE USING gist (room_id WITH =)`) {
		t.Fatalf("adding is not an ALTER ... ADD CONSTRAINT:\n%s", add.Up)
	}
	drop := only(t, diff(t, build(withExcl), build(plain)))
	if !strings.Contains(drop.Up, `DROP CONSTRAINT "rooms_excl"`) {
		t.Fatalf("dropping is not an ALTER ... DROP CONSTRAINT:\n%s", drop.Up)
	}
	if !strings.Contains(drop.Down, `ADD CONSTRAINT "rooms_excl" EXCLUDE`) {
		t.Fatalf("the drop does not reverse to the constraint it removed:\n%s", drop.Down)
	}
}

// A constraint naming a column the same migration adds must land after it, the
// way a check over a new column already does — the element list is an
// expression, so the columns are recognised by name.
func TestExclusionWaitsForItsColumns(t *testing.T) {
	before := func(r *schema.Registry) {
		r.Table("bookings", schema.UUIDv7("id").PrimaryKey())
	}
	after := func(r *schema.Registry) {
		r.Table("bookings",
			schema.UUIDv7("id").PrimaryKey(),
			schema.UUID("coach_id"),
		).AddExclude(schema.Exclusion{
			Name: "bookings_excl", Using: "gist", Elements: "coach_id WITH =",
		})
	}
	changes := diff(t, build(before), build(after))
	order := ups(changes)
	addCol, addCon := indexOf(order, `ADD COLUMN "coach_id"`), indexOf(order, "ADD CONSTRAINT")
	if addCol < 0 || addCon < 0 {
		t.Fatalf("expected both an add column and an add constraint:\n%s", render(changes))
	}
	if addCon < addCol {
		t.Fatalf("the constraint is added before the column it names:\n%s", render(changes))
	}
}
