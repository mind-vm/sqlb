package schema_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// AddField returns the field it added, so a caller has a handle back to a
// column without pulling it out of schema.Table's variadic call.
func TestAddFieldReturnsTheField(t *testing.T) {
	r := schema.NewRegistry()
	tasks := r.Table("tasks", schema.UUIDv7("id").PrimaryKey())
	status := tasks.AddField(schema.Enum("status", "todo", "done").Default(schema.Value("todo")))

	if status.Name() != "status" {
		t.Fatalf("AddField returned a field named %q, want %q", status.Name(), "status")
	}
	if got := tasks.Field("status"); got != status {
		t.Fatalf("Field(%q) did not return the same *Field AddField added", "status")
	}
}

// A table can be built entirely procedurally — schema.Table with no columns,
// then one AddField call per column — which is what the caller in
// TestAddFieldReturnsTheField's doc comment example is doing when it wants a
// handle to every column rather than just one.
func TestATableCanBeBuiltEntirelyWithAddField(t *testing.T) {
	r := schema.NewRegistry()
	tasks := r.Table("tasks")
	id := tasks.AddField(schema.UUIDv7("id").PrimaryKey())
	title := tasks.AddField(schema.Text("title"))
	tasks.Expose(schema.REST{Ops: schema.CRUD | schema.OpList})

	if err := r.Validate(); err != nil {
		t.Fatalf("a table built entirely with AddField was refused: %v", err)
	}
	if len(tasks.Fields()) != 2 {
		t.Fatalf("Fields() = %d, want 2", len(tasks.Fields()))
	}
	if id.Name() != "id" || title.Name() != "title" {
		t.Fatalf("AddField returned the wrong fields: %q, %q", id.Name(), title.Name())
	}
}

// The mixed case the whole method exists for: most columns declared the
// compact way, one pulled out because an Action needs to name it.
func TestAddFieldCoexistsWithTheCompactForm(t *testing.T) {
	r := schema.NewRegistry()
	tasks := r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	)
	status := tasks.AddField(schema.Enum("status", "todo", "done").Default(schema.Value("todo")))
	tasks.Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddAction(schema.Action{Name: "complete", Writes: []string{status.Name()}})

	if err := r.Validate(); err != nil {
		t.Fatalf("a mixed compact/procedural table was refused: %v", err)
	}
}
