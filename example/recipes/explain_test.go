package recipes_test

import (
	"context"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// The plan Postgres would return for the query below, on a table large enough
// for the sequential scan to matter. A recipe cannot ask a real planner without
// a database, so this stands in for one; everything after it is real.
const seqScanPlan = `[{"Plan":{
  "Node Type":"Sort","Total Cost":1892.4,"Plan Rows":4200,
  "Sort Key":["view_count DESC"],
  "Plans":[{
    "Node Type":"Seq Scan","Relation Name":"posts","Total Cost":1421.0,"Plan Rows":4200,
    "Filter":"(status = 'published'::text)"
  }]
}}]`

// Explain plans a query against the live schema without running it, which
// answers two questions SQL() cannot.
//
// First, whether the statement is valid against the database as it *is*: a
// column that a migration was written for and never applied fails here, which
// a compile-time column check cannot catch. Second, whether the plan is still
// the one you expected — an index scan that silently became a sequential scan
// is invisible in the SQL text and obvious in the plan.
//
// It does not execute the statement, so it is safe on a mutation.
// ExplainAnalyze does execute; run that inside a transaction you roll back.
func Example_explainAQuery() {
	db := recordingDBWith([]string{"QUERY PLAN"}, []any{[]byte(seqScanPlan)})

	plan, err := sqlb.Explain(context.Background(), db, sqlb.Query[recipes.Post]().
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("view_count").Desc()))
	if err != nil {
		panic(err)
	}

	fmt.Println("analyzed:", plan.Analyzed)
	fmt.Println("estimated rows:", plan.PlanRows)
	fmt.Println("scans posts sequentially:", plan.UsesSeqScan("posts"))
	fmt.Println("uses posts_status_idx:", plan.UsesIndex("posts_status_idx"))
	// Output:
	// analyzed: false
	// estimated rows: 4200
	// scans posts sequentially: true
	// uses posts_status_idx: false
}

// Diagnostics report plan shapes that usually mean a missing index or a query
// that will not scale. They are advisory — a sequential scan over a lookup
// table is correct, and so is a sort of twenty rows — which is why the
// threshold is a variable you can move rather than a constant.
//
// This is what makes a plan usable as a test assertion: a query whose plan
// regresses fails a build instead of a pager.
func Example_explainDiagnostics() {
	db := recordingDBWith([]string{"QUERY PLAN"}, []any{[]byte(seqScanPlan)})

	plan, err := sqlb.Explain(context.Background(), db, sqlb.Query[recipes.Post]().
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("view_count").Desc()))
	if err != nil {
		panic(err)
	}

	fmt.Print(sqlb.Diagnostics(plan.Diagnostics()))
	// Output:
	// [seq-scan] Seq Scan on posts: sequential scan (cost ~1421) over ~4200 rows filtering on (status = 'published'::text)
	//     fix: add an index covering the filtered columns on "posts"
}

// The tree, in the shape a reader — or an agent comparing two runs — can scan
// quickly.
func Example_explainPlanTree() {
	db := recordingDBWith([]string{"QUERY PLAN"}, []any{[]byte(seqScanPlan)})

	plan, err := sqlb.Explain(context.Background(), db, sqlb.Query[recipes.Post]().
		Where(sqlb.F("status").Eq("published")))
	if err != nil {
		panic(err)
	}
	fmt.Print(plan)
	// Output:
	// cost=1892.40 rows=4200
	//   -> Sort (cost=1892.40 rows=4200)
	//     -> Seq Scan on posts (cost=1421.00 rows=4200) filter=(status = 'published'::text)
}
