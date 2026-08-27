package introspect

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// The catalog rows below are the ones a real Postgres returned for DDL this
// project generated — the spellings are observed, not invented. That is the
// point of testing the mapping separately from the reading: these rows can be
// written by hand, so every branch is reachable without a database, and the
// only thing a database is still needed for is whether the queries return rows
// of this shape.

func TestBuildMapsAWholeTable(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "orgs"}, {Name: "posts", Comment: "articles"}},
		columns: []columnRow{
			{Table: "orgs", Name: "id", Type: "uuid", NotNull: true, Default: "uuid_generate_v7()"},
			{Table: "orgs", Name: "name", Type: "text", NotNull: true},
			{Table: "posts", Name: "id", Type: "uuid", NotNull: true, Default: "uuid_generate_v7()"},
			{Table: "posts", Name: "slug", Type: "text", NotNull: true, Comment: "url key"},
			{Table: "posts", Name: "title", Type: "character varying(200)", NotNull: true},
			{Table: "posts", Name: "views", Type: "integer", NotNull: true, Default: "0"},
			{Table: "posts", Name: "note", Type: "text", NotNull: true, Default: "'hello'::text"},
			{Table: "posts", Name: "created_at", Type: "timestamp with time zone", NotNull: true, Default: "now()"},
			{Table: "posts", Name: "score", Type: "double precision"},
			{Table: "posts", Name: "status", Type: "text", NotNull: true},
			{Table: "posts", Name: "org_id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			// Postgres 18 records every NOT NULL as a constraint of its own.
			{Table: "orgs", Name: "orgs_id_not_null", Type: "n", Def: "NOT NULL id"},
			{Table: "orgs", Name: "orgs_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "orgs", Name: "orgs_name_key", Type: "u", Columns: []string{"name"}},
			{Table: "posts", Name: "posts_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "posts", Name: "posts_slug_key", Type: "u", Columns: []string{"slug"}},
			{Table: "posts", Name: "posts_status_check", Type: "c", Columns: []string{"status"},
				Expr: "(status = ANY (ARRAY['draft'::text, 'live'::text]))"},
			{Table: "posts", Name: "views_non_negative", Type: "c", Columns: []string{"views"},
				Expr: "(views >= 0)"},
			{Table: "posts", Name: "posts_org_id_fkey", Type: "f", Columns: []string{"org_id"},
				RefTable: "orgs", RefCols: []string{"id"}, OnDelete: "c", OnUpdate: "a"},
		},
		indexes: []indexRow{
			{Table: "posts", Name: "posts_title_views_idx", Method: "btree",
				Columns: []string{"title", "views"}},
			{Table: "posts", Name: "posts_meta_gin", Method: "gin", Columns: []string{"slug"}},
		},
	}

	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("nothing here is beyond the DSL, but:\n%s", rep)
	}

	posts := r.Get("posts")
	if posts == nil {
		t.Fatal("posts was not built")
	}
	if posts.Comment() != "articles" {
		t.Errorf("table comment = %q", posts.Comment())
	}

	for _, tc := range []struct {
		column string
		check  func(*testing.T, *schema.FieldDesc)
	}{
		{"id", func(t *testing.T, d *schema.FieldDesc) {
			if !d.PrimaryKey || d.Type != schema.TypeUUID {
				t.Errorf("got %+v", d)
			}
			// Recognised by the exact text the schema package emits, so it
			// comes back as the generator rather than as raw SQL.
			if d.Default == nil || d.Default.Raw != "uuid_generate_v7()" {
				t.Errorf("default = %+v", d.Default)
			}
		}},
		{"slug", func(t *testing.T, d *schema.FieldDesc) {
			if !d.Unique || d.Comment != "url key" {
				t.Errorf("got %+v", d)
			}
		}},
		{"title", func(t *testing.T, d *schema.FieldDesc) {
			// "character varying(200)" is what format_type returns; the
			// spelling the DDL layer emits would have matched nothing.
			if d.Type != schema.TypeVarchar || d.Size != 200 {
				t.Errorf("got type %q size %d", d.Type, d.Size)
			}
		}},
		{"note", func(t *testing.T, d *schema.FieldDesc) {
			// The cast Postgres attaches to every stored literal is stripped,
			// so the default renders as the DDL layer would have written it.
			if d.Default == nil || d.Default.Value != "hello" {
				t.Errorf("default = %+v", d.Default)
			}
		}},
		{"score", func(t *testing.T, d *schema.FieldDesc) {
			if !d.Nullable || d.Type != schema.TypeFloat {
				t.Errorf("got %+v", d)
			}
		}},
		{"status", func(t *testing.T, d *schema.FieldDesc) {
			// An enum is text plus a CHECK, so recovering it means reading the
			// expression — in the normalised form Postgres stores, not the
			// IN () the DDL layer wrote.
			if d.Type != schema.TypeEnum {
				t.Fatalf("type = %q, want enum", d.Type)
			}
			if strings.Join(d.EnumValues, ",") != "draft,live" {
				t.Errorf("values = %v", d.EnumValues)
			}
		}},
		{"org_id", func(t *testing.T, d *schema.FieldDesc) {
			if d.Ref == nil {
				t.Fatal("no reference")
			}
			if d.Ref.Name != "org" {
				t.Errorf("relation = %q, want org (the _id suffix is stripped)", d.Ref.Name)
			}
			if d.Ref.Table == nil || d.Ref.Table.Name() != "orgs" {
				t.Errorf("target = %+v", d.Ref.Table)
			}
			if d.Ref.OnDelete != schema.Cascade {
				t.Errorf("on delete = %q", d.Ref.OnDelete)
			}
		}},
	} {
		f := posts.Field(tc.column)
		if f == nil {
			t.Errorf("%s: missing", tc.column)
			continue
		}
		t.Run(tc.column, func(t *testing.T) { tc.check(t, f.Desc()) })
	}

	// A check that is not an enum stays a check.
	if len(posts.Checks()) != 1 || posts.Checks()[0].Name != "views_non_negative" {
		t.Errorf("checks = %+v", posts.Checks())
	}

	// btree is the dialect default and the DDL layer omits it, so recording it
	// would make an unchanged index look changed.
	for _, idx := range posts.Indexes() {
		switch idx.Name {
		case "posts_title_views_idx":
			if idx.Method != "" {
				t.Errorf("btree should not be recorded, got %q", idx.Method)
			}
		case "posts_meta_gin":
			if idx.Method != "gin" {
				t.Errorf("method = %q", idx.Method)
			}
		}
	}
}

func TestBuildPinsOnlyTheNamesThatDiffer(t *testing.T) {
	// Adoption depends on this. A name that matches what the DDL layer would
	// generate is left unpinned, so the schema is not littered with them; one
	// that does not is pinned, or the first diff after an import would drop and
	// recreate the constraint.
	cat := &catalog{
		tables: []tableRow{{Name: "users"}},
		columns: []columnRow{{Table: "users", Name: "id", Type: "uuid", NotNull: true},
			{Table: "users", Name: "email", Type: "text", NotNull: true}},
		constraints: []constraintRow{
			{Table: "users", Name: "users_id_pk", Type: "p", Columns: []string{"id"}},
			{Table: "users", Name: "uq_user_email", Type: "u", Columns: []string{"email"}},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil || !rep.Empty() {
		t.Fatalf("build: %v %s", err, rep)
	}
	users := r.Get("users")
	if users.PrimaryKeyName() != "users_id_pk" {
		t.Errorf("primary key name = %q, want it pinned", users.PrimaryKeyName())
	}
	if got := users.Field("email").Desc().ConstraintName; got != "uq_user_email" {
		t.Errorf("constraint name = %q, want it pinned", got)
	}

	// And the conventional names are left alone.
	conv := &catalog{
		tables: []tableRow{{Name: "users"}},
		columns: []columnRow{{Table: "users", Name: "id", Type: "uuid", NotNull: true},
			{Table: "users", Name: "email", Type: "text", NotNull: true}},
		constraints: []constraintRow{
			{Table: "users", Name: "users_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "users", Name: "users_email_key", Type: "u", Columns: []string{"email"}},
		},
	}
	r2, _, err := build(conv, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := r2.Get("users").PrimaryKeyName(); n != "" {
		t.Errorf("conventional name should not be pinned, got %q", n)
	}
	if n := r2.Get("users").Field("email").Desc().ConstraintName; n != "" {
		t.Errorf("conventional name should not be pinned, got %q", n)
	}
}

func TestBuildReportsWhatItCannotRepresent(t *testing.T) {
	// The failure that matters is the quiet one: a schema missing a construct
	// still validates and still produces a migration, one that proposes undoing
	// whatever it failed to see.
	cat := &catalog{
		tables: []tableRow{{Name: "t"}},
		columns: []columnRow{
			{Table: "t", Name: "id", Type: "uuid", NotNull: true},
			{Table: "t", Name: "a", Type: "integer", NotNull: true},
			{Table: "t", Name: "b", Type: "integer", NotNull: true},
			{Table: "t", Name: "money", Type: "money"},
			{Table: "t", Name: "total", Type: "integer", Generated: "s"},
			{Table: "t", Name: "seq", Type: "integer", Identity: "a"},
			{Table: "t", Name: "ser", Type: "bigint", NotNull: true,
				Default: "nextval('t_ser_seq'::regclass)"},
		},
		constraints: []constraintRow{
			{Table: "t", Name: "t_pkey", Type: "p", Columns: []string{"a", "b"}, Def: "PRIMARY KEY (a, b)"},
			{Table: "t", Name: "t_ab_key", Type: "u", Columns: []string{"a", "b"}, Def: "UNIQUE (a, b)"},
			{Table: "t", Name: "t_excl", Type: "x", Def: "EXCLUDE USING gist (a WITH =)"},
		},
		indexes: []indexRow{
			{Table: "t", Name: "t_lower_idx", Expression: true, Def: "CREATE INDEX ... (lower(a))"},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The composite UNIQUE in this catalog is imported rather than reported
	// (issue #108); everything else here still has no declaration.
	if u := r.Get("t").Uniques(); len(u) != 1 || u[0].Name != "t_ab_key" {
		t.Errorf("composite unique should be imported, got %+v", u)
	}
	if strings.Contains(rep.String(), "composite unique constraint") {
		t.Errorf("composite unique should no longer be reported:\n%s", rep)
	}
	for _, want := range []string{
		"expression",
		"money",
		"generated column",
		// The identity and serial columns above are declarable now (#132), so
		// what remains unrepresentable about this one is that it is *also*
		// nullable — an arrangement Postgres permits by dropping the NOT NULL
		// it created, and one the DSL has no spelling for.
		"sequence",
		"nullable",
	} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report should mention %q:\n%s", want, rep)
		}
	}
	if rep.Err() == nil {
		t.Error("Err should describe a non-empty report")
	}
}

// A serial column used to import silently: attidentity is empty for one, so the
// identity check missed it, the nextval default carried a regclass cast that
// stripCast had no reason to touch, and the column arrived as an ordinary bigint
// whose default named a sequence. The table then reported clean while the DDL it
// produced failed to apply — "relation t_ser_seq does not exist" — and the
// table's indexes failed behind it (issue #119).
//
// Reporting it was the right first answer and the wrong last one: it left no
// auto-incrementing integer key expressible at all (issue #132). So the column
// is imported now, as a serial — and the assertion that matters is still the one
// #119 added, that the nextval default does not come with it. A declaration
// carrying that expression renders DDL binding to a sequence nothing creates;
// the serial spelling is what makes Postgres create it.
func TestSerialColumnImportsAsASerialWithoutItsDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  string
	}{
		{"bigserial", "nextval('t_ser_seq'::regclass)"},
		{"serial", "nextval('t_ser_seq'::regclass)"},
		{"schema qualified", "nextval('public.t_ser_seq'::regclass)"},
		{"unquoted", "nextval('t_ser_seq')"},
		{"uppercase", "NEXTVAL('t_ser_seq'::regclass)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := &catalog{
				tables: []tableRow{{Name: "t"}},
				columns: []columnRow{
					{Table: "t", Name: "id", Type: "uuid", NotNull: true},
					{Table: "t", Name: "ser", Type: "bigint", NotNull: true, Default: tc.def},
				},
			}
			r, rep, err := build(cat, Options{})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !rep.Empty() {
				t.Errorf("a serial is declarable and should no longer be reported:\n%s", rep)
			}
			f := r.Get("t").Field("ser")
			if f == nil {
				t.Fatal("the serial column was not imported")
			}
			d := f.Desc()
			if d.Auto != schema.AutoSerial {
				t.Errorf("Auto = %q, want %q", d.Auto, schema.AutoSerial)
			}
			// bigint, not "bigserial": the storage type is what a later type
			// change has to compare against, and the serial is how it is
			// rendered rather than what it is.
			if d.Type != schema.TypeBigInt {
				t.Errorf("Type = %q, want %q", d.Type, schema.TypeBigInt)
			}
			if d.Default != nil {
				t.Errorf("the nextval default came with the column: %+v", d.Default)
			}
		})
	}
}

// The two identity spellings, which Postgres records in attidentity rather than
// in the default — and which differ in whether an INSERT may name the column.
func TestIdentityColumnsImport(t *testing.T) {
	for _, tc := range []struct {
		attidentity string
		want        schema.Auto
		readOnly    bool
	}{
		{"d", schema.AutoIdentity, false},
		{"a", schema.AutoIdentityAlways, true},
	} {
		t.Run(tc.attidentity, func(t *testing.T) {
			cat := &catalog{
				tables: []tableRow{{Name: "t"}},
				columns: []columnRow{
					{Table: "t", Name: "id", Type: "bigint", NotNull: true, Identity: tc.attidentity},
				},
			}
			r, rep, err := build(cat, Options{})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !rep.Empty() {
				t.Errorf("an identity column is declarable and should no longer be reported:\n%s", rep)
			}
			d := r.Get("t").Field("id").Desc()
			if d.Auto != tc.want {
				t.Errorf("Auto = %q, want %q", d.Auto, tc.want)
			}
			// GENERATED ALWAYS refuses an INSERT that names it, so nothing may
			// offer the column to a caller.
			if d.ReadOnly != tc.readOnly {
				t.Errorf("ReadOnly = %v, want %v", d.ReadOnly, tc.readOnly)
			}
		})
	}
}

// A sequence under something that is not an integer is legal Postgres and has
// no reading here. It loses one column and names it, rather than failing the
// import — which is what letting it through to Validate would do.
func TestSequenceOverANonIntegerIsReported(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "t"}},
		columns: []columnRow{
			{Table: "t", Name: "id", Type: "uuid", NotNull: true},
			{Table: "t", Name: "code", Type: "text", NotNull: true, Default: "nextval('t_code_seq'::regclass)"},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(rep.String(), "sequence") {
		t.Errorf("report should name the sequence:\n%s", rep)
	}
	if f := r.Get("t").Field("code"); f != nil {
		t.Errorf("a text column drawing from a sequence should be refused: %+v", f.Desc())
	}
	if f := r.Get("t").Field("id"); f == nil {
		t.Error("the table's other columns should survive")
	}
}

// A default that merely mentions a sequence-like name is not a serial. Only a
// nextval draw is, and refusing anything else would drop ordinary columns.
func TestNonSequenceDefaultsAreNotMistakenForSerial(t *testing.T) {
	for _, def := range []string{
		"'nextval'::text",
		"'seq_'::text || (id)::text",
		"0",
		"now()",
	} {
		cat := &catalog{
			tables:  []tableRow{{Name: "t"}},
			columns: []columnRow{{Table: "t", Name: "c", Type: "text", Default: def}},
		}
		r, _, err := build(cat, Options{})
		if err != nil {
			t.Fatalf("build(%q): %v", def, err)
		}
		if f := r.Get("t").Field("c"); f == nil {
			t.Errorf("default %q should import as an ordinary column", def)
		}
	}
}

// A self-referential foreign key is common enough — manager_id, parent_id,
// reply_to — that dropping the column it sits on is a serious import bug: the
// registry ends up missing a column the database has, so the next Diff proposes
// adding one that exists, and a drift check proposes dropping a real one.
//
// Only the constraint is unrepresentable. The column is an ordinary uuid.
func TestSelfReferentialForeignKeyKeepsItsColumn(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "employees"}},
		columns: []columnRow{
			{Table: "employees", Name: "id", Type: "uuid", NotNull: true},
			{Table: "employees", Name: "name", Type: "text", NotNull: true},
			{Table: "employees", Name: "manager_id", Type: "uuid"},
		},
		constraints: []constraintRow{
			{Table: "employees", Name: "employees_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "employees", Name: "employees_manager_id_fkey", Type: "f",
				Columns: []string{"manager_id"}, RefTable: "employees",
				RefCols: []string{"id"}, Def: "FOREIGN KEY (manager_id) REFERENCES employees(id)"},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	tbl := r.Get("employees")
	if tbl == nil {
		t.Fatal("employees was not imported at all")
	}
	var found *schema.FieldDesc
	for _, f := range tbl.Fields() {
		if f.Desc().Name == "manager_id" {
			found = f.Desc()
		}
	}
	if found == nil {
		t.Fatal("manager_id was dropped; the column is an ordinary typed column")
	}

	// And the foreign key comes with it. A self-reference *is* declarable, and
	// only one way: Ref inside the table's own definition is a Go
	// initialisation cycle, so the declaration is forced to write
	// ExternalRef("manager", "employees.id").Enforced() — and an import that
	// reported the constraint as undeclarable made that declaration read as
	// permanent drift, plus a second waiver for the implicit index ExternalRef
	// wants (issue #82). Both sides produce the same field now.
	if found.Ref == nil {
		t.Fatalf("the self-referential foreign key was dropped: %+v", found)
	}
	if !found.Ref.External {
		t.Errorf("a self-reference must import as an ExternalRef, since that is the only " +
			"spelling a declaration can use")
	}
	_, target, col, enforced := found.Ref.EnforcedTarget()
	if !enforced || target != "employees" || col != "id" {
		t.Errorf("EnforcedTarget() = %q,%q,%v; want employees,id,true", target, col, enforced)
	}
	// Nothing left to report: the whole column round-trips.
	if strings.Contains(rep.String(), "self-referential") {
		t.Errorf("a declarable self-reference should not be reported as a gap:\n%s", rep)
	}
}

// A camelCase identifier is legal Postgres and routine in databases built by
// other tools. It used to abort the whole import at Validate, with an error
// framed as this package having built something impossible.
func TestUndeclarableNamesAreReportedRatherThanFatal(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "users"}, {Name: "userProfiles"}},
		columns: []columnRow{
			{Table: "users", Name: "id", Type: "uuid", NotNull: true},
			{Table: "users", Name: "createdAt", Type: "timestamp with time zone"},
			{Table: "userProfiles", Name: "id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "users", Name: "users_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "userProfiles", Name: "userProfiles_pkey", Type: "p", Columns: []string{"id"}},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("an undeclarable name should be reported, not fatal: %v", err)
	}
	// The table that *can* be declared still is.
	if r.Get("users") == nil {
		t.Error("a readable table was lost along with the unreadable one")
	}
	if r.Get("userProfiles") != nil {
		t.Error("a table whose name cannot be declared should be skipped")
	}
	for _, want := range []string{"userProfiles", "createdAt", "upper-case"} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("report should mention %q:\n%s", want, rep)
		}
	}
}

// A foreign-key cycle is broken rather than refused. It used to drop every table
// on the cycle, with advice — "make one side an ExternalRef" — that was right
// for a declaration and impossible to follow from here, so a drift gate diffing
// a declaration that *had* broken the cycle reported the table as absent from
// the database (issue #80). The only workaround was to exclude one of the
// tables, which meant the gate could never cover it.
func TestBuildBreaksAForeignKeyCycleWithAnExternalRef(t *testing.T) {
	// A reference names the target table's own value, so a cycle is a Go
	// initialisation cycle: there is no ordering that fixes it.
	cat := &catalog{
		tables: []tableRow{{Name: "a"}, {Name: "b"}},
		columns: []columnRow{
			{Table: "a", Name: "id", Type: "uuid", NotNull: true},
			{Table: "a", Name: "b_id", Type: "uuid", NotNull: true},
			{Table: "b", Name: "id", Type: "uuid", NotNull: true},
			{Table: "b", Name: "a_id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "a", Name: "a_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "b", Name: "b_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "a", Name: "a_b_id_fkey", Type: "f", Columns: []string{"b_id"},
				RefTable: "b", RefCols: []string{"id"}},
			{Table: "b", Name: "b_a_id_fkey", Type: "f", Columns: []string{"a_id"},
				RefTable: "a", RefCols: []string{"id"}},
		},
	}
	r, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Both tables are here. Dropping them was the bug.
	for _, name := range []string{"a", "b"} {
		if r.Get(name) == nil {
			t.Fatalf("%s was dropped because it sits on a cycle:\n%s", name, rep)
		}
	}

	// Every foreign key survives, one side of the cycle as an ExternalRef —
	// the same spelling the declaration is forced to use.
	var external int
	for _, name := range []string{"a", "b"} {
		for _, f := range r.Get(name).Fields() {
			ref := f.Desc().Ref
			if ref == nil {
				continue
			}
			if ref.External {
				external++
				if _, _, _, ok := ref.EnforcedTarget(); !ok {
					t.Errorf("%s.%s imported as an unenforced ExternalRef, so the live "+
						"foreign key would be proposed for deletion", name, f.Desc().Name)
				}
			}
		}
	}
	if external != 1 {
		t.Errorf("want exactly one side of the cycle broken, got %d ExternalRefs", external)
	}

	// And it is said out loud — as a note, not a gap: nothing was lost, so
	// Report.Err must stay nil or a clean round trip would fail.
	if !strings.Contains(rep.String(), "cycle") {
		t.Errorf("breaking a cycle must be noted:\n%s", rep)
	}
	if err := rep.Err(); err != nil {
		t.Errorf("a broken cycle loses nothing, so it must not read as an unrepresentable "+
			"construct: %v", err)
	}
}

func TestBuildRoundTripsThroughDiff(t *testing.T) {
	// The property ADR-0014 claims: introspection produces the same registry
	// the DSL produces, so diffing what was declared against what came back is
	// empty. This is that claim in miniature — the version against a real
	// database is run by hand, see the package's own notes.
	cat := &catalog{
		tables: []tableRow{{Name: "orgs"}},
		columns: []columnRow{
			{Table: "orgs", Name: "id", Type: "uuid", NotNull: true, Default: "uuid_generate_v7()"},
			{Table: "orgs", Name: "name", Type: "text", NotNull: true},
			{Table: "orgs", Name: "kind", Type: "text", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "orgs", Name: "orgs_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "orgs", Name: "orgs_name_key", Type: "u", Columns: []string{"name"}},
			{Table: "orgs", Name: "orgs_kind_check", Type: "c", Columns: []string{"kind"},
				Expr: "(kind = ANY (ARRAY['a'::text, 'b'::text]))"},
		},
	}
	imported, _, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	declared := schema.NewRegistry()
	declared.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Unique(),
		schema.Enum("kind", "a", "b"),
	)

	changes, err := migrate.Diff(imported, declared)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(changes) != 0 {
		for _, c := range changes {
			t.Logf("  %s", c.Up)
		}
		t.Fatalf("want no difference between what was declared and what came back, got %d", len(changes))
	}
}

func TestBuildStripsTheModulePrefix(t *testing.T) {
	cat := &catalog{
		tables: []tableRow{{Name: "billing_invoices"}, {Name: "unrelated"}},
		columns: []columnRow{
			{Table: "billing_invoices", Name: "id", Type: "uuid", NotNull: true},
			{Table: "unrelated", Name: "id", Type: "uuid", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "billing_invoices", Name: "billing_invoices_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "unrelated", Name: "unrelated_pkey", Type: "p", Columns: []string{"id"}},
		},
	}
	r, rep, err := build(cat, Options{Module: "billing"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r.Get("billing_invoices") == nil {
		t.Error("the module registry should re-apply the prefix it stripped")
	}
	if r.Get("billing_invoices").LocalName() != "invoices" {
		t.Errorf("local name = %q", r.Get("billing_invoices").LocalName())
	}
	// A table without the prefix would be renamed on the way back out, so it
	// is reported rather than quietly absorbed into the module.
	if !strings.Contains(rep.String(), "unrelated") {
		t.Errorf("a table outside the module must be reported:\n%s", rep)
	}
}

func TestColumnType(t *testing.T) {
	for _, tc := range []struct {
		formatted string
		want      schema.Type
		size      int
		scale     int
		ok        bool
	}{
		{"text", schema.TypeText, 0, 0, true},
		{"character varying(200)", schema.TypeVarchar, 200, 0, true},
		{"character varying", schema.TypeText, 0, 0, true},
		{"integer", schema.TypeInt, 0, 0, true},
		{"bigint", schema.TypeBigInt, 0, 0, true},
		{"double precision", schema.TypeFloat, 0, 0, true},
		{"numeric", schema.TypeNumeric, 0, 0, true},
		{"boolean", schema.TypeBool, 0, 0, true},
		{"uuid", schema.TypeUUID, 0, 0, true},
		{"timestamp with time zone", schema.TypeTimestamp, 0, 0, true},
		{"date", schema.TypeDate, 0, 0, true},
		{"time without time zone", schema.TypeTime, 0, 0, true},
		{"jsonb", schema.TypeJSON, 0, 0, true},
		{"bytea", schema.TypeBytes, 0, 0, true},
		// A numeric's precision and scale are part of the type, and since #81
		// the DSL can declare them, so they import rather than being refused.
		// Postgres formats a precision declared alone as "numeric(5,0)"; the
		// bare "numeric(5)" spelling is accepted for the same reading.
		{"numeric(10,2)", schema.TypeNumeric, 10, 2, true},
		{"numeric(5,0)", schema.TypeNumeric, 5, 0, true},
		{"numeric(5)", schema.TypeNumeric, 5, 0, true},
		// Types with no equivalent are refused rather than approximated: a
		// column imported as the wrong type produces a migration proposing to
		// change the real one.
		{"numeric(bad,2)", "", 0, 0, false},
		{"smallint", schema.TypeSmallInt, 0, 0, true},
		{"real", schema.TypeReal, 0, 0, true},
		{"money", "", 0, 0, false},
		{"timestamp without time zone", "", 0, 0, false},
		{"json", "", 0, 0, false},
	} {
		got, size, scale, ok := columnType(tc.formatted)
		if ok != tc.ok || (ok && (got != tc.want || size != tc.size || scale != tc.scale)) {
			t.Errorf("columnType(%q) = %q,%d,%d,%v; want %q,%d,%d,%v",
				tc.formatted, got, size, scale, ok, tc.want, tc.size, tc.scale, tc.ok)
		}
	}
}

func TestColumnDefault(t *testing.T) {
	for _, tc := range []struct {
		expr, formatted string
		typ             schema.Type
		wantRaw         string
		wantValue       any
	}{
		{"now()", "timestamp with time zone", schema.TypeTimestamp, "now()", nil},
		{"uuid_generate_v7()", "uuid", schema.TypeUUID, "uuid_generate_v7()", nil},
		{"gen_random_uuid()", "uuid", schema.TypeUUID, "gen_random_uuid()", nil},
		// The cast on a stored literal is stripped when it names the column's
		// own type, so the default renders as it was written.
		{"'draft'::text", "text", schema.TypeText, "", "draft"},
		// A length-bounded column is the case this got wrong. The old
		// expectation was `raw: "'x'::character varying"` — Postgres formats
		// the column as "character varying(10)" and stores the cast without the
		// length, so the two never matched, the literal survived as a raw
		// expression, and migrate.Diff proposed the same SET DEFAULT forever.
		// It has to reduce to the same Value the text case above does.
		{"'x'::character varying", "character varying(10)", schema.TypeVarchar, "", "x"},
		{"'x'::character", "character(4)", schema.TypeVarchar, "", "x"},
		{"'1.5'::numeric", "numeric(10,2)", schema.TypeNumeric, "", "1.5"},
		// An array of a length-bounded type: the modifier sits inside the
		// brackets on one side and is absent on the other.
		{"'{}'::character varying[]", "character varying(20)[]", schema.TypeVarchar, "", "{}"},
		// The other direction, so the loosened comparison is not loose: a cast
		// naming a DIFFERENT type is doing something, and survives.
		{"'x'::text", "character varying(10)", schema.TypeVarchar, "'x'::text", nil},
		// Bare literals need no stripping and pass through unchanged.
		{"0", "integer", schema.TypeInt, "0", nil},
		{"false", "boolean", schema.TypeBool, "false", nil},
		// Anything else is faithful rather than understood.
		{"('a'::text || 'b'::text)", "text", schema.TypeText, "('a'::text || 'b'::text)", nil},
	} {
		got := columnDefault(tc.expr, tc.formatted, tc.typ)
		if got == nil {
			t.Errorf("columnDefault(%q) = nil", tc.expr)
			continue
		}
		if got.Raw != tc.wantRaw || got.Value != tc.wantValue {
			t.Errorf("columnDefault(%q) = %+v; want raw %q value %v",
				tc.expr, got, tc.wantRaw, tc.wantValue)
		}
	}
	if columnDefault("", "text", schema.TypeText) != nil {
		t.Error("no default should map to nil")
	}
}

func TestEnumValues(t *testing.T) {
	// The form Postgres stores, not the IN () the DDL layer writes.
	got, ok := enumValues("status", "(status = ANY (ARRAY['draft'::text, 'live'::text]))")
	if !ok || strings.Join(got, ",") != "draft,live" {
		t.Errorf("got %v,%v", got, ok)
	}
	// A value containing a comma survives, because the split is not naive.
	got, ok = enumValues("k", "(k = ANY (ARRAY['a,b'::text, 'c'::text]))")
	if !ok || len(got) != 2 || got[0] != "a,b" {
		t.Errorf("got %v,%v", got, ok)
	}
	// An ordinary check is not an enum.
	if _, ok := enumValues("views", "(views >= 0)"); ok {
		t.Error("a comparison is not an enum")
	}
}

// An existing text[] column has to read back as one, or the module carrying it
// cannot be adopted at all: a dropped column makes the first Diff propose
// deleting production data, which is the failure the round trip exists to
// prevent (ADR-0033).
func TestSplitArrayType(t *testing.T) {
	tests := []struct {
		formatted string
		wantElem  string
		wantArray bool
	}{
		{"text[]", "text", true},
		{"integer[]", "integer", true},
		{"character varying(200)[]", "character varying(200)", true},
		{"timestamp with time zone[]", "timestamp with time zone", true},
		{"text", "text", false},
		{"jsonb", "jsonb", false},
	}
	for _, tc := range tests {
		elem, array := splitArrayType(tc.formatted)
		if elem != tc.wantElem || array != tc.wantArray {
			t.Errorf("splitArrayType(%q) = %q,%v; want %q,%v",
				tc.formatted, elem, array, tc.wantElem, tc.wantArray)
		}
	}

	// Two dimensions strip to a one-dimensional element spelling that
	// columnType then refuses, which is the refusal the DSL needs: it declares
	// one dimension, and importing the inner values would hide the difference.
	elem, array := splitArrayType("text[][]")
	if !array {
		t.Fatal("text[][] did not read as an array")
	}
	if _, _, _, ok := columnType(elem); ok {
		t.Errorf("columnType(%q) accepted a nested array spelling", elem)
	}
}

// An enum array's CHECK is a containment test rather than an ANY comparison, so
// the recovery has to read both spellings or an enum array round-trips as plain
// text and diffs forever.
func TestEnumValuesFromArrayCheck(t *testing.T) {
	got, ok := enumValues("labels", "(labels <@ ARRAY['red'::text, 'green'::text])")
	if !ok || strings.Join(got, ",") != "red,green" {
		t.Errorf("got %v,%v", got, ok)
	}
	// With the cast the DDL layer writes still attached.
	got, ok = enumValues("labels", "(labels <@ ARRAY['red'::text]::text[])")
	if !ok || strings.Join(got, ",") != "red" {
		t.Errorf("got %v,%v", got, ok)
	}
}

// An extension is invisible to Diff rather than skipped by it, so it is
// reported without changing whether the registry is clean. Both halves matter:
// the list is what turns 228 identical "function does not exist" errors into
// one line, and flipping Empty() would fail every adoption that uses pgvector
// on a gap it has no way to close (issue #115).
func TestReportExtensions(t *testing.T) {
	rep := &Report{Extensions: []string{"vector", "uuid-ossp"}}

	if !rep.Empty() {
		t.Fatal("an extension is not a construct the registry failed to describe; Empty() must stay true")
	}
	if err := rep.Err(); err != nil {
		t.Fatalf("Err() must stay nil for extensions alone: %v", err)
	}
	out := rep.String()
	for _, want := range []string{
		`CREATE EXTENSION IF NOT EXISTS "vector";`,
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp";`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say how to create the extension:\nwant %q in:\n%s", want, out)
		}
	}
	// The instruction, not just the names: the list is only useful as the step
	// before a bootstrap.
	if !strings.Contains(out, "Create them in the target database first") {
		t.Errorf("the report names the extensions without saying what to do:\n%s", out)
	}

	// And a report with a real skip still carries them, since that is the case
	// where the bootstrap is most likely to be attempted next.
	rep.Skipped = []Skip{{Table: "t", Object: "c", Reason: "unmodelable"}}
	if !strings.Contains(rep.String(), `CREATE EXTENSION IF NOT EXISTS "vector";`) {
		t.Errorf("extensions are dropped from a non-empty report:\n%s", rep.String())
	}
}

// An EXCLUDE constraint is declared rather than skipped.
//
// It is the one construct with no near miss, which is why it was worth the
// grammar: dropping it moves a database invariant into application code, where
// two concurrent requests interleave between the check and the insert
// (issue #121).
func TestExclusionIsDeclared(t *testing.T) {
	const def = `EXCLUDE USING gist (coach_id WITH =, ` +
		`tstzrange(starts_at, ends_at) WITH &&) WHERE ((status = 'confirmed'::text))`
	cat := &catalog{
		tables: []tableRow{{Name: "bookings"}},
		columns: []columnRow{
			{Table: "bookings", Name: "id", Type: "uuid", NotNull: true},
			{Table: "bookings", Name: "coach_id", Type: "uuid", NotNull: true},
			{Table: "bookings", Name: "status", Type: "text", NotNull: true},
			{Table: "bookings", Name: "starts_at", Type: "timestamp with time zone", NotNull: true},
			{Table: "bookings", Name: "ends_at", Type: "timestamp with time zone", NotNull: true},
		},
		constraints: []constraintRow{
			{Table: "bookings", Name: "bookings_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "bookings", Name: "bookings_no_overlap", Type: "x", Def: def},
		},
	}
	reg, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("an exclusion is declarable now, and this reported it:\n%s", rep)
	}
	excls := reg.Get("bookings").Exclusions()
	if len(excls) != 1 {
		t.Fatalf("Exclusions() = %v, want one", excls)
	}
	got := excls[0]
	if got.Name != "bookings_no_overlap" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Using != "gist" {
		t.Errorf("Using = %q, want gist — the method is what makes the operators available", got.Using)
	}
	// Rendered back byte for byte, which is what keeps the diff from proposing
	// to replace a constraint that has not changed.
	if rendered := got.Def(); rendered != def {
		t.Errorf("round trip\n got: %s\nwant: %s", rendered, def)
	}
}

// A form the parser cannot read back is reported rather than half-imported, the
// same contract every other construct here has: a constraint imported without a
// clause it carries is one whose next diff proposes replacing it.
func TestUnreadableExclusionIsReported(t *testing.T) {
	cat := &catalog{
		tables:  []tableRow{{Name: "t"}},
		columns: []columnRow{{Table: "t", Name: "id", Type: "uuid", NotNull: true}},
		constraints: []constraintRow{
			{Table: "t", Name: "t_pkey", Type: "p", Columns: []string{"id"}},
			{Table: "t", Name: "t_excl", Type: "x", Def: "EXCLUDE USING gist (id WITH =) DEFERRABLE"},
		},
	}
	reg, rep, err := build(cat, Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(reg.Get("t").Exclusions()) != 0 {
		t.Error("a constraint carrying a clause this cannot render must not be declared")
	}
	if !strings.Contains(rep.String(), "cannot read back") {
		t.Errorf("the report does not say what was wrong:\n%s", rep)
	}
}
