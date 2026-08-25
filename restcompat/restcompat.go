package restcompat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// Level is how a contract delta lands on a client that already exists.
type Level int

const (
	// LevelNeutral: no client is affected. A response field going from nullable
	// to always-present is neutral for a reader that already handled the type.
	LevelNeutral Level = iota
	// LevelAdditive: a new capability. Nothing that a client sends or reads
	// today changes meaning; a client that ignores the addition is unaffected.
	LevelAdditive
	// LevelBreaking: an existing client can break — a request that worked now
	// fails, or a response field it relied on is gone or changed shape.
	LevelBreaking
	// LevelUnknown: the delta is real but its effect depends on how a specific
	// client generated its types (a widened integer overflows a narrow one), so
	// it is surfaced for review rather than claimed safe. Treat it as breaking
	// under a strict gate.
	LevelUnknown
)

func (l Level) String() string {
	switch l {
	case LevelNeutral:
		return "neutral"
	case LevelAdditive:
		return "additive"
	case LevelBreaking:
		return "breaking"
	case LevelUnknown:
		return "unknown"
	default:
		return "?"
	}
}

// Facet names the part of the contract a break sits in. Facets are ordered so
// that a resource-level break sorts above the field-level breaks under it, and
// the schema-level one above every resource.
type Facet string

const (
	FacetWire     Facet = "wire"        // how every column is spelled on the wire
	FacetResource Facet = "resource"    // the endpoint set as a whole
	FacetOps      Facet = "ops"         // which operations exist
	FacetResponse Facet = "response"    // the fields a read returns
	FacetFilter   Facet = "filter"      // ?column=op.value parameters
	FacetSort     Facet = "sort"        // ?sort columns
	FacetExpand   Facet = "expand"      // ?expand relations
	FacetCreate   Facet = "create-body" // the POST body
	FacetPatch    Facet = "patch-body"  // the PATCH body
	FacetAction   Facet = "action"      // a declared domain verb and its body
)

var facetOrder = map[Facet]int{
	FacetWire: 0, FacetResource: 1, FacetOps: 2, FacetResponse: 3,
	FacetFilter: 4, FacetSort: 5, FacetExpand: 6, FacetCreate: 7,
	FacetPatch: 8, FacetAction: 9,
}

// Break is one classified delta between two generated contracts.
type Break struct {
	Level Level
	// Resource is the collection path, e.g. "/posts". It is empty for a
	// schema-level break, which belongs to no one resource because it belongs
	// to all of them.
	Resource string
	Facet    Facet
	Field    string // column or relation name; empty for resource- and ops-level
	Summary  string // one line, in the allow-list voice: what changed, for whom
}

func (b Break) String() string {
	loc := string(b.Facet)
	if b.Field != "" {
		loc += "." + b.Field
	}
	if b.Resource != "" {
		loc = b.Resource + " " + loc
	}
	return fmt.Sprintf("%-8s %-28s %s", b.Level, loc, b.Summary)
}

// Diff reports how the REST contract changes moving from old to new. The result
// is deterministic: sorted by resource, then by facet, then by field. An empty
// result means the contract is byte-for-byte compatible.
//
// This is the convenience form for two registries in hand. `sqlb impact` diffs
// a registry against a checked-in Snapshot instead — see DiffSnapshots — because
// "backward compatible relative to what?" needs a committed answer, not the
// other side of a comparison that only exists at generation time.
func Diff(old, new *schema.Registry) []Break {
	return DiffSnapshots(Capture(old), Capture(new))
}

// DiffSnapshots is Diff over two captured contracts. It is what the CLI runs:
// the old side is read from a file in the repository, the new side is captured
// from the current schema.
func DiffSnapshots(old, new Snapshot) []Break {
	oldR := index(old)
	newR := index(new)

	var breaks []Break
	add := func(b Break) { breaks = append(breaks, b) }

	diffWireCase(old, new, add)

	for _, path := range union(oldR, newR) {
		o, inOld := oldR[path]
		n, inNew := newR[path]
		switch {
		case inOld && !inNew:
			add(Break{LevelBreaking, path, FacetResource, "",
				"resource is no longer exposed; every endpoint under it is gone"})
		case !inOld && inNew:
			add(Break{LevelAdditive, path, FacetResource, "",
				"resource is newly exposed"})
		default:
			diffResource(o, n, add)
		}
	}

	sort.SliceStable(breaks, func(i, j int) bool {
		a, b := breaks[i], breaks[j]
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		if a.Facet != b.Facet {
			return facetOrder[a.Facet] < facetOrder[b.Facet]
		}
		return a.Field < b.Field
	})
	return breaks
}

// Breaking returns only the breaks a strict gate would fail on — the breaking
// ones and the unknowns, which cannot be shown safe. This is what
// `--api-compat=error` would count.
func Breaking(breaks []Break) []Break {
	var out []Break
	for _, b := range breaks {
		if b.Level == LevelBreaking || b.Level == LevelUnknown {
			out = append(out, b)
		}
	}
	return out
}

// diffWireCase reports a change to the schema's declared wire spelling.
//
// It is the break with the widest blast radius and the smallest diff: flipping
// Verbatim to Camel renames *every* field of *every* resource on the wire at
// once, while every column keeps its name, so the field-level comparison below
// — which matches columns by column name — is silent by construction and would
// pass a schema whose entire API was respelled.
//
// One finding rather than one per column. The per-column report would be
// accurate and useless: N lines of "renamed from" with the one edit that caused
// them nowhere in the output. So the finding names the setting, both spellings,
// and one column that moves, which is the shape of the rename.
//
// ADR-0036's amendment is what makes this breaking rather than neutral: what is
// frozen is that there is exactly one spelling and that it is derived, not which
// derivation a deployment chose — "changing a deployment's WireCase is a
// breaking change for that deployment, exactly as renaming a column is"
// (docs/compatibility.md, under Frozen).
func diffWireCase(old, new Snapshot, add func(Break)) {
	if old.WireCase == new.WireCase {
		return
	}
	o, n := schema.WireCase(old.WireCase), schema.WireCase(new.WireCase)
	add(Break{LevelBreaking, "", FacetWire, "", fmt.Sprintf(
		"wire case changed from %s to %s; every field of every resource is respelled at once%s, "+
			"so a deployed client reads and sends names the API no longer has",
		wirePhrase(o), wirePhrase(n), wireExample(new, o, n))})
}

// wireExample names one column whose spelling actually moves under the change,
// so the report shows the rename and not only the setting behind it. It is
// empty when no captured column spells differently in the two cases — a schema
// of single-word columns, where the flip is real and invisible.
func wireExample(s Snapshot, o, n schema.WireCase) string {
	for _, rs := range s.Resources {
		for _, fs := range rs.Fields {
			if was, now := o.WireName(fs.Name), n.WireName(fs.Name); was != now {
				return fmt.Sprintf(" (%s is now %s)", was, now)
			}
		}
	}
	return ""
}

// diffResource compares two contracts for the same path.
func diffResource(o, n resource, add func(Break)) {
	// Operations.
	for _, op := range []schema.Op{schema.OpCreate, schema.OpRead, schema.OpUpdate, schema.OpDelete, schema.OpList, schema.OpSingleton} {
		switch {
		case o.ops.Has(op) && !n.ops.Has(op):
			add(Break{LevelBreaking, n.path, FacetOps, "",
				fmt.Sprintf("operation %s removed", op)})
		case !o.ops.Has(op) && n.ops.Has(op):
			add(Break{LevelAdditive, n.path, FacetOps, "",
				fmt.Sprintf("operation %s added", op)})
		}
	}

	// Match fields, resolving renames first so a rename is one break rather than
	// a spurious drop-and-add.
	matched := map[string]bool{} // old column names consumed by a rename
	for _, nf := range n.order {
		nv := n.fields[nf]
		if nv.renamedFrom == "" {
			continue
		}
		ov, ok := o.fields[nv.renamedFrom]
		if !ok || n.fields[nv.renamedFrom] != nil {
			continue // hint's old name is gone or still present; not a rename here
		}
		matched[nv.renamedFrom] = true
		diffRename(o.path, ov, nv, add)
		diffField(n.path, ov, nv, add) // the renamed column may also have changed
	}

	for _, name := range o.order {
		if matched[name] {
			continue
		}
		ov := o.fields[name]
		if _, ok := n.fields[name]; !ok {
			diffRemoved(n.path, ov, add)
		}
	}
	for _, name := range n.order {
		nv := n.fields[name]
		if nv.renamedFrom != "" && matched[nv.renamedFrom] {
			continue
		}
		if _, ok := o.fields[name]; !ok {
			diffAdded(n.path, nv, add)
			continue
		}
		diffField(n.path, o.fields[name], nv, add)
	}

	diffBodyProps(n.path, FacetCreate, "", o.createInput, n.createInput, add)
	diffActions(n.path, o.actions, n.actions, add)
}

// diffRemoved reports the breaks caused by a column leaving the schema.
func diffRemoved(path string, ov *fieldView, add func(Break)) {
	if ov.inResponse() {
		add(Break{LevelBreaking, path, FacetResponse, ov.name,
			"field removed from responses"})
	}
	if ov.filterable {
		add(Break{LevelBreaking, path, FacetFilter, ov.name,
			"filter removed; a request using it now 400s"})
	}
	if ov.sortable {
		add(Break{LevelBreaking, path, FacetSort, ov.name, "sort key removed"})
	}
	if ov.expandable {
		add(Break{LevelBreaking, path, FacetExpand, ov.relName, "expand relation removed"})
	}
}

// diffAdded reports the breaks and additions caused by a new column.
func diffAdded(path string, nv *fieldView, add func(Break)) {
	if nv.inResponse() {
		add(Break{LevelAdditive, path, FacetResponse, nv.name, "field added to responses"})
	}
	if nv.filterable {
		add(Break{LevelAdditive, path, FacetFilter, nv.name, "filter added"})
	}
	if nv.sortable {
		add(Break{LevelAdditive, path, FacetSort, nv.name, "sort key added"})
	}
	if nv.expandable {
		add(Break{LevelAdditive, path, FacetExpand, nv.relName, "expand relation added"})
	}
	// The one addition that breaks a writer: a required field in the create body.
	if nv.requiredAtCreate() {
		add(Break{LevelBreaking, path, FacetCreate, nv.name,
			"new required field; a create that omits it now fails validation"})
	} else if nv.settableAtCreate() {
		add(Break{LevelAdditive, path, FacetCreate, nv.name, "new optional field accepted"})
	}
}

// diffRename reports the wire break a rename is, even though the migration under
// it is a clean RENAME (ADR-0036: the wire spelling is the column name).
func diffRename(path string, ov, nv *fieldView, add func(Break)) {
	note := fmt.Sprintf("renamed from %q; the migration is a clean RENAME but the wire name changed", ov.name)
	if ov.inResponse() {
		add(Break{LevelBreaking, path, FacetResponse, nv.name, note})
	}
	if ov.filterable {
		add(Break{LevelBreaking, path, FacetFilter, nv.name,
			fmt.Sprintf("renamed from %q; ?%s=… now 400s", ov.name, ov.name)})
	}
	if ov.sortable {
		add(Break{LevelBreaking, path, FacetSort, nv.name,
			fmt.Sprintf("renamed from %q", ov.name)})
	}
}

// diffField compares a column present on both sides (possibly under a new name
// after a rename) and reports capability, visibility, nullability and type
// deltas. Reader-side and writer-side effects are reported separately.
func diffField(path string, o, n *fieldView, add func(Break)) {
	// Visibility.
	switch {
	case o.inResponse() && !n.inResponse():
		add(Break{LevelBreaking, path, FacetResponse, n.name,
			"no longer in the response (now hidden or write-only); readers that selected it lose it"})
	case !o.inResponse() && n.inResponse():
		add(Break{LevelAdditive, path, FacetResponse, n.name, "now returned in responses"})
	}

	// Capabilities.
	capDelta(path, FacetFilter, n.name, o.filterable, n.filterable,
		"filter removed; a request using it now 400s", "filter added", add)
	capDelta(path, FacetSort, n.name, o.sortable, n.sortable,
		"sort key removed", "sort key added", add)
	// A null placement change keeps every request working and answers it in a
	// different order, which is the shape of break a capability diff cannot
	// see: no parameter was removed, so capDelta above is silent. It is
	// breaking rather than neutral because an outstanding cursor was issued
	// under the old placement and is refused under the new one, and because a
	// client that renders "latest first" starts rendering the un-dated rows
	// first without changing a line (#88).
	if o.sortable && n.sortable && o.sortNulls != n.sortNulls {
		add(Break{LevelBreaking, path, FacetSort, n.name, fmt.Sprintf(
			"null placement changed from %s to %s; lists come back in a different order "+
				"and cursors issued under the old one are refused",
			placementPhrase(o.sortNulls), placementPhrase(n.sortNulls))})
	}
	capDelta(path, FacetExpand, n.relName, o.expandable, n.expandable,
		"expand relation removed", "expand relation added", add)
	// A unique FK's Inverse resolves to the target row or null instead of the
	// {items, has_more} collection envelope every other expand relation uses
	// (docs/compatibility.md's carve-out on the Frozen list-envelope entry).
	// Flipping .Unique() on the forward Ref therefore changes the shape of an
	// already-expandable relation without adding or removing it — the case
	// capDelta above cannot see, since both sides stay expandable=true.
	if o.expandable && n.expandable && o.oneToOne != n.oneToOne {
		if n.oneToOne {
			add(Break{LevelBreaking, path, FacetExpand, n.relName,
				"the forward reference became a unique FK; this expansion is now one-to-one and " +
					"returns the target row or null instead of the {items, has_more} collection envelope"})
		} else {
			add(Break{LevelBreaking, path, FacetExpand, n.relName,
				"the forward reference is no longer a unique FK; this expansion reverts to the " +
					"{items, has_more} collection envelope instead of the target row or null"})
		}
	}

	// Writability. ReadOnly and Immutable were captured in the snapshot and
	// never compared, so three writer-side contract breaks passed the gate
	// silently (#68) — the reader side of this function was thorough and the
	// writer side was absent.
	diffWritable(path, o, n, add)

	// Nullability — the reader/writer asymmetry, reported on both sides.
	if o.nullable != n.nullable {
		diffNullable(path, o, n, add)
	}

	// Type.
	if o.typ != n.typ || o.array != n.array || o.size != n.size || !sameEnum(o, n) {
		diffType(path, o, n, add)
	}
}

// diffWritable reports what a change to ReadOnly or Immutable does to the two
// generated request bodies.
//
// The consequences are asymmetric, which is why this is not a capDelta pair:
// leaving a body is always a break for a client that sends the field, but
// *entering* the create body breaks only when the field arrives required — a
// NOT NULL column with no default, which every existing create omits.
func diffWritable(path string, o, n *fieldView, add func(Break)) {
	switch {
	case o.settableAtCreate() && !n.settableAtCreate():
		add(Break{LevelBreaking, path, FacetCreate, n.name,
			"now read-only; it leaves the create body and a request sending it now 422s"})
	case !o.settableAtCreate() && n.settableAtCreate():
		if n.requiredAtCreate() {
			add(Break{LevelBreaking, path, FacetCreate, n.name,
				"no longer read-only and has no default, so it is now required; a create that omits it now fails validation"})
		} else {
			add(Break{LevelAdditive, path, FacetCreate, n.name,
				"no longer read-only; the create body accepts it"})
		}
	}

	switch {
	case o.settableAtPatch() && !n.settableAtPatch():
		add(Break{LevelBreaking, path, FacetPatch, n.name,
			"no longer writable after create; it leaves the patch body and a PATCH naming it now 422s"})
	case !o.settableAtPatch() && n.settableAtPatch():
		add(Break{LevelAdditive, path, FacetPatch, n.name,
			"now writable after create; the patch body accepts it"})
	}
}

func capDelta(path string, facet Facet, field string, was, now bool, offMsg, onMsg string, add func(Break)) {
	switch {
	case was && !now:
		add(Break{LevelBreaking, path, facet, field, offMsg})
	case !was && now:
		add(Break{LevelAdditive, path, facet, field, onMsg})
	}
}

// diffNullable splits a nullability change into its reader and writer effects.
func diffNullable(path string, o, n *fieldView, add func(Break)) {
	if o.nullable && !n.nullable {
		// null -> not null.
		if n.inResponse() {
			add(Break{LevelNeutral, path, FacetResponse, n.name,
				"no longer null in responses; readers that handled the type are unaffected"})
		}
		// Writer: only breaking if it actually becomes required (no default).
		if n.requiredAtCreate() && !o.requiredAtCreate() {
			add(Break{LevelBreaking, path, FacetCreate, n.name,
				"now required in the create body; a create that omits it now fails validation"})
		}
		return
	}
	// not null -> null.
	if n.inResponse() {
		add(Break{LevelBreaking, path, FacetResponse, n.name,
			"may now be null; a non-nullable client type breaks on null"})
	}
	if o.requiredAtCreate() && !n.requiredAtCreate() {
		add(Break{LevelAdditive, path, FacetCreate, n.name,
			"now optional in the create body"})
	}
}

// diffType classifies a type change. Enum value sets, scalar/array shape, and
// integer/text width are handled explicitly; anything else is reported unknown
// rather than guessed, because a wrong "neutral" is the failure ADR-0016 names.
func diffType(path string, o, n *fieldView, add func(Break)) {
	// Scalar <-> array is breaking both ways: the JSON shape changes.
	if o.array != n.array {
		add(Break{LevelBreaking, path, FacetResponse, n.name,
			"changed between a scalar and an array; the JSON shape is different"})
		return
	}

	// Enum value set.
	if o.typ == schema.TypeEnum && n.typ == schema.TypeEnum {
		gained := minus(n.enumValues, o.enumValues)
		lost := minus(o.enumValues, n.enumValues)
		if len(gained) > 0 && n.inResponse() {
			add(Break{LevelBreaking, path, FacetResponse, n.name,
				fmt.Sprintf("enum gained %s; a client's closed union rejects the new value", quote(gained))})
		}
		if len(lost) > 0 {
			facet := FacetCreate
			if n.filterable {
				facet = FacetFilter
			}
			add(Break{LevelBreaking, path, facet, n.name,
				fmt.Sprintf("enum dropped %s; input that used it now 422s", quote(lost))})
		}
		return
	}

	// Integer and text width.
	if lvl, msg, ok := widthChange(o, n); ok {
		add(Break{lvl, path, FacetResponse, n.name, msg})
		return
	}

	// Everything else: a family change we do not model. Do not guess.
	add(Break{LevelUnknown, path, FacetResponse, n.name,
		fmt.Sprintf("type changed from %s to %s; classify by hand", o.typ, n.typ)})
}

// widthChange classifies a same-family widening or narrowing. Widening is
// reported unknown, not neutral: a reader with a narrower generated integer
// overflows on a value the wider server type now permits (ADR-0039's int4->int8
// case). Narrowing is breaking on the write side.
func widthChange(o, n *fieldView) (Level, string, bool) {
	if or, ok := intRank(o.typ); ok {
		if nr, ok := intRank(n.typ); ok {
			switch {
			case nr > or:
				return LevelUnknown, fmt.Sprintf(
					"widened %s->%s; a client with a narrower integer type may overflow", o.typ, n.typ), true
			case nr < or:
				return LevelBreaking, fmt.Sprintf(
					"narrowed %s->%s; a value that fit before is now rejected", o.typ, n.typ), true
			}
		}
	}
	if or, ok := floatRank(o.typ); ok {
		if nr, ok := floatRank(n.typ); ok {
			switch {
			case nr > or:
				return LevelUnknown, fmt.Sprintf(
					"widened %s->%s; a client with a narrower float type may overflow", o.typ, n.typ), true
			case nr < or:
				return LevelBreaking, fmt.Sprintf(
					"narrowed %s->%s; a value that fit before is now rejected", o.typ, n.typ), true
			}
		}
	}
	if ow, ok := textWidth(o); ok {
		if nw, ok := textWidth(n); ok {
			switch {
			case nw > ow:
				return LevelAdditive, "text length widened; accepts longer values", true
			case nw < ow:
				return LevelBreaking, "text length narrowed; a value that fit before is now rejected", true
			}
		}
	}
	return 0, "", false
}

// --- snapshot -----------------------------------------------------------------

// SnapshotVersion is the format version of a captured contract. It is stamped
// into every Snapshot so a future format change can be recognised rather than
// misread — the snapshot is a checked-in artefact, and ADR-0039 flags its format
// as the expensive thing to change once teams have committed one. This format is
// still experimental and may change before it is frozen.
const SnapshotVersion = 1

// Snapshot is the serialisable REST contract of a schema: one entry per exposed
// resource, holding exactly the capabilities a client couples to and nothing
// about storage. It is what `sqlb impact -write` records and what a later run
// diffs against.
type Snapshot struct {
	Version int `json:"version"`
	// WireCase is the schema's declared wire spelling (ADR-0036), recorded once
	// for the whole snapshot because that is what it is a property of. It is
	// absent when the schema is Verbatim, so a baseline recorded before this
	// field existed is byte-identical to one recorded after it, and every
	// committed restcontract.json stays valid without re-recording.
	//
	// Absent therefore reads as Verbatim rather than as "not recorded". That
	// costs a schema which *already* declared Camel one spurious break the first
	// time it is checked, and a re-record clears it. It is the safe direction:
	// reading absence as unknown would suppress the Verbatim -> Camel finding
	// against exactly the baselines that predate this check, which is the state
	// this field exists to stop being silent.
	WireCase  string         `json:"wire_case,omitempty"`
	Resources []ResourceSnap `json:"resources"`
}

// ResourceSnap is one exposed table's contract.
type ResourceSnap struct {
	Path   string      `json:"path"`
	Ops    []string    `json:"ops"` // create, read, update, delete, list
	Fields []FieldSnap `json:"fields"`
	// Actions are the declared domain verbs. A snapshot recorded before they
	// existed has none, which reads correctly: every verb in the new schema is
	// an addition.
	Actions []ActionSnap `json:"actions,omitempty"`
	// CreateInput is the create body's declared properties that are not columns
	// (#309). They are part of the contract for the reason the columns are: a
	// deployed client sends this body, and adding a required property to it
	// fails every request that client already makes.
	//
	// Recorded under its own key rather than as more fields, because it is not
	// a column and nothing else about the field list is true of it — it is not
	// in a response, not filterable, and not something a rename could reach.
	CreateInput []BodyPropSnap `json:"create_input,omitempty"`
}

// FieldSnap is one column's contract-relevant shape. Storage-only properties —
// the primary-key flag, the index list, the constraint names — are deliberately
// absent: they do not change how a client couples to the field.
type FieldSnap struct {
	Name string `json:"name"`
	Rel  string `json:"rel,omitempty"`
	// Type is omitempty because an inverse-relation entry (see the loop in
	// Capture that appends those) has none — it is a relation to another
	// table, not a scalar column — and every real column's Type is always
	// non-empty, so nothing that used to be recorded stops being recorded.
	Type        string   `json:"type,omitempty"`
	Array       bool     `json:"array,omitempty"`
	Size        int      `json:"size,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Nullable    bool     `json:"nullable,omitempty"`
	HasDefault  bool     `json:"has_default,omitempty"`
	Hidden      bool     `json:"hidden,omitempty"`
	WriteOnly   bool     `json:"write_only,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	Immutable   bool     `json:"immutable,omitempty"`
	Filterable  bool     `json:"filterable,omitempty"`
	Sortable    bool     `json:"sortable,omitempty"`
	SortNulls   string   `json:"sort_nulls,omitempty"`
	Expandable  bool     `json:"expandable,omitempty"`
	RenamedFrom string   `json:"renamed_from,omitempty"`
	// OneToOne marks a reverse (Inverse) relation whose forward Ref carries a
	// single-column unique constraint, so ?expand resolves it to the target row
	// or null rather than the {items, has_more} collection envelope every other
	// expand relation uses. Meaningless — and always false — on anything but an
	// inverse-relation entry; see the loop in Capture that appends those.
	OneToOne bool `json:"one_to_one,omitempty"`
}

// canonicalOps is the fixed order operations are captured and compared in, so a
// snapshot's op list is stable and a diff reads them in a predictable order.
var canonicalOps = []struct {
	op   schema.Op
	name string
}{
	{schema.OpCreate, "create"}, {schema.OpRead, "read"}, {schema.OpUpdate, "update"},
	{schema.OpDelete, "delete"}, {schema.OpList, "list"}, {schema.OpSingleton, "singleton"},
}

// Capture projects a registry into its serialisable REST contract. It is the
// same projection Diff uses, exposed so the CLI can record and re-read it.
// Resources are sorted by path so a re-record produces a minimal file diff.
func Capture(r *schema.Registry) Snapshot {
	s := Snapshot{Version: SnapshotVersion}
	if r == nil {
		return s
	}
	s.WireCase = string(r.Wire())
	for _, t := range r.Tables() {
		rest := t.Rest()
		if rest == nil {
			continue
		}
		path := rest.Path
		if path == "" {
			path = "/" + t.LocalName()
		}
		res := ResourceSnap{Path: path}
		for _, c := range canonicalOps {
			if rest.Ops.Has(c.op) {
				res.Ops = append(res.Ops, c.name)
			}
		}
		if rest.Ops.Has(schema.OpCreate) {
			res.CreateInput = captureBodyProps(rest.CreateInput)
		}
		for _, f := range t.Fields() {
			d := f.Desc()
			fs := FieldSnap{
				Name:        d.Name,
				Type:        string(d.Type),
				Array:       d.Array,
				Size:        d.Size,
				Enum:        d.EnumValues,
				Nullable:    d.Nullable,
				HasDefault:  d.DatabaseSupplied(),
				Hidden:      d.Hidden,
				WriteOnly:   d.WriteOnly,
				ReadOnly:    d.ReadOnly,
				Immutable:   d.Immutable,
				Filterable:  d.Filterable,
				Sortable:    d.Sortable,
				SortNulls:   string(d.SortNulls),
				Expandable:  d.Expandable,
				RenamedFrom: d.RenamedFrom,
			}
			if d.Ref != nil {
				fs.Rel = d.Ref.Name
			}
			res.Fields = append(res.Fields, fs)
		}
		// The reverse side of every Ref that named an Inverse and points at t:
		// the field the target gains, not the column the source declares.
		// Captured here, on t's own resource, because that is where a caller
		// actually observes it — GET t.Path?expand=<name> — the same reason
		// expandableRelations (codegen/models.go) walks Inverses(t) rather than
		// leaving the reverse relation implicit in the source's Ref.
		//
		// Only an Expandable one, matching inversesOf and expandableRelations
		// (codegen/models.go, "A declared-but-unexposed inverse names the
		// relationship for the manifest and stops there"): a bare Inverse(name)
		// with no InverseExpandable never emits a Go field, never exposes an
		// ?expand parameter, and never changes the wire — it is a name in the
		// manifest, not a contract. Capturing it anyway made removing one look
		// like a breaking "field removed from responses", for a field the
		// generated response never had.
		//
		// ReadOnly is set unconditionally: an expand relation is never a
		// create- or patch-body field, and leaving it false would make
		// diffAdded misclassify a newly-declared Inverse as a required create
		// field the moment it is captured, which it never is.
		for _, inv := range r.Inverses(t) {
			if !inv.Expandable {
				continue
			}
			res.Fields = append(res.Fields, FieldSnap{
				Name:       inv.Name,
				Rel:        inv.Name,
				ReadOnly:   true,
				Expandable: inv.Expandable,
				OneToOne:   inv.OneToOne,
			})
		}
		res.Actions = captureActions(t, path)
		s.Resources = append(s.Resources, res)
	}
	sort.Slice(s.Resources, func(i, j int) bool {
		return s.Resources[i].Path < s.Resources[j].Path
	})
	return s
}

// --- projection ---------------------------------------------------------------

// resource is the internal, comparison-ready form of one resource's contract:
// its fields indexed by name, with the op set decoded back to a mask.
type resource struct {
	path   string
	ops    schema.Op
	order  []string // column names, in declaration order, for stable output
	fields map[string]*fieldView
	// actions are the declared verbs, by name. Unlike a column, a verb has no
	// separate comparison form: the snapshot shape is already exactly what the
	// diff reads.
	actions map[string]ActionSnap
	// createInput is the create body's non-column half, in declaration order —
	// the snapshot's own shape, for the reason actions keeps it.
	createInput []BodyPropSnap
}

// fieldView is the contract-relevant view of one column. It carries only what
// decides how a client couples to the field, not how it is stored.
type fieldView struct {
	name       string
	relName    string // relation name for a reference (?expand target); else ""
	typ        schema.Type
	array      bool
	size       int
	enumValues []string

	nullable   bool
	hasDefault bool

	hidden     bool
	writeOnly  bool
	readOnly   bool
	immutable  bool
	filterable bool
	sortable   bool
	sortNulls  string
	expandable bool
	// oneToOne is only ever true on an inverse-relation entry (see the
	// Inverses loop in Capture); zero on every real column, so comparing it
	// is a no-op wherever a shape change is not the thing that happened.
	oneToOne bool

	// renamedFrom is not a property of the contract but the hint that matches
	// this field to its old name across the diff. Kept on the view so the
	// matcher has it without a second lookup.
	renamedFrom string
}

func (f *fieldView) inResponse() bool       { return !f.hidden && !f.writeOnly }
func (f *fieldView) settableAtCreate() bool { return !f.readOnly }

// settableAtPatch is the create rule plus Immutable, which is exactly what
// Immutable means: writable once, at create, and never again.
func (f *fieldView) settableAtPatch() bool { return !f.readOnly && !f.immutable }

func (f *fieldView) requiredAtCreate() bool {
	return f.settableAtCreate() && !f.nullable && !f.hasDefault
}

// index turns a Snapshot into the by-path, by-name form the comparison walks.
func index(s Snapshot) map[string]resource {
	out := map[string]resource{}
	for _, rs := range s.Resources {
		res := resource{path: rs.Path, fields: map[string]*fieldView{}, actions: map[string]ActionSnap{}}
		for _, name := range rs.Ops {
			for _, c := range canonicalOps {
				if c.name == name {
					res.ops |= c.op
				}
			}
		}
		for _, fs := range rs.Fields {
			fv := &fieldView{
				name:        fs.Name,
				relName:     fs.Rel,
				typ:         schema.Type(fs.Type),
				array:       fs.Array,
				size:        fs.Size,
				enumValues:  fs.Enum,
				nullable:    fs.Nullable,
				hasDefault:  fs.HasDefault,
				hidden:      fs.Hidden,
				writeOnly:   fs.WriteOnly,
				readOnly:    fs.ReadOnly,
				immutable:   fs.Immutable,
				filterable:  fs.Filterable,
				sortable:    fs.Sortable,
				sortNulls:   fs.SortNulls,
				expandable:  fs.Expandable,
				oneToOne:    fs.OneToOne,
				renamedFrom: fs.RenamedFrom,
			}
			res.order = append(res.order, fs.Name)
			res.fields[fs.Name] = fv
		}
		for _, as := range rs.Actions {
			res.actions[as.Name] = as
		}
		res.createInput = rs.CreateInput
		out[rs.Path] = res
	}
	return out
}

// --- helpers ------------------------------------------------------------------

func union(a, b map[string]resource) []string {
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

func intRank(t schema.Type) (int, bool) {
	switch t {
	case schema.TypeSmallInt:
		return 1, true
	case schema.TypeInt:
		return 2, true
	case schema.TypeBigInt:
		return 3, true
	}
	return 0, false
}

// floatRank is intRank for the binary float family.
//
// It is separate rather than one ranking over every numeric type, because a
// change between the families is not a width change at all: real to numeric
// swaps an approximate type for an exact one, which changes what the same
// arithmetic returns and is exactly the "classify by hand" case diffType falls
// through to. Within the family the ordinary width argument holds — widening
// admits values a narrower client overflows on, narrowing rejects values that
// fit before.
func floatRank(t schema.Type) (int, bool) {
	switch t {
	case schema.TypeReal:
		return 1, true
	case schema.TypeFloat:
		return 2, true
	}
	return 0, false
}

func textWidth(f *fieldView) (int, bool) {
	switch f.typ {
	case schema.TypeText:
		return 1 << 30, true
	case schema.TypeVarchar:
		if f.size == 0 {
			return 1 << 30, true
		}
		return f.size, true
	}
	return 0, false
}

func sameEnum(o, n *fieldView) bool {
	if o.typ != schema.TypeEnum || n.typ != schema.TypeEnum {
		return true // not both enums; type equality is checked elsewhere
	}
	return len(minus(o.enumValues, n.enumValues)) == 0 && len(minus(n.enumValues, o.enumValues)) == 0
}

// minus returns the elements of a not present in b.
func minus(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range b {
		set[v] = true
	}
	var out []string
	for _, v := range a {
		if !set[v] {
			out = append(out, v)
		}
	}
	return out
}

func quote(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(q, ", ")
}

// wirePhrase names a wire case for a diff line, including the default one. The
// zero value is the empty string, and a break reading "changed from  to camel"
// is a break the reader has to decode, so Verbatim gets a word too.
func wirePhrase(c schema.WireCase) string {
	switch c {
	case schema.Verbatim:
		return "verbatim"
	case schema.Camel:
		return "camel"
	default:
		return string(c)
	}
}

// placementPhrase names a null placement for a diff line, including the default
// one — a break that reads "changed from  to nulls last" is a break a reader
// has to decode, so the empty declaration gets words too.
func placementPhrase(n string) string {
	switch schema.Nulls(n) {
	case schema.NullsFirst:
		return "nulls first"
	case schema.NullsLast:
		return "nulls last"
	default:
		return "the Postgres default (nulls last ascending, nulls first descending)"
	}
}
