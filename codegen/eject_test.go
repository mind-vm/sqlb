package codegen_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/schema"
)

func ejectFixture() *schema.Registry {
	r := schema.NewRegistry()
	org := r.Table("orgs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", org).OnDelete(schema.Cascade).Filterable().ReadOnly().Scoped(),
		schema.Text("title").Searchable().Sortable(),
		schema.Text("secret").Hidden(),
		schema.Enum("status", "draft", "published").Default(schema.Value("draft")).Filterable(),
		schema.BigInt("view_count").Default(schema.Value(0)).Filterable().Sortable().ReadOnly(),
		schema.Computed("is_published", schema.TypeBool,
			schema.FromSQL("status = 'published'")).Filterable(),
		schema.Timestamps(),
		schema.SoftDelete(),
		// OpDelete is absent for the reason the schema validator gives: a table
		// that soft-deletes and exposes a generated DELETE is a contradiction.
	).Index("org_id").Expose(schema.REST{
		Ops:         schema.OpCreate | schema.OpRead | schema.OpUpdate | schema.OpList,
		MaxPageSize: 50,
	})
	return r
}

func eject(t *testing.T, r *schema.Registry) map[string]string {
	t.Helper()
	dir := t.TempDir()
	written, err := codegen.Eject(codegen.EjectOptions{Registry: r, Dir: dir, Package: "ejected"})
	if err != nil {
		t.Fatalf("Eject: %v", err)
	}
	out := map[string]string{}
	for _, path := range written {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(path)] = string(data)
	}
	return out
}

// The exit is a package, and this is what is in it.
func TestEjectWritesASelfContainedPackage(t *testing.T) {
	files := eject(t, ejectFixture())

	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"README.md", "handlers.go", "models.go", "schema.sql", "store.go", "support.go"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("wrote %v, want %v", names, want)
	}

	// The whole argument of the verb: nothing here imports sqlb. A package that
	// did would be an exit you cannot take.
	for name, src := range files {
		if strings.HasSuffix(name, ".go") && strings.Contains(src, `"github.com/jryannel/sqlb`) {
			t.Errorf("%s imports sqlb, so the exit is not one", name)
		}
	}
}

// The schema comes out as SQL, and a computed column comes out as an expression
// in the projection rather than as a column that does not exist.
func TestEjectRendersTheSchemaAsSQL(t *testing.T) {
	files := eject(t, ejectFixture())

	ddl := files["schema.sql"]
	for _, want := range []string{"CREATE TABLE \"posts\"", "\"title\" text NOT NULL", "CREATE INDEX"} {
		if !strings.Contains(ddl, want) {
			t.Errorf("the DDL is missing %q:\n%s", want, ddl)
		}
	}
	if strings.Contains(ddl, "is_published") {
		t.Errorf("a computed column reached the DDL:\n%s", ddl)
	}
	if !strings.Contains(files["store.go"], `(status = 'published') AS \"is_published\"`) {
		t.Errorf("the computed expression should be in the projection:\n%s", files["store.go"])
	}
}

// Capabilities are opt-in in the exit exactly as they were in the schema, and a
// hidden column has no spelling at all.
func TestEjectCarriesTheCapabilities(t *testing.T) {
	store := eject(t, ejectFixture())["store.go"]

	for _, want := range []string{
		// Searchable implies Filterable in the schema, and the exit inherits the
		// implication rather than re-deriving it.
		`{Name: "title", Filterable: true, Sortable: true, Searchable: true, Parse: ParseText}`,
		`{Name: "status", Filterable: true, Sortable: false, Searchable: false, Parse: ParseText}`,
		`{Name: "view_count", Filterable: true, Sortable: true, Searchable: false, Parse: ParseInt}`,
	} {
		if !contains(store, want) {
			t.Errorf("the column table is missing %s:\n%s", want, store)
		}
	}
	if strings.Contains(store, `{Name: "secret"`) {
		t.Error("a hidden column is nameable in the exit")
	}
}

// ADR-0030 survives the loss of everything it was implemented in: the resource
// refuses to register without the hook that confines it.
func TestEjectKeepsTheObligation(t *testing.T) {
	handlers := eject(t, ejectFixture())["handlers.go"]

	for _, want := range []string{
		`"ejected: %s: Confine is required (%s)", "/posts", "org_id is Scoped; deleted_at declares a soft delete"`,
		`Assign is required (%s is Scoped and read-only`,
	} {
		if !contains(handlers, want) {
			t.Errorf("handlers are missing the refusal %q:\n%s", want, handlers)
		}
	}
	// A table that declared nothing needs no hook, and does not pretend to.
	if strings.Contains(handlers, `"/orgs": Confine`) {
		t.Error("a table with no obligation should not require a hook")
	}
}

// A column that no request body carries and the database does not default has
// to be written by the insert, or the first POST fails on a not-null the caller
// never heard of.
func TestEjectSuppliesTheValuesNoBodyCarries(t *testing.T) {
	handlers := eject(t, ejectFixture())["handlers.go"]
	if !contains(handlers, `var postInsertDefaults = map[string]any{`) ||
		!contains(handlers, `"secret": "",`) {
		t.Errorf("the insert does not supply the hidden not-null column:\n%s", handlers)
	}
}

// The exit serves the row with wire-spelled json tags, so the body it accepts
// has to be spelled the same way — otherwise a project that ejects keeps its
// committed clients for reads and loses them for writes, which is the one thing
// the exit promises not to do.
//
// The decoder is where the two spellings meet: a property is matched by wire
// name and recorded under the column name, because the map it returns is what
// the INSERT and the UPDATE are built from.
func TestEjectBodyDecoderSpellsTheWire(t *testing.T) {
	files := eject(t, wireFixture(schema.Camel))
	handlers, models := files["handlers.go"], files["models.go"]

	for _, want := range []string{
		`allowed := []string{"title", "createdAt", "publishedBy"}`,
		`case "createdAt":`,
		`badRequest("body.createdAt", "this property is required", allowed)`,
	} {
		if !strings.Contains(handlers, want) {
			t.Errorf("the ejected decoder does not carry the wire spelling %q:\n%s", want, handlers)
		}
	}
	// And the column is what comes out, since that is what the statement sets.
	for _, want := range []string{`out["created_at"] = *v`, `if _, ok := out["created_at"]; !ok {`} {
		if !strings.Contains(handlers, want) {
			t.Errorf("the ejected decoder does not name the column %q:\n%s", want, handlers)
		}
	}
	if strings.Contains(handlers, `case "created_at":`) {
		t.Errorf("the ejected decoder matches a property the client never sends:\n%s", handlers)
	}
	// The half that was already right, asserted here so the pair cannot drift
	// apart again: what the exit writes back is spelled the same way.
	if !strings.Contains(models, `json:"createdAt"`) {
		t.Errorf("the ejected row stopped serving the wire spelling:\n%s", models)
	}
}

// The README is the honest half of the feature, so it is generated with the
// code rather than written once and left behind.
func TestEjectDocumentsWhatItDoesNotCarry(t *testing.T) {
	readme := eject(t, ejectFixture())["README.md"]
	for _, want := range []string{
		"?cursor", "?select", "?expand", "?filter=",
		"is a computed column",
		"Scoped", "Confine",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README does not mention %q:\n%s", want, readme)
		}
	}
}

// A registry with no REST surface still ejects: the schema and the statements
// are the whole exit for a project that used sqlb as a query builder.
func TestEjectWithoutARestSurface(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("events", schema.UUIDv7("id").PrimaryKey(), schema.Text("kind"))

	files := eject(t, r)
	if _, ok := files["handlers.go"]; ok {
		t.Error("nothing is exposed, so there should be no handlers")
	}
	if !strings.Contains(files["store.go"], "func ListEvent(") {
		t.Error("the statements should come out even with no REST surface")
	}
}

// The exit rots the same way generated code does, so it has the same gate.
func TestEjectCheckReportsDrift(t *testing.T) {
	dir := t.TempDir()
	opts := codegen.EjectOptions{Registry: ejectFixture(), Dir: dir, Package: "ejected"}

	if _, err := codegen.Eject(opts); err != nil {
		t.Fatalf("Eject: %v", err)
	}
	stale, err := codegen.EjectCheck(opts)
	if err != nil {
		t.Fatalf("EjectCheck: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("a freshly written exit is stale: %v", stale)
	}

	if err := os.WriteFile(filepath.Join(dir, "store.go"), []byte("package ejected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "schema.sql")); err != nil {
		t.Fatal(err)
	}
	stale, err = codegen.EjectCheck(opts)
	if err != nil {
		t.Fatalf("EjectCheck: %v", err)
	}
	if len(stale) != 2 {
		t.Errorf("stale = %v, want the edited file and the missing one", stale)
	}
}

func TestEjectRefusesAnInvalidPackageName(t *testing.T) {
	_, err := codegen.Eject(codegen.EjectOptions{Registry: ejectFixture(), Dir: t.TempDir() + "/api-client"})
	if err == nil || !strings.Contains(err.Error(), "EjectOptions.Package") {
		t.Errorf("want a refusal naming the option to set, got %v", err)
	}
}

// The exit's read surface is spelled the way the clients speak, not the way
// Postgres does.
//
// The column table is the one place both names live, and everything downstream
// of it picks one: ?createdAt is what a request says, "created_at" is what the
// WHERE and the ORDER BY say, and a rejection lists the spellings that would
// have been accepted rather than the database's. Before this, the exit answered
// `?createdAt=eq.x` with a 400 whose allowed list named `created_at` — a
// grammar no generated client here has ever sent.
func TestEjectReadSurfaceSpellsTheWire(t *testing.T) {
	files := eject(t, wireFixture(schema.Camel))
	store, support := files["store.go"], files["support.go"]

	// Both names, on the one row that has two.
	if !contains(store, `{Name: "created_at", Wire: "createdAt", Filterable: true, Sortable: true, Searchable: false, Parse: ParseTime}`) {
		t.Errorf("the column table does not carry both spellings:\n%s", store)
	}
	// And only one where they agree: a Wire beside an identical Name is noise
	// in a file meant to be read, and Column.wire falls back to Name.
	if contains(store, `Wire: "title"`) {
		t.Errorf("a column whose two names are the same emitted both:\n%s", store)
	}

	// The request side matches and reports by wire.
	for _, want := range []string{
		"func findColumn(cols []Column, wire string) (Column, bool) {",
		"if c.wire() == wire {",
		`wireNames(cols, func(c Column) bool { return c.Filterable })`,
		`wireNames(cols, func(c Column) bool { return c.Sortable })`,
		`badRequest("query."+col.wire()`,
	} {
		if !contains(support, want) {
			t.Errorf("support.go does not match or report by wire, missing %q", want)
		}
	}
	// And the SQL side is still built from the column.
	for _, want := range []string{
		`Condition{Column: col.Name, Op: OpEq, Value: v}`,
		`Order{Column: col.Name, Desc: desc}`,
		// ?search fans out into predicates nobody is shown, so it stays on the
		// column names.
		`searchable := columnNames(cols, func(c Column) bool { return c.Searchable })`,
		`for _, name := range searchable {`,
		// A path segment is not a query-string key, so the primary key is
		// looked up by the name the schema emitted.
		"func findByColumn(cols []Column, name string) (Column, bool) {",
		"col, ok := findByColumn(cols, pk)",
	} {
		if !contains(support, want) {
			t.Errorf("support.go stopped building SQL from the column name, missing %q", want)
		}
	}

	// The write half agrees, or the exit would accept two grammars at once.
	handlers := files["handlers.go"]
	for _, want := range []string{
		`allowed := []string{"title", "createdAt"}`,
		`case "createdAt":`,
		`out["created_at"] = *v`,
		`[]struct{ column, wire string }{{"title", "title"}, {"created_at", "createdAt"}}`,
		`badRequest("body."+want.wire, "this property is required", allowed)`,
	} {
		if !contains(handlers, want) {
			t.Errorf("the body decoder does not spell the wire, missing %q:\n%s", want, handlers)
		}
	}

	// The README documents the grammar, so it has to say which spelling it is.
	if !strings.Contains(files["README.md"], "column table carries both names") {
		t.Errorf("the README does not say which spelling a request uses:\n%s", files["README.md"])
	}
}

// Verbatim is the default, and it emits one name because there is only one.
func TestEjectUnderVerbatimNamesAColumnOnce(t *testing.T) {
	files := eject(t, wireFixture(schema.Verbatim))

	if !contains(files["store.go"], `{Name: "created_at", Filterable: true`) {
		t.Errorf("the default gained a second spelling:\n%s", files["store.go"])
	}
	if strings.Contains(files["store.go"], "Wire:") {
		t.Error("Verbatim emitted a Wire field, which by definition says nothing")
	}
	if !contains(files["handlers.go"], `allowed := []string{"title", "created_at"}`) {
		t.Errorf("the default's body decoder moved:\n%s", files["handlers.go"])
	}
}

// The ordinary shapes of the exit have to compile.
//
// codegen runs format.Source over every emitted file, which parses without
// type-checking, so an import nothing uses is valid Go *source* and reaches the
// adopter's first build intact. handlers.go and store.go each declared a fixed
// import set at the top of their emitter while the uses of four of those
// packages were conditions scattered through the templates below, and a schema
// that met none of them emitted a `fmt` nothing used.
//
// The exit was already compiled here — by TestEjectedSingletonCompiles, whose
// helper this borrows — but only in shapes that could not show it. A singleton
// cannot be addressed without a Confine hook, so every one of those fixtures
// carries an obligation and so emits the fmt.Errorf that refuses it; and
// `example/blog`, the one committed exit, has a `posts` table that is both
// Scoped and SoftDelete. The case that was broken is the plain one: an exposed
// table declaring neither.
//
// This test names nothing. It ejects each shape and lets the compiler decide.
// The cases are the conditions the import set used to predict, one apiece.
func TestEjectedGoCompiles(t *testing.T) {
	plain := func() *schema.Registry {
		// No Scoped, no SoftDelete: nothing needs a hook, so nothing calls
		// fmt.Errorf. This is the case that did not compile.
		r := schema.NewRegistry()
		r.Table("articles",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title").Searchable().Sortable(),
			schema.Timestamp("created_at").Filterable().Sortable(),
		).Expose(schema.REST{Path: "/articles", Ops: schema.CRUD | schema.OpList})
		return r
	}
	listOnly := func() *schema.Registry {
		// No by-id read, so no errors.Is; no body, so no encoding/json.
		r := schema.NewRegistry()
		r.Table("events",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("kind").Filterable(),
		).Expose(schema.REST{Path: "/events", Ops: schema.OpList})
		return r
	}
	noKey := func() *schema.Registry {
		// No primary key and nothing exposed: store.go gets a list and a count
		// and neither a Get nor an Update, which is what used to name pgx and
		// errors there. No handlers.go at all.
		r := schema.NewRegistry()
		r.Table("samples", schema.Text("label"), schema.Float("value"))
		return r
	}
	prose := func() *schema.Registry {
		// A comment that names a package is prose, not a use — and these files
		// are more comment than code, so it is worth one case of its own. A
		// column comment reaches models.go verbatim, and reading the emitted
		// *text* for "time." rather than its tokens imports a package this
		// schema has no timestamp to need.
		r := schema.NewRegistry()
		r.Table("notes",
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("body").Comment("free text; the API used to carry a time.Time here"),
		)
		return r
	}
	camel := func() *schema.Registry { return wireFixture(schema.Camel) }

	ejectCompiles(t, map[string]*schema.Registry{
		"plain":    plain(),
		"listonly": listOnly(),
		"nokey":    noKey(),
		"prose":    prose(),
		"camel":    camel(),
		"obliged":  ejectFixture(),
	})
}
