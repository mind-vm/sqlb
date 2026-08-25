package restcompat

import (
	"fmt"
	"sort"

	"github.com/jryannel/sqlb/schema"
)

// A declared action is part of the REST contract, so it is part of this diff
// (ADR-0043).
//
// The reason it has to be is the same reason a column is: a deployed client
// has a named method calling POST /tasks/{id}/complete, and withdrawing the
// route or tightening its body breaks that client with no DDL anywhere in
// sight — which is the whole premise of `sqlb impact` (ADR-0039).
//
// What is *not* diffed here is the verb. The func the envelope calls is
// application code; it can change on every commit without the contract moving,
// and a tool that claimed otherwise would report a break every time somebody
// fixed a bug.

// ActionSnap is one declared verb's contract.
type ActionSnap struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Body is the request body's properties, in declaration order.
	Body []BodyPropSnap `json:"body,omitempty"`
	// Returns is the declared response body, in declaration order. Absent means
	// the default answer — the row for an item verb, nothing for a collection
	// one — and moving between the two is a change of response type, which is
	// why it is diffed as a whole and not only property by property.
	Returns []BodyPropSnap `json:"returns,omitempty"`
	// Writes names the columns the envelope persists. No client couples to it,
	// so a change here is neutral — but it widens or narrows what one route
	// can mutate, which is exactly the blast-radius question this tool is for.
	Writes []string `json:"writes,omitempty"`
	// Touches names the tables the verb writes through its transaction, as
	// declared. Also neutral, and for a sharper reason than Writes: the
	// declaration is unenforced, so a change here is a change in what the route
	// *claims* — which is the only thing a diff can see, and the thing a
	// reviewer most wants shown when a verb's reach grows.
	Touches []string `json:"touches,omitempty"`
}

// BodyPropSnap is one declared property of a request body — an action's, or the
// non-column half of a create's (#309).
//
// It was ActionPropSnap until a create body could declare one too; the alias
// below keeps a baseline reader compiling, and the JSON is unchanged either way.
type BodyPropSnap struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Enum       []string `json:"enum,omitempty"`
	Nullable   bool     `json:"nullable,omitempty"`
	HasDefault bool     `json:"has_default,omitempty"`
}

// ActionPropSnap is the former name of [BodyPropSnap].
//
// Deprecated: use BodyPropSnap.
type ActionPropSnap = BodyPropSnap

// required reports whether a request must carry the property. It is the create
// body's rule: a nullable property may be absent as null, and a defaulted one
// may be absent so the default applies.
func (p BodyPropSnap) required() bool { return !p.Nullable && !p.HasDefault }

// captureActions projects a table's verbs into their contract form.
func captureActions(t *schema.TableDef, path string) []ActionSnap {
	var out []ActionSnap
	for _, a := range t.Actions() {
		snap := ActionSnap{
			Name: a.Name, Path: a.FullPath(path),
			Body:    captureBodyProps(a.Body),
			Returns: captureBodyProps(a.Returns),
			Writes:  a.Writes, Touches: a.Touches,
		}
		out = append(out, snap)
	}
	// Sorted, so that reordering the declarations in a schema file is not a
	// diff in the recorded contract.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// captureBodyProps projects a declared body into its contract form, in
// declaration order. One function for both declarations that have a body, so
// that the two cannot record the same property differently.
func captureBodyProps(body []*schema.Field) []BodyPropSnap {
	var out []BodyPropSnap
	for _, f := range body {
		d := f.Desc()
		out = append(out, BodyPropSnap{
			Name:       d.Name,
			Type:       string(d.Type),
			Enum:       d.EnumValues,
			Nullable:   d.Nullable,
			HasDefault: d.DatabaseSupplied(),
		})
	}
	return out
}

// diffActions compares the verb sets of two contracts for the same path.
func diffActions(path string, o, n map[string]ActionSnap, add func(Break)) {
	for _, name := range unionActions(o, n) {
		ov, inOld := o[name]
		nv, inNew := n[name]
		switch {
		case inOld && !inNew:
			add(Break{LevelBreaking, path, FacetAction, name,
				fmt.Sprintf("action removed; POST %s now 404s", ov.Path)})
			continue
		case !inOld && inNew:
			add(Break{LevelAdditive, path, FacetAction, name, "action added"})
			continue
		}

		// A moved route is a removed one as far as a deployed client is
		// concerned: it holds the old URL, not the name this diff matched on.
		if ov.Path != nv.Path {
			add(Break{LevelBreaking, path, FacetAction, name,
				fmt.Sprintf("action moved from %s to %s; a deployed client holds the old URL", ov.Path, nv.Path)})
		}
		diffActionBody(path, name, ov, nv, add)
		diffActionResult(path, name, ov, nv, add)

		if !sameStrings(ov.Writes, nv.Writes) {
			add(Break{LevelNeutral, path, FacetAction, name,
				fmt.Sprintf("write set changed from %v to %v; no client breaks, but the route now touches different columns",
					ov.Writes, nv.Writes)})
		}
		if !sameStrings(ov.Touches, nv.Touches) {
			add(Break{LevelNeutral, path, FacetAction, name,
				fmt.Sprintf("declared reach changed from %v to %v; no client breaks, but the route claims to write different tables",
					ov.Touches, nv.Touches)})
		}
	}
}

// diffActionBody compares two versions of one verb's request body.
func diffActionBody(path, action string, o, n ActionSnap, add func(Break)) {
	diffBodyProps(path, FacetAction, action+".", o.Body, n.Body, add)
}

// diffActionResult compares what two versions of one verb answer with.
//
// It is a separate function from the request side rather than the same one
// under a flag, because every classification is inverted and a boolean
// parameter whose only job is "invert this" reads as one rule when it is two: a
// property leaving a *request* breaks the client that sends it, and one leaving
// a *response* breaks the client that reads it; a widened value set is safe to
// send and unsafe to receive.
func diffActionResult(path, action string, o, n ActionSnap, add func(Break)) {
	// Acquiring or losing a declared result changes the response *type*, not
	// one of its properties. A client holding the old signature gets the row
	// where it expects a score, or nothing where it expects a row.
	switch {
	case len(o.Returns) == 0 && len(n.Returns) > 0:
		add(Break{LevelBreaking, path, FacetAction, action,
			"now answers with a declared result instead of the row or 204; the client's return type changes"})
		return
	case len(o.Returns) > 0 && len(n.Returns) == 0:
		add(Break{LevelBreaking, path, FacetAction, action,
			"no longer answers with a declared result; the client's return type changes"})
		return
	}

	oldProps := propsByName(o.Returns)
	newProps := propsByName(n.Returns)
	for _, name := range unionProps(oldProps, newProps) {
		field := action + ".result." + name
		op, inOld := oldProps[name]
		np, inNew := newProps[name]
		switch {
		case inOld && !inNew:
			add(Break{LevelBreaking, path, FacetAction, field, "result property removed"})
			continue
		case !inOld && inNew:
			add(Break{LevelAdditive, path, FacetAction, field, "result property added"})
			continue
		}
		if op.Type != np.Type {
			add(Break{LevelBreaking, path, FacetAction, field,
				fmt.Sprintf("result property type changed from %s to %s", op.Type, np.Type)})
		}
		if !op.Nullable && np.Nullable {
			add(Break{LevelBreaking, path, FacetAction, field,
				"result property may now be null; a non-nullable client type breaks on null"})
		}
		if op.Nullable && !np.Nullable {
			add(Break{LevelNeutral, path, FacetAction, field,
				"result property is no longer null; readers that handled the type are unaffected"})
		}
		if len(np.Enum) > 0 && !sameStrings(op.Enum, np.Enum) {
			// The mirror image of the request side: a value the response may
			// now carry is one a closed client type has no case for, and one it
			// no longer carries breaks nothing.
			add(Break{levelForEnum(np.Enum, op.Enum), path, FacetAction, field,
				fmt.Sprintf("result property values changed from %v to %v", op.Enum, np.Enum)})
		}
	}
}

// diffBodyProps compares two versions of one declared request body, whichever
// declaration it came from.
//
// prefix qualifies the field name in the report: an action's properties are
// reported under "complete.note", and a create's under the bare property name,
// because a resource has one create body and does not need it qualified.
func diffBodyProps(path string, facet Facet, prefix string, oldBody, newBody []BodyPropSnap, add func(Break)) {
	oldProps := propsByName(oldBody)
	newProps := propsByName(newBody)

	for _, name := range unionProps(oldProps, newProps) {
		field := prefix + name
		op, inOld := oldProps[name]
		np, inNew := newProps[name]
		switch {
		case inOld && !inNew:
			// The property is gone. A client still sending it is now sending
			// something the operation does not declare.
			add(Break{LevelBreaking, path, facet, field,
				"body property removed"})
		case !inOld && inNew:
			if np.required() {
				add(Break{LevelBreaking, path, facet, field,
					"required body property added; every existing request omits it"})
				continue
			}
			add(Break{LevelAdditive, path, facet, field, "optional body property added"})
		default:
			// Sequential, not a switch. A property whose enum narrows *and*
			// whose requiredness relaxes is two deltas, and a switch reported
			// only the first arm it matched — the additive half, which is the
			// one that does not need reporting (#68). The field path beside
			// this one has always used sequential ifs; this is it agreeing.
			if op.Type != np.Type {
				add(Break{LevelBreaking, path, facet, field,
					fmt.Sprintf("body property type changed from %s to %s", op.Type, np.Type)})
			}
			if !op.required() && np.required() {
				add(Break{LevelBreaking, path, facet, field,
					"body property became required"})
			}
			if op.required() && !np.required() {
				add(Break{LevelAdditive, path, facet, field,
					"body property became optional"})
			}
			if len(np.Enum) > 0 && !sameStrings(op.Enum, np.Enum) {
				// Narrowing the accepted set rejects a value that used to work;
				// widening it does not. Reported as one delta either way, because
				// the enum is a set and the honest summary names both spellings.
				add(Break{levelForEnum(op.Enum, np.Enum), path, facet, field,
					fmt.Sprintf("body property values changed from %v to %v", op.Enum, np.Enum)})
			}
		}
	}
}

// levelForEnum classifies a change to an accepted value set: strictly adding
// values is additive, anything else can reject a request that worked.
func levelForEnum(old, new []string) Level {
	have := make(map[string]bool, len(new))
	for _, v := range new {
		have[v] = true
	}
	for _, v := range old {
		if !have[v] {
			return LevelBreaking
		}
	}
	return LevelAdditive
}

func propsByName(props []BodyPropSnap) map[string]BodyPropSnap {
	out := make(map[string]BodyPropSnap, len(props))
	for _, p := range props {
		out[p.Name] = p
	}
	return out
}

func unionActions(a, b map[string]ActionSnap) []string {
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

func unionProps(a, b map[string]BodyPropSnap) []string {
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

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
