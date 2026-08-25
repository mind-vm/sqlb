package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/schema"
)

// Generated Go has to compile, and nothing else in this suite checks that.
//
// codegen runs format.Source over every file, which parses without
// type-checking: a missing import, an unused one, and an assignment across a
// pointer boundary are all valid Go *source*, so all three passed the emitter
// and failed at the consumer's next build. Four such bugs have shipped, one per
// emitter and one per direction, and each was caught by a person compiling the
// output rather than by this package.
//
// Substring assertions cannot close that off, because the assertion has to name
// the mistake in advance. This test names nothing: it compiles what the emitters
// produce, and the compiler decides. The cases below are shapes, not claims —
// add one when a schema construct is new, not when a bug is found.
func TestGeneratedGoCompiles(t *testing.T) {
	// pgtype.UUID rather than the google/uuid the documentation uses: the
	// override has to name a package this module already requires, because
	// what is compiled here is compiled against this go.mod, and a test is a
	// poor reason for a third direct dependency. The import mechanics are the
	// same either way.
	uuid := []codegen.TypeOverride{
		{Type: schema.TypeUUID, GoType: "pgtype.UUID", Import: "github.com/jackc/pgx/v5/pgtype"},
	}
	compiles(t, map[string]codegen.Options{
		// Exposed for reads only: the file is Register and nothing else, so
		// every import past the three constant ones is unused. This is the
		// case that prompted the test.
		"listonly": {Registry: fixture()},
		// A full CRUD resource, which is both bodies and every optionality
		// rule they encode.
		"crud": {Registry: restFixture()},
		// Expandable relations, which put a db:"-" field in the model.
		"expandable": {Registry: expandFixture()},
		// The slice-typed columns, where the model is not a pointer but the
		// create body is, so Row() has to dereference. Create-only, which is
		// the shape that names json.RawMessage without a patch body to import
		// it.
		"slices": {Registry: sliceFixture()},
		// A patch body over columns that are all nullable, so Changes() never
		// rejects an explicit null and the file names errors nowhere.
		"nullablepatch": {Registry: nullablePatchFixture()},
		// An override whose only matching column is a primary key: rendered by
		// the models and columns files, carried by no request body.
		"overridepk": {Registry: pkOnlyOverrideFixture(), Types: uuid},
		// The same override where a body does carry it.
		"overridebody": {Registry: bodyOverrideFixture(), Types: uuid},
		// A vector column, which is hidden and so reaches the Go artefacts
		// only.
		"vector": {Registry: vectorFixture()},
		// The columns file's two halves disagree about hidden columns: the
		// facade omits them, the typed update sets them. A hidden column whose
		// type needs an import is the shape where that matters.
		"hidden": {Registry: hiddenFixture()},
		// A nullable vector, which renders as *sqlb.Vector — the spelling the
		// models emitter's import switch did not have a case for.
		"nullablevector": {Registry: nullableVectorFixture()},
		// Computed columns (#58), which are in the facade but not in the typed
		// update, and which put a sqlb.Computed-returning method on the model.
		// A construct this new is exactly what the case list is for; the
		// fixture is computed_test.go's, so the two cannot describe different
		// schemas under the same name.
		"computed": {Registry: computedFixture()},
		// The same, overridden. The computed column's sqlb import is earned by
		// the method rather than by the field's type, so it must survive the
		// guard that skips an overridden column.
		"computedoverride": {Registry: computedFixture(), Types: []codegen.TypeOverride{
			{Type: schema.TypeBool, GoType: "pgtype.Bool", Import: "github.com/jackc/pgx/v5/pgtype"},
		}},
		// Declared actions (#18): an Actions struct whose fields are func
		// types, a parameter on Register, and one input type per verb. It is
		// the first thing in this file whose imports are earned by a request
		// body rather than by a column — context by the signatures, time by a
		// body property on a table whose own columns need neither.
		"actions": {Registry: actionFixture()},
		// A singleton (#166), whose mount names an op the other cases do not
		// and whose keyless sibling is the first exposed table in this list
		// with no primary key at all — the emitters that reach for one have to
		// keep working without it.
		"singleton":        {Registry: singletonFixture()},
		"keylesssingleton": {Registry: keylessSingletonFixture()},
		// A create body carrying properties that are not columns (#309): a
		// second body type, an Input() method returning it, and — like the
		// actions case — imports earned by a property rather than by a column,
		// on a table whose own columns need none.
		"createinput": {Registry: createInputCompileFixture()},
	})
}

func sliceFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("documents",
		schema.UUIDv7("id").PrimaryKey(),
		schema.JSON("meta"),
		schema.JSON("attachment_ids").Nullable(),
		schema.Bytes("thumbnail").Nullable(),
		schema.Text("tags").Array().Nullable(),
	).Expose(schema.REST{Ops: schema.OpCreate})
	return r
}

func nullablePatchFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Nullable(),
		schema.Timestamp("published_at").Nullable(),
	).Expose(schema.REST{Ops: schema.OpUpdate})
	return r
}

func pkOnlyOverrideFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Timestamp("published_at").Nullable(),
	).Expose(schema.REST{Ops: schema.OpList})
	return r
}

func bodyOverrideFixture() *schema.Registry {
	r := schema.NewRegistry()
	orgs := r.Table("orgs", schema.UUIDv7("id").PrimaryKey(), schema.Text("name"))
	r.Table("posts",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("org", orgs).OnDelete(schema.Cascade),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

func hiddenFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		// Both types that need an import, hidden, and nothing else in the
		// schema needing either — so the facade's field set alone accounts for
		// neither.
		schema.Timestamp("last_seen_at").Hidden(),
		schema.JSON("audit").Hidden(),
	)
	return r
}

func nullableVectorFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Vector("embedding", 1536).Nullable(),
	)
	return r
}

func vectorFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("chunks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
		schema.Vector("embedding", 1536),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

// gencheckPrefix names the scratch directory the generated packages are
// compiled in. The leading dot is what keeps them out of ./... — `go vet ./...`
// and `go build ./...` skip a directory beginning with "." or "_", while an
// explicit path to one still builds — and .gitignore carries the rule for the
// same reason it carries .sqlb-driver-*: a run killed between generating and
// cleaning up leaves a directory in somebody's working tree, and one of those
// has already reached a commit.
const gencheckPrefix = ".sqlb-gencheck-"

// compiles generates each case into a package and hands the lot to the Go
// compiler.
//
// Inside the module rather than in a temporary directory of its own, because
// the generated code imports sqlb and huma and this module's go.mod is what
// resolves both. A temp module elsewhere would need its own go.mod and go.sum,
// and would answer a question about *that* module's dependency graph rather
// than about the one a consumer generates into.
//
// One `go build` over every package, so the dependencies are compiled once.
func compiles(t *testing.T, cases map[string]codegen.Options) {
	t.Helper()

	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool on PATH, so generated code cannot be compiled: %v", err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := os.MkdirTemp(root, gencheckPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })

	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)

	pkgs := make([]string, 0, len(names))
	for _, name := range names {
		opts := cases[name]
		opts.Dir = filepath.Join(scratch, name)
		// The package clause, not the directory name: the two need not agree,
		// and every case emitting the same one keeps the failure output about
		// the path rather than about an identifier chosen here.
		opts.Package = "gencheck"
		if _, err := codegen.Generate(opts); err != nil {
			t.Fatalf("%s: Generate: %v", name, err)
		}
		pkgs = append(pkgs, "./"+filepath.Base(scratch)+"/"+name)
	}

	// A non-main package builds to nothing, so this leaves no artefact behind
	// beyond the build cache.
	build := exec.Command(gotool, append([]string{"build"}, pkgs...)...)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Errorf("generated code does not compile: %v\n%s", err, out)
	}
}

// createInputCompileFixture is the declared create input in the shape that
// exercises the emitter's type rules: a required property, a nullable one that
// becomes a pointer, and one whose Go type needs an import this table's columns
// do not.
func createInputCompileFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("children",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name"),
		schema.Varchar("pin_hash", 255).Hidden(),
	).Expose(schema.REST{
		Ops: schema.CRUD | schema.OpList,
		CreateInput: schema.Body(
			schema.Varchar("pin", 4),
			schema.Text("invite_code").Nullable(),
			schema.Timestamp("consent_at"),
		),
	})
	return r
}
