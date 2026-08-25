package sqlb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

// A plan shaped like Postgres emits: an index scan under a sort.
const goodPlan = `[{"Plan":{
  "Node Type":"Sort","Total Cost":12.5,"Plan Rows":40,"Sort Method":"quicksort","Sort Space Type":"Memory",
  "Plans":[{"Node Type":"Index Scan","Relation Name":"users","Index Name":"users_org_id_idx",
            "Index Cond":"(org_id = 'acme')","Total Cost":8.2,"Plan Rows":40}]}}]`

// The same query after the index was dropped: a scan and a disk sort.
const regressedPlan = `[{"Plan":{
  "Node Type":"Sort","Total Cost":98000.0,"Plan Rows":250000,
  "Sort Method":"external merge","Sort Space Type":"Disk",
  "Plans":[{"Node Type":"Seq Scan","Relation Name":"users",
            "Filter":"(org_id = 'acme')","Total Cost":81000.0,"Plan Rows":250000}]}}]`

func explainHarness(t *testing.T, plan string) *harness {
	t.Helper()
	return newHarness(t, []string{"QUERY PLAN"}, [][]any{{[]byte(plan)}})
}

func TestExplainParsesThePlanTree(t *testing.T) {
	h := explainHarness(t, goodPlan)
	defer h.close()

	q := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).OrderBy(sqlb.F("name").Asc())
	plan, err := sqlb.Explain(context.Background(), h.db, q)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if !strings.HasPrefix(h.lastQuery(), "EXPLAIN (FORMAT JSON, VERBOSE, COSTS) SELECT") {
		t.Errorf("unexpected statement: %s", h.lastQuery())
	}
	if strings.Contains(h.lastQuery(), "ANALYZE") {
		t.Error("Explain must not execute the statement")
	}
	if len(plan.Nodes) != 2 {
		t.Fatalf("parsed %d nodes, want 2", len(plan.Nodes))
	}
	if plan.Nodes[1].Type != "Index Scan" || plan.Nodes[1].Depth != 1 {
		t.Errorf("child node = %+v", plan.Nodes[1])
	}
	if !plan.UsesIndex("users_org_id_idx") {
		t.Error("UsesIndex should find the index")
	}
	if plan.UsesSeqScan("") {
		t.Error("this plan has no sequential scan")
	}
	if d := plan.Diagnostics(); len(d) != 0 {
		t.Errorf("a healthy plan should produce no diagnostics, got:\n%s", sqlb.Diagnostics(d))
	}
}

// This is the regression-guard case: the same query, a dropped index, and a
// test that fails because of it.
func TestExplainCatchesARegression(t *testing.T) {
	h := explainHarness(t, regressedPlan)
	defer h.close()

	q := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).OrderBy(sqlb.F("name").Asc())
	plan, err := sqlb.Explain(context.Background(), h.db, q)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if !plan.UsesSeqScan("users") {
		t.Fatal("should have detected the sequential scan")
	}
	ds := plan.Diagnostics()
	if len(ds) != 2 {
		t.Fatalf("want a seq-scan and an external-sort diagnostic, got %d:\n%s", len(ds), sqlb.Diagnostics(ds))
	}
	text := sqlb.Diagnostics(ds)
	for _, want := range []string{"seq-scan", "external-sort", "fix:", "org_id"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostics should mention %q:\n%s", want, text)
		}
	}
}

func TestExplainIgnoresSmallSequentialScans(t *testing.T) {
	h := explainHarness(t, `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"lookup",
	                                  "Total Cost":1.2,"Plan Rows":12}}]`)
	defer h.close()

	plan, err := sqlb.Explain(context.Background(), h.db, sqlb.Query[User]())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if d := plan.Diagnostics(); len(d) != 0 {
		t.Errorf("scanning a small table is fine, got:\n%s", sqlb.Diagnostics(d))
	}
}

// #176: a sequential scan's "Plan Rows" is what its own filter lets through,
// not what it read to get there. These two plans are Postgres's real answer
// (captured against a 20,000-row unindexed table) for a selective filter and
// an unselective one — both scan every row, and only the row *estimate*
// differs, because that estimate is taken after the filter runs. Gating the
// diagnostic on that estimate made it fire for the unselective query and stay
// silent for the selective one, which is backwards: the selective query is
// the textbook missing-index case.
const selectiveSeqScanPlan = `[{"Plan":{
  "Node Type":"Limit","Total Cost":437.0,"Plan Rows":1,
  "Plans":[{"Node Type":"Seq Scan","Relation Name":"posts",
            "Filter":"(title = 'P17'::text)","Total Cost":437.0,"Plan Rows":1}]}}]`

const unselectiveSeqScanPlan = `[{"Plan":{
  "Node Type":"Seq Scan","Relation Name":"posts",
  "Filter":"(title <> ''::text)","Total Cost":437.0,"Plan Rows":19999}}]`

func TestExplainCatchesASeqScanRegardlessOfFilterSelectivity(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan string
	}{
		{"selective filter, one row out of 20000", selectiveSeqScanPlan},
		{"unselective filter, nearly every row", unselectiveSeqScanPlan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := explainHarness(t, tc.plan)
			defer h.close()

			plan, err := sqlb.Explain(context.Background(), h.db, sqlb.Query[User]())
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			d := plan.Diagnostics()
			if len(d) == 0 {
				t.Fatalf("both queries read the whole 20,000-row table; want a seq-scan diagnostic, got none")
			}
			text := sqlb.Diagnostics(d)
			if !strings.Contains(text, "seq-scan") {
				t.Errorf("diagnostics should mention seq-scan:\n%s", text)
			}
		})
	}
}

func TestExplainAnalyzeOptsInExplicitly(t *testing.T) {
	h := explainHarness(t, `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"users",
	                                  "Total Cost":1.0,"Plan Rows":2,"Actual Rows":2},
	                          "Execution Time":0.31}]`)
	defer h.close()

	plan, err := sqlb.ExplainAnalyze(context.Background(), h.db, sqlb.Query[User]())
	if err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	if !strings.Contains(h.lastQuery(), "ANALYZE") {
		t.Errorf("ExplainAnalyze should request ANALYZE: %s", h.lastQuery())
	}
	if plan.ActualMS == 0 || plan.ActualRows != 2 {
		t.Errorf("actual measurements not parsed: %+v", plan)
	}
}

// Explain doubles as a validity check: a statement the database rejects fails
// here, with the SQL attached, rather than in production.
func TestExplainSurfacesInvalidQueries(t *testing.T) {
	h := newHarness(t, nil, nil)
	defer h.close()
	h.failWith("ERROR: column \"titel\" does not exist")

	_, err := sqlb.Explain(context.Background(), h.db,
		sqlb.Query[User]().Where(sqlb.F("titel").Eq("x")))
	if err == nil {
		t.Fatal("an invalid query should fail at Explain")
	}
	if !strings.Contains(err.Error(), "titel") {
		t.Errorf("error should carry the database's complaint: %v", err)
	}
	if !strings.Contains(err.Error(), "SELECT") {
		t.Errorf("error should carry the offending SQL: %v", err)
	}
}

func TestPlanStringIsReadable(t *testing.T) {
	h := explainHarness(t, goodPlan)
	defer h.close()

	plan, _ := sqlb.Explain(context.Background(), h.db, sqlb.Query[User]())
	s := plan.String()
	for _, want := range []string{"Sort", "Index Scan on users", "using users_org_id_idx", "cost="} {
		if !strings.Contains(s, want) {
			t.Errorf("plan output should contain %q:\n%s", want, s)
		}
	}
}
