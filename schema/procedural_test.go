package schema_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// withAudit applies a reusable base column set — the procedural equivalent
// of passing schema.Timestamps() inline to every schema.Table call that
// wants one. It takes and returns *TableDef so callers can use it either as
// a statement (withAudit(Task)) or chained the way Expose and AddIndex are
// (Task.AddField(...); withAudit(Task).Expose(...)).
func withAudit(t *schema.TableDef) *schema.TableDef {
	t.AddFields(schema.Timestamps())
	return t
}

// Two tables built procedurally, both taking their audit columns from one
// declaration. This is the "how do I declare a base table" answer: not a
// second table type to embed, but a func every table under construction can
// be handed to.
func TestProceduralTablesShareABaseColumnSet(t *testing.T) {
	r := schema.NewRegistry()

	tasks := withAudit(r.Table("tasks", schema.UUIDv7("id").PrimaryKey())).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	lists := withAudit(r.Table("lists", schema.UUIDv7("id").PrimaryKey())).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList})

	if err := r.Validate(); err != nil {
		t.Fatalf("refused: %v", err)
	}
	for _, tbl := range []*schema.TableDef{tasks, lists} {
		if tbl.Field("created_at") == nil || tbl.Field("updated_at") == nil {
			t.Errorf("%s: withAudit did not add both columns", tbl.Name())
		}
	}
	// Each table got its own *Field for created_at, not the same pointer —
	// Timestamps() is a func, called once per withAudit call, and that is
	// what makes this safe. A capability set on one table's created_at must
	// never be visible on another's; sharing a *Field across tables would
	// make it so, silently, the moment either declaration changed.
	if tasks.Field("created_at") == lists.Field("created_at") {
		t.Fatal("tasks and lists share one *Field for created_at; a capability added to one would leak to the other")
	}
}

// noteField is a factory, not a value: it returns a fresh *Field on every
// call. That is the difference between reusing a *column shape* safely and
// reusing a *Field unsafely — see the pointer-identity assertion above and
// its mirror below. A package-level `var note = schema.Text("note")...`
// would compile and pass every test until two tables' notes columns started
// answering for each other.
func noteField() *schema.Field {
	return schema.Text("note").Nullable().Comment("Free-form text.")
}

func TestProceduralFieldFactoryProducesIndependentColumns(t *testing.T) {
	r := schema.NewRegistry()
	tasks := r.Table("tasks", schema.UUIDv7("id").PrimaryKey())
	tasks.AddField(noteField())
	comments := r.Table("comments", schema.UUIDv7("id").PrimaryKey())
	comments.AddField(noteField())

	if tasks.Field("note") == comments.Field("note") {
		t.Fatal("tasks and comments share one *Field for note; noteField() should return a fresh one each call")
	}
}

// The reason to reach for AddField over the compact form at all: a shape
// that depends on a build-time condition. The declarative form can express
// this too (build a []schema.FieldSpec first, append conditionally, pass it
// in) but an if around one AddField call is what "declarative when you can,
// procedural when you need branches" looks like in practice.
func TestProceduralTableAddsAFieldConditionally(t *testing.T) {
	build := func(withDueDate bool) *schema.Registry {
		r := schema.NewRegistry()
		tasks := r.Table("tasks", schema.UUIDv7("id").PrimaryKey())
		if withDueDate {
			tasks.AddField(schema.Timestamp("due_at").Nullable().Filterable().Sortable())
		}
		tasks.Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
		return r
	}

	with := build(true)
	if with.Get("tasks").Field("due_at") == nil {
		t.Fatal("withDueDate=true should have added due_at")
	}
	without := build(false)
	if without.Get("tasks").Field("due_at") != nil {
		t.Fatal("withDueDate=false should not have added due_at")
	}
}
