package recipes_test

import (
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
	"github.com/mind-vm/sqlb/filter"
)

// A JSON filter tree is the second frontend over the same compiler. Use it when
// the nesting a client needs outgrows what a query string can spell — and when
// the client is a program rather than a person, which is most of the time an
// agent is involved.
//
// It is gated identically: the same column capabilities, the same coercion, the
// same bind discipline, the same budget. "Arbitrary nesting" is not a way in.
func Example_filterTreeFromAPostBody() {
	body := []byte(`{
	  "op": "and",
	  "children": [
	    {"op": "eq",  "field": "org_id", "value": "acme"},
	    {"op": "or",  "children": [
	      {"op": "eq",  "field": "status",     "value": "published"},
	      {"op": "gte", "field": "view_count", "value": 1000}
	    ]}
	  ]
	}`)

	pred, err := filter.ParseFilterTree(body, filter.Options{Model: sqlb.ModelOf[recipes.Post]()})
	if err != nil {
		panic(err)
	}

	showWhere(sqlb.Query[recipes.Post]().Where(pred))
	// Output:
	// WHERE ("org_id" = $1) AND (("status" = $2) OR ("view_count" >= $3))
	// args: [acme published 1000]
}

// The refusals are the same too, and every problem is reported at once rather
// than one per round trip — which is the difference between a client that can
// fix its request and one that plays twenty questions.
func Example_filterTreeRefusals() {
	body := []byte(`{
	  "op": "and",
	  "children": [
	    {"op": "eq",       "field": "password_hash", "value": "x"},
	    {"op": "contains", "field": "view_count",    "value": "12"}
	  ]
	}`)

	_, err := filter.ParseFilterTree(body, filter.Options{Model: sqlb.ModelOf[recipes.Author]()})
	showFilterErrors(err)
	// Output:
	// filter: password_hash: unknown parameter (allowed: id, org_id, name, email)
	// filter: view_count: unknown parameter (allowed: id, org_id, name, email)
}

// A tree may also arrive inside a query string, in the reserved `filter`
// parameter, alongside the URL grammar. Parse charges both to one MaxFilters
// budget — the point of the budget being what a request costs, not which
// spelling it used.
func Example_filterTreeReservedParameter() {
	showConst("filter.TreeParam", filter.TreeParam)
	// Output:
	// filter.TreeParam = filter
}
