package codegen

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// renderModels emits one struct per table, plus a named string type for each
// enum column.
//
// The struct tags are the contract with the runtime: `db` names the column,
// `sqlb` carries the capabilities the schema declared, and `json` names the
// property the REST layer serialises it as. Everything the engine knows about a
// model at runtime comes from here.
func renderModels(opts Options) ([]byte, error) {
	// A view (schema.View) gets a struct the same way a table does — every
	// field below reads Name/Fields/Comment, none of which a view lacks —
	// appended after the tables so declaration order stays table-then-view
	// rather than interleaved by name, which would put a view's struct in
	// the middle of an unrelated table's block for no reason a reader could
	// see.
	tables := append(opts.Registry.Tables(), opts.Registry.Views()...)

	ov, err := newOverrides(opts.Types, opts.Registry)
	if err != nil {
		return nil, err
	}

	imports := map[string]bool{}
	for _, path := range ov.imports(opts.Registry) {
		imports[path] = true
	}
	for _, t := range tables {
		for _, f := range t.Fields() {
			// A computed column's expression is carried by a method returning
			// sqlb.Computed, for the reason renderComputed gives. Ahead of the
			// override guard, not behind it: an override replaces the column's
			// Go type and not the fact that it is computed, so the method is
			// emitted either way and needs the import either way.
			if f.Desc().Computed() {
				imports["github.com/mind-vm/sqlb"] = true
			}
			// The default mapping decides which stdlib import a column needs;
			// an overridden column brings its own, above.
			if _, replaced := ov.base(t.Name(), f.Desc()); replaced {
				continue
			}
			// Contains rather than a case per spelling. The list of spellings
			// was maintained by hand and was already one short: a nullable
			// vector renders as *sqlb.Vector, which matched none of them, so
			// the model named the type with nothing importing it. The wrapping
			// a nullable or array column adds is not information this switch
			// wants, so it does not ask for it.
			switch goType := f.Desc().GoType(); {
			case strings.Contains(goType, "time.Time"):
				imports["time"] = true
			case strings.Contains(goType, "json.RawMessage"):
				imports["encoding/json"] = true
			case strings.Contains(goType, "sqlb.Vector"):
				// The second thing in this file that is not a plain Go type,
				// and the first that is a *column*. An embedding needs the
				// codec that moves it in binary, so the model cannot be
				// importable without sqlb the way the rest of them are.
				imports["github.com/mind-vm/sqlb"] = true
			}
			// A computed column's expression is carried by a method returning
			// sqlb.Computed, for the reason renderComputed gives.
			if f.Desc().Computed() {
				imports["github.com/mind-vm/sqlb"] = true
			}
		}
		// An expanded collection lands in a sqlb.Collection. Models are
		// otherwise importable without sqlb — a table with neither a reverse
		// relation nor a vector column stays that way. A one-to-one inverse emits
		// a bare pointer instead, so it does not need the import.
		for _, inv := range opts.Registry.Inverses(t) {
			if inv.Expandable && !inv.OneToOne {
				imports["github.com/mind-vm/sqlb"] = true
			}
		}
	}

	b := header(opts.Package, sortedSet(imports))
	wire := opts.Registry.Wire()

	// sharedEnumUsers maps a SharedAs name to every "table.column" that
	// declares it, in table order — collected ahead of the render loop below
	// so the type's doc comment can name every column it serves, not only the
	// first one to render.
	sharedEnumUsers := map[string][]string{}
	for _, t := range tables {
		for _, f := range t.Fields() {
			if d := f.Desc(); d.Type == schema.TypeEnum && d.SharedAs != "" {
				sharedEnumUsers[d.SharedAs] = append(sharedEnumUsers[d.SharedAs], t.Name()+"."+d.Name)
			}
		}
	}
	// emittedShared tracks which SharedAs names have already produced a type
	// and const block, so the second and later columns claiming one render
	// their struct field against it without emitting it again — Go does not
	// allow a type or a const to be declared twice, and Registry.Validate
	// already refused two declarations of the same name with different values,
	// so every column reaching here for a given name agrees on what it means.
	emittedShared := map[string]bool{}

	for _, t := range tables {
		typeName := TypeName(t)

		// Enum types first, so the struct that uses them reads top-down.
		for _, f := range t.Fields() {
			d := f.Desc()
			if d.Type != schema.TypeEnum || len(d.EnumValues) == 0 {
				continue
			}
			enum := typeName + GoName(d.Name)
			if d.SharedAs != "" {
				enum = d.SharedAs
				if emittedShared[enum] {
					continue
				}
				emittedShared[enum] = true
			}
			consts, err := enumConsts(t.Name(), d)
			if err != nil {
				return nil, err
			}
			if others := otherSharedUsers(sharedEnumUsers[enum], t.Name()+"."+d.Name); len(others) > 0 {
				fmt.Fprintf(b, "\n// %s is the %s.%s column's value set, shared with %s.\n",
					enum, t.Name(), d.Name, strings.Join(others, ", "))
			} else {
				fmt.Fprintf(b, "\n// %s is the %s.%s column's value set.\n", enum, t.Name(), d.Name)
			}
			fmt.Fprintf(b, "type %s string\n\n", enum)
			fmt.Fprintln(b, "const (")
			for i, v := range d.EnumValues {
				fmt.Fprintf(b, "\t%s%s %s = %q\n", enum, consts[i], enum, v)
			}
			fmt.Fprintln(b, ")")
		}

		fmt.Fprintln(b)
		if c := t.Comment(); c != "" {
			docLines(b, "", typeName+" "+lowerFirst(c))
		} else {
			fmt.Fprintf(b, "// %s is a row of %s.\n", typeName, t.Name())
		}
		rels, err := relationsOf(t)
		if err != nil {
			return nil, err
		}

		fmt.Fprintf(b, "type %s struct {\n", typeName)
		for _, f := range t.Fields() {
			d := f.Desc()
			fmt.Fprintf(b, "\t%s %s `db:%q %s%s`",
				GoName(d.Name), goType(typeName, t.Name(), d, ov), d.Name,
				jsonTag(d, wire), capTag(d, wire))
			if c := d.Comment; c != "" {
				fmt.Fprintf(b, " // %s", oneLine(c))
			}
			fmt.Fprintln(b)

			// The relation sits directly under the key it expands, because the
			// two are one declaration split across two fields and reading them
			// apart is how they come to disagree.
			if rel, ok := rels[d.Name]; ok {
				fmt.Fprintf(b, "\t%s *%s `db:\"-\" json:%q sqlb:%q` // filled in by ?expand=%s\n",
					rel.field, rel.target, rel.relation+",omitempty", "expands="+d.Name, rel.relation)
			}
		}

		// The reverse relations come after every column, because they belong to
		// no column of this table: they are declared on the far side, by the
		// reference that points here.
		inverses, err := inversesOf(opts.Registry, t)
		if err != nil {
			return nil, err
		}
		for _, inv := range inverses {
			if inv.oneToOne {
				fmt.Fprintf(b, "\t%s *%s `db:\"-\" json:%q sqlb:%q` // filled in by ?expand=%s\n",
					inv.field, inv.target, inv.relation+",omitempty", inv.tag, inv.relation)
				continue
			}
			fmt.Fprintf(b, "\t%s *sqlb.Collection[%s] `db:\"-\" json:%q sqlb:%q` // filled in by ?expand=%s\n",
				inv.field, inv.target, inv.relation+",omitempty", inv.tag, inv.relation)
		}
		fmt.Fprintln(b, "}")

		// TableName is always emitted, so the mapping never depends on the
		// singulariser guessing the type name back into the table name.
		fmt.Fprintf(b, "\n// TableName is the table %s maps to.\n", typeName)
		fmt.Fprintf(b, "func (%s) TableName() string { return %q }\n", typeName, t.Name())

		renderComputed(b, typeName, t)
	}

	return gofmt(opts.modelsFile(), b.Bytes())
}

// renderComputed emits the ComputedColumns method for a table with derived
// columns, and nothing at all for one without.
//
// The expression goes in a method rather than in the `sqlb` struct tag, which
// is where every other thing the runtime knows about a column lives. A tag is a
// comma-separated list of words; a SQL expression contains commas, quotes and
// parentheses, and encoding one into a tag would mean inventing an escape and
// then reading it back with a parser. The method is Go, so the compiler checks
// it and a reader can see what will be run (ADR-0041).
func renderComputed(b *bytes.Buffer, typeName string, t *schema.TableDef) {
	var derived []*schema.FieldDesc
	for _, f := range t.Fields() {
		if d := f.Desc(); d.Computed() {
			derived = append(derived, d)
		}
	}
	if len(derived) == 0 {
		return
	}

	fmt.Fprintf(b, "\n// ComputedColumns are the derived columns %s declares: expressions the\n", typeName)
	fmt.Fprintf(b, "// query renders in place of a column name. None of them is stored, so none of\n")
	fmt.Fprintf(b, "// them is written by an insert or an update.\n")
	fmt.Fprintf(b, "func (%s) ComputedColumns() []sqlb.Computed {\n", typeName)
	fmt.Fprintln(b, "\treturn []sqlb.Computed{")
	for _, d := range derived {
		fmt.Fprintf(b, "\t\t{Name: %q, Expr: %q", d.Name, d.Expr)
		if len(d.Needs) > 0 {
			fmt.Fprintf(b, ", Needs: []string{")
			for i, key := range d.Needs {
				if i > 0 {
					fmt.Fprint(b, ", ")
				}
				fmt.Fprintf(b, "%q", key)
			}
			fmt.Fprint(b, "}")
		}
		fmt.Fprintln(b, "},")
	}
	fmt.Fprintln(b, "\t}")
	fmt.Fprintln(b, "}")
}

// relation is the second half of an expandable reference: the typed field an
// expanded row lands in.
type relation struct {
	field    string // Go field name, e.g. "List"
	target   string // Go type of the expanded model, e.g. "List"
	relation string // name on the wire, e.g. "list"
}

// relationsOf returns the relation field to emit after each expandable
// reference column, keyed by that column's name.
//
// An expandable reference is two struct fields working together. The foreign
// key stays an ordinary column carrying the `expand` capability, and beside it
// sits a `db:"-"` field the joined row is scanned into — no projection selects
// it, no insert writes it, no update sets it. The runtime links the two through
// the `expands=` tag; sqlb's relation.go is where that is read.
//
// Only internal references qualify. An ExternalRef names a table this module
// does not own, and the schema already refuses to mark one Expandable.
//
// Note what this does *not* check: whether the target table is itself exposed
// over REST. Expanding a relation into an unexposed table is a legitimate
// design — the row is reachable inline without the table acquiring a collection
// endpoint of its own — and `.Expandable()` is the explicit opt-in that says so.
// Hidden columns of the target are stripped by the join either way.
func relationsOf(t *schema.TableDef) (map[string]relation, error) {
	// Every column already owns its Go name, and a relation that collided with
	// one would emit a struct with two identical fields — which go/format
	// accepts, because it parses without type-checking, so the break would
	// surface at the consumer's next build instead of here.
	taken := map[string]string{}
	for _, f := range t.Fields() {
		taken[GoName(f.Desc().Name)] = "column " + f.Desc().Name
	}

	out := map[string]relation{}
	for _, f := range t.Fields() {
		d := f.Desc()
		// A nil Ref.Table on an internal reference is already refused by
		// Registry.Validate, which render runs first.
		if !d.Expandable || d.Ref == nil || d.Ref.External || d.Ref.Table == nil {
			continue
		}
		name := GoName(d.Ref.Name)
		if by, dup := taken[name]; dup {
			return nil, fmt.Errorf(
				"codegen: table %s: relation %q wants the Go field %s, which %s already uses; "+
					"rename one of them, or drop .Expandable()",
				t.Name(), d.Ref.Name, name, by)
		}
		taken[name] = "relation " + d.Ref.Name

		out[d.Name] = relation{
			field:    name,
			target:   TypeName(d.Ref.Table),
			relation: d.Ref.Name,
		}
	}
	return out, nil
}

// inverse is the field a reverse relation contributes to the *target's*
// struct: the collection the children land in.
type inverse struct {
	field    string // Go field name, e.g. "Tasks"
	target   string // Go type of the child model, e.g. "Task"
	relation string // name on the wire, e.g. "tasks"
	tag      string // the sqlb tag, e.g. "expands=list_id,order=-created_at"
	oneToOne bool   // emit a bare pointer instead of *sqlb.Collection[T]
}

// inversesOf returns the collection fields to emit on t, one per reference
// elsewhere in the registry that named an inverse and exposed it.
//
// Only exposed ones produce a field. A declared-but-unexposed inverse names the
// relationship for the manifest and stops there — the field exists to be filled
// in by ?expand, so emitting one nothing can ask for would be vocabulary with
// no consumer. ADR-0022, and ADR-0006 for why the two are separate decisions.
func inversesOf(reg *schema.Registry, t *schema.TableDef) ([]inverse, error) {
	taken := map[string]string{}
	for _, f := range t.Fields() {
		taken[GoName(f.Desc().Name)] = "column " + f.Desc().Name
	}
	for _, rel := range relationsIn(t) {
		taken[rel.field] = "relation " + rel.relation
	}

	var out []inverse
	for _, inv := range reg.Inverses(t) {
		if !inv.Expandable {
			continue
		}
		name := GoName(inv.Name)
		if by, dup := taken[name]; dup {
			return nil, fmt.Errorf(
				"codegen: table %s: inverse relation %q wants the Go field %s, which %s already uses; "+
					"rename the Inverse on %s.%s",
				t.Name(), inv.Name, name, by, inv.Table.Name(), inv.Column)
		}
		taken[name] = "inverse relation " + inv.Name

		// The cap is always written out, even when it is the default. A
		// generated model that stated no limit would be relying on the
		// engine's, and the number would then live in two places with only one
		// of them readable from the file.
		tag := "expands=" + inv.Column
		if inv.OneToOne {
			tag += ",reverse"
		} else {
			if inv.Order != "" {
				tag += ",order=" + inv.Order
			}
			tag += ",limit=" + strconv.Itoa(inv.Cap())
		}
		out = append(out, inverse{
			field:    name,
			target:   TypeName(inv.Table),
			relation: inv.Name,
			tag:      tag,
			oneToOne: inv.OneToOne,
		})
	}
	return out, nil
}

// relationsIn is relationsOf without the collision report, for callers that
// only need the field names already spoken for.
func relationsIn(t *schema.TableDef) []relation {
	var out []relation
	for _, f := range t.Fields() {
		d := f.Desc()
		if !d.Expandable || d.Ref == nil || d.Ref.External || d.Ref.Table == nil {
			continue
		}
		out = append(out, relation{
			field:    GoName(d.Ref.Name),
			target:   TypeName(d.Ref.Table),
			relation: d.Ref.Name,
		})
	}
	return out
}

// expandableRelations names the relations a resource may expand, in
// declaration order and forward direction first. It drives both the generated
// rest.Options and the manifest, so the two cannot drift apart.
func expandableRelations(reg *schema.Registry, t *schema.TableDef) []string {
	var out []string
	for _, f := range t.Fields() {
		if d := f.Desc(); d.Expandable && d.Ref != nil && !d.Ref.External {
			out = append(out, d.Ref.Name)
		}
	}
	for _, inv := range reg.Inverses(t) {
		if inv.Expandable {
			out = append(out, inv.Name)
		}
	}
	return out
}

// enumConsts is the constant-name suffix for each of a column's enum values,
// in declaration order.
//
// The collision is refused rather than emitted. task.assigned and
// task_assigned both spell TaskAssigned, and a duplicate const is a compile
// error in the consumer's package with nothing in it naming which two values
// caused it — where this says both, and says which column they are on.
func enumConsts(table string, d *schema.FieldDesc) ([]string, error) {
	out := make([]string, len(d.EnumValues))
	from := map[string]string{}
	for i, v := range d.EnumValues {
		name := EnumConst(v)
		if prev, taken := from[name]; taken {
			return nil, fmt.Errorf(
				"codegen: table %s: column %s has two values, %q and %q, that both spell the Go constant name %s; "+
					"rename one, or drop back to Text plus a Check", table, d.Name, prev, v, name)
		}
		from[name] = v
		out[i] = name
	}
	return out, nil
}

// goType is the Go type for a column, using the generated enum type where the
// schema declared one.
func goType(typeName, table string, d *schema.FieldDesc, ov *overrides) string {
	// An override replaces the base type only. Nullable and Array wrap it
	// afterwards, in the same place they always did, which is why an override
	// never has to know about either (ADR-0035).
	if base, replaced := ov.base(table, d); replaced {
		switch {
		case d.Array:
			return "[]" + base
		case d.Nullable:
			return "*" + base
		}
		return base
	}
	if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
		enum := typeName + GoName(d.Name)
		if d.SharedAs != "" {
			enum = d.SharedAs
		}
		switch {
		case d.Array:
			// A nil slice already says NULL, so an array is the plain slice
			// whether or not the column is nullable.
			return "[]" + enum
		case d.Nullable:
			return "*" + enum
		}
		return enum
	}
	return d.GoType()
}

// jsonTag renders the `json` struct tag for the row struct.
//
// A hidden or write-only column gets `json:"-"`. The REST layer already
// excludes both from every projection, but a column that could still be
// marshalled is one stray json.Marshal away from leaking — a debug log, a
// hand-written handler — and the tag closes that off at the type. This is the
// row struct's own tag; a write-only column's separate create/update body
// struct gets a real tag elsewhere, since that is how the value gets in.
func jsonTag(d *schema.FieldDesc, wire schema.WireCase) string {
	if d.Hidden || d.WriteOnly {
		return `json:"-"`
	}
	return fmt.Sprintf("json:%q", wire.WireName(d.Name))
}

// capTag renders the `sqlb` struct tag, omitted entirely when a column has
// nothing to say so the common case stays readable.
//
// The logical type leads, and it is written for every column rather than only
// the ones a bug has been found on: timestamptz, date and time are one Go type
// and three different things to Postgres, so a runtime reading `reflect.Type`
// alone cannot tell them apart — which is how expanding a relation over a date
// column came to answer 500 (#84).
//
// It is composed here rather than added to FieldDesc.Capabilities because a
// type is not a capability. Capabilities are the things a request may reach the
// column through, and putting the type in that list made the schema's own
// documentation print it twice on one line.
func capTag(d *schema.FieldDesc, wire schema.WireCase) string {
	parts := make([]string, 0, 3)
	if d.Type != "" {
		parts = append(parts, "type:"+string(d.Type))
	}
	// Only when it differs, so a Verbatim schema emits byte-for-byte what it
	// always did and this change is invisible to every existing project. The
	// runtime is told the spelling rather than computing one: nothing on the
	// request path may import the schema package (ADR-0036's amendment).
	if w := wire.WireName(d.Name); w != d.Name {
		parts = append(parts, "wire:"+w)
	}
	if caps := d.Capabilities(); caps != "" {
		parts = append(parts, caps)
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf(` sqlb:%q`, strings.Join(parts, ","))
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	// Only lower an ordinary capitalised word: an acronym or a proper noun
	// should stay as written.
	r := []rune(s)
	if len(r) > 1 && r[1] >= 'A' && r[1] <= 'Z' {
		return s
	}
	r[0] = []rune(strings.ToLower(string(r[0])))[0]
	return string(r)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// otherSharedUsers returns every "table.column" in users besides self, in the
// order they were collected, for the doc comment a shared enum type gets.
func otherSharedUsers(users []string, self string) []string {
	var out []string
	for _, u := range users {
		if u != self {
			out = append(out, u)
		}
	}
	return out
}
