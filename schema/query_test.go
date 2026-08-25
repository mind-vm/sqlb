package schema_test

import (
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// tasksWithQuery builds a tasks table carrying one query, for the refusals
// below.
func tasksWithQuery(q schema.Query) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("status", "open", "done").Default(schema.Value("open")),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).AddQuery(q)
	return r
}

func TestAValidQueryPasses(t *testing.T) {
	r := tasksWithQuery(schema.Query{
		Name:   "overdue",
		Params: schema.Body(schema.Timestamp("as_of")),
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("a well-formed query was refused: %v", err)
	}
}

// Reads is typed, so a query can name another table without a string that
// might not match it.
func TestAQueryCanReadAnotherTable(t *testing.T) {
	r := schema.NewRegistry()
	comments := r.Table("comments", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	r.Table("tasks", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddQuery(schema.Query{Name: "overdue", Reads: []*schema.TableDef{comments}})

	if err := r.Validate(); err != nil {
		t.Fatalf("a query reading another table was refused: %v", err)
	}
}

// The table a query is declared on is implicit in Reads and naming it again
// is refused, on the grounds that a declaration accepted as a no-op is a
// declaration nobody can trust to mean something.
func TestAQueryNamingItsOwnTableInReadsIsRefused(t *testing.T) {
	r := schema.NewRegistry()
	tasks := r.Table("tasks", schema.UUIDv7("id").PrimaryKey())
	tasks.Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddQuery(schema.Query{Name: "overdue", Reads: []*schema.TableDef{tasks}})

	if err := r.Validate(); err == nil {
		t.Fatal("expected the schema to be refused")
	}
}

// The default path is the collection form: a query addresses no single row.
func TestAQueryDefaultsToTheCollectionPath(t *testing.T) {
	r := tasksWithQuery(schema.Query{Name: "overdue"})
	q := r.Get("tasks").Queries()[0]

	if q.Path != "/overdue" {
		t.Errorf("path = %q, want the collection form", q.Path)
	}
	if got := q.FullPath("/tasks"); got != "/tasks/overdue" {
		t.Errorf("full path = %q", got)
	}
}

// A table with no resource has nowhere to put a route — the same refusal
// an action gets, for the same reason.
func TestAQueryOnAnUnexposedTableIsRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("tasks", schema.UUIDv7("id").PrimaryKey()).
		AddQuery(schema.Query{Name: "overdue"})

	if err := r.Validate(); err == nil {
		t.Fatal("expected the schema to be refused")
	}
}

// A query named for an operation the resource already generates would
// collide on one operation id, exactly like an action's collision check.
func TestAQueryCollidingWithAnOperationIsRefused(t *testing.T) {
	r := tasksWithQuery(schema.Query{Name: "list"})
	if err := r.Validate(); err == nil {
		t.Fatal("expected the schema to be refused")
	}
}

// A query parameter is a value, not a column: capabilities that describe a
// column's place in a table have no meaning here.
func TestAQueryParamCannotClaimColumnCapabilities(t *testing.T) {
	r := tasksWithQuery(schema.Query{
		Name:   "overdue",
		Params: schema.Body(schema.Text("org").Filterable()),
	})
	if err := r.Validate(); err == nil {
		t.Fatal("expected the schema to be refused")
	}
}
