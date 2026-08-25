package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// sharedStatusRegistry is two tables that declare the identical value set —
// the shape issue #197 named: two structurally identical, nominally
// incompatible Go types with no way to say they are the same enum — opting
// into sharing it under one name.
func sharedStatusRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").SharedAs("Status").Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})
	r.Table("courses",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").SharedAs("Status").Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})
	return r
}

// A shared value set emits exactly one Go type and one constant block, and
// both models' fields are typed against it — the point of SharedAs, and the
// reason it has to dedupe rather than emit the same type and consts twice:
// Go refuses two declarations of one identifier in the same package.
func TestSharedAsEmitsOneType(t *testing.T) {
	out := generate(t, sharedStatusRegistry())
	models := out["models_gen.go"]
	squashed := squash(models)

	// gofmt column-aligns struct fields and const blocks, so counting on the
	// squashed text is what keeps this assertion about repetition rather than
	// about whitespace — see contains's own comment.
	if n := strings.Count(squashed, squash("type Status string")); n != 1 {
		t.Errorf("models_gen.go declares `type Status string` %d times, want 1:\n%s", n, models)
	}
	if n := strings.Count(squashed, squash(`StatusDraft Status = "draft"`)); n != 1 {
		t.Errorf("models_gen.go declares StatusDraft %d times, want 1:\n%s", n, models)
	}
	// No table-and-column-derived type was also emitted for either column.
	for _, unwanted := range []string{"type LessonStatus string", "type CourseStatus string"} {
		if contains(models, unwanted) {
			t.Errorf("models_gen.go still declares %s, which SharedAs should have replaced", unwanted)
		}
	}
	// Both structs' fields are typed Status, not a type of their own.
	if !contains(models, "type Lesson struct") || !contains(models, "type Course struct") {
		t.Fatalf("models_gen.go missing Lesson or Course struct:\n%s", models)
	}
	field := squash(`Status Status ` + "`db:\"status\"")
	if n := strings.Count(squashed, field); n != 2 {
		t.Errorf("models_gen.go has the field %q %d times, want 2 (one per table):\n%s", field, n, models)
	}
}

// Two columns that opt in under the same name but declare different value
// sets is refused, naming both columns, rather than silently picking one of
// them — the "actionable errors" rule (ADR-0011) applied to a mechanism that
// only exists to save a string(x) round trip and should not cost more than it
// saves when it is misused.
func TestSharedAsMismatchRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").SharedAs("Status"),
	).Expose(schema.REST{Ops: schema.OpList})
	r.Table("courses",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published").SharedAs("Status"),
	).Expose(schema.REST{Ops: schema.OpList})

	_, err := codegen.Generate(codegen.Options{Registry: r, Dir: t.TempDir(), Package: "gen"})
	if err == nil {
		t.Fatal("expected a SharedAs mismatch error, generated cleanly")
	}
	for _, want := range []string{"courses", "lessons", "status", "Status", "draft", "published", "archived"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// Two tables that declare the identical value set without SharedAs still get
// two independent types — the regression guard proving the default behaviour
// (issue #138's shape, and every enum column before this) is unchanged.
func TestUnsharedIdenticalEnumsStayIndependent(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})
	r.Table("courses",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("status", "draft", "published", "archived").Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})

	out := generate(t, r)
	models := out["models_gen.go"]

	for _, want := range []string{"type LessonStatus string", "type CourseStatus string"} {
		if !contains(models, want) {
			t.Errorf("models_gen.go missing %s", want)
		}
	}
	if contains(models, "type Status string") {
		t.Errorf("models_gen.go declares a bare Status type; two unshared columns should not have merged:\n%s", models)
	}
}
