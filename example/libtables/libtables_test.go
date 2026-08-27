// Package libtables_test is the host's half of the worked example, and every
// claim the README and sessionkit's doc comment make is asserted here.
//
// No database: what is being claimed is about the DDL a composed registry
// produces and the SQL a library's own model compiles to, both of which are
// values. Run it with `go test ./example/libtables/`.
package libtables_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/example/libtables/appschema"
	"github.com/mind-vm/sqlb/example/libtables/sessionkit"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/rest"
	"github.com/mind-vm/sqlb/schema"
)

// ddl renders what `sqlb migrate` would write for a registry with nothing
// applied yet, which is the shape the composition question is really about.
func ddl(t *testing.T, reg *schema.Registry) string {
	t.Helper()
	if err := reg.Validate(); err != nil {
		t.Fatalf("the composed registry does not validate: %v", err)
	}
	changes, err := migrate.Diff(schema.NewRegistry(), reg)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(c.Up)
		b.WriteString("\n")
	}
	return b.String()
}

// The point of the whole convention: a library's column is a real foreign key,
// because the table it points at is in the same registry.
func TestTheLibrarysReferenceIsEnforced(t *testing.T) {
	out := ddl(t, appschema.Registry)

	const want = `ALTER TABLE "sessionkit_sessions" ADD CONSTRAINT ` +
		`"sessionkit_sessions_user_id_fkey" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE`
	if !strings.Contains(out, want) {
		t.Errorf("the library's reference is not enforced:\n%s", out)
	}
}

// And the ordering claim, which is the reason a library must not own its own
// sequence: one registry emits every table before any constraint, so no
// declaration order can make a cross-boundary reference fail to apply.
func TestConstraintsFollowEveryTable(t *testing.T) {
	out := ddl(t, appschema.Registry)
	lastCreate := strings.LastIndex(out, "CREATE TABLE")
	firstFK := strings.Index(out, "ADD CONSTRAINT")
	if lastCreate < 0 || firstFK < 0 {
		t.Fatalf("expected both statements:\n%s", out)
	}
	if firstFK < lastCreate {
		t.Errorf("a foreign key is added before the last table exists:\n%s", out)
	}
}

// The same Declare, with no host table to point at, still works — which is what
// lets the library be used standalone and tested without the host. One code
// path, two outcomes, and the column keeps its name either way.
func TestStandaloneDeclarationDropsTheKeyAndKeepsTheColumn(t *testing.T) {
	reg := schema.NewRegistry()
	sessionkit.Declare(reg, sessionkit.Options{})

	out := ddl(t, reg)
	if strings.Contains(out, "FOREIGN KEY") {
		t.Errorf("a foreign key appeared with nothing to reference:\n%s", out)
	}
	if !strings.Contains(out, `"user_id" uuid`) {
		t.Errorf("the column changed shape when the reference was dropped:\n%s", out)
	}
}

// The prefix earns its keep: an unprefixed name would collide with a host that
// happens to own a table of the same name, and a collision is a panic at
// declaration rather than something discovered in the DDL.
func TestACollidingNameIsRefusedAtDeclaration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("two tables with one name should panic at declaration")
		}
	}()
	reg := schema.NewRegistry()
	reg.Table(sessionkit.TablePrefix+"sessions", schema.UUIDv7("id").PrimaryKey())
	sessionkit.Declare(reg, sessionkit.Options{})
}

// The library runs its own queries with no generated code anywhere, because the
// engine reflects over the struct tags on sessionkit.Session.
func TestTheLibraryQueriesItsOwnTableWithoutCodegen(t *testing.T) {
	q := sqlb.Query[sessionkit.Session]().Where(sqlb.F("token_hash").Eq("abc"))
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `FROM "sessionkit_sessions"`) {
		t.Errorf("the model named the wrong table: %s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want the one bind", args)
	}
}

// hostSession stands in for the model the host's own `sqlb generate` emits for
// the library's table. Two Go types over one table is not a conflict: the model
// cache is keyed by type, so the library's queries and the host's generated
// resource address the same rows through different structs.
type hostSession struct {
	ID        string    `db:"id" json:"id" sqlb:"pk,default"`
	UserID    string    `db:"user_id" json:"user_id" sqlb:"filter,scope"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at" sqlb:"filter,sort"`
}

func (hostSession) TableName() string { return sessionkit.TablePrefix + "sessions" }

func TestAHostModelAndTheLibrarysCoexist(t *testing.T) {
	libSQL, _, err := sqlb.Query[sessionkit.Session]().SQL()
	if err != nil {
		t.Fatal(err)
	}
	hostSQL, _, err := sqlb.Query[hostSession]().SQL()
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{libSQL, hostSQL} {
		if !strings.Contains(sql, `FROM "sessionkit_sessions"`) {
			t.Errorf("both models must address the same table: %s", sql)
		}
	}
	// Different projections, same table — which is the property that makes the
	// host's generated model and the library's hand-written one independent.
	if strings.Contains(hostSQL, "token_hash") {
		t.Errorf("the host's narrower model projected a column it does not carry: %s", hostSQL)
	}
	if !strings.Contains(libSQL, "token_hash") {
		t.Errorf("the library's model lost a column it does carry: %s", libSQL)
	}
}

// Confinement travels from the library to the host, and the host cannot ignore
// it: a mount over a scope-declaring model refuses to start until a hook
// confines it. The library knows the rows are confidential; only the host knows
// what confines them, so this is the direction the obligation has to travel.
func TestConfinementObligesTheHost(t *testing.T) {
	srv := rest.NewServer(rest.Config{Title: "libtables", Version: "0"})
	db := sqlb.New(deadExec{})

	err := rest.Resource[sessionkit.Session, rest.None[sessionkit.Session], rest.None[sessionkit.Session]](
		srv.API, db, rest.Options{Path: "/sessions", Name: "session", Ops: rest.OpList})
	if err == nil {
		t.Fatal("a scope-declaring model mounted with nothing confining it")
	}
	if !strings.Contains(err.Error(), "nothing confines") {
		t.Errorf("the refusal should name the problem: %v", err)
	}

	// With the host's rule registered, the same mount succeeds.
	reg := sqlb.NewRegistry()
	sqlb.On[sessionkit.Session](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[sessionkit.Session]) error {
		user, ok := sqlb.PrincipalFrom[string](ctx)
		if !ok {
			return errors.New("no principal on this context")
		}
		q.Where(sqlb.F("user_id").Eq(user))
		return nil
	})

	srv2 := rest.NewServer(rest.Config{Title: "libtables", Version: "0"})
	if err := rest.Resource[sessionkit.Session, rest.None[sessionkit.Session], rest.None[sessionkit.Session]](
		srv2.API, db.WithHooks(reg), rest.Options{Path: "/sessions", Name: "session", Ops: rest.OpList}); err != nil {
		t.Fatalf("the mount still refused with a hook registered: %v", err)
	}
}

// A host may add a column to a library's table, and it lands in the migration
// like any other. What it does not do is reach the library's own model, which
// is the half worth knowing before reaching for this: the added column is the
// host's to read, through the host's own struct.
func TestTheHostCanExtendALibraryTable(t *testing.T) {
	reg := schema.NewRegistry()
	tables := sessionkit.Declare(reg, sessionkit.Options{})
	tables.Sessions.AddField(schema.Text("device_name").Nullable().Filterable())

	out := ddl(t, reg)
	if !strings.Contains(out, `"device_name" text`) {
		t.Errorf("the host's column is missing from the migration:\n%s", out)
	}

	sql, _, err := sqlb.Query[sessionkit.Session]().SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "device_name") {
		t.Errorf("the library's model projected a column the host added: %s", sql)
	}
}

// The library's query takes an Executor, so the host decides whether it carries
// the hook registry — which is how a library's rows end up confined by rules
// written for tables the library has never heard of.
func TestTheLibrarysQueryRunsThroughTheHostsRules(t *testing.T) {
	reg := sqlb.NewRegistry()
	sqlb.On[sessionkit.Session](reg).BeforeQuery(func(_ context.Context, q *sqlb.Builder[sessionkit.Session]) error {
		q.Where(sqlb.F("workspace_id").Eq("acme"))
		return nil
	})

	rec := &recorder{}
	db := sqlb.New(rec).WithHooks(reg)
	if _, err := sessionkit.Live(context.Background(), db, time.Now()); err == nil {
		t.Fatal("the recording executor should have failed the read")
	}
	if !strings.Contains(rec.last, `"workspace_id" = $2`) {
		t.Errorf("the host's rule did not reach the library's own query: %s", rec.last)
	}
}

// deadExec is an Executor that never succeeds. Mounting a resource does not run
// a statement, so nothing here is ever called — it exists because sqlb.New
// refuses a nil executor.
type deadExec struct{}

func (deadExec) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("libtables: this executor never runs a statement")
}

func (deadExec) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("libtables: this executor never runs a statement")
}

// BeginTx exists so a mount that wraps its writes in a transaction gets past
// the capability check rest.Resource makes at registration. It is never called:
// nothing here executes a statement.
func (deadExec) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("libtables: this executor never begins a transaction")
}

// recorder keeps the last statement so a test can assert what a hook added.
type recorder struct {
	deadExec
	last string
}

func (r *recorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.last = sql
	return r.deadExec.Query(ctx, sql, args...)
}

// The host's codegen emits no model for the library's table, because the
// library said where its models live (#284).
//
// This is not the same claim as TestAHostModelAndTheLibrarysCoexist above. That
// one is about a model the host wrote *on purpose* — narrower, over the same
// table, and its author knows both exist. This is about the one nobody asked
// for: `sqlb generate` sees the whole registry, so it used to emit a full row
// struct for every library table into the host's package, identical in shape to
// the library's and carrying none of its hooks.
//
// Hooks are keyed by Go type, so a query written against the generated copy runs
// with no confinement at all — same table, same rows, no error, plausible data.
// And the shadow lands in the package the host's author is already importing for
// their own types.
func TestTheHostsCodegenEmitsNoModelForTheLibrarysTable(t *testing.T) {
	dir := t.TempDir()
	files, err := codegen.Generate(codegen.Options{
		Registry: appschema.Registry, Dir: dir, Package: "data",
		ClientImportPath: "example.com/app/cli/client",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	var models string
	for _, f := range files {
		if filepath.Base(f) != "models_gen.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		models = string(b)
	}
	if models == "" {
		t.Fatal("no models file was generated")
	}

	// The host's own table is generated as always.
	if !strings.Contains(models, `return "users"`) {
		t.Errorf("the host's own model is missing:\n%s", models)
	}
	// The library's is not.
	if strings.Contains(models, `return "sessionkit_sessions"`) {
		t.Errorf("the host generated a shadow model for the library's table:\n%s", models)
	}

	// And the table is still migrated, which is what keeps the host's foreign
	// key into it real. Skipping the model must not skip the DDL.
	if !strings.Contains(ddl(t, appschema.Registry), `CREATE TABLE "sessionkit_sessions"`) {
		t.Error("the library's table must still be migrated by the host")
	}
}
