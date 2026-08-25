package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestVersion is bumped when the manifest shape changes incompatibly.
const ManifestVersion = "1"

// Manifest is a machine-readable description of a schema: every table, every
// column, and — the part that matters most — exactly what a client may filter,
// sort, search and expand on each exposed resource.
//
// It reports capabilities that work, not capabilities that are declared. The
// two coincide today; where they ever diverge again, this file is what has to
// keep telling the truth.
//
// It exists because reading a Go DSL to answer "what can I query here?" is a
// poor interface for a program. The manifest answers it directly, in one file,
// with worked example requests. Emit it next to the generated code and point
// tooling at it.
type Manifest struct {
	Version string `json:"version"`
	Module  string `json:"module,omitempty"`

	// WireCase is the schema's declared [WireCase], absent when the schema is
	// Verbatim and a column's wire name is its own name.
	//
	// It is here so that a consumer holding only this document can *compute* a
	// spelling rather than assume one. Every wire name the manifest reports is
	// already converted — the REST capability lists, the examples, and
	// [ColumnManifest.Wire] — so nothing has to call [WireCase.WireName] to read
	// the manifest correctly. What this field buys is the sentence a renderer
	// writing prose about the mapping needs: "there is no mapping layer" is true
	// under Verbatim and false under Camel, and a renderer with no way to tell
	// them apart writes the wrong one (#143).
	WireCase string `json:"wireCase,omitempty"`

	Tables    []TableManifest `json:"tables"`
	Operators []OperatorDoc   `json:"filterOperators"`
	Params    []ParamDoc      `json:"reservedParams"`
}

// TableManifest describes one table.
type TableManifest struct {
	Name       string           `json:"name"`
	Module     string           `json:"module,omitempty"`
	LocalName  string           `json:"localName,omitempty"`
	Comment    string           `json:"comment,omitempty"`
	PrimaryKey string           `json:"primaryKey,omitempty"`
	Columns    []ColumnManifest `json:"columns"`
	Indexes    []IndexManifest  `json:"indexes,omitempty"`

	// CollectedBy describes the reverse relations pointing at this table: the
	// rows of another table that this one collects, and the name it knows them
	// by. Declared on the referencing side, which is where the column and the
	// constraint already live, so reading this table alone would otherwise not
	// show that its endpoint has them. ADR-0022.
	CollectedBy []InverseManifest `json:"collectedBy,omitempty"`

	REST *RESTManifest `json:"rest,omitempty"`
}

// InverseManifest describes one reverse relation from the target's side.
type InverseManifest struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Column string `json:"column"`
	// Order is the column an expansion sorts the collected rows by, with a
	// leading "-" for descending. Empty means the primary key. Meaningless
	// when OneToOne, since there is at most one row to order.
	Order string `json:"order,omitempty"`
	// Limit is how many rows one expansion returns at most, with the default
	// already resolved: a client reading this is never left to guess the cap.
	// Past it the response reports has_more and the caller pages the collected
	// table's own endpoint by Column. Omitted when OneToOne: a unique FK's
	// reverse relation returns the one row or null, never a capped envelope,
	// so a limit would misdescribe it as the collection it is not.
	Limit int `json:"limit,omitempty"`
	// OneToOne reports that the foreign key backing this relation is unique,
	// so an expansion returns one row or null rather than the capped
	// {items, has_more} envelope every other reverse relation uses. See
	// InverseRelation.OneToOne and ADR-0060.
	OneToOne bool `json:"oneToOne,omitempty"`
	// Expandable reports whether ?expand on this table may ask for it. A
	// relation that is named but not expandable is still described here,
	// because the relationship exists whether or not this endpoint serves it.
	Expandable bool `json:"expandable"`
}

// ColumnManifest describes one column. Hidden columns are omitted entirely
// rather than listed as hidden: the manifest is publishable, and a name is
// itself information.
type ColumnManifest struct {
	Name string `json:"name"`

	// Wire is how this column is spelled on the wire, present only when the
	// schema's WireCase makes it differ from Name. Absent means the two are the
	// same, which is every column of a Verbatim schema.
	//
	// Name is the database's spelling and is what a migration, an index and a
	// hand-written query name. Wire is what a request sends and a response
	// carries. They coincide by default, and ADR-0036's amendment is that they
	// are one *derived* spelling rather than one literal one — so a consumer
	// that needs the request-side name reads this and falls back to Name.
	Wire string `json:"wire,omitempty"`

	// Type names the element type of an array column, with Array set beside
	// it — the same split the declaration uses, so a consumer reading the
	// manifest sees the enum values and the varchar length attached to the
	// thing that has them.
	Type       string   `json:"type"`
	Array      bool     `json:"array,omitempty"`
	GoType     string   `json:"goType"`
	Nullable   bool     `json:"nullable,omitempty"`
	Comment    string   `json:"comment,omitempty"`
	Enum       []string `json:"enum,omitempty"`
	HasDefault bool     `json:"hasDefault,omitempty"`
	ReadOnly   bool     `json:"readOnly,omitempty"`
	Immutable  bool     `json:"immutable,omitempty"`
	// WriteOnly reports that the column is settable through create and update
	// but never appears in a response. Unlike a Hidden column — omitted from
	// the manifest entirely, because a name is itself information for a true
	// secret — a write-only column is listed: the fact that it can be written
	// is exactly what a caller assembling a request needs to know.
	WriteOnly    bool         `json:"writeOnly,omitempty"`
	Capabilities []string     `json:"capabilities,omitempty"`
	References   *RefManifest `json:"references,omitempty"`

	// SortNulls is where NULLs sit when this column is sorted on, present only
	// when the column departs from Postgres's direction-following default. It
	// is beside Capabilities rather than in it because it is not a capability:
	// a request cannot ask for it and cannot decline it, and a reader deciding
	// what an endpoint returns wants to know that `?sort=-published_at` puts
	// the NULLs at the bottom rather than the top (#88).
	SortNulls string `json:"sortNulls,omitempty"`

	// Obligations, kept out of Capabilities because a capability is something
	// a request may reach and these are things the server must have done. A
	// client generator has no use for either; a reader auditing the boundary
	// has.
	Scoped     bool `json:"scoped,omitempty"`
	SoftDelete bool `json:"softDelete,omitempty"`

	// Computed reports that the column is an expression rather than storage.
	// A consumer reading the manifest to decide what a schema edit costs needs
	// the distinction: this column appears in every response and in no
	// migration.
	Computed bool `json:"computed,omitempty"`
	// Needs names the per-request binds the expression takes. It is the
	// obligation half — a resource exposing this column does not mount until a
	// hook supplies each of them — and it is listed for the same reason Scoped
	// is: a reader auditing the boundary wants to see what the server had to
	// have done.
	Needs []string `json:"needs,omitempty"`
}

// RefManifest describes a relationship. External references carry a target
// string and no enforced constraint, so a reader can see the relationship even
// though the database does not.
type RefManifest struct {
	Relation string `json:"relation"`
	Table    string `json:"table,omitempty"`
	Column   string `json:"column,omitempty"`
	OnDelete string `json:"onDelete,omitempty"`
	External bool   `json:"external,omitempty"`
	Target   string `json:"target,omitempty"`
	Enforced bool   `json:"enforced"`
}

// IndexManifest describes a secondary index.
type IndexManifest struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Method  string   `json:"method,omitempty"`
}

// RESTManifest is the queryable surface of an exposed table: the single most
// useful thing in the document.
type RESTManifest struct {
	Path            string   `json:"path"`
	Operations      []string `json:"operations"`
	DefaultPageSize int      `json:"defaultPageSize"`
	MaxPageSize     int      `json:"maxPageSize"`
	MaxFilters      int      `json:"maxFilters,omitempty"`
	MaxSortTerms    int      `json:"maxSortTerms,omitempty"`
	MaxOffset       int      `json:"maxOffset,omitempty"`

	// DefaultSort is the ordering a list request that names no ?sort gets,
	// spelled exactly as a client would send it back — wire names, leading "-"
	// for descending. Absent means primary-key order.
	//
	// It is here because it is the one thing about a list that a consumer cannot
	// discover any other way: an unsorted list looks well-formed whatever order
	// it is in, so a client, a generated skill or an agent reading this document
	// has no signal that the resource meant something more specific (#165).
	DefaultSort []string `json:"defaultSort,omitempty"`

	// Filterable, Sortable and Searchable name the columns a request may reach,
	// in their **wire** spelling rather than the database's.
	//
	// Everything else in this struct is already the wire's: Path is a route,
	// Operations are methods, Examples are requests that can be pasted. A
	// capability list is read the same way — `?orgId=eq.…` is what the endpoint
	// accepts for a Camel schema, and reporting `org_id` here made this document
	// describe a request that 400s (#143). [ColumnManifest] carries both
	// spellings for a reader who needs the column instead.
	Filterable []string `json:"filterable"`
	Sortable   []string `json:"sortable"`
	Searchable []string `json:"searchable"`

	// Expandable names the relations ?expand may pull in. Each is the relation
	// name, not the foreign key column: ?expand=list, not ?expand=list_id.
	//
	// Only internal references appear. An ExternalRef crosses a module
	// boundary, which is exactly the join this schema will not perform.
	Expandable []string `json:"expandable,omitempty"`

	// CreateInput names the create body's properties that are not columns —
	// what the request carries beyond the row (#309). A reader that had only the
	// column list would conclude the body is the columns, which for a resource
	// declaring one of these is exactly wrong: the property is usually the
	// required one, since it is the reason the create needs a hook at all.
	CreateInput []BodyProperty `json:"createInput,omitempty"`

	// Actions are the domain verbs the table declares. Without them an agent
	// reading this document sees a CRUD-only API and concludes that completing
	// a task means PATCHing its status — which is the transition the verb
	// exists to own (ADR-0043).
	Actions []ActionManifest `json:"actions,omitempty"`

	Examples []string `json:"examples,omitempty"`
}

// ActionManifest documents one declared verb.
type ActionManifest struct {
	Name string `json:"name"`
	// Path is the full route, resource path included.
	Path   string `json:"path"`
	Method string `json:"method"`
	// Summary is the one-line description, as it appears in the OpenAPI
	// document.
	Summary string `json:"summary,omitempty"`
	// Body names the request body's properties. An action that declares none
	// carries no body at all, which is not the same as one whose properties
	// happen to be optional.
	Body []BodyProperty `json:"body,omitempty"`
	// Writes names the columns the envelope persists after the verb returns.
	// It is not the blast radius: it is one row of this table, and a verb may
	// write anything else through the transaction it holds.
	Writes []string `json:"writes,omitempty"`
	// Touches names the tables the verb writes beyond that row, as declared.
	// Nothing enforces it — see schema.Action.Touches — and it is here because
	// a reader that had only Writes would conclude the route is confined to one
	// row, which is what this field exists to contradict.
	Touches []string `json:"touches,omitempty"`
}

// BodyProperty is one declared property of a request body — an action's, or
// the non-column half of a create's.
//
// It was ActionProperty until a create body could carry one too (#309); the
// alias below keeps the old spelling compiling, since a manifest type is
// something a consumer reads with.
type BodyProperty struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Nullable bool     `json:"nullable,omitempty"`
	Enum     []string `json:"enum,omitempty"`
}

// ActionProperty is the former name of [BodyProperty].
//
// Deprecated: use BodyProperty. A create body declares these too now, so the
// type is no longer about actions.
type ActionProperty = BodyProperty

// OperatorDoc documents one filter operator.
type OperatorDoc struct {
	Name    string `json:"name"`
	Form    string `json:"form"`
	Applies string `json:"applies"`
}

// ParamDoc documents one reserved query parameter.
type ParamDoc struct {
	Name string `json:"name"`
	Form string `json:"form"`
}

// BuildManifest describes every table in the registry.
func (r *Registry) BuildManifest() *Manifest {
	wire := r.Wire()
	m := &Manifest{
		Version:   ManifestVersion,
		Module:    r.module,
		WireCase:  string(wire),
		Operators: operatorDocs(),
		Params:    paramDocs(),
	}
	for _, t := range r.Tables() {
		m.Tables = append(m.Tables, t.manifest(r.Inverses(t), wire))
	}
	return m
}

// BuildManifest describes the default registry.
func BuildManifest() *Manifest { return defaultRegistry.BuildManifest() }

func (t *TableDef) manifest(inverses []InverseRelation, wire WireCase) TableManifest {
	tm := TableManifest{Name: t.name, Comment: t.comment}
	if t.module != "" {
		tm.Module, tm.LocalName = t.module, t.local
	}
	if pk := t.PrimaryKey(); pk != nil {
		tm.PrimaryKey = pk.Desc().Name
	}

	for _, f := range t.fields {
		d := f.Desc()
		if d.Hidden {
			continue
		}
		cm := ColumnManifest{
			Name:       d.Name,
			Wire:       differingWire(wire, d.Name),
			Type:       string(d.Type),
			Array:      d.Array,
			GoType:     d.GoType(),
			Nullable:   d.Nullable,
			Comment:    d.Comment,
			Enum:       d.EnumValues,
			HasDefault: d.DatabaseSupplied(),
			ReadOnly:   d.ReadOnly,
			Immutable:  d.Immutable,
			WriteOnly:  d.WriteOnly,
			Scoped:     d.Scoped,
			SoftDelete: d.SoftDelete,
			Computed:   d.Computed(),
			Needs:      d.Needs,
			SortNulls:  string(d.SortNulls),
		}
		for _, c := range []struct {
			on   bool
			name string
		}{
			{d.Filterable, "filter"}, {d.Sortable, "sort"},
			{d.Searchable, "search"}, {d.Expandable, "expand"},
		} {
			if c.on {
				cm.Capabilities = append(cm.Capabilities, c.name)
			}
		}
		switch {
		case d.Ref != nil && d.Ref.External:
			// An external reference carries a constraint only when it was
			// declared Enforced, and then the manifest names the table and
			// column that constraint points at — a reader auditing the
			// boundary wants to know which of the two kinds this is.
			table, column, enforced := d.Ref.EnforcedTarget()
			cm.References = &RefManifest{
				Relation: d.Ref.Name,
				External: true,
				Target:   d.Ref.Target,
				Table:    table,
				Column:   column,
				Enforced: enforced,
			}
		case d.Ref != nil && d.Ref.Table != nil:
			cm.References = &RefManifest{
				Relation: d.Ref.Name,
				Table:    d.Ref.Table.name,
				Column:   d.Ref.Column,
				OnDelete: string(d.Ref.OnDelete),
				Enforced: true,
			}
		}
		tm.Columns = append(tm.Columns, cm)
	}

	for _, idx := range t.Indexes() {
		tm.Indexes = append(tm.Indexes, IndexManifest{
			Name: idx.Name, Columns: idx.Columns, Unique: idx.Unique, Method: idx.Method,
		})
	}

	for _, inv := range inverses {
		im := InverseManifest{
			Name:       inv.Name,
			Table:      inv.Table.Name(),
			Column:     inv.Column,
			Order:      inv.Order,
			OneToOne:   inv.OneToOne,
			Expandable: inv.Expandable,
		}
		// A one-to-one relation has no cap to report: it is one row or null,
		// never the capped collection Limit describes.
		if !inv.OneToOne {
			im.Limit = inv.Cap()
		}
		tm.CollectedBy = append(tm.CollectedBy, im)
	}

	if t.rest != nil {
		tm.REST = t.restManifest(inverses, wire)
	}
	return tm
}

// differingWire is the wire spelling of a column, or empty when it is the
// column's own name. Empty rather than repeated, so that a Verbatim schema's
// manifest is byte-identical to the one it emitted before this field existed.
func differingWire(c WireCase, column string) string {
	if w := c.WireName(column); w != column {
		return w
	}
	return ""
}

// wireSortTerms respells declared sort terms as a request would send them.
//
// The declaration names columns the way the schema does; ?sort names them the
// way the wire does. This is the one place the two meet, and it keeps the
// direction prefix attached to whichever spelling it is on.
func wireSortTerms(terms []string, wire WireCase) []string {
	if len(terms) == 0 {
		return nil
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		name, desc, err := SortTerm(term)
		if err != nil {
			// Validate reports it; this document reproduces what was declared
			// rather than dropping it, so the two do not disagree about what
			// the schema says.
			out = append(out, term)
			continue
		}
		// Normalised onto the leading-minus spelling, because the document is
		// read as something to paste and one form is easier to paste than two.
		spelled := wire.WireName(name)
		if desc {
			spelled = "-" + spelled
		}
		out = append(out, spelled)
	}
	return out
}

func (t *TableDef) restManifest(inverses []InverseRelation, wire WireCase) *RESTManifest {
	rm := &RESTManifest{
		Path:            t.rest.Path,
		Operations:      strings.Split(t.rest.Ops.String(), "|"),
		DefaultPageSize: t.rest.DefaultPageSize,
		MaxPageSize:     t.rest.MaxPageSize,
		MaxFilters:      t.rest.MaxFilters,
		MaxSortTerms:    t.rest.MaxSortTerms,
		MaxOffset:       t.rest.MaxOffset,
		DefaultSort:     wireSortTerms(t.rest.DefaultSort, wire),
		Filterable:      []string{},
		Sortable:        []string{},
		Searchable:      []string{},
	}
	// A singleton has no list, so it has no ?filter, ?sort or ?search either —
	// its one GET rejects every query parameter but ?expand. Reporting the
	// column capabilities anyway would describe requests that answer 400, which
	// is the failure #143 was: a document faithfully rendering a surface that
	// is not there.
	singleton := t.rest.Ops.Has(OpSingleton)
	if singleton {
		rm.DefaultSort = nil
		rm.DefaultPageSize, rm.MaxPageSize = 0, 0
		rm.MaxFilters, rm.MaxSortTerms, rm.MaxOffset = 0, 0, 0
	}
	for _, f := range t.fields {
		d := f.Desc()
		if d.Hidden {
			continue
		}
		// The wire spelling, not the column's own: this section describes what a
		// request may send, and under a non-default WireCase the two differ.
		name := wire.WireName(d.Name)
		if singleton {
			// Expansion is the one parameter the singleton read does take, so
			// the loop still runs for it.
			if d.Expandable && d.Ref != nil && !d.Ref.External {
				rm.Expandable = append(rm.Expandable, d.Ref.Name)
			}
			continue
		}
		if d.Filterable {
			rm.Filterable = append(rm.Filterable, name)
		}
		if d.Sortable {
			rm.Sortable = append(rm.Sortable, name)
		}
		if d.Searchable {
			rm.Searchable = append(rm.Searchable, name)
		}
		// The relation name, not the column: ?expand names the relation, and a
		// caller reading this should not have to strip an "_id" to guess it.
		if d.Expandable && d.Ref != nil && !d.Ref.External {
			rm.Expandable = append(rm.Expandable, d.Ref.Name)
		}
	}
	// The reverse direction is expandable on this endpoint too, and a client
	// reading the vocabulary should not have to infer it from another table.
	for _, inv := range inverses {
		if inv.Expandable {
			rm.Expandable = append(rm.Expandable, inv.Name)
		}
	}
	rm.CreateInput = bodyProperties(t.rest.CreateInput)
	for _, a := range t.actions {
		rm.Actions = append(rm.Actions, a.manifest(t.rest.Path))
	}
	rm.Examples = t.examples(rm)
	return rm
}

// manifest documents one action for the machine-readable surface.
func (a Action) manifest(resourcePath string) ActionManifest {
	am := ActionManifest{
		Name:    a.Name,
		Path:    a.FullPath(resourcePath),
		Method:  "POST",
		Summary: a.Summary,
		Writes:  a.Writes,
		Touches: a.Touches,
	}
	am.Body = bodyProperties(a.Body)
	return am
}

// bodyProperties documents a declared request body, whichever declaration it
// came from.
func bodyProperties(body []*Field) []BodyProperty {
	var out []BodyProperty
	for _, f := range body {
		d := f.Desc()
		out = append(out, BodyProperty{
			Name:     d.Name,
			Type:     string(d.Type),
			Nullable: d.Nullable,
			Enum:     d.EnumValues,
		})
	}
	return out
}

// examples renders a few requests that are valid against this resource. A
// worked example is worth more than a grammar to a caller assembling its first
// request, and unlike prose it can be checked against the parser.
func (t *TableDef) examples(rm *RESTManifest) []string {
	var out []string
	// A singleton's whole request surface is the path and, where it has one,
	// ?expand. Every other example below is a list request it does not serve.
	if t.rest.Ops.Has(OpSingleton) {
		out = append(out, "GET "+rm.Path)
		if len(rm.Expandable) > 0 {
			out = append(out, fmt.Sprintf("GET %s?expand=%s", rm.Path, rm.Expandable[0]))
		}
		return out
	}
	if len(rm.Filterable) > 0 {
		out = append(out, fmt.Sprintf("GET %s?%s=eq.VALUE", rm.Path, rm.Filterable[0]))
	}
	if len(rm.Sortable) > 0 {
		out = append(out, fmt.Sprintf("GET %s?sort=-%s&page=2&per_page=20", rm.Path, rm.Sortable[0]))
	}
	if len(rm.Searchable) > 0 {
		out = append(out, fmt.Sprintf("GET %s?search=TERM", rm.Path))
	}
	if len(rm.Expandable) > 0 {
		out = append(out, fmt.Sprintf("GET %s?expand=%s", rm.Path, rm.Expandable[0]))
	}
	if len(rm.Filterable) > 1 {
		out = append(out, fmt.Sprintf("GET %s?or=(%s.eq.A,%s.eq.B)",
			rm.Path, rm.Filterable[0], rm.Filterable[1]))
	}
	return out
}

func operatorDocs() []OperatorDoc {
	return []OperatorDoc{
		{"eq", "col=eq.VALUE (or col=VALUE)", "any filterable column"},
		{"ne", "col=ne.VALUE", "any filterable column"},
		{"gt", "col=gt.VALUE", "ordered columns"},
		{"gte", "col=gte.VALUE", "ordered columns"},
		{"lt", "col=lt.VALUE", "ordered columns"},
		{"lte", "col=lte.VALUE", "ordered columns"},
		{"in", "col=in.A,B,C", "any filterable column"},
		{"nin", "col=nin.A,B", "any filterable column"},
		{"between", "col=between.LO,HI", "ordered columns"},
		{"isnull", "col=isnull", "nullable columns"},
		{"notnull", "col=notnull", "nullable columns"},
		{"contains", "col=contains.TEXT", "text columns; wildcards escaped"},
		{"startswith", "col=startswith.TEXT", "text columns; wildcards escaped"},
		{"endswith", "col=endswith.TEXT", "text columns; wildcards escaped"},
		{"like", "col=like.PATTERN", "text columns; wildcards NOT escaped"},
		{"ilike", "col=ilike.PATTERN", "text columns; wildcards NOT escaped"},
	}
}

func paramDocs() []ParamDoc {
	return []ParamDoc{
		{"select", "select=id,name — projection; the primary key is always included"},
		{"sort", "sort=-created_at,name — leading '-' is descending"},
		{"search", "search=TERM — fans out over searchable columns"},
		// "expand" is not listed: paramDocs is unconditional, so listing it
		// would advertise expansion on every resource in the document,
		// including the ones that declare no expandable relation at all.
		{"page", "page=2 — 1-based"},
		{"per_page", "per_page=50 — capped by maxPageSize"},
		{"limit", "limit=50 — alternative to per_page"},
		{"offset", "offset=100 — alternative to page"},
		{"or", "or=(a.eq.1,b.gt.2) — explicit disjunction, nestable"},
		{"and", "and=(a.eq.1,b.gt.2) — explicit conjunction"},
		{"not", "not=(a.eq.1,b.gt.2) — negation; several conditions are NOT (a AND b)"},
	}
}

// JSON renders the manifest.
func (m *Manifest) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// WriteManifest writes the default registry's manifest to path, creating
// parent directories as needed.
func WriteManifest(path string) error {
	b, err := BuildManifest().JSON()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o644)
}
