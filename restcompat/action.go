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
	Body []PropSnap `json:"body,omitempty"`
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

// PropSnap is one declared property of a request: a property of an action's
// body, or a parameter of a query. The two are one type because they are one
// shape — a name, a type, and the three flags that decide whether a request
// must carry it — and because the rules that classify a change to one are the
// rules that classify a change to the other; see propKind.
type PropSnap struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Enum       []string `json:"enum,omitempty"`
	Nullable   bool     `json:"nullable,omitempty"`
	HasDefault bool     `json:"has_default,omitempty"`
}

// required reports whether a request must carry the property. It is the create
// body's rule: a nullable property may be absent as null, and a defaulted one
// may be absent so the default applies.
func (p PropSnap) required() bool { return !p.Nullable && !p.HasDefault }

// captureActions projects a table's verbs into their contract form.
func captureActions(t *schema.TableDef, path string) []ActionSnap {
	var out []ActionSnap
	for _, a := range t.Actions() {
		snap := ActionSnap{Name: a.Name, Path: a.FullPath(path), Writes: a.Writes, Touches: a.Touches}
		for _, f := range a.Body {
			d := f.Desc()
			snap.Body = append(snap.Body, PropSnap{
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
	// diff in the recorded contract.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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

// actionBody is the vocabulary diffProps uses for an action's request body.
var actionBody = propKind{
	facet: FacetAction,
	noun:  "body property",
	// The property is gone. A client still sending it is now sending something
	// the operation does not declare.
	removed: "body property removed",
}

// propKind is the vocabulary one property-bearing surface uses to describe its
// own properties to a reader.
//
// The classification below it is identical for an action's request body and a
// query's parameters: the same rule for whether a request must carry the
// property, the same rule for a type change, the same rule for a narrowed enum.
// Only the words differ. So the rules live once and the words are passed in —
// which is the point, because the next fix to one of those rules would
// otherwise have to be made twice and would be made once (#68 was that fix,
// and it landed when there was only one caller to make it in).
type propKind struct {
	facet Facet
	// noun names one property in a summary: "body property", "parameter".
	noun string
	// removed is the whole summary for a property that is gone, rather than a
	// suffix on noun, because it is the one case where the two surfaces differ
	// in substance and not only in wording — see queryParam.
	removed string
}

// diffActionBody compares two versions of one verb's request body.
func diffActionBody(path, action string, o, n ActionSnap, add func(Break)) {
	diffProps(path, action, actionBody, o.Body, n.Body, add)
}

// diffProps classifies the change to one operation's declared properties.
//
// owner is the action or query the properties belong to; it prefixes the field
// name so a break reads as complete.note rather than as note, which matters
// once a resource declares more than one operation with a property of the same
// name.
func diffProps(path, owner string, kind propKind, oldSide, newSide []PropSnap, add func(Break)) {
	oldProps := propsByName(oldSide)
	newProps := propsByName(newSide)

	for _, name := range unionProps(oldProps, newProps) {
		field := owner + "." + name
		op, inOld := oldProps[name]
		np, inNew := newProps[name]
		switch {
		case inOld && !inNew:
			add(Break{LevelBreaking, path, kind.facet, field, kind.removed})
		case !inOld && inNew:
			if np.required() {
				add(Break{LevelBreaking, path, kind.facet, field,
					"required " + kind.noun + " added; every existing request omits it"})
				continue
			}
			add(Break{LevelAdditive, path, kind.facet, field, "optional " + kind.noun + " added"})
		default:
			// Sequential, not a switch. A property whose enum narrows *and*
			// whose requiredness relaxes is two deltas, and a switch reported
			// only the first arm it matched — the additive half, which is the
			// one that does not need reporting (#68). The field path beside
			// this one has always used sequential ifs; this is it agreeing.
			if op.Type != np.Type {
				add(Break{LevelBreaking, path, kind.facet, field,
					fmt.Sprintf("%s type changed from %s to %s", kind.noun, op.Type, np.Type)})
			}
			if !op.required() && np.required() {
				add(Break{LevelBreaking, path, kind.facet, field,
					kind.noun + " became required"})
			}
			if op.required() && !np.required() {
				add(Break{LevelAdditive, path, kind.facet, field,
					kind.noun + " became optional"})
			}
			if len(np.Enum) > 0 && !sameStrings(op.Enum, np.Enum) {
				// Narrowing the accepted set rejects a value that used to work;
				// widening it does not. Reported as one delta either way, because
				// the enum is a set and the honest summary names both spellings.
				add(Break{levelForEnum(op.Enum, np.Enum), path, kind.facet, field,
					fmt.Sprintf("%s values changed from %v to %v", kind.noun, op.Enum, np.Enum)})
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

func propsByName(props []PropSnap) map[string]PropSnap {
	out := make(map[string]PropSnap, len(props))
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

func unionProps(a, b map[string]PropSnap) []string {
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
