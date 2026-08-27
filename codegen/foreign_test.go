package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// A library declaring its tables into the host's registry (#284).
//
// That arrangement is what lets a host's column carry a real foreign key into a
// library's table — a module-qualified target cannot — so the registry has to
// hold everything. What must not follow is a second Go type per table: hooks
// are keyed by Go type, so a confinement hook registered on the library's
// `User` does not fire for a query written against the host's `User`. Same
// table, same rows, no hook, no error. The shadow sits in the package the
// author is already importing for the host's own types, and autocomplete offers
// it.

// generateInto is `generate` with the package name under this run's control,
// which is the whole of what decides ownership.
func generateInto(t *testing.T, r *schema.Registry, pkg string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	files, err := codegen.Generate(codegen.Options{
		Registry: r, Dir: dir, Package: pkg,
		ClientImportPath: "example.com/app/cli/client",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = string(b)
	}
	return out
}

// shared is the registry a host ends up with: its own tables, plus the ones a
// library declared into it and already generates models for.
func shared() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("users",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("email").Unique(),
	).ModelsIn("authitstore").
		Expose(schema.REST{Ops: schema.Reads})
	users := r.Get("users")
	r.Table("coaches",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("user", users),
		schema.Text("bio"),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

func TestATableAnotherPackageModelsIsNotEmittedHere(t *testing.T) {
	files := generate(t, shared())

	// The host's own table is generated as before.
	if !strings.Contains(files["models_gen.go"], "type Coach struct {") {
		t.Errorf("the host's own model is missing:\n%s", files["models_gen.go"])
	}
	// The library's is not — this is the shadow that ran without hooks.
	for name, src := range files {
		if strings.Contains(src, "type User struct {") {
			t.Errorf("%s emits a shadow model for a table another package owns:\n%s", name, src)
		}
		if strings.Contains(src, `func (User) TableName()`) {
			t.Errorf("%s emits a TableName for a table another package owns", name)
		}
	}
}

// The half that makes the skip safe rather than merely quiet: the table is
// still migrated. Dropping it from the DDL would turn a shadowed model into a
// missing table, which is a worse outcome than the one being fixed, and the
// cross-boundary foreign key depends on it existing.
func TestASkippedTableIsStillMigrated(t *testing.T) {
	changes, err := migrate.Diff(schema.NewRegistry(), shared())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var ddl strings.Builder
	for _, c := range changes {
		ddl.WriteString(c.Up)
		ddl.WriteString("\n")
	}
	for _, want := range []string{
		`CREATE TABLE "users"`,
		`CREATE TABLE "coaches"`,
		// The reason the registry is shared at all.
		`REFERENCES "users"`,
	} {
		if !strings.Contains(ddl.String(), want) {
			t.Errorf("the migration should still contain %s:\n%s", want, ddl.String())
		}
	}
}

// The manifest and the skill describe what this package serves, so a table it
// no longer emits anything for has no business in either. Left in, the manifest
// would advertise a resource this package does not mount and the agent skill
// would offer a model that is not there.
// The generated code has to compile, which is the assertion that catches a
// half-applied filter: one emitter still referring to a type another has
// stopped emitting is a package that does not build, which is a worse outcome
// than the shadow this removes. TestGeneratedGoCompiles carries the fixture for
// the whole set; this pins the specific pairing that would break first.
func TestASkippedTableLeavesNoDanglingReference(t *testing.T) {
	files := generate(t, shared())
	for name, src := range files {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		for _, dangling := range []string{"UserCols", "UserCreate", "UserPatch", "[]User{"} {
			if strings.Contains(src, dangling) {
				t.Errorf("%s refers to %s, whose model is not emitted here:\n%s", name, dangling, src)
			}
		}
	}
}

func TestASkippedTableIsAbsentFromTheManifest(t *testing.T) {
	files := generate(t, shared())
	manifest, ok := files["sqlb.json"]
	if !ok {
		t.Fatalf("no manifest was generated; got %v", keysOf(files))
	}
	// The table entries, not any mention of the name: coaches.user is a real
	// foreign key into users, so the *reference target* names it and must keep
	// naming it. What must not be there is an entry describing users as a
	// resource this package serves.
	if strings.Contains(manifest, `"name": "users"`) {
		t.Errorf("the manifest describes a table this package does not serve:\n%s", manifest)
	}
	if !strings.Contains(manifest, `"name": "coaches"`) {
		t.Errorf("the manifest is missing this package's own table:\n%s", manifest)
	}
	// And the reference is intact, which is the reason the registry is shared.
	if !strings.Contains(manifest, `"table": "users"`) {
		t.Errorf("the cross-package foreign key is missing from the manifest:\n%s", manifest)
	}
}

// The library's own generation still emits everything, which is what makes the
// declaration safe to write once in the library rather than per host: the
// package it names is the package it generates into.
func TestTheOwningPackageStillEmitsItsOwnModel(t *testing.T) {
	src := generateInto(t, shared(), "authitstore")["models_gen.go"]
	if !strings.Contains(src, "type User struct {") {
		t.Errorf("the owning package must still generate its own model:\n%s", src)
	}
}
