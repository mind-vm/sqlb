package restcompat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/restcompat"
	"github.com/mind-vm/sqlb/schema"
)

// withQueries builds a tasks resource carrying the given declared reads.
//
// A second table comes with it, because Query.Reads is typed as []*TableDef
// and there has to be something for it to name.
func withQueries(queries ...func(*schema.Registry) schema.Query) *schema.Registry {
	r := schema.NewRegistry()
	r.Table("lists", schema.UUIDv7("id").PrimaryKey())
	t := r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
		schema.Enum("status", "open", "done").Default(schema.Value("open")),
		schema.Timestamp("closed_at").Nullable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList})
	for _, q := range queries {
		t.AddQuery(q(r))
	}
	return r
}

func overdue(*schema.Registry) schema.Query {
	return schema.Query{
		Name:   "overdue",
		Params: schema.Body(schema.Timestamp("as_of").Nullable()),
	}
}

// A declared read is a route, and withdrawing a route breaks the client
// holding its URL — with no DDL anywhere in the change, which is the premise
// of `sqlb impact`. This is the case the tool could not see at all before
// queries were captured.
func TestRemovingAQueryIsBreaking(t *testing.T) {
	breaks := restcompat.Diff(withQueries(overdue), withQueries())

	b := find(t, breaks, "query removed")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Facet != restcompat.FacetQuery {
		t.Errorf("facet = %s, want query", b.Facet)
	}
	// The URL, not just the name: that is what a deployed client actually has.
	if !strings.Contains(b.Summary, "/tasks/overdue") {
		t.Errorf("summary does not name the route: %s", b.Summary)
	}
}

func TestAddingAQueryIsAdditive(t *testing.T) {
	breaks := restcompat.Diff(withQueries(), withQueries(overdue))

	b := find(t, breaks, "query added")
	if b.Level != restcompat.LevelAdditive {
		t.Errorf("level = %s, want additive", b.Level)
	}
}

// Moving the route is removing it, as far as the client holding the old URL is
// concerned. The name is what this diff matches on, so without this the move
// would be silent.
func TestMovingAQueryIsBreaking(t *testing.T) {
	moved := func(*schema.Registry) schema.Query {
		q := overdue(nil)
		q.Path = "/past-due"
		return q
	}
	breaks := restcompat.Diff(withQueries(overdue), withQueries(moved))

	b := find(t, breaks, "query moved")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if !strings.Contains(b.Summary, "/tasks/overdue") || !strings.Contains(b.Summary, "/tasks/past-due") {
		t.Errorf("summary does not name both URLs: %s", b.Summary)
	}
}

// Withdrawing a parameter is breaking for a sharper reason than an action
// body's equivalent, and the summary has to say so: rest.Query mounts with
// RejectUnknownQueryParameters, so the request is refused rather than accepted
// with something undeclared in it.
func TestRemovingAQueryParamSaysTheRequestIsRefused(t *testing.T) {
	noParams := func(*schema.Registry) schema.Query {
		return schema.Query{Name: "overdue"}
	}
	breaks := restcompat.Diff(withQueries(overdue), withQueries(noParams))

	b := find(t, breaks, "parameter removed")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Field != "overdue.as_of" {
		t.Errorf("field = %q, want overdue.as_of", b.Field)
	}
	if !strings.Contains(b.Summary, "refused, not ignored") {
		t.Errorf("summary does not say the request is refused: %s", b.Summary)
	}
}

// A parameter every existing request omits cannot be required without breaking
// every existing request. This is the action body's rule, reached through the
// shared classifier — the test is here to prove the query side actually calls
// it, not to re-test the rule.
func TestAddingARequiredQueryParamIsBreaking(t *testing.T) {
	required := func(*schema.Registry) schema.Query {
		return schema.Query{
			Name:   "overdue",
			Params: schema.Body(schema.Timestamp("as_of").Nullable(), schema.Text("scope")),
		}
	}
	breaks := restcompat.Diff(withQueries(overdue), withQueries(required))

	b := find(t, breaks, "required parameter added")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
	if b.Facet != restcompat.FacetQuery {
		t.Errorf("facet = %s, want query", b.Facet)
	}
}

func TestQueryParamBecomingRequiredIsBreaking(t *testing.T) {
	tightened := func(*schema.Registry) schema.Query {
		return schema.Query{Name: "overdue", Params: schema.Body(schema.Timestamp("as_of"))}
	}
	breaks := restcompat.Diff(withQueries(overdue), withQueries(tightened))

	b := find(t, breaks, "parameter became required")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
}

// The enum rule reaches the query side too, and narrows the same way.
func TestNarrowingAQueryParamEnumIsBreaking(t *testing.T) {
	withValues := func(values ...string) func(*schema.Registry) schema.Query {
		return func(*schema.Registry) schema.Query {
			return schema.Query{
				Name:   "by-state",
				Params: schema.Body(schema.Enum("state", values...).Nullable()),
			}
		}
	}
	breaks := restcompat.Diff(
		withQueries(withValues("open", "done", "blocked")),
		withQueries(withValues("open", "done")))

	b := find(t, breaks, "parameter values changed")
	if b.Level != restcompat.LevelBreaking {
		t.Errorf("level = %s, want breaking", b.Level)
	}
}

// The two directions are classified apart, and the summary names which one
// happened: widening leaves a deployed client's cache stale, narrowing costs it
// a refetch. A finding that said only "changed" would leave a reviewer to work
// out which.
//
// Widening was neutral until the TypeScript emitter shipped, on the argument
// that nothing consumed Query.Reads yet. Something does now, so "no client is
// affected" — which is what LevelNeutral means — stopped being true: a client
// generated before the widening never refetches on the new table. Unknown
// rather than breaking, because whether it bites depends on which emitter built
// the deployed client (#316).
func TestWideningReadsIsUnknownBecauseTheClientMayUnderInvalidate(t *testing.T) {
	reads := func(r *schema.Registry) schema.Query {
		return schema.Query{Name: "overdue", Reads: []*schema.TableDef{r.Get("lists")}}
	}
	breaks := restcompat.Diff(withQueries(overdue), withQueries(reads))

	b := find(t, breaks, "declared read set changed")
	if b.Level != restcompat.LevelUnknown {
		t.Errorf("level = %s, want unknown — a TypeScript client generated before this widening under-invalidates", b.Level)
	}
	if !strings.Contains(b.Summary, "goes stale") {
		t.Errorf("summary does not name the widening consequence: %s", b.Summary)
	}
}

// Narrowing stays neutral, and that is a claim rather than a leftover: the
// client keeps invalidating on a table the query no longer reads, which costs
// one refetch and changes nothing it displays.
func TestNarrowingReadsSaysNothingBreaks(t *testing.T) {
	reads := func(r *schema.Registry) schema.Query {
		return schema.Query{Name: "overdue", Reads: []*schema.TableDef{r.Get("lists")}}
	}
	breaks := restcompat.Diff(withQueries(reads), withQueries(overdue))

	b := find(t, breaks, "declared read set changed")
	if b.Level != restcompat.LevelNeutral {
		t.Errorf("level = %s, want neutral", b.Level)
	}
	if !strings.Contains(b.Summary, "breaks nothing") {
		t.Errorf("summary does not name the narrowing consequence: %s", b.Summary)
	}
}

// Reordering declarations in a schema file must not show up as a contract
// change, or every reviewer learns to ignore the tool.
func TestQueryOrderIsNotAContractChange(t *testing.T) {
	a := func(*schema.Registry) schema.Query { return schema.Query{Name: "a"} }
	b := func(*schema.Registry) schema.Query { return schema.Query{Name: "b"} }

	if breaks := restcompat.Diff(withQueries(a, b), withQueries(b, a)); len(breaks) != 0 {
		t.Errorf("reordering declarations produced %v", breaks)
	}
}

// A resource that declares no query records none, so every baseline committed
// before this field existed stays byte-identical and needs no re-record.
func TestNoQueriesMeansNoQueriesKeyInTheSnapshot(t *testing.T) {
	data, err := json.Marshal(restcompat.Capture(withQueries()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"queries"`) {
		t.Errorf("a resource with no declared query still wrote a queries key:\n%s", data)
	}
}

// The other direction of the same property: a declared query does reach the
// serialised snapshot, so `sqlb impact -write` records it and the next run has
// something to compare against.
func TestADeclaredQueryReachesTheSnapshot(t *testing.T) {
	data, err := json.Marshal(restcompat.Capture(withQueries(overdue)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"queries"`, `"overdue"`, `"/tasks/overdue"`, `"as_of"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("snapshot does not carry %s:\n%s", want, data)
		}
	}
}

// The guard proven the other way (docs/architecture.md, "Guards proven both
// ways"). Every assertion above is about a break being *reported*; this one
// fails if the whole facet is dropped, which is the failure mode a suite of
// positive assertions can have while still passing — capture a query, diff it
// against a contract that has none, and require the strict gate to count it.
func TestTheStrictGateCountsAWithdrawnQuery(t *testing.T) {
	breaking := restcompat.Breaking(restcompat.Diff(withQueries(overdue), withQueries()))
	if len(breaking) == 0 {
		t.Fatal("withdrawing a declared query is invisible to `sqlb impact -error`")
	}
	for _, b := range breaking {
		if b.Facet == restcompat.FacetQuery {
			return
		}
	}
	t.Errorf("no query-facet break among %v", breaking)
}
