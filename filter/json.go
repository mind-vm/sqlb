package filter

// A JSON filter tree is the second wire format the filter package accepts, next
// to the URL grammar. It exists because the URL grammar cannot spell arbitrary
// and/or nesting without punctuation the query string fights, and because a
// client assembling filters from a form is already holding a tree, not a string.
//
// Both formats terminate in the same place. The URL parser and this one each
// turn their own wire format into typed operands and hand them to applyOp, so a
// JSON filter and the equivalent URL filter compile to the identical predicate
// and are subject to the same bind discipline, the same hooks, and the same
// authorisation as a query written by hand (ADR-0003).
//
// Shape:
//
//	{"op":"and","children":[
//	  {"op":"eq","field":"status","value":"active"},
//	  {"op":"in","field":"tag","value":["a","b"]},
//	  {"op":"isnull","field":"deleted_at"}
//	]}
//
// A node is a group (op is "and"/"or"/"not", with children) or a leaf condition
// (op is a comparison, with a field and — unless the operator is nullary — a
// value). The operator vocabulary is the URL grammar's, so `ne`/`nin`/`isnull`
// are the spellings, not `neq`/`not_in`/`is_empty`.
//
// `not` is the one group that is unary: it takes exactly one child, and a
// caller negating several conditions spells the grouping itself.
//
//	{"op":"not","children":[
//	  {"op":"or","children":[
//	    {"op":"eq","field":"status","value":"draft"},
//	    {"op":"isnull","field":"published_at"}
//	  ]}
//	]}
//
// Requiring the inner group is the point. `not` over a child list would have to
// pick between NOT(a AND b) and (NOT a AND NOT b), and those differ on exactly
// the rows a filter is written to separate. Refusing the second child means the
// tree never has to be read for a convention.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/mind-vm/sqlb"
)

// Structural limits for a JSON filter tree, the analogue of MaxGroupDepth and
// the per-request condition budget. A hostile tree is bounded by shape before a
// single column is resolved, so the cost of rejecting it does not scale with
// how deep or wide the attacker made it.
const (
	MaxTreeDepth = 4
	MaxTreeNodes = 64
)

// Node is one node of a JSON filter tree: a logical group (Op is "and"/"or",
// with Children) or a leaf condition (Op is a comparison, with Field and
// Value). Exactly one shape per node; validateTree enforces it.
type Node struct {
	Op       string `json:"op"`
	Children []Node `json:"children,omitempty"`
	Field    string `json:"field,omitempty"`
	Value    any    `json:"value,omitempty"`
}

func (n *Node) isGroup() bool { return n.Op == "and" || n.Op == "or" || n.Op == "not" }

// ParseFilterTree decodes a standalone JSON filter tree and compiles it into a
// single predicate, gated by opts.Model exactly as the URL frontend is. Use it
// when the tree arrives on its own — a POST body, say — rather than in a query
// string: a tree in `?filter=` is compiled by [Parse], which shares its
// MaxFilters budget with the URL filters in the same request. On its own the
// tree has the whole budget to itself.
//
// Every problem is collected rather than reported one at a time, so a malformed
// tree takes one round trip to fix (ADR-0011). The error is a filter.Errors, so
// WriteError renders it as the same 400 the URL frontend produces.
func ParseFilterTree(data []byte, opts Options) (sqlb.Pred, error) {
	if opts.Model == nil {
		return sqlb.Pred{}, fmt.Errorf("filter: Options.Model is required")
	}
	p := &parser{opts: opts, model: opts.Model}
	pred, ok := p.compileTree(data)
	if len(p.errs) > 0 {
		return sqlb.Pred{}, p.errs
	}
	if !ok {
		return sqlb.Pred{}, Errors{&Error{Param: TreeParam, Reason: "empty filter"}}
	}
	return pred, nil
}

// compileTree decodes and compiles a JSON filter tree with this parser, so its
// conditions charge the same budget as any URL filters the parser has already
// read — a request cannot exceed MaxFilters by splitting its conditions across
// the two formats. Problems are recorded on p.errs alongside everything else,
// so a bad tree and a bad query parameter come back together (ADR-0011).
//
// ok reports whether the tree yielded a predicate. It is false when the tree
// was structurally bad, was empty, or had every leaf fail; the caller tells
// "nothing because errors" from "nothing, cleanly" by inspecting p.errs.
func (p *parser) compileTree(data []byte) (sqlb.Pred, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Numbers stay as their source text rather than becoming float64. ADR-0003's
	// claim is that a JSON filter and the equivalent URL filter compile to the
	// identical predicate, and float64 breaks it above 2^53: the URL token goes
	// straight to Coerce and binds exactly, while a decoded-then-reformatted
	// float64 binds a neighbouring integer. Coerce parses the exact text for
	// every numeric column type, so handing it the original digits is both the
	// fix and the simpler path.
	dec.UseNumber()
	var root Node
	if err := dec.Decode(&root); err != nil {
		p.errf(TreeParam, "", "invalid filter JSON: %v", err)
		return sqlb.Pred{}, false
	}
	if dec.More() {
		p.errf(TreeParam, "", "trailing data after filter")
		return sqlb.Pred{}, false
	}
	// Only tree-shape problems gate compilation, not errors the URL parse may
	// already have recorded: a malformed tree neither resolves columns nor
	// spends the condition budget, but a bad sort parameter must not stop the
	// tree from being checked and its own faults reported.
	before := len(p.errs)
	nodes := 0
	p.validateTree(&root, 1, &nodes)
	if len(p.errs) > before {
		return sqlb.Pred{}, false
	}
	return p.jsonNode(&root)
}

// validateTree checks shape and structural limits before any column is
// resolved. It mirrors the URL frontend's depth cap and the MaxFilters backstop:
// depth and node count are bounded here, and the per-leaf budget is charged
// later in jsonLeaf, so a group full of conditions costs what those conditions
// cost written out.
//
// It records every shape problem rather than stopping at the first, except the
// two limits, which return early because a tree that has already blown its size
// budget should not be walked further to enumerate faults in the part beyond it.
func (p *parser) validateTree(n *Node, depth int, nodes *int) {
	*nodes++
	if *nodes > MaxTreeNodes {
		p.errf("filter", "", "filter has more than %d nodes", MaxTreeNodes)
		return
	}
	switch {
	case n.Op == "":
		p.errf("filter", "", "node is missing an op")
	case n.isGroup():
		if n.Field != "" || n.Value != nil {
			p.errf("filter", n.Op, "group %q cannot carry a field or value", n.Op)
		}
		if len(n.Children) == 0 {
			p.errf("filter", n.Op, "group %q must have at least one child", n.Op)
		}
		// `not` is unary. A second child is refused rather than read as an
		// implicit conjunction, because the two readings a bare list invites
		// disagree about the rows the filter was written to separate.
		if n.Op == "not" && len(n.Children) > 1 {
			p.errf("filter", n.Op, "group %q takes exactly one child, got %d; "+
				"wrap several conditions in an \"and\" or \"or\" group", n.Op, len(n.Children))
		}
		if depth >= MaxTreeDepth && len(n.Children) > 0 {
			p.errf("filter", n.Op, "filter groups nested deeper than %d levels", MaxTreeDepth)
			return
		}
		for i := range n.Children {
			p.validateTree(&n.Children[i], depth+1, nodes)
		}
	default: // leaf condition
		if n.Field == "" {
			p.errf("filter", n.Op, "condition is missing a field")
		}
		if len(n.Children) > 0 {
			p.errf("filter", n.Field, "condition on %q cannot have children", n.Field)
		}
	}
}

// jsonNode compiles a validated tree into a predicate. A group recurses and
// combines with And/Or/Not; a leaf goes through jsonLeaf. A group whose children
// all fail contributes nothing rather than an empty predicate, matching how the
// URL frontend drops a group that parsed to no conditions.
//
// That drop is why `not` returns nothing when its child fails rather than
// negating the zero predicate: sqlb.Not leaves a zero Pred zero, so the two
// agree, but going through the same empty check keeps a failed negation from
// silently becoming an absent one. Any child that failed also recorded an
// error, so the request is a 400 either way and no predicate reaches Postgres.
func (p *parser) jsonNode(n *Node) (sqlb.Pred, bool) {
	if !n.isGroup() {
		return p.jsonLeaf(n)
	}
	preds := make([]sqlb.Pred, 0, len(n.Children))
	for i := range n.Children {
		if pred, ok := p.jsonNode(&n.Children[i]); ok {
			preds = append(preds, pred)
		}
	}
	if len(preds) == 0 {
		return sqlb.Pred{}, false
	}
	switch n.Op {
	case "or":
		return sqlb.Or(preds...), true
	case "not":
		// validateTree has already refused a second child.
		return sqlb.Not(preds[0]), true
	}
	return sqlb.And(preds...), true
}

// jsonLeaf resolves the column, charges the budget and coerces the typed value,
// then hands off to applyOp — the same compiler the URL frontend ends in. The
// steps up to applyOp are the JSON frontend's own: budget, column gate, the
// array/scalar operator gate, and coercion from a JSON value rather than a
// string.
func (p *parser) jsonLeaf(n *Node) (sqlb.Pred, bool) {
	if !p.charge() {
		return sqlb.Pred{}, false
	}
	col := p.filterableColumn(n.Field)
	if col == nil {
		return sqlb.Pred{}, false
	}
	kind, known := operators[n.Op]
	if !known {
		p.errAllowed(n.Field, n.Op, fmt.Sprintf("unknown operator %q", n.Op), operatorNames())
		return sqlb.Pred{}, false
	}
	f := sqlb.F(col.Name)
	elem, isArray, ok := p.gateColumnKind(col, n.Op, kind, false, n.Field, n.Op)
	if !ok {
		return sqlb.Pred{}, false
	}
	if !p.refuseBareDate(col, n.Op, jsonStrings(n.Value), n.Field, n.Op) {
		return sqlb.Pred{}, false
	}
	operands, ok := p.jsonOperands(col, elem, isArray, n, kind)
	if !ok {
		return sqlb.Pred{}, false
	}
	return p.applyOp(col, f, n.Op, kind, isArray, operands, n.Field, n.Op)
}

// jsonOperands turns a leaf's JSON value into the coerced operands applyOp
// expects, the JSON counterpart of urlOperands. It reuses Coerce so the column
// type — not the JSON type — drives parsing, which is why a value may arrive as
// either 42 or "42": uuid, time and TextUnmarshaler columns get the exact
// parsing they get from the URL frontend.
func (p *parser) jsonOperands(col *sqlb.ColumnInfo, elem reflect.Type, isArray bool,
	n *Node, kind opKind) ([]any, bool) {

	switch kind {
	case opNullary:
		if n.Value != nil {
			p.errf(n.Field, n.Op, "operator %q takes no value", n.Op)
			return nil, false
		}
		return nil, true

	case opDay:
		s, ok := n.Value.(string)
		if !ok || !isBareDate(s) {
			p.errf(n.Field, n.Op, "operator %q takes a calendar date, e.g. {%q: {%q: %q}}",
				n.Op, col.Wire, "day", "2026-09-01")
			return nil, false
		}
		return []any{s}, true

	case opPattern:
		s, ok := n.Value.(string)
		if !ok {
			p.errf(n.Field, n.Op, "operator %q requires a string value", n.Op)
			return nil, false
		}
		if !p.withinLength(s, n.Field, s) {
			return nil, false
		}
		return []any{s}, true

	case opElem:
		v, ok := p.jsonScalarValue(n, n.Value, elem)
		if !ok {
			return nil, false
		}
		return []any{v}, true

	case opList:
		vals, ok := p.jsonList(n, col.Type, 1, p.opts.maxListValues())
		if !ok {
			return nil, false
		}
		return vals, true

	case opSet:
		// Unlike `in`, an empty set is meaningful: it is the empty array, which
		// every array contains and none overlaps. So the floor is zero.
		return p.jsonList(n, elem, 0, p.opts.maxListValues())

	case opRange:
		vals, ok := p.jsonList(n, col.Type, 2, 2)
		if !ok {
			return nil, false
		}
		return vals, true

	case opDoc:
		// The URL frontend validates document text; here the document arrived as
		// part of the filter tree and is already parsed, so it is re-marshalled
		// rather than re-checked. Round-tripping also normalises the spelling, so
		// the same filter written either way binds the same parameter.
		if n.Value == nil {
			p.errf(n.Field, n.Op, "operator %q needs a JSON document", n.Op)
			return nil, false
		}
		doc, err := json.Marshal(n.Value)
		if err != nil {
			p.errf(n.Field, n.Op, "operator %q was given a value that is not a JSON document: %v", n.Op, err)
			return nil, false
		}
		if !p.withinLength(string(doc), n.Field, string(doc)) {
			return nil, false
		}
		return []any{string(doc)}, true

	default: // opBinary
		if isArray {
			// A whole-array eq/ne binds an array literal built from an element
			// list, held to the same bounds as `hasany`.
			return p.jsonList(n, elem, 0, p.opts.maxListValues())
		}
		v, ok := p.jsonScalarValue(n, n.Value, col.Type)
		if !ok {
			return nil, false
		}
		return []any{v}, true
	}
}

// jsonList coerces a JSON array to typed operands, holding each element to the
// per-member length budget and the [minLen, maxLen] count the operator allows.
func (p *parser) jsonList(n *Node, t reflect.Type, minLen, maxLen int) ([]any, bool) {
	raw, ok := n.Value.([]any)
	if !ok {
		p.errf(n.Field, n.Op, "operator %q requires an array value", n.Op)
		return nil, false
	}
	switch {
	case maxLen == minLen && len(raw) != minLen:
		// A fixed-arity operator (between) states the exact count it wants.
		p.errf(n.Field, n.Op, "operator %q needs exactly %d values, got %d", n.Op, minLen, len(raw))
		return nil, false
	case len(raw) < minLen:
		p.errf(n.Field, n.Op, "operator %q needs at least %d value(s), got %d", n.Op, minLen, len(raw))
		return nil, false
	case len(raw) > maxLen:
		p.errf(n.Field, n.Op, "operator %q was given %d values, the limit is %d", n.Op, len(raw), maxLen)
		return nil, false
	}
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		v, ok := p.jsonScalarValue(n, item, t)
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// jsonScalarValue renders one JSON scalar to text and coerces it to t. Rendering
// to text first is deliberate: it routes every value through the same Coerce the
// URL frontend uses, so there is one place that knows how a uuid or a timestamp
// parses. An object, array or null in scalar position is a shape error.
func (p *parser) jsonScalarValue(n *Node, raw any, t reflect.Type) (any, bool) {
	s, ok := jsonScalar(raw)
	if !ok {
		p.errf(n.Field, n.Op, "value must be a string, number or boolean")
		return nil, false
	}
	if !p.withinLength(s, n.Field, s) {
		return nil, false
	}
	v, err := Coerce(s, t)
	if err != nil {
		p.errf(n.Field, s, "%v", err)
		return nil, false
	}
	return v, true
}

// jsonScalar renders a JSON scalar into the text form Coerce expects. A JSON
// number arrives as json.Number — the decoder is set to UseNumber — so it is
// handed on as the digits the caller sent, which is what makes the tree and the
// URL grammar bind the same value for an int64 above 2^53. The float64 case
// remains for a Node assembled in Go rather than decoded, and renders without an
// exponent because Coerce's integer path needs to see "42", not "4.2e+01".
// Objects, arrays and null have no scalar rendering and are rejected by the
// caller.
func jsonScalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// jsonStrings is the string operands of a leaf's value, which is what the
// bare-date refusal reads. A JSON filter spells a date as a string in both the
// scalar and the list form, and anything else cannot be a date.
func jsonStrings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
