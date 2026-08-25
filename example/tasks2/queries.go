package tasks2

import (
	"context"

	"github.com/mind-vm/sqlb"
)

// OverdueTasks answers the "overdue" query declared on Task.
//
// OverdueTaskParams is generated now (rest_gen.go, from Query.Params) —
// codegen caught up to schema.Query, so this file only writes the read.
// It is still mounted by hand in cmd/server/main.go rather than through
// Register: Register bundles every exposed table onto one shared api, and
// this table's routes need to go through the auth group List's do not.
//
// Nothing here mentions a boundary otherwise, because this example has none
// to mention — there is exactly one tenant, the whole database. What it does
// show is that db is an ordinary sqlb.Executor: nothing about how this
// function is written differs from a helper an application would write
// anyway for its own use, before rest.Query existed to give it a route.
func OverdueTasks(ctx context.Context, db sqlb.Executor, in OverdueTaskParams) ([]Task, error) {
	return sqlb.Query[Task]().
		Where(
			sqlb.F("due_at").Lt(in.AsOf),
			sqlb.F("status").Neq(TaskStatusDone),
		).
		OrderBy(sqlb.F("due_at").Asc()).
		All(ctx, db)
}
