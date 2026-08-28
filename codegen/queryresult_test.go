package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A declared read whose answer is not rows of its own table (#240).
//
// `rest.Resource` returns rows of T and a generated `Query` returned `[]T`, so
// a metering table's actual read — per-bucket sums for a chart — could not be
// mounted from the declaration at all. A bucketed sum is a row of no declared
// table, so every application with a chart hand-wrote that one endpoint outside
// the generated surface however much of the rest was generated.
//
// ADR-0057 left this open with a trigger rather than answering it: revisit if
// the fixed `[]T` turns out to be the wrong default often enough that a result
// type is worth declaring.

// rollupFixture is the shape the issue describes: events in, buckets out.
func rollupFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("usage_events",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Timestamp("at").Filterable().Sortable(),
		schema.Numeric("amount", 18, 2),
	).Expose(schema.REST{Ops: schema.Reads}).
		AddQuery(schema.Query{
			Name:   "usage",
			Params: schema.Body(schema.Date("from"), schema.Date("to")),
			Returns: schema.Result(
				schema.Timestamp("bucket"),
				schema.Numeric("total", 18, 2).Comment("summed over the bucket"),
			),
		})
	return r
}

func TestADeclaredQueryResultChangesWhatTheFuncReturns(t *testing.T) {
	src := generate(t, rollupFixture())["rest_gen.go"]

	for _, want := range []string{
		"type UsageUsageEventResult struct {",
		// The signature is the point: the compiler now demands a func
		// returning the declared shape, where before it demanded []T and a
		// rollup could not be written against it at all.
		"UsageUsageEvent func(context.Context, sqlb.Executor, UsageUsageEventParams) ([]UsageUsageEventResult, error)",
		// A db tag as well as json, because this type is the destination of a
		// sqlb.Collect and that matches result columns to fields by db tag.
		"Bucket time.Time `db:\"bucket\" json:\"bucket\"`",
		"// summed over the bucket",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated query result missing %q:\n%s", want, src)
		}
	}
}

// The other direction, and the reason this is a declaration rather than a
// change of default: a query that declares no result still answers with rows of
// its own table, which is what the reads that already existed want.
func TestAQueryWithNoDeclaredResultStillReturnsTheModel(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.Reads}).
		AddQuery(schema.Query{Name: "overdue"})

	src := generate(t, r)["rest_gen.go"]
	if !strings.Contains(src, "OverdueTask func(context.Context, sqlb.Executor, OverdueTaskParams) ([]Task, error)") {
		t.Errorf("an undeclared result should still be the model:\n%s", src)
	}
	if strings.Contains(src, "OverdueTaskResult") {
		t.Errorf("a query declaring no result should emit no result type:\n%s", src)
	}
}

// The result goes through the same validator an action's does, because it is
// the same vocabulary making the same claims — and a second copy of that rule
// list is a place the two can disagree.
func TestAQueryResultCannotClaimAColumnCapability(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("usage_events", schema.UUIDv7("id").PrimaryKey()).
		Expose(schema.REST{Ops: schema.Reads}).
		AddQuery(schema.Query{
			Name:    "usage",
			Returns: schema.Result(schema.Timestamp("bucket").Filterable()),
		})

	err := r.Validate()
	if err == nil {
		t.Fatal("a query result property claiming Filterable was accepted")
	}
	for _, want := range []string{`query "usage": result`, "Filterable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}
