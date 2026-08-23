package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jryannel/sqlb/schema"
)

func rules(ds schema.Diagnostics) map[string]bool {
	out := map[string]bool{}
	for _, d := range ds {
		out[d.Rule] = true
	}
	return out
}

func TestLintCatchesUnindexedFilter(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("a",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Filterable(), // no index
		schema.Text("slug").Unique().Filterable(),
	)
	got := rules(r.Lint())
	if !got["unindexed-filter"] {
		t.Error("a filterable column with no index should be flagged")
	}
	for _, d := range r.Lint() {
		if d.Rule == "unindexed-filter" && d.Column == "slug" {
			t.Error("a unique column is already indexed and should not be flagged")
		}
		if d.Rule == "unindexed-filter" && d.Fix == "" {
			t.Error("the diagnostic should carry a concrete fix")
		}
	}
}

func TestLintIgnoresLowCardinalityColumns(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("b",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Bool("active").Filterable(),
		schema.Enum("state", "draft", "live").Filterable(),
	)
	for _, d := range r.Lint() {
		if d.Rule == "unindexed-filter" {
			t.Errorf("a boolean or short enum should not be flagged: %s", d)
		}
	}
}

func TestLintCatchesSearchWithoutTrigram(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("c", schema.UUIDv7("id").PrimaryKey(), schema.Text("title").Searchable())
	if !rules(r.Lint())["search-without-trigram"] {
		t.Error("a searchable column with no GIN index should be flagged")
	}

	r2 := schema.NewRegistry()
	r2.Table("d", schema.UUIDv7("id").PrimaryKey(), schema.Text("title").Searchable()).
		AddIndex(schema.Index{Columns: []string{"title"}, Method: "gin"})
	if rules(r2.Lint())["search-without-trigram"] {
		t.Error("a GIN index should satisfy the search rule")
	}
}

func TestLintCatchesUnstablePagination(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("e", schema.UUIDv7("id").PrimaryKey(), schema.Text("x").Filterable()).
		Index("x").
		Expose(schema.REST{Ops: schema.OpList, MaxPageSize: 50})
	if !rules(r.Lint())["list-without-sort"] {
		t.Error("a list endpoint with no sortable column should be flagged")
	}
}

// #201: every one of sixteen tables in a real port set DefaultPageSize and
// MaxPageSize with Ops == CRUD, no OpList in sight, and nothing said so until
// end-to-end testing found it. This is the same trap as no-max-page-size, one
// register down: page-size fields configured with no list route to bound.
func TestLintCatchesPageSizeWithoutList(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("e", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.CRUD, DefaultPageSize: 20, MaxPageSize: 50})
	got := rules(r.Lint())
	if !got["page-size-without-list"] {
		t.Error("page-size fields set without OpList should be flagged")
	}
}

func TestLintIsQuietOnPageSizeWithList(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("e", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList, DefaultPageSize: 20, MaxPageSize: 50})
	if rules(r.Lint())["page-size-without-list"] {
		t.Error("page-size fields with OpList present should not be flagged")
	}
}

func TestLintCatchesUnindexedExpansion(t *testing.T) {
	r := schema.NewRegistry()
	org := r.Table("orgs", schema.UUIDv7("id").PrimaryKey())
	r.Table("f", schema.UUIDv7("id").PrimaryKey(), schema.Ref("org", org).Expandable())
	if !rules(r.Lint())["unindexed-expand"] {
		t.Error("an expandable relation with no index on its key should be flagged")
	}
}

func TestLintSeparatesWarningsFromInfo(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("g", schema.UUIDv7("id").PrimaryKey(), schema.Text("x").Sortable()).
		Expose(schema.REST{Ops: schema.OpList})

	all := r.Lint()
	warn := all.Warnings()
	if len(warn) >= len(all) {
		t.Error("this schema should produce info-level diagnostics that are not warnings")
	}
	for _, d := range warn {
		if d.Severity != schema.SeverityWarn {
			t.Errorf("Warnings returned a %s diagnostic", d.Severity)
		}
	}
}

// A Text column with no Default and no Nullable is required on the generated
// create body — correct, but the first symptom a porting author sees is a 422
// on a field they assumed behaved like Django's blank=True. The rule is
// scoped to TypeText, exposed via OpCreate, and excludes ReadOnly, Hidden and
// PrimaryKey columns, none of which are part of the ordinary create body.
func TestLintNotesARequiredTextColumnWithNoDefault(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
	).Expose(schema.REST{Ops: schema.OpCreate | schema.OpRead})

	got := rules(r.Lint())
	if !got["text-required-on-create"] {
		t.Error("a required Text column with no default should get a note")
	}
	for _, d := range r.Lint() {
		if d.Rule == "text-required-on-create" {
			if d.Severity != schema.SeverityInfo {
				t.Errorf("text-required-on-create should be info, not %s", d.Severity)
			}
			if d.Fix == "" {
				t.Error("the diagnostic should carry a concrete fix")
			}
		}
	}
}

// The negative case matters more here than usual: a required Text column is
// the common, correct case, not the exception, so the rule must stay quiet
// once the schema already answers "is this deliberately required" — a
// Default, Nullable, ReadOnly, Hidden or PrimaryKey column, a table that
// never exposes OpCreate, or a Varchar rather than a Text column.
func TestLintIsQuietOnDeliberatelyRequiredOrNonTextColumns(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Default(schema.Value("untitled")),
		schema.Text("subtitle").Nullable(),
		schema.Text("rendered_html").ReadOnly(),
		schema.Text("internal_note").Hidden(),
		schema.Varchar("slug", 64),
	).Expose(schema.REST{Ops: schema.OpCreate | schema.OpRead})

	if got := rules(r.Lint())["text-required-on-create"]; got {
		t.Errorf("none of these columns should be flagged, got: %s", r.Lint())
	}

	noCreate := schema.NewRegistry()
	noCreate.Table("drafts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
	).Expose(schema.REST{Ops: schema.OpRead})
	if got := rules(noCreate.Lint())["text-required-on-create"]; got {
		t.Error("a table with no OpCreate has no create body to warn about")
	}
}

func TestLintIsQuietOnAWellFormedSchema(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("h",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique().Filterable(),
		schema.Timestamp("created_at").Sortable(),
	).
		Index("created_at").
		Expose(schema.REST{Ops: schema.OpList, MaxPageSize: 100})

	if w := r.Lint().Warnings(); len(w) > 0 {
		t.Errorf("a well-formed schema should produce no warnings, got:\n%s", w)
	}
}

func TestManifestDescribesTheQueryableSurface(t *testing.T) {
	r := schema.NewRegistry()
	org := r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", org).Expandable(),
		schema.Text("title").Searchable().Sortable(),
		schema.Enum("status", "draft", "live").Filterable(),
		schema.Text("secret").Hidden(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList, MaxPageSize: 100})

	m := r.BuildManifest()
	var posts *schema.TableManifest
	for i := range m.Tables {
		if m.Tables[i].Name == "posts" {
			posts = &m.Tables[i]
		}
	}
	if posts == nil {
		t.Fatal("posts missing from the manifest")
	}

	// A hidden column must not appear at all: the manifest is publishable, and
	// the name is itself information.
	for _, c := range posts.Columns {
		if c.Name == "secret" {
			t.Error("a hidden column leaked into the manifest")
		}
	}
	if posts.REST == nil {
		t.Fatal("REST surface missing")
	}
	if !contains(posts.REST.Filterable, "status") || !contains(posts.REST.Searchable, "title") {
		t.Errorf("capabilities not reported: %+v", posts.REST)
	}
	// The manifest reports what a caller can actually ask for, and expansion
	// now performs the join. It is reported under the *relation* name: the
	// column is "org_id", but the request is ?expand=org.
	if !contains(posts.REST.Expandable, "org") {
		t.Errorf("manifest does not advertise the expandable relation: %+v", posts.REST.Expandable)
	}
	if contains(posts.REST.Expandable, "org_id") {
		t.Errorf("manifest advertises the foreign key column, not the relation: %+v", posts.REST.Expandable)
	}
	if !contains(columnByName(posts, "org_id").Capabilities, "expand") {
		t.Error("org_id does not carry the expand capability")
	}
	if len(posts.REST.Examples) == 0 {
		t.Error("no worked examples emitted")
	}
	if posts.PrimaryKey != "id" {
		t.Errorf("primary key = %q", posts.PrimaryKey)
	}

	// The enum's values are what a client needs to send a valid filter.
	for _, c := range posts.Columns {
		if c.Name == "status" && len(c.Enum) != 2 {
			t.Errorf("enum values not reported: %+v", c)
		}
	}

	b, err := m.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !json.Valid(b) {
		t.Error("manifest is not valid JSON")
	}
	if !strings.Contains(string(b), "filterOperators") {
		t.Error("the operator vocabulary should be in the manifest")
	}
}

// columnByName panics rather than returning a zero value, so an assertion
// about a column that vanished reads as a missing column instead of a
// capability that mysteriously stopped being reported.
func columnByName(t *schema.TableManifest, name string) schema.ColumnManifest {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	panic("no column " + name + " in " + t.Name)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A reverse expansion runs one subquery per row of the page, so an unindexed
// foreign key costs a scan per row rather than one for the statement — and the
// cap's escape hatch is the child's own endpoint filtered by that column, which
// does not exist unless the column is filterable. ADR-0022.
func TestLintCatchesAnExpensiveInverse(t *testing.T) {
	r := schema.NewRegistry()
	authors := r.Table("authors", schema.UUIDv7("id").PrimaryKey())
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("author", authors).Inverse("posts").InverseExpandable(),
	)

	got := rules(r.Lint())
	if !got["unindexed-inverse-expand"] {
		t.Error("a collected foreign key with no index should be flagged")
	}
	if !got["uncapped-inverse-overflow"] {
		t.Error("a capped collection whose overflow cannot be filtered should be flagged")
	}

	// And both go quiet once the schema answers them.
	ok := schema.NewRegistry()
	okAuthors := ok.Table("authors", schema.UUIDv7("id").PrimaryKey())
	ok.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("author", okAuthors).Filterable().Inverse("posts").InverseExpandable(),
	).Index("author_id")
	got = rules(ok.Lint())
	if got["unindexed-inverse-expand"] || got["uncapped-inverse-overflow"] {
		t.Errorf("an indexed, filterable foreign key should not be flagged: %v", ok.Lint())
	}
}

// The rule that closes #293's first ask, and the false positive that would
// make it useless.
//
// A raw default is indistinguishable from a helper's by value — GenUUIDv7 and
// Expr both produce a *Default with a Raw string, and every other reader in the
// pipeline treats that string as the helper's identity (codegen renders it back
// out as the constructor, introspect maps it back in). So the rule cannot fire
// on the canonical spelling without firing on every column that used the
// helper. What it fires on instead is the spelling no helper produces but
// migrate emits: the target-specific builtin.
func TestLintNamesTheHelperBehindAHandWrittenBuiltinDefault(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("a",
		schema.UUID("id").PrimaryKey().Default(schema.Expr("uuidv7()")),
	)

	var got *schema.Diagnostic
	for _, d := range r.Lint() {
		if d.Rule == "raw-default-has-helper" {
			got = &d
		}
	}
	if got == nil {
		t.Fatalf("Expr(\"uuidv7()\") should be flagged, got:\n%s", r.Lint())
	}
	if got.Column != "id" {
		t.Errorf("the diagnostic should name the column, got %q", got.Column)
	}
	// The whole value of the rule is that it names the call to write instead.
	// A message that only said "this is raw SQL" would send the reader looking.
	if !strings.Contains(got.Fix, "schema.GenUUIDv7()") {
		t.Errorf("the fix should name the constructor, got %q", got.Fix)
	}
	if got.Severity != schema.SeverityWarn {
		t.Errorf("giving up the portability the helper provides is a warning, got %q", got.Severity)
	}
}

func TestLintIsQuietOnDefaultsItCannotDistinguishFromAHelper(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("a",
		schema.UUIDv7("id").PrimaryKey(),
		// The canonical spelling: what GenUUIDv7 itself produces, so flagging
		// it would flag every column in every schema that used the helper.
		schema.UUID("other").Default(schema.GenUUIDv7()),
		// A composite the helper could not have produced. migrate's resolve
		// leaves this alone too — both match exactly or not at all.
		schema.Text("note").Default(schema.Expr("coalesce(uuidv7()::text, '')")),
		schema.Timestamp("at").Default(schema.Now()),
	)
	for _, d := range r.Lint() {
		if d.Rule == "raw-default-has-helper" {
			t.Errorf("nothing here spells a target-specific builtin: %s", d)
		}
	}
}
