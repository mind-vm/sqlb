package introspect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// build turns catalog rows into a registry. It is a pure function, which is the
// whole reason reading and interpreting are separate: every mapping decision
// below can be tested against rows written by hand, and the only thing that
// needs a real database is whether the queries return the right rows.
func build(cat *catalog, opts Options) (*schema.Registry, *Report, error) {
	rep := &Report{}
	r := schema.NewRegistry()
	if opts.Module != "" {
		r = schema.NewModule(opts.Module)
	}

	// Recorded whatever else happens, and before the table walk can fail: the
	// list is most valuable exactly when the import went badly, because a
	// missing extension is what makes the *next* step fail rather than this one
	// (issue #115).
	for _, e := range cat.extensions {
		rep.Extensions = append(rep.Extensions, e.Name)
	}

	byTable := groupByTable(cat)
	selectTables(byTable, opts, rep)
	order, err := dependencyOrder(cat, byTable, rep)
	if err != nil {
		return nil, rep, err
	}

	built := map[string]*schema.TableDef{}
	for _, name := range order {
		local, ok := localName(name, opts.Module)
		if !ok {
			rep.add(name, "", "table is not prefixed with the module name, and a module "+
				"registry would rename it on the way back out", "")
			continue
		}
		// A name the DSL cannot declare is skipped and reported like any other
		// unrepresentable construct. Letting it through to Validate below would
		// fail the entire import with a message blaming this package, when the
		// cause is a database that quotes its identifiers — a camelCase table
		// is legal Postgres and routine in schemas built by other tools.
		if err := schema.CheckIdent(local); err != nil {
			rep.add(name, "", "table name cannot be declared: "+err.Error(), "")
			continue
		}
		t, err := buildTable(r, name, local, byTable[name], built, rep)
		if err != nil {
			return nil, rep, err
		}
		built[name] = t
	}

	if err := r.Validate(); err != nil {
		return nil, rep, fmt.Errorf("introspect: the imported schema does not validate, "+
			"which means this package built something the DSL considers impossible: %w", err)
	}
	return r, rep, nil
}

// tableParts is one table's rows, gathered from the flat catalog.
type tableParts struct {
	table       tableRow
	columns     []columnRow
	constraints []constraintRow
	indexes     []indexRow
}

func groupByTable(cat *catalog) map[string]*tableParts {
	out := map[string]*tableParts{}
	part := func(name string) *tableParts {
		if p, ok := out[name]; ok {
			return p
		}
		p := &tableParts{}
		out[name] = p
		return p
	}
	for _, t := range cat.tables {
		part(t.Name).table = t
	}
	for _, c := range cat.columns {
		p := part(c.Table)
		p.columns = append(p.columns, c)
	}
	for _, c := range cat.constraints {
		p := part(c.Table)
		p.constraints = append(p.constraints, c)
	}
	for _, i := range cat.indexes {
		p := part(i.Table)
		p.indexes = append(p.indexes, i)
	}
	return out
}

// dependencyOrder returns table names ordered so that every foreign key's
// target is built before the table referencing it.
//
// A cycle is broken rather than refused. The DSL declares a reference by
// passing the target table's own value — schema.Ref("org", Org) — so a cycle is
// a Go initialisation cycle and no ordering fixes it. The declaration's answer
// is to make one side an ExternalRef, and that answer is available to the
// import too: emitting the back edge's table *first* leaves its target unbuilt,
// which is exactly the condition under which newField already produces an
// enforced ExternalRef.
//
// It used to report the cycle and drop every table on it. The advice was right
// for a declaration and impossible to follow from here — a consumer cannot make
// introspect's side an ExternalRef — so a drift gate diffing a declared table
// that *had* broken the cycle correctly reported it as absent from the database
// (issue #80). The workaround was to exclude one of the tables, which meant the
// gate could never cover it. Import and declaration now agree by construction,
// and which side was broken is noted on the Report.
func dependencyOrder(cat *catalog, byTable map[string]*tableParts, rep *Report) ([]string, error) {
	names := make([]string, 0, len(byTable))
	for n := range byTable {
		names = append(names, n)
	}
	sort.Strings(names)

	deps := map[string][]string{}
	for _, n := range names {
		seen := map[string]bool{}
		for _, c := range byTable[n].constraints {
			if c.Type != "f" || c.RefTable == "" || c.RefTable == n || seen[c.RefTable] {
				continue
			}
			seen[c.RefTable] = true
			deps[n] = append(deps[n], c.RefTable)
		}
	}

	var out []string
	state := map[string]int{} // 0 unvisited, 1 in progress, 2 done
	var visit func(string, []string)
	visit = func(n string, path []string) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		for _, d := range deps[n] {
			if _, known := byTable[d]; !known {
				continue // a reference out of the schema being read
			}
			// d is an ancestor on the path being walked, so n → d closes a
			// cycle. Breaking the edge here — rather than waiting for d —
			// emits n first, and a table emitted before its target is one
			// whose foreign key imports as an enforced ExternalRef.
			if state[d] == 1 {
				rep.note("%s.%s: the foreign key to %s closes a cycle (%s), "+
					"so it is imported as an enforced ExternalRef — the same spelling a "+
					"declaration is forced to use, since a cycle of Refs is a Go "+
					"initialisation cycle",
					n, refColumn(byTable[n], d), d,
					strings.Join(append(cycleFrom(path, d), n), " → "))
				continue
			}
			visit(d, append(path, n))
		}
		state[n] = 2
		out = append(out, n)
	}
	for _, n := range names {
		visit(n, nil)
	}
	return out, nil
}

// cycleFrom trims a walk down to the loop it closed, so the note names the
// cycle rather than the route taken to reach it.
func cycleFrom(path []string, start string) []string {
	for i, n := range path {
		if n == start {
			return append([]string{}, path[i:]...)
		}
	}
	return append([]string{}, path...)
}

// refColumn names the column carrying a table's foreign key to target, for the
// note above. Empty if it cannot be found, which would make the note read a
// little worse and nothing else.
func refColumn(p *tableParts, target string) string {
	if p == nil {
		return ""
	}
	for _, c := range p.constraints {
		if c.Type == "f" && c.RefTable == target && len(c.Columns) == 1 {
			return c.Columns[0]
		}
	}
	return ""
}

// localName strips a module prefix, reporting whether the table belongs to the
// module at all.
func localName(name, module string) (string, bool) {
	if module == "" {
		return name, true
	}
	prefix := module + "_"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

func buildTable(r *schema.Registry, name, local string, p *tableParts,
	built map[string]*schema.TableDef, rep *Report) (*schema.TableDef, error) {

	cons := classify(name, p.constraints, rep)

	fields := make([]schema.FieldSpec, 0, len(p.columns))
	// A column this package cannot declare takes its dependents with it. One
	// tsvector column used to abort the import of a whole database: the column
	// was skipped correctly, the index over it was not, and the registry then
	// failed its own validation with an error blaming this package for building
	// something impossible (issue #54).
	skipped := map[string]bool{}
	for _, col := range p.columns {
		f, ok := buildColumn(name, col, cons, built, rep)
		if !ok {
			skipped[col.Name] = true
			continue
		}
		fields = append(fields, f)
	}

	t := r.Table(local, fields...)
	if p.table.Comment != "" {
		t.Describe(p.table.Comment)
	}
	if cons.pk != nil && len(cons.pk.Columns) > 1 {
		if col, dependent := coversSkippedColumn(cons.pk.Columns, skipped); dependent {
			// The key itself is unimportable, so the table has no identity at
			// all. Reported rather than declared without it, because a table
			// silently missing its primary key is the quiet failure ADR-0014 is
			// about — every later diff would propose adding one.
			rep.add(name, cons.pk.Name, "primary key covers "+col+
				", which was not imported, so the key cannot be declared either", cons.pk.Def)
		} else {
			t.PrimaryKeyColumns(cons.pk.Columns...)
		}
	}
	if cons.pk != nil && cons.pk.Name != name+"_pkey" && !coversAnySkipped(cons.pk.Columns, skipped) {
		t.PrimaryKeyNamed(cons.pk.Name)
	}
	for _, e := range cons.exclusions {
		if col, dependent := coversSkippedColumn(excludeColumns(e, p.columns), skipped); dependent {
			rep.add(name, e.Name, "exclusion constrains "+col+
				", which was not imported, so the constraint cannot be declared either", e.Def())
			continue
		}
		t.AddExclude(e)
	}

	for _, c := range cons.tableChecks {
		if col, dependent := namesSkippedColumn(c.Expr, skipped); dependent {
			rep.add(name, c.Name, "check constrains "+col+
				", which was not imported, so the check cannot be declared either", c.Expr)
			continue
		}
		t.Check(c.Name, c.Expr)
	}
	for _, u := range cons.uniques {
		if col, dependent := coversSkippedColumn(u.Columns, skipped); dependent {
			rep.add(name, u.Name, "unique constraint covers "+col+
				", which was not imported, so the constraint cannot be declared either", u.Name)
			continue
		}
		// Pin the name only when it is not the one the convention derives, for
		// the reason the primary key above is treated the same way: a pinned
		// name that matches the convention is noise in the declaration, and a
		// declaration that omits a name Postgres did not choose is a rename.
		switch {
		case u.Deferrable != schema.NotDeferrable:
			// The whole-struct form, because neither shorthand carries the
			// clause — and a constraint imported without it diffs to nothing
			// against a database that has it, which is the blindness this
			// field exists to end (issue #154). AddUnique derives the name
			// when it is the conventional one, so the rule above still holds.
			if u.Name == name+"_"+strings.Join(u.Columns, "_")+"_key" {
				u.Name = ""
			}
			t.AddUnique(u)
		case u.Name == name+"_"+strings.Join(u.Columns, "_")+"_key":
			t.Unique(u.Columns...)
		default:
			t.UniqueNamed(u.Name, u.Columns...)
		}
	}
	for _, idx := range p.indexes {
		if idx.Expression || len(idx.Columns) == 0 {
			rep.add(name, idx.Name, "index is over an expression rather than plain columns, "+
				"which the DSL cannot declare", idx.Def)
			continue
		}
		if col, dependent := coversSkippedColumn(idx.Columns, skipped); dependent {
			rep.add(name, idx.Name, "index covers "+col+
				", which was not imported, so the index cannot be declared either", idx.Def)
			continue
		}
		t.AddIndex(schema.Index{
			Name: idx.Name, Columns: idx.Columns, Unique: idx.Unique,
			Method: indexMethod(idx.Method), Where: idx.Where,
			Opclasses: opclassesByColumn(idx),
			Orders:    ordersByColumn(idx),
			With:      storageParameters(idx.Options),
		})
	}
	return t, nil
}

// coversSkippedColumn reports whether an index names a column that was not
// imported, and which one.
func coversSkippedColumn(columns []string, skipped map[string]bool) (string, bool) {
	for _, c := range columns {
		if skipped[c] {
			return c, true
		}
	}
	return "", false
}

// namesSkippedColumn reports whether a CHECK expression mentions a column that
// was not imported.
//
// The match is on word boundaries over the expression text, which is a heuristic
// and is deliberately the conservative direction: a check wrongly kept would
// produce DDL naming a column the registry does not have, while a check wrongly
// dropped is one line in the report and a constraint the diff proposes adding —
// visible either way, and only one of them fails the whole import.
func namesSkippedColumn(expr string, skipped map[string]bool) (string, bool) {
	if len(skipped) == 0 {
		return "", false
	}
	for col := range skipped {
		if mentionsWord(expr, col) {
			return col, true
		}
	}
	return "", false
}

// mentionsWord reports whether s contains word delimited by something other
// than an identifier character, so that "search_vector" does not match inside
// "search_vector_backup".
func mentionsWord(s, word string) bool {
	isIdentByte := func(b byte) bool {
		return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
	}
	for i := 0; ; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(word)
		beforeOK := start == 0 || !isIdentByte(s[start-1])
		afterOK := end == len(s) || !isIdentByte(s[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + len(word)
	}
}

// indexMethod omits btree, which is the dialect default and which the DDL layer
// leaves off — so recording it would make an unchanged index look changed.
func indexMethod(m string) string {
	if m == "btree" {
		return ""
	}
	return m
}

// constraints is a table's constraints, sorted into what each one means for the
// columns it covers.
type constraints struct {
	pk          *constraintRow
	unique      map[string]constraintRow // by column, single-column only
	foreign     map[string]constraintRow // by column, single-column only
	exclusions  []schema.Exclusion
	enums       map[string][]string // by column, recovered from a CHECK
	enumName    map[string]string   // by column, the CHECK's own name
	tableChecks []schema.Check
	uniques     []schema.Unique // composite, table-level
}

// classify sorts a table's constraints, reporting the ones the DSL has no way
// to declare.
func classify(table string, rows []constraintRow, rep *Report) *constraints {
	c := &constraints{
		unique:   map[string]constraintRow{},
		foreign:  map[string]constraintRow{},
		enums:    map[string][]string{},
		enumName: map[string]string{},
	}
	for _, row := range rows {
		switch row.Type {
		case "n":
			// Postgres 18 records every NOT NULL as a constraint of its own.
			// The column already carries it through attnotnull, and importing
			// these as anything would invent constraints the DSL never emits.
			continue
		case "p":
			r := row
			c.pk = &r
			reportDeferral(table, row, rep, "a primary key")
		case "u":
			if len(row.Columns) != 1 {
				// A composite UNIQUE is a table-level constraint, not a column
				// one. It used to be reported, because approximating it with a
				// unique index would have diffed as a drop and a rebuild — the
				// index being a different object (issue #108).
				c.uniques = append(c.uniques, schema.Unique{
					Name:       row.Name,
					Columns:    row.Columns,
					Deferrable: deferralOf(row),
				})
				continue
			}
			c.unique[row.Columns[0]] = row
		case "f":
			if len(row.Columns) != 1 || len(row.RefCols) != 1 {
				rep.add(table, row.Name, "composite foreign key; the DSL declares single-column "+
					"references only", row.Def)
				continue
			}
			c.foreign[row.Columns[0]] = row
			reportDeferral(table, row, rep, "a foreign key")
		case "x":
			e, ok := schema.ParseExclusion(row.Def)
			if !ok {
				rep.add(table, row.Name, "exclusion constraint in a form this cannot read back "+
					"(a clause beyond USING, the element list and WHERE)", row.Def)
				continue
			}
			e.Name = row.Name
			c.exclusions = append(c.exclusions, e)
			reportDeferral(table, row, rep, "an exclusion")
		case "c":
			c.check(table, row, rep)
			reportDeferral(table, row, rep, "a check")
		default:
			rep.add(table, row.Name, "constraint of a kind the DSL cannot declare (contype "+
				row.Type+")", row.Def)
		}
	}
	return c
}

// deferralOf reads a constraint's check timing back into the declaration's
// vocabulary.
func deferralOf(row constraintRow) schema.Deferrable {
	switch {
	case row.Deferred:
		return schema.DeferredCheck
	case row.Deferrable:
		return schema.DeferrableCheck
	}
	return schema.NotDeferrable
}

// reportDeferral records a constraint whose check timing the DSL cannot say,
// which is every kind but UNIQUE.
//
// The consequence of not reporting it is narrower than dropping a constraint and
// points the same way: a migration that recreated a deferred constraint without
// its clause would break every write that relies on the deferral, and the drift
// gate — the thing that exists to catch precisely that — would stay green,
// because the declaration and the database were blind to the same property
// (issue #154). That is ADR-0016's failure mode stated about a field rather than
// about an object.
//
// It is a Skip rather than a Note: the registry genuinely does not describe the
// database, and reconciling it means keeping the hand-written migration that put
// the clause there.
func reportDeferral(table string, row constraintRow, rep *Report, kind string) {
	if !row.Deferrable {
		return
	}
	when := "deferrable"
	if row.Deferred {
		when = "deferrable and deferred by default"
	}
	rep.add(table, row.Name, kind+" that is "+when+
		"; the DSL declares deferral on UNIQUE constraints only, so this one has to stay "+
		"in a hand-written migration", row.Def)
}

// check decides whether a CHECK is an enum in disguise or a check in its own
// right. An enum is text plus a CHECK (ADR-0017), so this is the only place the
// two can be told apart.
func (c *constraints) check(table string, row constraintRow, rep *Report) {
	for _, col := range candidateEnumColumns(row) {
		if values, ok := enumValues(col, row.Expr); ok {
			c.enums[col] = values
			// The name is the one thing about an enum's check that the
			// expression does not carry, and losing it makes every later diff
			// propose dropping and re-adding the constraint (issue #53).
			c.enumName[col] = row.Name
			return
		}
	}
	c.tableChecks = append(c.tableChecks, schema.Check{Name: row.Name, Expr: row.Expr})
}

// candidateEnumColumns is the columns a CHECK covers, which for an enum's check
// is exactly one.
func candidateEnumColumns(row constraintRow) []string {
	if len(row.Columns) == 1 {
		return row.Columns
	}
	return nil
}

func buildColumn(table string, col columnRow, cons *constraints,
	built map[string]*schema.TableDef, rep *Report) (*schema.Field, bool) {

	if err := schema.CheckIdent(col.Name); err != nil {
		rep.add(table, col.Name, "column name cannot be declared: "+err.Error(), col.Type)
		return nil, false
	}
	if col.Generated != "" {
		rep.add(table, col.Name, "generated column, which the DSL cannot declare", col.Type)
		return nil, false
	}
	// An auto-incrementing integer key, in either of the two spellings Postgres
	// has for it: `attidentity` for a GENERATED … AS IDENTITY column, and a
	// nextval default for a serial, which records no identity at all and so is
	// invisible to the first check.
	//
	// Both used to be refused, honestly — the DSL could declare neither, and
	// importing a serial as an ordinary column whose default happens to name a
	// sequence produced DDL that does not run, since Diff renders no CREATE
	// SEQUENCE. The cost of that refusal was the whole construct: no
	// auto-incrementing integer key was expressible, so a table with one and
	// every module holding it stayed outside the gate (issue #132).
	//
	// The auto-ness is read here rather than left in the default, because a
	// serial's nextval names a sequence *object* and a declaration that carried
	// the expression verbatim would render a default binding to a sequence
	// nothing creates.
	auto, refused := columnAuto(col)
	if refused != "" {
		rep.add(table, col.Name, refused, col.Type)
		return nil, false
	}

	elemType, isArray := splitArrayType(col.Type)
	t, typeArg, scale, ok := columnType(elemType)
	if !ok {
		rep.add(table, col.Name, "column type "+col.Type+" has no equivalent in the DSL; "+
			"importing it as anything else would propose changing the real column", col.Type)
		return nil, false
	}
	// A refusal rather than a demotion to text, for the reason the round trip
	// exists: a column imported as something it is not produces a Diff that
	// proposes rewriting production data.
	if isArray && !schema.IsArrayElement(t) {
		rep.add(table, col.Name, "an array of "+string(t)+" is not an element type the DSL declares; "+
			"arrays hold scalars and have one dimension", col.Type)
		return nil, false
	}

	f := newField(col, t, typeArg, scale, isArray, cons, built, rep, table)
	if f == nil {
		return nil, false
	}
	if isArray {
		f.Array()
	}

	if !col.NotNull {
		f.Nullable()
	}
	switch auto {
	case schema.AutoSerial:
		// The nextval default is the serial and is not read as a default of its
		// own: a declaration carrying it would render a default naming a
		// sequence nothing creates, where the serial spelling makes Postgres
		// create it.
		f.Serial()
	case schema.AutoIdentity:
		f.Identity()
	case schema.AutoIdentityAlways:
		f.IdentityAlways()
	default:
		if d := columnDefault(col.Default, col.Type, t); d != nil {
			f.Default(d)
		}
	}
	if col.Comment != "" {
		f.Comment(col.Comment)
	}

	if u, isUnique := cons.unique[col.Name]; isUnique {
		f.Unique()
		// Pinned only when it differs from what the DDL layer would generate,
		// so an imported schema does not carry a name for every constraint it
		// happens to agree about.
		if u.Name != table+"_"+col.Name+"_key" {
			f.ConstraintNamed(u.Name)
		}
		switch deferralOf(u) {
		case schema.DeferredCheck:
			f.Deferred()
		case schema.DeferrableCheck:
			// The column form says "deferred" and not "deferrable but not
			// deferred", which is the setting that only matters to a
			// transaction issuing SET CONSTRAINTS. Reported rather than
			// rounded to either neighbour: importing it as deferred would
			// change when every other writer's statement is checked, and
			// importing it as not deferrable would take the option away.
			// AddUnique can say it, at the cost of naming the column twice.
			rep.add(table, u.Name, "a unique constraint that is deferrable and initially immediate; "+
				"Field.Deferred declares the initially-deferred form, so this one wants "+
				"AddUnique or a hand-written migration", u.Def)
		}
	}
	// Only a single-column key marks its column. A composite one is declared on
	// the table below, and marking each of its columns would say something the
	// DSL means differently — Field.PrimaryKey() is one key per column.
	if cons.pk != nil && len(cons.pk.Columns) == 1 && cons.pk.Columns[0] == col.Name {
		f.PrimaryKey()
	}
	return f, true
}

// newField creates the column in whichever of the DSL's forms it belongs to: a
// reference, an enum, or a plain typed column.
func newField(col columnRow, t schema.Type, typeArg, scale int, isArray bool, cons *constraints,
	built map[string]*schema.TableDef, rep *Report, table string) *schema.Field {

	// An array column is never a foreign key — Postgres has no such constraint
	// — so the reference branch below cannot apply to one.
	if fk, isRef := cons.foreign[col.Name]; isRef && !isArray {
		// built holds the tables finished so far, so a target that is missing
		// from it is either genuinely outside the schema or the table
		// currently being built — a self-reference, which is common enough
		// (manager_id, parent_id, reply_to) that reporting it as absent would
		// be both wrong and confusing.
		//
		// Either way only the *constraint* is unrepresentable. The column
		// itself is an ordinary typed column and is imported as one: dropping
		// it would leave the registry missing a column the database has, which
		// makes the next Diff propose adding a column that exists, or dropping
		// one that is load-bearing.
		if target := built[fk.RefTable]; target != nil {
			return refField(col, fk, target, rep, table)
		}
		if f := externalRefField(col, fk, t); f != nil {
			// The target is outside what was read — a table in another schema,
			// or one this import deliberately left out — but the constraint is
			// real and the declaration can say so without resolving it
			// (issue #55). Importing it as a plain column instead is what made
			// a drift gate propose dropping a live foreign key forever.
			//
			// A self-reference lands here too, and deliberately: it *is*
			// declarable, and only as this. `Ref("supervisor", Member)` inside
			// Member's own definition is a Go initialisation cycle, so the one
			// spelling available to a declaration is
			// `ExternalRef("supervisor", "members.id").Enforced()` — and an
			// import that reported it as undeclarable made the declared FK read
			// as permanent drift, with a second waiver for the implicit index
			// ExternalRef wants and the import did not (issue #82). Both sides
			// now produce the same field, so they agree by construction.
			return f
		} else if fk.RefTable == table {
			rep.add(table, col.Name, "self-referential foreign key whose target column "+
				"cannot be declared; the column is imported without it", fk.Def)
		} else {
			rep.add(table, col.Name, "foreign key points at "+fk.RefTable+
				", which is not in the schema being read, and its column or table name "+
				"cannot be declared; the column is imported without it", fk.Def)
		}
	}
	if values, isEnum := cons.enums[col.Name]; isEnum && t == schema.TypeText {
		f := schema.Enum(col.Name, values...)
		// Pinned only when it differs from what the DDL layer would generate,
		// for the reason the unique constraint's name is: a pin that agrees
		// with the convention is noise in every declaration that has one.
		if name := cons.enumName[col.Name]; name != "" && name != table+"_"+col.Name+"_check" {
			f.CheckNamed(name)
		}
		return f
	}
	return plainField(col.Name, t, typeArg, scale)
}

// externalRefField imports a foreign key whose target is not in the schema
// being read, as an enforced external reference.
//
// The column type comes from the column itself rather than from the target's
// primary key, because the target is a name here and nothing about it is
// resolvable — which is the entire point of the enforced form. Returns nil when
// the constraint cannot be declared at all, in which case the caller reports it
// and imports the column plainly.
func externalRefField(col columnRow, fk constraintRow, t schema.Type) *schema.Field {
	if len(fk.RefCols) != 1 {
		return nil
	}
	target := fk.RefTable + "." + fk.RefCols[0]
	f := schema.ExternalRef(relationName(col.Name), target).Enforced().OfType(t)
	if f.Name() != col.Name {
		f.Named(col.Name)
	}
	if _, _, ok := f.Desc().Ref.EnforcedTarget(); !ok {
		return nil
	}
	if onDelete, ok := referentialAction(fk.OnDelete); ok {
		f.OnDelete(onDelete)
	}
	if onUpdate, ok := referentialAction(fk.OnUpdate); ok {
		f.OnUpdate(onUpdate)
	}
	if fk.Name != fk.Table+"_"+col.Name+"_fkey" {
		f.ConstraintNamed(fk.Name)
	}
	return f
}

func refField(col columnRow, fk constraintRow, target *schema.TableDef,
	rep *Report, table string) *schema.Field {

	f := schema.Ref(relationName(col.Name), target)
	if f.Name() != col.Name {
		f.Named(col.Name)
	}
	if onDelete, ok := referentialAction(fk.OnDelete); ok {
		f.OnDelete(onDelete)
	} else {
		rep.add(table, fk.Name, "ON DELETE action "+fk.OnDelete+" has no equivalent in the DSL", fk.Def)
	}
	if onUpdate, ok := referentialAction(fk.OnUpdate); ok {
		f.OnUpdate(onUpdate)
	} else {
		rep.add(table, fk.Name, "ON UPDATE action "+fk.OnUpdate+" has no equivalent in the DSL", fk.Def)
	}
	if fk.Name != table+"_"+col.Name+"_fkey" {
		f.ConstraintNamed(fk.Name)
	}
	// The referenced column is whatever the foreign key names, which is not
	// always the target's primary key.
	if len(fk.RefCols) == 1 {
		f.Desc().Ref.Column = fk.RefCols[0]
	}
	return f
}

// relationName strips the conventional _id suffix, so that org_id imports as a
// relation called org and ?expand=org keeps working.
func relationName(column string) string {
	return strings.TrimSuffix(column, "_id")
}

// plainField builds the column. typeArg is the type's parenthesised argument
// where it has one, and which field it lands in depends on the type: a length
// for a varchar, a dimension for a vector, a precision for a numeric. scale is
// the numeric's second argument and is zero everywhere else.
func plainField(name string, t schema.Type, typeArg, scale int) *schema.Field {
	switch t {
	case schema.TypeText:
		return schema.Text(name)
	case schema.TypeVarchar:
		return schema.Varchar(name, typeArg)
	case schema.TypeSmallInt:
		return schema.SmallInt(name)
	case schema.TypeInt:
		return schema.Int(name)
	case schema.TypeBigInt:
		return schema.BigInt(name)
	case schema.TypeReal:
		return schema.Real(name)
	case schema.TypeFloat:
		return schema.Float(name)
	case schema.TypeNumeric:
		// Unbounded when there is no precision, which is the ordinary case and
		// the only one the DSL could express before #81.
		if typeArg == 0 {
			return schema.Numeric(name)
		}
		return schema.Numeric(name, typeArg, scale)
	case schema.TypeBool:
		return schema.Bool(name)
	case schema.TypeUUID:
		return schema.UUID(name)
	case schema.TypeTimestamp:
		return schema.Timestamp(name)
	case schema.TypeDate:
		return schema.Date(name)
	case schema.TypeTime:
		return schema.Time(name)
	case schema.TypeJSON:
		return schema.JSON(name)
	case schema.TypeBytes:
		return schema.Bytes(name)
	case schema.TypeVector:
		// Hidden comes with the constructor and is not optional, so an adopted
		// database's embedding column does not start being serialised into REST
		// responses the moment it is imported (ADR-0026).
		return schema.Vector(name, typeArg)
	}
	// columnType only returns types this switch covers, so reaching here means
	// the two have drifted apart.
	panic("introspect: no constructor for type " + string(t))
}

// opclassesByColumn pairs an index's operator classes with the columns they
// apply to, dropping the ones that are the type's default.
//
// The catalog reports one class per indexed column and the query blanks the
// defaults, so what arrives here is positional and mostly empty. Keying it by
// column is what lets the DDL layer render `(embedding vector_cosine_ops)`
// without knowing anything about index order.
func opclassesByColumn(idx indexRow) map[string]string {
	var out map[string]string
	for i, class := range idx.Opclasses {
		if class == "" || i >= len(idx.Columns) {
			continue
		}
		if out == nil {
			out = make(map[string]string, 1)
		}
		out[idx.Columns[i]] = class
	}
	return out
}

// ordersByColumn decodes pg_index.indoption into the per-column sort orders the
// schema declares, dropping the ones that are Postgres's default.
//
// The bitmask is Postgres's own: bit 0 is DESC, bit 1 is NULLS FIRST. Only a
// column that departs from the default gets an entry, so an ordinary index
// imports with a nil map and reads exactly as it did before this existed.
func ordersByColumn(idx indexRow) map[string]schema.IndexOrder {
	var out map[string]schema.IndexOrder
	for i, opt := range idx.Sort {
		if i >= len(idx.Columns) {
			break
		}
		order := schema.IndexOrder{Desc: opt&1 != 0}
		switch {
		case opt&2 != 0:
			order.Nulls = schema.NullsFirst
		default:
			order.Nulls = schema.NullsLast
		}
		// Normalised the way the declaration is, so the two compare equal: the
		// placement that follows from the direction is the default and is not
		// recorded.
		if order.Suffix() == "" {
			continue
		}
		if out == nil {
			out = make(map[string]schema.IndexOrder, 1)
		}
		out[idx.Columns[i]] = order
	}
	return out
}

// storageParameters turns reloptions — "m=16", "ef_construction=64" — into the
// map the schema declares. An option with no "=" is recorded with an empty
// value rather than dropped, so that nothing the database has disappears
// silently.
func storageParameters(options []string) map[string]string {
	if len(options) == 0 {
		return nil
	}
	out := make(map[string]string, len(options))
	for _, opt := range options {
		key, value, _ := strings.Cut(opt, "=")
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

// selectTables narrows the import to what Options asked for.
//
// It removes tables from the grouped catalog rather than filtering later, so
// everything downstream — the dependency order, the foreign keys, the report —
// sees one set of tables and cannot disagree about which. A foreign key into a
// table that was excluded is not lost by this: it imports as an enforced
// external reference, which is the shape a partial declaration needs anyway
// (issue #55).
func selectTables(byTable map[string]*tableParts, opts Options, rep *Report) {
	if len(opts.Only) > 0 {
		wanted := make(map[string]bool, len(opts.Only))
		for _, name := range opts.Only {
			wanted[name] = true
			if _, present := byTable[name]; !present {
				// Reported rather than ignored: a typo here silently shrinks
				// what a drift gate covers, and a gate that checks less than it
				// says is worse than no gate.
				rep.add(name, "", "named in Only but not present in the schema being read", "")
			}
		}
		for name := range byTable {
			if !wanted[name] {
				delete(byTable, name)
			}
		}
	}
	for _, name := range opts.Exclude {
		if _, present := byTable[name]; !present {
			rep.add(name, "", "named in Exclude but not present in the schema being read", "")
			continue
		}
		delete(byTable, name)
	}
}

// coversAnySkipped reports whether any of the columns was left out of the
// import. It is coversSkippedColumn without the name, for the callers that only
// need the answer.
func coversAnySkipped(columns []string, skipped map[string]bool) bool {
	_, any := coversSkippedColumn(columns, skipped)
	return any
}

// excludeColumns lists the table's columns an exclusion mentions, for the
// skipped-column check. Same approach and same reason as migrate's
// excludeCovers: the elements are expressions, so the columns are recognised by
// name rather than parsed out.
func excludeColumns(e schema.Exclusion, columns []columnRow) []string {
	body := e.Elements + " " + e.Where
	var out []string
	for _, c := range columns {
		if containsWord(body, c.Name) {
			out = append(out, c.Name)
		}
	}
	return out
}

func containsWord(s, name string) bool {
	for i := 0; i+len(name) <= len(s); i++ {
		if s[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		if j := i + len(name); j < len(s) && isIdentByte(s[j]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
