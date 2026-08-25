// Package computed shows how to get a derived value — one the row does not
// store — out of Postgres through sqlb.
//
// [schema.Computed] now declares one, and declared.go is this package's three
// derived values written that way. The four techniques below are still here
// because three of them did not go away when it landed: a column Postgres
// maintains is cheaper than an expression evaluated per read, and it is the
// right answer often enough that a declaration slot should not be allowed to
// hide it. [ADR-0041] says which tier is which.
//
// The four, in the order to reach for them:
//
//  1. A STORED generated column. Postgres computes it on write, it is a real
//     column, and it can be indexed. Restricted to IMMUTABLE expressions over
//     the same row, so nothing involving now() or another table qualifies.
//  2. A trigger-maintained counter. For values that come from other rows —
//     "how many tasks does this project have" — where recomputing per read
//     would be a correlated subquery on every request.
//  3. A projected expression, via [sqlb.RawSel] and [sqlb.Collect]. Costs
//     nothing on write, evaluated per read, and it is the only one of the four
//     that can take a bind parameter — which is what a per-viewer field like
//     "did *I* star this" requires.
//  4. A database VIEW, described with Table(). One hand-written object, and
//     the whole generated read path works over it.
//
// (1) and (2) produce ordinary columns, so sqlb needs to be told nothing: they
// are Filterable, Sortable and indexable like any other, and the only sqlb-side
// decision is ReadOnly, which keeps them out of the generated request bodies.
// (3) is where the ceiling was, and where schema.Computed now sits — see
// declared.go, which produces the same SQL from a declaration the emitters can
// read. What (3) still has that a declaration does not is a per-call-site
// expression: schema.Computed is a property of the table, so a value only one
// query wants is still a RawSel.
//
// [ADR-0041]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#computed-fields
package computed

import (
	"time"

	"github.com/mind-vm/sqlb"
)

// Schema is the DDL the techniques below assume. It is written out rather than
// declared through the schema package because two of the three objects — the
// generated column and the trigger — have no spelling in the DSL today, so a
// project using them writes this migration by hand and declares the resulting
// columns as ordinary ones.
const Schema = `
CREATE TABLE projects (
    id              bigserial PRIMARY KEY,
    org_id          bigint      NOT NULL,
    name            text        NOT NULL,
    due_date        date,

    -- (2) Maintained by a trigger on tasks. Ordinary columns to every reader.
    total_tasks     int         NOT NULL DEFAULT 0,
    completed_tasks int         NOT NULL DEFAULT 0,

    -- (1) Computed by Postgres on write. Indexable, which is the whole point:
    -- ORDER BY open_tasks over a million rows is an index scan, where the same
    -- arithmetic in the projection is a sort of the whole table.
    open_tasks      int GENERATED ALWAYS AS (total_tasks - completed_tasks) STORED
);

CREATE INDEX projects_open_tasks ON projects (org_id, open_tasks);

-- (3)'s backing table. Which projects a given member has starred is not a fact
-- about the project, so it cannot be a column of one.
CREATE TABLE project_stars (
    org_id     bigint NOT NULL,
    project_id bigint NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    member_id  bigint NOT NULL,
    PRIMARY KEY (project_id, member_id)
);
`

// Project is the row. The three derived columns are declared exactly like the
// stored ones, because to everything above Postgres that is what they are.
//
// ReadOnly is the load-bearing word. Without it the generated create and update
// bodies would accept a total_tasks, and a request could make the counter
// disagree with the tasks it counts. Postgres would reject a write to
// open_tasks on its own — it is GENERATED ALWAYS — but as a 500 naming a
// constraint, where ReadOnly is a 400 naming the field.
type Project struct {
	ID      int64      `db:"id" sqlb:"pk,default"`
	OrgID   int64      `db:"org_id" sqlb:"filter,scope,readonly"`
	Name    string     `db:"name" sqlb:"filter,sort,search"`
	DueDate *time.Time `db:"due_date" sqlb:"filter,sort"`

	TotalTasks     int `db:"total_tasks" sqlb:"filter,sort,readonly,default"`
	CompletedTasks int `db:"completed_tasks" sqlb:"filter,sort,readonly,default"`
	OpenTasks      int `db:"open_tasks" sqlb:"filter,sort,readonly,default"`
}

func (Project) TableName() string { return "projects" }

// ProjectView is the row plus the values no column holds. Embedding Project
// keeps one definition of the shared columns — an untagged embedded struct
// contributes its own fields, so the view maps every column the table has and
// then some.
//
// It is a separate type from Project on purpose. Query[Project] stays the plain
// table read that INSERT ... RETURNING and every hook already work against;
// asking for the derived values is a different, more expensive query, and the
// type is what says so at the call site.
type ProjectView struct {
	Project

	// Evaluated per read. IsOverdue reads current_date, so it can change
	// without the row changing — which is exactly why it cannot be technique
	// (1): Postgres requires a generated column's expression to be IMMUTABLE.
	IsOverdue bool `db:"is_overdue"`

	// NULL when the project has no tasks, which is why this is a pointer.
	// NULLIF is doing that: a zero denominator would otherwise be a division
	// error taking the whole request with it, not a zero.
	Progress *int `db:"progress"`

	// Depends on who is asking, not on the row. No column and no view can hold
	// this, because there is no "the" answer — it is a function of the request.
	IsStarred bool `db:"is_starred"`
}

func (ProjectView) TableName() string { return "projects" }

// Derived is the projection ProjectView expects: every column of the table,
// then the three expressions.
//
// Select appends to the projection but replaces the default one, so the table's
// own columns have to be named once something else is added. columnsOf does
// that from the model rather than by hand, so a column added to Project reaches
// the view without a second edit.
func Derived(viewer int64) []sqlb.Selectable {
	sel := columnsOf[Project]()
	return append(sel,
		// A plain expression over the row. Nothing here needs a bind.
		//
		// The parentheses are not decoration. RawSel splices its text
		// verbatim, so an expression containing AND or OR sits directly beside
		// whatever the compiler writes next; wrapping it means the fragment
		// cannot be re-associated by its surroundings. sqlb parenthesises
		// Raw automatically when it nests one under an operator — see
		// (*compiler).operand — but a projection is not a nesting, so this one
		// is the author's to write.
		sqlb.RawSel(
			"(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)",
		).As("is_overdue"),

		sqlb.RawSel(
			"(completed_tasks * 100 / NULLIF(total_tasks, 0))",
		).As("progress"),

		// The one a static expression cannot express. `?` is renumbered into
		// $N along with every other bind in the statement, so this composes
		// with whatever the filter grammar and the hooks added.
		sqlb.RawSel(
			"EXISTS (SELECT 1 FROM project_stars s "+
				"WHERE s.project_id = projects.id AND s.member_id = ?)",
			viewer,
		).As("is_starred"),
	)
}

// View builds the read. It is Query[Project] rather than Query[ProjectView]
// because the table is the same table — ProjectView describes the *result*, and
// Collect is what pairs the two.
//
// Note what did not have to change: hooks registered on Project still run, so
// the tenant predicate applies to this query as much as to a plain list. The
// derived values ride along on a query the domain still constrains.
func View(viewer int64) *sqlb.Builder[Project] {
	return sqlb.Query[Project]().Select(Derived(viewer)...)
}

// OverdueFirst orders by a derived value without projecting it.
//
// This is the half of technique (3) that has no ceiling problem: an expression
// in ORDER BY or WHERE is just SQL, and sqlb has never stopped anyone writing
// it. What it costs is the guarantee — `sqlb.Raw` is not checked against the
// schema, so a column renamed under it fails at the database rather than at
// generate time, and a REST client cannot reach it at all because the filter
// grammar only admits declared columns.
//
// That gap is the argument for ADR-0041: not that this cannot be written, but
// that writing it here leaves the TypeScript client, the CLI and the OpenAPI
// document not knowing the field exists.
func OverdueFirst(b *sqlb.Builder[Project]) *sqlb.Builder[Project] {
	return b.OrderBy(
		sqlb.OrderByDesc(sqlb.Raw{
			SQL: "(due_date IS NOT NULL AND due_date < current_date AND open_tasks > 0)",
		}),
		sqlb.F("due_date").Asc(),
	)
}

// StarredBy filters on the per-viewer fact, which is the same EXISTS as the
// projection and the same reason it needs a bind.
func StarredBy(b *sqlb.Builder[Project], viewer int64) *sqlb.Builder[Project] {
	return b.Where(sqlb.RawPred(
		"EXISTS (SELECT 1 FROM project_stars s "+
			"WHERE s.project_id = projects.id AND s.member_id = ?)",
		viewer,
	))
}

// columnsOf names every mapped column of T as a selection.
func columnsOf[T any]() []sqlb.Selectable {
	names := sqlb.ModelOf[T]().ColumnNames()
	out := make([]sqlb.Selectable, 0, len(names)+3)
	for _, n := range names {
		out = append(out, sqlb.F(n))
	}
	return out
}
