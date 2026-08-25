package filter_test

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

// mustQuery parses a URL query string or fails the test.
func mustQuery(t *testing.T, query string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	return values
}

// jsonSQL compiles a JSON filter tree the way a handler would — parse to a
// predicate, put it on a builder — and renders the statement.
func jsonSQL[T any](t *testing.T, o filter.Options, body string) (string, []any) {
	t.Helper()
	pred, err := filter.ParseFilterTree([]byte(body), o)
	if err != nil {
		t.Fatalf("ParseFilterTree(%s): %v", body, err)
	}
	sql, args, err := sqlb.Query[T]().Select(sqlb.F("id")).Where(pred).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql, args
}

// urlSQL compiles the equivalent URL query onto the same shape of builder, so a
// test can hold the two frontends' output side by side.
func urlSQL[T any](t *testing.T, o filter.Options, query string) (string, []any) {
	t.Helper()
	values := mustQuery(t, query)
	q, err := filter.Parse(values, o)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	sql, args, err := sqlb.Query[T]().Select(sqlb.F("id")).Where(q.Where...).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	return sql, args
}

// TestJSONMatchesURL is the load-bearing test: a JSON tree and the URL query
// that means the same thing must compile to the byte-identical statement. It is
// what proves the two frontends share one compiler rather than two that agree
// today and drift tomorrow (ADR-0003).
func TestJSONMatchesURL(t *testing.T) {
	cases := []struct{ name, json, query string }{
		{"eq string", `{"op":"eq","field":"status","value":"active"}`, "status=eq.active"},
		{"eq number, native", `{"op":"eq","field":"views","value":100}`, "views=eq.100"},
		{"eq number, as string", `{"op":"eq","field":"views","value":"100"}`, "views=eq.100"},
		// Above 2^53, where a float64 round-trip would bind a neighbouring
		// integer. The URL token reaches Coerce as digits and always bound
		// exactly; this is the case that says the tree does too.
		{"eq int64 past float64", `{"op":"eq","field":"views","value":9007199254740993}`, "views=eq.9007199254740993"},
		{"in int64 past float64", `{"op":"in","field":"views","value":[9007199254740993]}`, "views=in.9007199254740993"},
		{"eq bool", `{"op":"eq","field":"draft","value":true}`, "draft=eq.true"},
		{"ne", `{"op":"ne","field":"status","value":"draft"}`, "status=ne.draft"},
		{"gte", `{"op":"gte","field":"views","value":10}`, "views=gte.10"},
		{"in", `{"op":"in","field":"status","value":["a","b","c"]}`, "status=in.a,b,c"},
		{"nin", `{"op":"nin","field":"views","value":[1,2]}`, "views=nin.1,2"},
		{"between", `{"op":"between","field":"views","value":[1,10]}`, "views=between.1,10"},
		{"isnull", `{"op":"isnull","field":"published_at"}`, "published_at=isnull"},
		{"notnull", `{"op":"notnull","field":"published_at"}`, "published_at=notnull"},
		{"contains", `{"op":"contains","field":"title","value":"go"}`, "title=contains.go"},
		{
			"and group",
			`{"op":"and","children":[{"op":"eq","field":"status","value":"active"},{"op":"gte","field":"views","value":10}]}`,
			"and=(status.eq.active,views.gte.10)",
		},
		{
			"or group",
			`{"op":"or","children":[{"op":"eq","field":"status","value":"draft"},{"op":"lt","field":"views","value":18}]}`,
			"or=(status.eq.draft,views.lt.18)",
		},
		{
			"nested",
			`{"op":"and","children":[{"op":"eq","field":"status","value":"active"},{"op":"or","children":[{"op":"gte","field":"views","value":10},{"op":"isnull","field":"published_at"}]}]}`,
			"and=(status.eq.active,or(views.gte.10,published_at.isnull))",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jSQL, jArgs := jsonSQL[Article](t, opts(), tc.json)
			uSQL, uArgs := urlSQL[Article](t, opts(), tc.query)
			if jSQL != uSQL {
				t.Errorf("SQL differs:\n json: %s\n url:  %s", jSQL, uSQL)
			}
			if !reflect.DeepEqual(jArgs, uArgs) {
				t.Errorf("args differ:\n json: %#v\n url:  %#v", jArgs, uArgs)
			}
		})
	}
}

// TestJSONArraysMatchURL is the same equivalence check for array columns, whose
// operators go through applyOp's array arm.
func TestJSONArraysMatchURL(t *testing.T) {
	cases := []struct{ name, json, query string }{
		{"has", `{"op":"has","field":"tags","value":"go"}`, "tags=has.go"},
		{"hasany", `{"op":"hasany","field":"tags","value":["go","rust"]}`, "tags=hasany.go,rust"},
		{"hasall", `{"op":"hasall","field":"tags","value":["go","rust"]}`, "tags=hasall.go,rust"},
		{"has int", `{"op":"has","field":"sizes","value":42}`, "sizes=has.42"},
		{"whole-array eq", `{"op":"eq","field":"sizes","value":[1,2,3]}`, "sizes=eq.1,2,3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jSQL, jArgs := jsonSQL[Post](t, postOpts(), tc.json)
			uSQL, uArgs := urlSQL[Post](t, postOpts(), tc.query)
			if jSQL != uSQL {
				t.Errorf("SQL differs:\n json: %s\n url:  %s", jSQL, uSQL)
			}
			if !reflect.DeepEqual(jArgs, uArgs) {
				t.Errorf("args differ:\n json: %#v\n url:  %#v", jArgs, uArgs)
			}
		})
	}
}

// TestJSONDocumentsMatchURL is the same equivalence check for a jsonb column.
// It earns its place separately from the array one because the two frontends
// reach opDoc from opposite directions: the URL carries the document as text and
// validates it, while the tree carries it as a parsed value and re-marshals it.
// Those are two different code paths producing one bind parameter, and this is
// what says they agree — including on key order, which round-tripping normalises
// and a hand-written string does not.
func TestJSONDocumentsMatchURL(t *testing.T) {
	cases := []struct{ name, json, query string }{
		{
			"one key",
			`{"op":"hasdoc","field":"metadata","value":{"lang":"de"}}`,
			`metadata=hasdoc.{"lang":"de"}`,
		},
		{
			// The comma inside the object is not a value separator on either side.
			"nested, with commas",
			`{"op":"hasdoc","field":"metadata","value":{"a":{"b":1},"d":[1,2]}}`,
			`metadata=hasdoc.{"a":{"b":1},"d":[1,2]}`,
		},
		{
			"array document",
			`{"op":"hasdoc","field":"metadata","value":["urgent"]}`,
			`metadata=hasdoc.["urgent"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jSQL, jArgs := jsonSQL[Doc](t, docOpts(), tc.json)
			uSQL, uArgs := urlSQL[Doc](t, docOpts(), tc.query)
			if jSQL != uSQL {
				t.Errorf("SQL differs:\n json: %s\n url:  %s", jSQL, uSQL)
			}
			if !reflect.DeepEqual(jArgs, uArgs) {
				t.Errorf("args differ:\n json: %#v\n url:  %#v", jArgs, uArgs)
			}
		})
	}
}

// A document column refuses the same operators through the tree as through the
// URL. The gate is shared, so this is guarding that the tree keeps reaching it
// rather than growing a second, laxer path.
func TestJSONTreeRefusesNonDocumentOperators(t *testing.T) {
	for _, body := range []string{
		`{"op":"gt","field":"metadata","value":1}`,
		`{"op":"startswith","field":"metadata","value":"x"}`,
		`{"op":"contains","field":"metadata","value":"x"}`,
	} {
		_, err := filter.ParseFilterTree([]byte(body), docOpts())
		if err == nil {
			t.Errorf("ParseFilterTree(%s) should have been refused", body)
			continue
		}
		if !strings.Contains(err.Error(), "hasdoc") {
			t.Errorf("error = %q, want it to offer \"hasdoc\"", err)
		}
	}
}

// TestParseReadsFilterParam checks that Parse compiles a tree carried in
// ?filter= and that it lands the identical predicate the URL grammar would — the
// query string is just a second way in to the one compiler.
func TestParseReadsFilterParam(t *testing.T) {
	tree := `{"op":"eq","field":"status","value":"active"}`
	viaParam, argsA := urlSQL[Article](t, opts(), "filter="+url.QueryEscape(tree))
	viaGrammar, argsB := urlSQL[Article](t, opts(), "status=eq.active")
	if viaParam != viaGrammar {
		t.Errorf("?filter= and the grammar differ:\n param: %s\n gramr: %s", viaParam, viaGrammar)
	}
	if !reflect.DeepEqual(argsA, argsB) {
		t.Errorf("args differ:\n param: %#v\n gramr: %#v", argsA, argsB)
	}
}

// TestUnifiedBudgetAcrossFormats is the point of folding the tree into Parse: a
// request cannot escape MaxFilters by putting some conditions in the query
// string and the rest in a tree. Two independent budgets would pass this; one
// shared budget refuses it.
func TestUnifiedBudgetAcrossFormats(t *testing.T) {
	o := opts()
	o.MaxFilters = 3

	tree := url.QueryEscape(`{"op":"and","children":[` +
		`{"op":"gte","field":"views","value":1},` +
		`{"op":"lt","field":"views","value":9}]}`)
	// Two conditions in the query string, two in the tree: four against a budget
	// of three.
	values := mustQuery(t, "status=eq.active&status=ne.archived&filter="+tree)

	_, err := filter.Parse(values, o)
	if err == nil || !strings.Contains(err.Error(), "filter conditions requested") {
		t.Fatalf("want a shared-budget rejection, got %v", err)
	}

	// The two query conditions alone are within budget, so the tree is what tips
	// it over — confirm the same tree passes when it has the budget to itself.
	if _, err := filter.Parse(mustQuery(t, "filter="+tree), o); err != nil {
		t.Fatalf("the tree alone is two conditions and should fit a budget of three: %v", err)
	}
}

// TestJSONTreeErrors covers the rejections. Every one is a 400 that names the
// problem, and the column-level ones name what would have been accepted, exactly
// as the URL frontend does (ADR-0011).
func TestJSONTreeErrors(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"unknown field", `{"op":"eq","field":"nope","value":"x"}`, "unknown parameter"},
		{"unfilterable column", `{"op":"eq","field":"created_at","value":"x"}`, "not filterable"},
		{"hidden column reads as unknown", `{"op":"eq","field":"internal_note","value":"x"}`, "unknown parameter"},
		{"unknown operator", `{"op":"zap","field":"status","value":"x"}`, `unknown operator "zap"`},
		{"missing field", `{"op":"eq","value":"x"}`, "missing a field"},
		{"missing op", `{"field":"status","value":"x"}`, "missing an op"},
		{"group without children", `{"op":"and","children":[]}`, "at least one child"},
		{"group carrying a field", `{"op":"and","field":"status","children":[{"op":"eq","field":"status","value":"a"}]}`, "cannot carry a field"},
		{"condition with children", `{"op":"eq","field":"status","value":"a","children":[{"op":"eq","field":"status","value":"b"}]}`, "cannot have children"},
		{"nullary with a value", `{"op":"isnull","field":"published_at","value":"x"}`, "takes no value"},
		{"between wrong arity", `{"op":"between","field":"views","value":[1]}`, "exactly 2 values"},
		{"in wants an array", `{"op":"in","field":"status","value":"x"}`, "requires an array value"},
		{"scalar wants a scalar", `{"op":"eq","field":"views","value":{"a":1}}`, "string, number or boolean"},
		{"bad coercion", `{"op":"eq","field":"views","value":"abc"}`, "expected an integer"},
		{"unknown JSON field", `{"op":"eq","field":"status","value":"x","bogus":1}`, "invalid filter JSON"},
		{"trailing data", `{"op":"isnull","field":"published_at"}{}`, "trailing data"},
		{"pattern needs text", `{"op":"contains","field":"views","value":"x"}`, "needs a text column"},
		{"scalar op on array column", `{"op":"gt","field":"tags","value":"x"}`, "does not apply to the array column"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := opts()
			if strings.Contains(tc.json, "tags") {
				o = postOpts()
			}
			_, err := filter.ParseFilterTree([]byte(tc.json), o)
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if _, ok := filter.AsErrors(err); !ok {
				t.Fatalf("error is not a filter.Errors: %T", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestJSONStructuralLimits bounds the tree by shape before any column is
// resolved, so a hostile payload is cheap to refuse.
func TestJSONStructuralLimits(t *testing.T) {
	t.Run("too deep", func(t *testing.T) {
		// Nest groups well past the limit around one leaf.
		body := strings.Repeat(`{"op":"and","children":[`, 8) +
			`{"op":"eq","field":"status","value":"a"}` +
			strings.Repeat(`]}`, 8)
		_, err := filter.ParseFilterTree([]byte(body), opts())
		if err == nil || !strings.Contains(err.Error(), "nested deeper") {
			t.Fatalf("want depth error, got %v", err)
		}
	})

	t.Run("too many nodes", func(t *testing.T) {
		var leaves []string
		for i := 0; i < filter.MaxTreeNodes+1; i++ {
			leaves = append(leaves, `{"op":"eq","field":"status","value":"a"}`)
		}
		body := `{"op":"and","children":[` + strings.Join(leaves, ",") + `]}`
		_, err := filter.ParseFilterTree([]byte(body), opts())
		if err == nil || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("want node-count error, got %v", err)
		}
	})

	t.Run("filter budget still applies", func(t *testing.T) {
		// Under the node cap but over the leaf-condition budget: the per-leaf
		// charge in jsonLeaf catches it, not the structural check.
		o := opts()
		o.MaxFilters = 3
		var leaves []string
		for i := 0; i < 5; i++ {
			leaves = append(leaves, `{"op":"eq","field":"status","value":"a"}`)
		}
		body := `{"op":"and","children":[` + strings.Join(leaves, ",") + `]}`
		_, err := filter.ParseFilterTree([]byte(body), o)
		if err == nil || !strings.Contains(err.Error(), "filter conditions requested") {
			t.Fatalf("want budget error, got %v", err)
		}
	})
}

// TestJSONMergesWithURLQuery shows the intended wiring: the filter tree comes
// from the body, while sort and pagination stay on the URL, and the two are
// combined before Apply.
func TestJSONMergesWithURLQuery(t *testing.T) {
	q, err := filter.Parse(mustQuery(t, "sort=-views&per_page=10"), opts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pred, err := filter.ParseFilterTree([]byte(`{"op":"eq","field":"status","value":"active"}`), opts())
	if err != nil {
		t.Fatalf("ParseFilterTree: %v", err)
	}
	q.Where = append(q.Where, pred)

	sql, args, err := filter.Apply(sqlb.Query[Article]().Select(sqlb.F("id")), q).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{"WHERE", "status", "ORDER BY", "LIMIT"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL %q missing %q", sql, want)
		}
	}
	if len(args) == 0 {
		t.Errorf("expected the status bind parameter in %#v", args)
	}
}
