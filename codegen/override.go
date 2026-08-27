package codegen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// Type overrides: the Go type only.
//
// ADR-0035 is the record, and its one load-bearing sentence is that an override
// is a *rendering* decision. It changes what the generated Go says and reaches
// neither the SQL type nor the wire — the DDL comes from schema.Type and every
// client emitter maps from schema.Type too, so an override is invisible to all
// of them by construction rather than by care.
//
// The one thing it does change downstream is filter coercion, because that
// reads the model's reflect.Type. That is correct: `?id=eq.019…` against a
// uuid.UUID column should parse into a uuid.UUID, and filter.Coerce already
// delegates to encoding.TextUnmarshaler for exactly this shape.

// TypeOverride replaces the Go type codegen emits for the columns it matches.
//
// At least one matcher must be set. More specific wins: a Table+Column override
// beats a Column one, which beats a Type one.
//
//	{Type: schema.TypeUUID, GoType: "uuid.UUID", Import: "github.com/google/uuid"}
//	{Table: "invoices", Column: "amount", GoType: "decimal.Decimal",
//	 Import: "github.com/shopspring/decimal"}
type TypeOverride struct {
	// Type matches every column of a logical type.
	Type schema.Type
	// Table narrows to one table, by its storage name — the name including any
	// module prefix, which is what the registry holds.
	Table string
	// Column narrows to one column name.
	Column string

	// GoType is the type as it should appear in the generated source,
	// qualified by package where it needs to be: "uuid.UUID". Required.
	GoType string
	// Import is the package path providing GoType, or empty when it needs none.
	// It is emitted verbatim into the import block and is not resolved — a
	// wrong one fails to compile in the consuming repository, one command
	// later.
	Import string
}

// specificity orders overrides so the narrowest match wins. The values are
// ordinal only; nothing outside this file reads them.
func (o TypeOverride) specificity() int {
	switch {
	case o.Table != "" && o.Column != "":
		return 3
	case o.Column != "":
		return 2
	case o.Type != "":
		return 1
	}
	return 0
}

func (o TypeOverride) matches(table string, d *schema.FieldDesc) bool {
	if o.Table != "" && o.Table != table {
		return false
	}
	if o.Column != "" && o.Column != d.Name {
		return false
	}
	if o.Type != "" && o.Type != d.Type {
		return false
	}
	return true
}

// String renders an override for a diagnostic, naming only the fields it set.
func (o TypeOverride) String() string {
	var parts []string
	if o.Type != "" {
		parts = append(parts, "Type: "+string(o.Type))
	}
	if o.Table != "" {
		parts = append(parts, "Table: "+o.Table)
	}
	if o.Column != "" {
		parts = append(parts, "Column: "+o.Column)
	}
	return "{" + strings.Join(parts, ", ") + " → " + o.GoType + "}"
}

// overrides resolves a column to its emitted Go type. The zero value overrides
// nothing, so every emitter can hold one unconditionally.
type overrides struct {
	rules []TypeOverride
}

// newOverrides validates the rules and returns the resolver.
//
// The validation is at generation time on purpose: a contradictory config
// should say so before it writes a file, not produce Go whose type depends on
// slice order.
func newOverrides(rules []TypeOverride, reg *schema.Registry) (*overrides, error) {
	for _, r := range rules {
		if r.GoType == "" {
			return nil, fmt.Errorf("codegen: type override %s has no GoType", r)
		}
		if r.specificity() == 0 {
			return nil, fmt.Errorf(
				"codegen: type override → %s matches every column, because it sets "+
					"none of Type, Table or Column; name what it applies to", r.GoType)
		}
	}
	o := &overrides{rules: rules}
	if reg != nil {
		if err := o.check(reg); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// check reports the problems that need the schema to see: two rules of equal
// specificity fighting over one column, an override on an enum, and a rule that
// matches nothing.
func (o *overrides) check(reg *schema.Registry) error {
	used := make([]bool, len(o.rules))
	for _, t := range reg.Tables() {
		for _, f := range t.Fields() {
			d := f.Desc()
			winners := o.candidates(t.Name(), d)
			for _, i := range winners.all {
				used[i] = true
			}
			if len(winners.top) > 1 {
				return fmt.Errorf(
					"codegen: %s.%s is matched by %d type overrides of equal "+
						"specificity (%s); one of them has to be narrowed with Table "+
						"or Column, because which one applied would otherwise depend "+
						"on the order they were written in",
					t.Name(), d.Name, len(winners.top), o.describe(winners.top))
			}
			if len(winners.top) == 1 && d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
				return fmt.Errorf(
					"codegen: %s.%s is an enum and cannot be type-overridden (%s). "+
						"The generated string type carries the value set — its constants, "+
						"the TypeScript union and the CLI's --help all come from it — and "+
						"an override would replace it with a type that has none of that",
					t.Name(), d.Name, o.rules[winners.top[0]])
			}
		}
	}
	// A rule that matches nothing is almost always a typo in a table or column
	// name, and it fails silently by definition: the generated code is simply
	// the code that would have been generated anyway.
	for i, r := range o.rules {
		if !used[i] {
			return fmt.Errorf(
				"codegen: type override %s matches no column in the schema; check "+
					"the table and column names against the declaration", r)
		}
	}
	return nil
}

type matchSet struct {
	all []int // every rule that matched, for the unused check
	top []int // those at the highest specificity
}

func (o *overrides) candidates(table string, d *schema.FieldDesc) matchSet {
	var out matchSet
	best := 0
	for i, r := range o.rules {
		if !r.matches(table, d) {
			continue
		}
		out.all = append(out.all, i)
		switch s := r.specificity(); {
		case s > best:
			best, out.top = s, []int{i}
		case s == best:
			out.top = append(out.top, i)
		}
	}
	return out
}

func (o *overrides) describe(idx []int) string {
	parts := make([]string, len(idx))
	for i, n := range idx {
		parts[i] = o.rules[n].String()
	}
	sort.Strings(parts)
	return strings.Join(parts, " and ")
}

// rule returns the override that applies to a column, or nil when none does.
//
// Ambiguity resolves to nil rather than to a winner, but newOverrides has
// already refused a schema where two rules tie, so this is the belt to that
// braces: nothing reaches here whose type would depend on rule order.
func (o *overrides) rule(table string, d *schema.FieldDesc) *TypeOverride {
	if o == nil || len(o.rules) == 0 {
		return nil
	}
	top := o.candidates(table, d).top
	if len(top) != 1 {
		return nil
	}
	return &o.rules[top[0]]
}

// base returns the overridden base type for a column, and whether one applied.
//
// "Base" is the type before Nullable and Array wrap it: those compose on top,
// in the same place they did before, so an override never has to know about
// either.
func (o *overrides) base(table string, d *schema.FieldDesc) (string, bool) {
	r := o.rule(table, d)
	if r == nil {
		return "", false
	}
	return r.GoType, true
}

// imports returns the package paths the overrides that actually matched need,
// so an override written for a table the schema does not have cannot add an
// import to a file that never uses it.
//
// Every column of every table, which is what the models and columns emitters
// render. A file that renders a subset — rest_gen.go, which sees only the body
// columns of the exposed tables — collects its imports per field instead, or
// the same argument that rejects an unmatched rule would let a matched one in
// through a door the file never opens.
func (o *overrides) imports(reg *schema.Registry) []string {
	if o == nil || len(o.rules) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, t := range reg.Tables() {
		for _, f := range t.Fields() {
			r := o.rule(t.Name(), f.Desc())
			if r == nil {
				continue
			}
			if r.Import != "" {
				seen[r.Import] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// applyOverridesToManifest rewrites the manifest's goType for every overridden
// column, so sqlb.json describes the code that was emitted rather than the code
// the default mapping would have emitted.
//
// The manifest is built from the registry, which knows nothing about rendering.
// This is the one place the two meet, and it is deliberately a post-pass rather
// than a parameter threaded into schema: the registry is also what migrate and
// introspect read, and a rendering preference has no business in it.
func applyOverridesToManifest(m *schema.Manifest, opts Options) error {
	ov, err := newOverrides(opts.Types, opts.Registry)
	if err != nil {
		return err
	}
	if len(ov.rules) == 0 {
		return nil
	}
	byName := map[string]*schema.TableDef{}
	for _, t := range ownTables(opts) {
		byName[t.Name()] = t
	}
	for ti := range m.Tables {
		table := byName[m.Tables[ti].Name]
		if table == nil {
			continue
		}
		for ci := range m.Tables[ti].Columns {
			col := &m.Tables[ti].Columns[ci]
			f := table.Field(col.Name)
			if f == nil {
				continue
			}
			d := f.Desc()
			base, replaced := ov.base(table.Name(), d)
			if !replaced {
				continue
			}
			switch {
			case d.Array:
				col.GoType = "[]" + base
			case d.Nullable:
				col.GoType = "*" + base
			default:
				col.GoType = base
			}
		}
	}
	return nil
}
