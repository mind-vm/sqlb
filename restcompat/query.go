package restcompat

import (
	"fmt"
	"sort"

	"github.com/jryannel/sqlb/schema"
)

// A declared query is a route, so it is part of this diff for the same reason
// a declared action is: a deployed client holds GET /tasks/overdue, and
// withdrawing it or tightening its parameters breaks that client with no DDL
// anywhere in the change (ADR-0039, and the read half of "A read is a query
// and a row scoped write is a mutation").
//
// It was absent from the contract until it was not, which is worth stating
// rather than quietly fixing: between the release that added TableDef.AddQuery
// and this file, adding, removing or repathing a declared query was a REST
// contract change `sqlb impact -error` could not see. A gate that is silent
// about a whole class of route reads as coverage it does not have (ADR-0016),
// which is the same argument that put actions here.
//
// # What is not captured
//
// The result type. A generated query's result is `[]T` for the table it is
// declared on and cannot be anything else today (codegen's resultType), so
// recording it would be recording a constant — a field that is the same in
// every snapshot ever written tells a reader nothing and still has to be
// migrated if the format changes. If a result type ever becomes declarable,
// it belongs here, and it is a response-shape break in the direction a
// narrowing is: this is the note that says where to put it.
//
// The Do func, for the reason captureActions gives about the verb: it is
// application code, it changes on every commit that fixes a bug, and a diff
// that claimed otherwise would report a break for each one.

// QuerySnap is one declared read's contract.
type QuerySnap struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Params is the query string's parameters, in declaration order.
	Params []PropSnap `json:"params,omitempty"`
	// Reads names the tables the query declares it reads. See diffQueries for
	// why a change to it is neutral and why the honest summary names the
	// direction.
	Reads []string `json:"reads,omitempty"`
}

// queryParam is the vocabulary diffProps uses for a query's parameters.
var queryParam = propKind{
	facet: FacetQuery,
	noun:  "parameter",
	// Sharper than the action body's equivalent, and the difference is real
	// rather than a wording preference. rest.Query mounts the operation with
	// RejectUnknownQueryParameters, so a client still sending a withdrawn
	// parameter is *refused* — where an extra property in a JSON body is
	// merely undeclared. Same level either way; a reader deciding whether to
	// ship this deserves to know which one it is.
	removed: "parameter removed; a client still sending it is refused, not ignored",
}

// captureQueries projects a table's declared reads into their contract form.
func captureQueries(t *schema.TableDef, path string) []QuerySnap {
	var out []QuerySnap
	for _, q := range t.Queries() {
		snap := QuerySnap{Name: q.Name, Path: q.FullPath(path)}
		for _, tbl := range q.Reads {
			snap.Reads = append(snap.Reads, tbl.Name())
		}
		for _, f := range q.Params {
			d := f.Desc()
			snap.Params = append(snap.Params, PropSnap{
				Name:       d.Name,
				Type:       string(d.Type),
				Enum:       d.EnumValues,
				Nullable:   d.Nullable,
				HasDefault: d.DatabaseSupplied(),
			})
		}
		out = append(out, snap)
	}
	// Sorted, so that reordering the declarations in a schema file is not a
	// diff in the recorded contract. captureActions says the same.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// diffQueries compares the declared reads of two contracts for the same path.
func diffQueries(path string, o, n map[string]QuerySnap, add func(Break)) {
	for _, name := range unionQueries(o, n) {
		ov, inOld := o[name]
		nv, inNew := n[name]
		switch {
		case inOld && !inNew:
			add(Break{LevelBreaking, path, FacetQuery, name,
				fmt.Sprintf("query removed; GET %s now 404s", ov.Path)})
			continue
		case !inOld && inNew:
			add(Break{LevelAdditive, path, FacetQuery, name, "query added"})
			continue
		}

		// A moved route is a removed one as far as a deployed client is
		// concerned: it holds the old URL, not the name this diff matched on.
		if ov.Path != nv.Path {
			add(Break{LevelBreaking, path, FacetQuery, name,
				fmt.Sprintf("query moved from %s to %s; a deployed client holds the old URL", ov.Path, nv.Path)})
		}
		diffProps(path, name, queryParam, ov.Params, nv.Params, add)

		if !sameStrings(ov.Reads, nv.Reads) {
			add(Break{LevelNeutral, path, FacetQuery, name, readsSummary(ov.Reads, nv.Reads)})
		}
	}
}

// readsSummary describes a change to a query's declared read set, and names
// which direction the change went in.
//
// Neutral, like Action.Touches and for the same first reason: the declaration
// is unenforced, so what moved is what the route *claims*. But unlike Touches
// it is a claim something is meant to consume — a generated client cache
// invalidates this query when a change-feed event for one of these tables
// arrives, which is the whole of what Query.Reads is for — so the two
// directions have different consequences and a summary that did not say which
// one happened would be the less useful half of the finding.
//
// The asymmetry is the opposite way round from the intuition. A deployed
// client holds the *old* set. Widening the server's set means the client does
// not know about the new table and under-invalidates, so it shows stale data;
// narrowing it means the client keeps invalidating on a table the query no
// longer reads, which costs a refetch and is otherwise harmless.
//
// Still neutral rather than breaking, because a stale cache is a degradation
// and not a rejected request, and because nothing generates that subscriber
// yet — no client emitter reads a declared query at all today. When one does,
// this is the classification to revisit first.
func readsSummary(old, new []string) string {
	base := fmt.Sprintf("declared read set changed from %v to %v", old, new)
	if widened(old, new) {
		return base + "; a deployed client holds the old set and will not refetch on the new table, so a cache keyed on it goes stale"
	}
	return base + "; a deployed client keeps invalidating on a table the query no longer reads, which costs a refetch and breaks nothing"
}

// widened reports whether new names a table old did not. A set that both gains
// and loses a table is widened: the gain is the half with a consequence.
func widened(old, new []string) bool {
	had := make(map[string]bool, len(old))
	for _, v := range old {
		had[v] = true
	}
	for _, v := range new {
		if !had[v] {
			return true
		}
	}
	return false
}

func unionQueries(a, b map[string]QuerySnap) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
