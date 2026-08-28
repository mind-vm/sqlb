package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// The tenancy bundle's missing half (#274).
//
// A `Scoped` column travels as one declaration and obliges a hook. The hook
// itself could not travel: a consumer with nineteen tables wrote the column on
// sixteen of them and paired it by hand with one function registered sixteen
// times, and two of those tables denormalised a column purely so the generic
// hook would apply — which is the schema bending to fit the seam.
//
// The carrier is codegen, which the "Mixins carry behaviour" decision settled:
// the schema package imports nothing from the engine and the model type a hook
// keys on does not exist when a schema is declared, so codegen is the only
// layer holding both.

func tenancyFixture() *schema.Registry {
	r := schema.NewRegistry()
	workspaces := r.Table("workspaces", schema.UUIDv7("id").PrimaryKey())
	for _, name := range []string{"notes", "tasks"} {
		r.Table(name,
			schema.UUIDv7("id").PrimaryKey(),
			schema.Text("title"),
			schema.Ref("workspace", workspaces).ReadOnly().Filterable().Scoped(),
		)
	}
	return r
}

func TestOneScopeColumnBecomesOneFuncForEveryTableThatCarriesIt(t *testing.T) {
	src := generate(t, tenancyFixture())["scopes_gen.go"]

	// One field, not one per table: sixteen tables carrying workspace_id are
	// one question about the caller, and answering it sixteen times is the
	// repetition this exists to remove.
	if n := strings.Count(src, "func(context.Context) (string, error)"); n != 1 {
		t.Errorf("want exactly one resolver field, got %d:\n%s", n, src)
	}
	for _, want := range []string{
		"WorkspaceID func(context.Context) (string, error)",
		"func RegisterScopes(reg *sqlb.Registry, name string, s Scopes) error {",
		// The three predicates go under the releasable name, so an admin
		// handle can be released from them. Without this the emitted hooks
		// would confine correctly and could never be released, which is the
		// affordance a named scope exists for.
		"sqlb.On[Note](reg).Scope(name).",
		// Every operation the declaration obliges.
		"BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Note]) error {",
		"BeforeUpdate(func(ctx context.Context, u *sqlb.Update[Note]) error {",
		"BeforeDelete(func(ctx context.Context, d *sqlb.Delete[Note]) error {",
		`sqlb.On[Note](reg).BeforeCreate(func(ctx context.Context, row *Note) error {`,
		// The predicate, and the stamp written through the typed field —
		// which is what codegen can do and a hand-written generic helper
		// cannot without reflection.
		`q.Where(sqlb.F("workspace_id").Eq(v))`,
		"row.WorkspaceID = v",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("emitted scopes missing %q:\n%s", want, src)
		}
	}
}

// The create stamp is not registered under the releasable name, and that is the
// whole difference between releasing a predicate and releasing a stamp: one
// shows a row more, the other writes a row belonging to nobody. ScopedHooks
// refuses BeforeCreate outright, so emitting it there would not even compile.
func TestTheCreateStampIsNotReleasable(t *testing.T) {
	src := generate(t, tenancyFixture())["scopes_gen.go"]
	stamp := src[strings.Index(src, "sqlb.On[Note](reg).BeforeCreate"):]
	if strings.Contains(stamp[:strings.Index(stamp, "})")], "Scope(name)") {
		t.Errorf("the create stamp is registered under a releasable scope:\n%s", stamp)
	}
}

// A table scoped on its own primary key is left alone unless it names the
// scope. Its confinement is particular to it — an identity, a membership
// subquery — and is not the `column = value` this writes, so generating an
// equality for it would be confidently wrong.
func TestAnUnnamedIdentityScopeIsSkippedAndSaidSo(t *testing.T) {
	r := tenancyFixture()
	r.Table("accounts", schema.UUIDv7("id").PrimaryKey().Scoped())

	src := generate(t, r)["scopes_gen.go"]
	if strings.Contains(src, "sqlb.On[Account](reg)") {
		t.Errorf("an unnamed identity scope should not be registered:\n%s", src)
	}
	// And the file says so, because one that reads as complete stops the
	// reader looking at exactly the wrong moment — and what they would stop
	// looking for is a confinement.
	if !strings.Contains(src, "does not cover accounts") {
		t.Errorf("the file should name the table it does not cover:\n%s", src)
	}
}

// Naming the scope is how a table that *is* the tenant shares one answer with
// the tables pointing at it: one func, and each table's own column in its own
// predicate.
func TestANamedIdentityScopeJoinsTheGroup(t *testing.T) {
	r := schema.NewRegistry()
	workspaces := r.Table("workspaces", schema.UUIDv7("id").PrimaryKey().Scoped("workspace"))
	r.Table("notes",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("workspace", workspaces).ReadOnly().Scoped("workspace"),
	)

	src := generate(t, r)["scopes_gen.go"]
	if n := strings.Count(src, "func(context.Context) (string, error)"); n != 1 {
		t.Errorf("a named scope shared by two tables is one func, got %d:\n%s", n, src)
	}
	// Each table is confined on its own column, which is the point of grouping
	// by the question rather than by the column name.
	for _, want := range []string{
		`q.Where(sqlb.F("id").Eq(v))`,
		`q.Where(sqlb.F("workspace_id").Eq(v))`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("emitted scopes missing %q:\n%s", want, src)
		}
	}
}

// A schema declaring no scope gains no file, so this is inert for every project
// that does not use it.
func TestASchemaWithNoScopeGetsNoScopesFile(t *testing.T) {
	if src, ok := generate(t, restFixture())["scopes_gen.go"]; ok {
		t.Errorf("an unscoped schema should emit no scopes file:\n%s", src)
	}
}
