package computed

import (
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/schema"
)

// Technique (5): declare the expression.
//
// The four techniques in computed.go all work and three of them are still the
// right answer in the cases their doc comments name. What none of them can do
// is tell the *generator* that a derived value exists — so `is_overdue` is in
// the SQL and nowhere else: not in the TypeScript type, not in the CLI's
// columns, not in the OpenAPI response schema, and not in the filter grammar a
// REST client is allowed to use. That was the gap [ADR-0041] set out to close,
// and this file is the closed version of the same three values.
//
// [ADR-0041]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#computed-fields

// Declared is the schema half of the closed gap: a computed column emits no
// DDL — the CREATE TABLE this registry produces is the one in Schema minus the
// three expressions — and it reaches every emitter that describes a row.
var Declared = declaredRegistry()

func declaredRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.BigInt("id").PrimaryKey(),
		schema.BigInt("org_id").Filterable().ReadOnly().Scoped(),
		schema.Text("name").Searchable().Sortable(),
		schema.Date("due_date").Nullable().Filterable().Sortable(),
		schema.Int("total_tasks").Filterable().Sortable().ReadOnly(),
		schema.Int("completed_tasks").Filterable().Sortable().ReadOnly(),
		schema.Int("open_tasks").Filterable().Sortable().ReadOnly(),

		// Tier 1: row-local, so it may be filtered and — because this
		// expression reads current_date — may not be sorted. The refusal is at
		// declaration time, not at request time: a keyset pages on the sort
		// column, and this one is a different value on the next page.
		//
		// NotNull is what the leading `due_date IS NOT NULL` earns. Without the
		// guard the comparison would be NULL for every row with no due date,
		// and a computed column is nullable unless it says otherwise (#147).
		schema.Computed("is_overdue", schema.TypeBool,
			schema.FromSQL("(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)")).
			NotNull().Filterable(),

		// Arithmetic over two stored columns. Stable, so it may be sorted.
		// NULLIF is there to avoid dividing by zero, and what it divides by
		// instead is NULL — so this one keeps the default, and Nullable says so
		// out loud rather than relying on it.
		schema.Computed("progress", schema.TypeInt,
			schema.FromSQL("(completed_tasks * 100 / NULLIF(total_tasks, 0))")).
			Nullable().Filterable().Sortable(),

		// Tier 3: the value depends on who is asking. `?` takes the bind named
		// by Needs, and rest.Resource refuses to mount this table until a
		// BeforeQuery hook supplies it — the alternative being `member_id =
		// NULL`, which is false for every row forever and looks like a feature
		// that works.
		schema.Computed("is_starred", schema.TypeBool,
			schema.FromSQL("EXISTS (SELECT 1 FROM project_stars s "+
				"WHERE s.project_id = projects.id AND s.member_id = ?)")).
			NotNull().Needs("viewer").Filterable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	return r
}

// DeclaredProject is what codegen emits from the table above: an ordinary
// struct whose derived fields are ordinary fields, plus one method carrying the
// expressions.
//
// The expressions are in a method rather than in the `sqlb` tag because a tag
// is a comma-separated list of words and SQL is neither. Everything else about
// these columns is said in the tag exactly as it is for a stored one, which is
// the property that makes them work everywhere without a second code path.
type DeclaredProject struct {
	ID             int64      `db:"id" json:"id" sqlb:"pk"`
	OrgID          int64      `db:"org_id" json:"org_id" sqlb:"filter,readonly,scope"`
	Name           string     `db:"name" json:"name" sqlb:"filter,search,sort"`
	DueDate        *time.Time `db:"due_date" json:"due_date" sqlb:"filter,sort"`
	TotalTasks     int32      `db:"total_tasks" json:"total_tasks" sqlb:"filter,sort,readonly"`
	CompletedTasks int32      `db:"completed_tasks" json:"completed_tasks" sqlb:"filter,sort,readonly"`
	OpenTasks      int32      `db:"open_tasks" json:"open_tasks" sqlb:"filter,sort,readonly"`

	IsOverdue bool   `db:"is_overdue" json:"is_overdue" sqlb:"filter,readonly"`
	Progress  *int32 `db:"progress" json:"progress" sqlb:"filter,sort,readonly"`
	IsStarred bool   `db:"is_starred" json:"is_starred" sqlb:"filter,readonly"`
}

func (DeclaredProject) TableName() string { return "projects" }

// ComputedColumns carries the expressions the schema declared.
func (DeclaredProject) ComputedColumns() []sqlb.Computed {
	return []sqlb.Computed{
		{
			Name: "is_overdue",
			Expr: "(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)",
		},
		{
			Name: "progress",
			Expr: "(completed_tasks * 100 / NULLIF(total_tasks, 0))",
		},
		{
			Name: "is_starred",
			Expr: "EXISTS (SELECT 1 FROM project_stars s " +
				"WHERE s.project_id = projects.id AND s.member_id = ?)",
			Needs: []string{"viewer"},
		},
	}
}

// DeclaredValues names the derived columns this screen wants. Computed columns
// are opt-in per reader: the model declares three, and a reader that wants none
// of them — Exists, below — pays for none and needs no viewer (#92).
var DeclaredValues = []string{"is_overdue", "progress", "is_starred"}

// DeclaredView is the read. Compare it with View: there is no projection to
// assemble, no RawSel to parenthesise by hand, and the per-viewer bind is
// supplied once rather than written at each site that mentions it.
//
// In a REST application the Bind lives in a BeforeQuery hook and WithComputed
// is rest.Options.Computed, so no handler and no caller passes the viewer
// around — which is also what makes the mount-time check able to insist on it,
// and to insist only of the resources that render the column.
func DeclaredView(viewer int64) *sqlb.Builder[DeclaredProject] {
	return sqlb.Query[DeclaredProject]().
		WithComputed(DeclaredValues...).
		Bind("viewer", viewer)
}

// Exists is the query that made the opt-in necessary. It asks whether a row is
// there; it has no viewer, and there is no sense in which it should need one.
//
// While computed columns were projected by default this could not be written
// against the same model: the projection carried is_starred, is_starred needed
// the "viewer" bind, and the query failed before it reached the database.
func Exists(id int64) *sqlb.Builder[DeclaredProject] {
	return sqlb.Query[DeclaredProject]().Where(sqlb.F("id").Eq(id))
}
