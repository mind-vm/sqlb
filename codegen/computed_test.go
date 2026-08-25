package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

func computedFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Searchable().Sortable(),
		schema.Date("due_date").Nullable().Filterable().Sortable(),
		schema.Int("open_tasks").Filterable(),

		// Nullable, and not by an oversight: due_date is nullable, so the
		// comparison is NULL for every row without one.
		schema.Computed("is_overdue", schema.TypeBool,
			schema.FromSQL("due_date < current_date AND open_tasks > 0")).Filterable(),
		// EXISTS is true or false and never NULL, which is what NotNull claims.
		schema.Computed("is_starred", schema.TypeBool,
			schema.FromSQL("EXISTS (SELECT 1 FROM stars s "+
				"WHERE s.project_id = projects.id AND s.member_id = ?)")).
			NotNull().Needs("viewer").Filterable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

// One declaration, and the field is in the row type with the expression beside
// it — the method rather than the tag, because SQL does not fit in a
// comma-separated list.
//
// The two derived fields also pin the nullability default from #147: a computed
// column is a pointer unless the declaration claims NotNull, because an
// expression can be NULL and there is no DDL to read the answer off.
func TestGeneratedModelCarriesTheExpression(t *testing.T) {
	models := generate(t, computedFixture())["models_gen.go"]

	for _, want := range []string{
		`IsOverdue *bool ` + "`" + `db:"is_overdue" json:"is_overdue" sqlb:"type:bool,filter,readonly"` + "`",
		`IsStarred bool ` + "`" + `db:"is_starred" json:"is_starred" sqlb:"type:bool,filter,readonly"` + "`",
		"func (Project) ComputedColumns() []sqlb.Computed {",
		`{Name: "is_overdue", Expr: "due_date < current_date AND open_tasks > 0"},`,
		`Needs: []string{"viewer"}`,
	} {
		if !contains(models, want) {
			t.Errorf("models are missing %q:\n%s", want, models)
		}
	}
}

// A computed column is not writable, so it is absent from the request bodies
// and from the typed update — the same exclusion ReadOnly already buys, plus
// the setter, which ReadOnly deliberately does not remove.
func TestComputedIsNotWritable(t *testing.T) {
	files := generate(t, computedFixture())

	if contains(files["rest_gen.go"], "IsOverdue") {
		t.Errorf("a computed column reached a request body:\n%s", files["rest_gen.go"])
	}
	if contains(files["columns_gen.go"], "func (u *ProjectUpdate) SetIsOverdue") {
		t.Error("a computed column got a setter, which every statement using it would fail on")
	}
	// It is still addressable as a column, which is what makes a filter and a
	// sort over it compile.
	if !contains(files["columns_gen.go"], "IsOverdue") {
		t.Error("a computed column should still have a typed column handle")
	}
}

// No DDL in either direction: the table Postgres holds does not have the
// column, and a diff against a database that matches proposes nothing.
func TestComputedEmitsNoDDL(t *testing.T) {
	r := computedFixture()
	changes, err := migrate.Diff(nil, r)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var ddl strings.Builder
	for _, c := range changes {
		ddl.WriteString(c.Up + "\n")
	}
	if strings.Contains(ddl.String(), "is_overdue") || strings.Contains(ddl.String(), "is_starred") {
		t.Errorf("a computed column reached the DDL:\n%s", ddl.String())
	}
	if !strings.Contains(ddl.String(), "open_tasks") {
		t.Errorf("the stored columns should still be created:\n%s", ddl.String())
	}
}

// A generated resource opts into the columns its table declares, so its
// responses are unchanged by #92's opt-in. That is the half of the change that
// must *not* be visible: what the opt-in alters is everything else reading the
// model — a hand-written query no longer inherits a list screen's correlated
// subqueries or its per-request binds.
func TestAGeneratedMountOptsIntoItsComputedColumns(t *testing.T) {
	rest := generate(t, computedFixture())["rest_gen.go"]
	if want := `Computed: []string{"is_overdue", "is_starred"}`; !contains(rest, want) {
		t.Errorf("generated mount is missing %q:\n%s", want, rest)
	}
}
