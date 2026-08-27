package migrate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mind-vm/sqlb/schema"
)

// This file renders Postgres DDL from schema declarations. It is the only
// dialect-specific part of the package: Format decides what a *runner* wants a
// file to look like, this decides what a *database* wants a statement to look
// like. Diff composes the two.

// quoteIdent renders an identifier.
//
// Doubling embedded quotes is the only escape Postgres defines. Names reaching
// here have normally already passed schema validation, which rejects anything
// needing it; this is the backstop for the names that bypass validation,
// namely those pinned with ConstraintNamed and PrimaryKeyNamed.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// sqlString renders a string as a SQL literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sqlType maps a logical column type onto a concrete Postgres type.
//
// Enums render as text with a CHECK constraint rather than as a native
// Postgres ENUM. A native enum cannot have a value removed at all, and adding
// one is a DDL statement that cannot run inside a transaction — so a schema
// change as ordinary as editing a list of statuses would become an
// irreversible, unbatchable migration. A CHECK constraint is replaced by
// dropping and adding it, which the diff engine already knows how to do.
func sqlType(d *schema.FieldDesc) (string, error) {
	base, err := scalarSQLType(d)
	if err != nil {
		return "", err
	}
	if d.Array {
		if !schema.IsArrayElement(d.Type) {
			return "", fmt.Errorf("migrate: column %q is an array of %q, which is not an element type", d.Name, d.Type)
		}
		return base + "[]", nil
	}
	return base, nil
}

func scalarSQLType(d *schema.FieldDesc) (string, error) {
	switch d.Type {
	case schema.TypeText, schema.TypeEnum:
		return "text", nil
	case schema.TypeVarchar:
		// A varchar with no length is text; Postgres treats them identically
		// and text is the name that says so.
		if d.Size > 0 {
			return fmt.Sprintf("varchar(%d)", d.Size), nil
		}
		return "text", nil
	case schema.TypeSmallInt:
		return "smallint", nil
	case schema.TypeInt:
		return "integer", nil
	case schema.TypeBigInt:
		return "bigint", nil
	case schema.TypeReal:
		return "real", nil
	case schema.TypeFloat:
		return "double precision", nil
	case schema.TypeNumeric:
		// A numeric with a precision is a different type from an unbounded
		// one, the way a varchar with a length is, so it is part of the type
		// name rather than a constraint on it (issue #81).
		switch {
		case d.Size > 0 && d.Scale != 0:
			return fmt.Sprintf("numeric(%d,%d)", d.Size, d.Scale), nil
		case d.Size > 0:
			return fmt.Sprintf("numeric(%d,0)", d.Size), nil
		}
		return "numeric", nil
	case schema.TypeBool:
		return "boolean", nil
	case schema.TypeUUID:
		return "uuid", nil
	case schema.TypeTimestamp:
		return "timestamptz", nil
	case schema.TypeDate:
		return "date", nil
	case schema.TypeTime:
		return "time", nil
	case schema.TypeJSON:
		return "jsonb", nil
	case schema.TypeBytes:
		return "bytea", nil
	case schema.TypeVector:
		// The dimension is part of the type name, which is why it is on the
		// declaration rather than being a constraint: Postgres will not store a
		// vector(768) value in a vector(1536) column, and the two are different
		// types in the catalog (ADR-0026).
		if d.Dim <= 0 {
			return "", fmt.Errorf("migrate: vector column %q has no dimension", d.Name)
		}
		return fmt.Sprintf("vector(%d)", d.Dim), nil
	}
	return "", fmt.Errorf("migrate: column %q has unknown type %q", d.Name, d.Type)
}

// widens reports whether every value of the from type fits in the to type, so
// that changing between them cannot lose data. Anything not listed here is
// treated as potentially lossy, which is the safe default: a false negative
// produces a migration that is commented out until reviewed, a false positive
// produces one that silently truncates.
func widens(from, to *schema.FieldDesc) bool {
	// Adding or removing the array is never a widening: Postgres will not cast
	// text to text[] without a USING clause, and the reverse discards every
	// element but one. Both directions need a migration a person wrote.
	if from.Array != to.Array {
		return false
	}
	switch from.Type {
	case schema.TypeSmallInt:
		switch to.Type {
		case schema.TypeInt, schema.TypeBigInt, schema.TypeNumeric:
			return true
		}
	case schema.TypeInt:
		switch to.Type {
		case schema.TypeBigInt, schema.TypeNumeric:
			return true
		}
	case schema.TypeBigInt:
		if to.Type == schema.TypeNumeric {
			return true
		}
	case schema.TypeReal:
		// real to double precision only. Not to numeric: the cast is legal but
		// it converts a binary float to an exact decimal, so the value that
		// comes back is the rounded decimal expansion of the stored
		// approximation rather than what anyone wrote. That is a conversion a
		// person decides on, not one a generated migration performs unasked.
		if to.Type == schema.TypeFloat {
			return true
		}
	case schema.TypeVarchar:
		switch to.Type {
		case schema.TypeText:
			return true
		case schema.TypeVarchar:
			// Zero means unbounded, so it accepts anything.
			return to.Size == 0 || to.Size >= from.Size
		}
	case schema.TypeNumeric:
		// Same rule as varchar, one dimension at a time: an unbounded numeric
		// accepts anything, and a bounded one accepts a bounded one whose
		// integer part and fractional part both fit. Narrowing either is a
		// migration a person writes, because Postgres rounds the scale and
		// errors on the precision.
		if to.Type != schema.TypeNumeric {
			return false
		}
		if to.Size == 0 {
			return true
		}
		if from.Size == 0 {
			return false
		}
		return to.Size-to.Scale >= from.Size-from.Scale && to.Scale >= from.Scale
	case schema.TypeEnum:
		// An enum is already text; only the CHECK constraint changes.
		return to.Type == schema.TypeText
	}
	return false
}

// Lock hazards.
//
// Most DDL is a catalog write: it takes ACCESS EXCLUSIVE, holds it for
// microseconds, and nobody notices. The statements below are the ones whose
// lock is held for a time proportional to the number of rows — because they
// rewrite the table, scan it, or build an index over it — and those are the
// ones that turn a routine migration into an outage.
//
// Knowing which is which is Postgres knowledge, so it lives here with the rest
// of it. Where the remedy is a fixed rewrite, Unblock performs it and the note
// says so. Where it needs a decision the schema does not contain — how large a
// backfill batch should be, when a cutover can happen — the note is all there
// is, and authoring the rest is a person's job (ADR-0014).

// Lock modes, spelled as Postgres spells them.
const (
	lockAccessExclusive   = "ACCESS EXCLUSIVE"
	lockShareRowExclusive = "SHARE ROW EXCLUSIVE"
)

// notValidRemedy is the two-step sequence that turns a validating ADD
// CONSTRAINT into a brief lock and a slow check that lets writes through. It
// applies to CHECK and FOREIGN KEY alike, which is why it is written once.
const notValidRemedy = "Add it NOT VALID first, then VALIDATE CONSTRAINT in a " +
	"later migration. The NOT VALID add still takes a lock of its own, but holds " +
	"it only for as long as the catalog write takes; the validation that follows " +
	"takes SHARE UPDATE EXCLUSIVE, which readers and writers pass through. " +
	"migrate.Unblock writes that sequence for you."

// rewrites reports whether changing a column from one type to the other makes
// Postgres rewrite every row, rather than accepting the new type over the bytes
// already stored.
//
// Only the text family is exempt, because that is the only place this package's
// type system distinguishes types that are binary coercible: a varchar losing
// or widening its length limit changes what is *checked*, not what is stored.
// Everything else is treated as a rewrite, which is the safe direction — a
// false positive is a warning nobody needed, a false negative is the outage.
func rewrites(from, to *schema.FieldDesc) bool {
	// varchar(n)[] to text[] is the same catalog-only change as varchar(n) to
	// text; anything that gains or loses the array rewrites.
	if from.Array != to.Array {
		return true
	}
	if from.Type != schema.TypeVarchar {
		return true
	}
	switch to.Type {
	case schema.TypeText:
		return false
	case schema.TypeVarchar:
		// Zero means unbounded. A shorter limit has to be checked against
		// every row, which is a scan rather than a rewrite, but it is not free
		// and the distinction does not change what to do about it.
		return to.Size != 0 && to.Size < from.Size
	}
	return true
}

// typeChangeHazard describes what changing a column's type costs.
func typeChangeHazard(table string, from, to *schema.FieldDesc) (lock, hazard string) {
	if !rewrites(from, to) {
		return "", ""
	}
	return lockAccessExclusive, "rewriting " + table + "." + to.Name +
		" reads and writes every row in the table, and blocks all access until it " +
		"finishes. There is no in-place version and no concurrent one. On a table " +
		"too large to lock, add a second column, backfill it in batches, and swap — " +
		"a sequence this cannot generate, because only you know what a batch costs " +
		"and when the cutover can happen."
}

// setNotNullHazard describes what requiring a column costs.
func setNotNullHazard(table, column string) (lock, hazard string) {
	return lockAccessExclusive, "proving that no row has NULL in " + table + "." + column +
		" scans the whole table with all access blocked. On a large table, add " +
		"CHECK (" + quoteIdent(column) + " IS NOT NULL) NOT VALID, VALIDATE CONSTRAINT " +
		"it, and only then SET NOT NULL: Postgres 12 and later find the validated " +
		"check and skip the scan entirely. migrate.Unblock writes that sequence " +
		"for you."
}

// constraintHazard describes what adding a constraint to a table that already
// holds rows costs. A table created by the same migration is empty by
// construction and its constraints are free — the same reason its indexes do
// not need CONCURRENTLY.
func constraintHazard(table string, c constraint) (lock, hazard string) {
	switch {
	case c.ref != nil:
		// Both tables are locked against writes: the referencing one is
		// scanned, and the referenced one has to hold still while it is.
		return lockShareRowExclusive, "checking that every row of " + table +
			" already points at a row of " + c.ref.qualified() + " blocks writes to both " +
			"tables until it finishes. " + notValidRemedy
	case c.unique:
		return lockAccessExclusive, "the unique constraint is enforced by an index, " +
			"and building it over every row of " + table + " blocks all access " +
			"until it finishes. Build the index first with CREATE UNIQUE INDEX " +
			"CONCURRENTLY, then adopt it with ADD CONSTRAINT ... USING INDEX, which " +
			"holds the lock only for as long as the catalog write takes. " +
			"migrate.Unblock writes that sequence for you."
	}
	return lockAccessExclusive, "checking that every row of " + table +
		" already satisfies the constraint scans the whole table with all access " +
		"blocked. " + notValidRemedy
}

// renderDefault renders a column default for a DEFAULT clause.
//
// It has two callers with different needs: one writes the result into DDL, the
// other compares the current and target renderings to decide whether the
// default changed at all. Both pass the same opts, which is what keeps the
// comparison honest — resolving one side against a different Postgres version
// from the other would report a change that is only a change of spelling.
func renderDefault(d *schema.Default, opts diffOptions) (string, error) {
	if d == nil {
		return "", nil
	}
	if d.Raw != "" {
		return opts.resolve(d.Raw), nil
	}
	return literal(d.Value)
}

// literal renders a Go value as a SQL literal. Only the types a default can
// plausibly hold are supported; anything else is an authoring mistake with an
// obvious fix, so it names the fix.
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return sqlString(x), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", x), nil
	case float32, float64:
		return fmt.Sprintf("%v", x), nil
	case time.Time:
		return sqlString(x.Format(time.RFC3339Nano)), nil
	}
	return "", fmt.Errorf("migrate: cannot render %T as a SQL literal; use schema.Expr for anything else", v)
}

// columnDef renders a column's definition: everything after the name in a
// CREATE TABLE, and the whole of an ADD COLUMN.
func columnDef(d *schema.FieldDesc, opts diffOptions) (string, error) {
	t, err := sqlType(d)
	if err != nil {
		return "", err
	}
	// A serial is spelled as its own type name, and letting Postgres expand it
	// is what makes the sequence exist at all: `bigserial` creates the
	// sequence, sets the default and marks it OWNED BY the column, in one word.
	// Rendering `bigint` plus a CREATE SEQUENCE would be three statements that
	// have to agree about a name, and Diff would have to order them.
	//
	// It works the same in ALTER TABLE ADD COLUMN, which is the other caller
	// here, so neither site needs to know.
	if d.Auto == schema.AutoSerial {
		if t, err = serialSQLType(d); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	b.WriteString(quoteIdent(d.Name) + " " + t)
	if !d.Nullable {
		b.WriteString(" NOT NULL")
	}
	if d.Auto.Identity() {
		b.WriteString(" " + identityClause(d.Auto))
	}
	if d.Default != nil {
		def, err := renderDefault(d.Default, opts)
		if err != nil {
			return "", fmt.Errorf("migrate: column %q: %w", d.Name, err)
		}
		b.WriteString(" DEFAULT " + def)
	}
	return b.String(), nil
}

// serialSQLType is the serial spelling of an integer type.
//
// Kept out of scalarSQLType on purpose. That function answers "what type is
// this column", and the answer for a bigserial column is bigint — which is what
// the database reports, what a type change has to compare against, and what an
// ALTER COLUMN TYPE may name. `bigserial` is not a type, it is a macro for one,
// and putting it in the type function would make every diff over a serial
// column propose changing bigint to bigserial forever.
func serialSQLType(d *schema.FieldDesc) (string, error) {
	switch d.Type {
	case schema.TypeSmallInt:
		return "smallserial", nil
	case schema.TypeInt:
		return "serial", nil
	case schema.TypeBigInt:
		return "bigserial", nil
	}
	return "", fmt.Errorf("migrate: column %q is a serial over %q, which has no serial spelling; "+
		"a serial counts, and only smallint, int and bigint do", d.Name, d.Type)
}

func identityClause(a schema.Auto) string {
	if a == schema.AutoIdentityAlways {
		return "GENERATED ALWAYS AS IDENTITY"
	}
	return "GENERATED BY DEFAULT AS IDENTITY"
}

// hasForeignKey reports whether the column produces a FOREIGN KEY.
//
// An external reference deliberately does not — that is the whole point of
// module isolation (ADR-0015) — unless it was declared Enforced, which is the
// adoption case: a live constraint whose target table has not been declared
// yet, and which a diff would otherwise propose dropping (issue #55).
func hasForeignKey(d *schema.FieldDesc) bool {
	if d.Ref == nil {
		return false
	}
	if d.Ref.External {
		_, _, _, ok := d.Ref.EnforcedTarget()
		return ok
	}
	return d.Ref.Table != nil
}

// Constraint names follow the conventions Postgres itself uses, so that a
// schema imported from an existing database matches without every constraint
// needing to be pinned by hand.

func primaryKeyName(t *schema.TableDef) string {
	if n := t.PrimaryKeyName(); n != "" {
		return n
	}
	return t.Name() + "_pkey"
}

// uniqueConstraintName names a column's UNIQUE constraint. ConstraintName
// pins the foreign key when the column is a real reference, so a reference
// that is also unique takes the generated name for its unique constraint.
func uniqueConstraintName(t *schema.TableDef, d *schema.FieldDesc) string {
	if d.ConstraintName != "" && !hasForeignKey(d) {
		return d.ConstraintName
	}
	return t.Name() + "_" + d.Name + "_key"
}

func foreignKeyName(t *schema.TableDef, d *schema.FieldDesc) string {
	if d.ConstraintName != "" {
		return d.ConstraintName
	}
	return t.Name() + "_" + d.Name + "_fkey"
}

func enumCheckName(t *schema.TableDef, d *schema.FieldDesc) string {
	if d.CheckName != "" {
		return d.CheckName
	}
	return t.Name() + "_" + d.Name + "_check"
}

// checkConstraintName falls back to a positional name for a check declared
// without one, so that it can still be diffed by name later.
func checkConstraintName(t *schema.TableDef, c schema.Check, i int) string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("%s_check%d", t.Name(), i+1)
}

// enumCheckExpr constrains an enum column to its declared values.
//
// An enum array is constrained with containment rather than IN: the check has
// to hold for every element, and `col IN (...)` compares the whole array to
// each value instead — which is not merely wrong but permissive, admitting any
// array at all. Containment against the declared set says exactly the intended
// thing, and it is true of the empty array, which is what an array column with
// no values in it should be.
func enumCheckExpr(d *schema.FieldDesc) string {
	vals := make([]string, len(d.EnumValues))
	for i, v := range d.EnumValues {
		vals[i] = sqlString(v)
	}
	if d.Array {
		return quoteIdent(d.Name) + " <@ ARRAY[" + strings.Join(vals, ", ") + "]::text[]"
	}
	return quoteIdent(d.Name) + " IN (" + strings.Join(vals, ", ") + ")"
}

// constraint is one named table constraint, reduced to a comparable form.
// The diff compares def strings: two constraints with the same name and the
// same def are the same constraint.
type constraint struct {
	name string
	def  string // the SQL after CONSTRAINT <name>, e.g. `UNIQUE ("slug")`

	// fk marks a foreign key. These are ordered separately from other
	// constraints because they are the only ones that depend on another table
	// existing.
	fk bool

	// unique marks a PRIMARY KEY or UNIQUE, the two Postgres enforces by
	// building an index rather than by scanning. They cost differently enough
	// to be worth telling apart when saying what adding one locks, and the
	// index is something a migration can build for itself beforehand.
	unique bool
	// pk narrows that to a PRIMARY KEY, which is spelled differently when
	// adopting an index and carries a NOT NULL the plain UNIQUE does not.
	pk bool
	// cols are the columns a unique constraint covers, which is what the index
	// backing it has to be built over.
	cols []string
	// deferrable is the DEFERRABLE clause, already part of def. It is kept
	// separately because the two-step form — build the index concurrently,
	// then ADD CONSTRAINT ... USING INDEX — renders the constraint from its
	// parts rather than from def, and a clause dropped there would produce a
	// constraint the next diff proposes replacing (issue #154).
	deferrable string

	// covers are the columns of its own table the constraint names, for every
	// kind of constraint rather than only the unique ones. It answers a
	// different question from cols — whether the constraint can be added yet,
	// rather than what an index would be built over — and a diff uses it to
	// recognise a constraint that depends on a column being added by the same
	// migration.
	covers []string
	// handWritten marks a definition whose column references are arbitrary SQL
	// this package did not build: a table-level CHECK. Which columns such an
	// expression names cannot be known without parsing it, so the constraint
	// depends on its whole table rather than on a list. See dependencyOf.
	handWritten bool

	// enum holds the permitted values when this is an enum column's CHECK,
	// so that removing a value can be told from adding one.
	enum []string

	// ref holds a foreign key's parts. A def is otherwise an opaque string,
	// and a foreign key is the one constraint naming something outside its own
	// table — so applying a rename to it has to know which identifier is
	// which rather than substituting across the whole line.
	ref *fkRef
}

// fkRef is a foreign key's parts, kept apart so a rename can re-render it.
type fkRef struct {
	column    string // the constrained column, in this table
	schema    string // the referenced table's schema; empty for this one
	table     string // the referenced table
	refColumn string // the referenced column, in that table
	actions   string // " ON DELETE CASCADE" and the like; no identifiers in it
}

// qualified is the referenced table as prose names it: unquoted, and carrying
// the schema only when the declaration did.
func (r fkRef) qualified() string {
	if r.schema == "" {
		return r.table
	}
	return r.schema + "." + r.table
}

// target renders the referenced table for DDL, qualified only when the
// declaration qualified it. An unqualified name is left to the search path,
// which is what every other identifier this package renders already does.
func (r fkRef) target() string {
	if r.schema == "" {
		return quoteIdent(r.table)
	}
	return quoteIdent(r.schema) + "." + quoteIdent(r.table)
}

func (r fkRef) render() string {
	return fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)%s",
		quoteIdent(r.column), r.target(), quoteIdent(r.refColumn), r.actions)
}

// renamed returns the constraint as it will read once cols and tables have been
// renamed — which is how a constraint that merely follows a renamed column is
// recognised as unchanged instead of being dropped and rebuilt.
//
// A hand-written CHECK expression is rewritten only where it spells a column in
// quotes, which in Postgres is unambiguously an identifier: a string literal is
// single-quoted, so a substitution cannot reach inside one. An expression that
// spells its columns bare is left exactly as written, because telling an
// identifier from a keyword or a function name there would mean parsing SQL,
// and a generator that guesses at that produces something plausible and wrong.
// Such a check is dropped and re-added instead — correct and cheap, since
// Postgres validates it against the existing rows either way.
//
// The result of this is only ever compared, never rendered: a drop emits the
// original definition and an add emits the target's. So the worst a rewrite
// that misreads hand-written SQL can do is fail to match, which costs a drop
// and an add. It cannot produce a statement.
func (c constraint) renamed(cols, tables map[string]string) constraint {
	if len(cols) == 0 && len(tables) == 0 {
		return c
	}
	c.covers = renameAll(cols, c.covers)
	if c.ref != nil {
		r := *c.ref
		r.column = rename(cols, r.column)
		// A rename map names tables in the schema being migrated, so a target
		// in another one is not in it — and a table there sharing a name with
		// one being renamed here would otherwise be rewritten to a name its
		// own schema has never had.
		if r.schema == "" {
			r.table = rename(tables, r.table)
		}
		c.ref = &r
		c.def = r.render()
		return c
	}
	c.def = rewriteIdents(c.def, cols)
	c.cols = renameAll(cols, c.cols)
	return c
}

func renameAll(m map[string]string, names []string) []string {
	if len(names) == 0 {
		return names
	}
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = rename(m, name)
	}
	return out
}

func rename(m map[string]string, name string) string {
	if to, ok := m[name]; ok {
		return to
	}
	return name
}

// rewriteIdents maps the quoted identifiers in a rendered definition.
//
// Every definition this package renders spells a column with quoteIdent and a
// value with sqlString, so a quoted identifier is unambiguously a column name
// and a substitution cannot reach inside a string literal. Text that arrived
// from somewhere else — a hand-written CHECK expression, a partial index
// predicate — spells its columns bare and is left untouched, which is the
// intent: see constraint.renamed.
func rewriteIdents(def string, m map[string]string) string {
	if len(m) == 0 || !strings.Contains(def, `"`) {
		return def
	}
	var b strings.Builder
	for i := 0; i < len(def); {
		if def[i] != '"' {
			b.WriteByte(def[i])
			i++
			continue
		}
		name, end, ok := scanIdent(def, i)
		if !ok {
			b.WriteString(def[i:]) // unterminated: not ours to interpret
			return b.String()
		}
		b.WriteString(quoteIdent(rename(m, name)))
		i = end
	}
	return b.String()
}

// scanIdent reads the quoted identifier starting at s[i], returning its
// unescaped text and the index just past its closing quote.
func scanIdent(s string, i int) (string, int, bool) {
	var name strings.Builder
	for j := i + 1; j < len(s); j++ {
		if s[j] != '"' {
			name.WriteByte(s[j])
			continue
		}
		if j+1 < len(s) && s[j+1] == '"' {
			name.WriteByte('"')
			j++
			continue
		}
		return name.String(), j + 1, true
	}
	return "", 0, false
}

// constraints returns every named constraint the table declares, in a stable
// order.
//
// Foreign keys are included here rather than inlined into CREATE TABLE so that
// one code path serves both a new table and an altered one, and so that table
// creation never has to be ordered by dependency — the references are added
// once every table exists.
func constraints(t *schema.TableDef) []constraint {
	var out []constraint

	if pk := t.PrimaryKey(); pk != nil {
		out = append(out, constraint{
			name:   primaryKeyName(t),
			def:    "PRIMARY KEY (" + quoteIdent(pk.Name()) + ")",
			unique: true,
			pk:     true,
			cols:   []string{pk.Name()},
			covers: []string{pk.Name()},
		})
	} else if cols := t.CompositeKey(); len(cols) > 0 {
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = quoteIdent(c)
		}
		out = append(out, constraint{
			name:   primaryKeyName(t),
			def:    "PRIMARY KEY (" + strings.Join(quoted, ", ") + ")",
			unique: true,
			pk:     true,
			cols:   append([]string(nil), cols...),
			covers: append([]string(nil), cols...),
		})
	}

	for _, e := range t.Exclusions() {
		out = append(out, constraint{
			name: e.Name,
			def:  e.Def(),
			// Not `unique`. That flag says a constraint is backed by an index a
			// migration could build for itself beforehand with CREATE UNIQUE
			// INDEX CONCURRENTLY and then adopt — and there is no such
			// spelling for an exclusion: ADD CONSTRAINT ... EXCLUDE builds its
			// own index and Postgres offers no USING INDEX form for it. Saying
			// otherwise would make Unblock propose a rewrite that does not
			// exist.
			covers: excludeCovers(t, e),
		})
	}

	for _, f := range t.StoredFields() {
		d := f.Desc()
		if d.Unique && !d.PrimaryKey {
			out = append(out, constraint{
				name:       uniqueConstraintName(t, d),
				def:        "UNIQUE (" + quoteIdent(d.Name) + ")" + d.UniqueDeferrable.Suffix(),
				unique:     true,
				cols:       []string{d.Name},
				covers:     []string{d.Name},
				deferrable: d.UniqueDeferrable.Suffix(),
			})
		}
		if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
			// The expression is rendered from the column and its values, so
			// which column it names is known here even though it is a CHECK.
			out = append(out, constraint{
				name:   enumCheckName(t, d),
				def:    "CHECK (" + enumCheckExpr(d) + ")",
				enum:   d.EnumValues,
				covers: []string{d.Name},
			})
		}
		if hasForeignKey(d) {
			ref := foreignKeyRef(d)
			out = append(out, constraint{
				name:   foreignKeyName(t, d),
				def:    ref.render(),
				fk:     true,
				ref:    &ref,
				covers: []string{d.Name},
			})
		}
	}

	// Table-level UNIQUE constraints. These carry cols like the per-column
	// form above, so a migration can build the backing index beforehand and
	// adopt it, rather than holding a write lock while ADD CONSTRAINT builds
	// one inline.
	for _, u := range t.Uniques() {
		cols := make([]string, len(u.Columns))
		quoted := make([]string, len(u.Columns))
		for i, c := range u.Columns {
			cols[i] = c
			quoted[i] = quoteIdent(c)
		}
		out = append(out, constraint{
			name:       u.Name,
			def:        "UNIQUE (" + strings.Join(quoted, ", ") + ")" + u.Deferrable.Suffix(),
			unique:     true,
			cols:       cols,
			covers:     cols,
			deferrable: u.Deferrable.Suffix(),
		})
	}

	for i, c := range t.Checks() {
		out = append(out, constraint{
			name:        checkConstraintName(t, c, i),
			def:         "CHECK (" + c.Expr + ")",
			handWritten: true,
		})
	}

	return out
}

// duplicateConstraint reports two constraints on one table sharing a name.
//
// Postgres refuses `CREATE TABLE` carrying two constraints with the same name,
// and the collision is easy to write because half of it is invisible: an Enum
// column's CHECK is named `<table>_<column>_check` with nothing in the Go
// source saying so, and that is exactly the name somebody writing a second
// constraint about the same column reaches for (#303).
//
// It has to be caught here rather than by the shadow database, which replays
// the committed history and diffs the result — the file about to be written is
// not itself applied, so generation was clean and the failure surfaced on the
// next migrate run, in CI, or at deploy. The whole cost is that distance; the
// DDL is wrong the moment it is written.
//
// The diff path loses the second one silently instead, because it keys
// constraints by name (byName) — same mistake, no error at all.
func duplicateConstraint(t *schema.TableDef) error {
	seen := make(map[string]constraint, 8)
	for _, c := range constraints(t) {
		first, ok := seen[c.name]
		if !ok {
			seen[c.name] = c
			continue
		}
		// Byte-identical definitions are one constraint declared twice, which
		// is still a refusal — Postgres rejects the DDL either way — but the
		// advice is different, so the message says which it is.
		if first.def == c.def {
			return fmt.Errorf("migrate: %s declares the constraint %q twice, with the same definition %s; remove one",
				t.Name(), c.name, first.def)
		}
		return fmt.Errorf("migrate: %s has two constraints named %q — %s and %s — and Postgres accepts one; %s",
			t.Name(), c.name, constraintSource(first), constraintSource(c), renameAdvice(first, c))
	}
	return nil
}

// constraintSource says where a constraint came from, in the terms the schema
// is written in. The generated ones have no name in the Go source at all, so
// naming the column and the constructor is the only way to point at them.
func constraintSource(c constraint) string {
	switch {
	case c.pk:
		return "the primary key"
	case c.fk:
		return "a reference on " + quoteList(c.covers)
	case c.unique:
		return "a uniqueness constraint on " + quoteList(c.cols)
	case len(c.enum) > 0:
		return "an Enum column's generated check on " + quoteList(c.covers)
	case c.handWritten:
		return "an explicit Check"
	}
	return "a check on " + quoteList(c.covers)
}

// renameAdvice names the half that can be moved. A generated constraint has no
// name to change without changing the schema's shape, so the explicit one is
// what gets renamed — and naming it for what it asserts rather than for its
// column is what keeps it from colliding again.
func renameAdvice(a, b constraint) string {
	if a.handWritten || b.handWritten {
		return "name the explicit Check for what it asserts rather than for its column"
	}
	return "give one of them a name of its own"
}

// quoteList renders column names for a diagnostic: `"a"`, `"a" and "b"`.
func quoteList(cols []string) string {
	if len(cols) == 0 {
		return "this table"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = strconv.Quote(c)
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
}

func foreignKeyRef(d *schema.FieldDesc) fkRef {
	r := d.Ref
	var refSchema, table, col string
	if r.External {
		// The target is a name rather than a declaration, which is the point:
		// the constraint can be emitted without the target table being in this
		// schema at all — and "auth.users.id" says so outright, for a target in
		// a schema this application shares the database with but does not own.
		refSchema, table, col, _ = r.EnforcedTarget()
	} else {
		table, col = r.Table.Name(), r.Column
		if col == "" {
			if pk := r.Table.PrimaryKey(); pk != nil {
				col = pk.Name()
			}
		}
	}
	out := fkRef{column: d.Name, schema: refSchema, table: table, refColumn: col}
	// NO ACTION is the Postgres default, so emitting it would only add noise
	// and make a diff against an introspected schema look like a change.
	if r.OnDelete != "" && r.OnDelete != schema.NoAction {
		out.actions += " ON DELETE " + string(r.OnDelete)
	}
	if r.OnUpdate != "" && r.OnUpdate != schema.NoAction {
		out.actions += " ON UPDATE " + string(r.OnUpdate)
	}
	return out
}

// createTable renders CREATE TABLE with its columns and every constraint that
// does not depend on another table, followed by any COMMENT statements.
func createTable(t *schema.TableDef, opts diffOptions) (string, error) {
	var lines []string
	for _, f := range t.StoredFields() {
		def, err := columnDef(f.Desc(), opts)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    "+def)
	}
	for _, c := range constraints(t) {
		if c.fk {
			continue
		}
		lines = append(lines, "    CONSTRAINT "+quoteIdent(c.name)+" "+c.def)
	}

	var b strings.Builder
	if len(lines) == 0 {
		// Postgres accepts a table with no columns, and refusing it here would
		// be a second place that decides what a valid schema is.
		b.WriteString("CREATE TABLE " + quoteIdent(t.Name()) + " ();")
	} else {
		b.WriteString("CREATE TABLE " + quoteIdent(t.Name()) + " (\n")
		b.WriteString(strings.Join(lines, ",\n"))
		b.WriteString("\n);")
	}
	for _, c := range commentStatements(t) {
		b.WriteString("\n" + c)
	}
	return b.String(), nil
}

// commentStatements renders the table's and its columns' descriptions.
func commentStatements(t *schema.TableDef) []string {
	var out []string
	if t.Comment() != "" {
		out = append(out, commentOnTable(t.Name(), t.Comment()))
	}
	for _, f := range t.StoredFields() {
		if d := f.Desc(); d.Comment != "" {
			out = append(out, commentOnColumn(t.Name(), d.Name, d.Comment))
		}
	}
	return out
}

// commentOnTable renders a table comment. An empty comment renders as NULL,
// which is how Postgres removes one.
func commentOnTable(table, comment string) string {
	return "COMMENT ON TABLE " + quoteIdent(table) + " IS " + commentValue(comment) + ";"
}

// commentOnView is commentOnTable's counterpart for a view — Postgres
// refuses COMMENT ON TABLE against a relation of kind 'v'.
func commentOnView(view, comment string) string {
	return "COMMENT ON VIEW " + quoteIdent(view) + " IS " + commentValue(comment) + ";"
}

func commentOnColumn(table, column, comment string) string {
	return "COMMENT ON COLUMN " + quoteIdent(table) + "." + quoteIdent(column) +
		" IS " + commentValue(comment) + ";"
}

func commentValue(s string) string {
	if s == "" {
		return "NULL"
	}
	return sqlString(s)
}

func dropTable(t *schema.TableDef) string {
	return "DROP TABLE " + quoteIdent(t.Name()) + ";"
}

// createView renders CREATE VIEW. There is no CREATE OR REPLACE here — a
// replace only works when the new definition keeps every existing output
// column, in the same order, with the same type, which a diff would have to
// verify to rely on; DROP then CREATE is correct unconditionally, at the
// cost of a view briefly not existing mid-migration, which is fine for
// something with no rows and (in v1) no dependents recorded to preserve
// across the gap. IF EXISTS on the drop half is what lets one Change cover
// both "the view is new" and "the view changed" without the differ having
// to tell them apart.
func createView(t *schema.TableDef) string {
	var b strings.Builder
	b.WriteString("DROP VIEW IF EXISTS " + quoteIdent(t.Name()) + ";\n")
	b.WriteString("CREATE VIEW " + quoteIdent(t.Name()) + " AS\n" + t.ViewQuery() + ";")
	// Not commentStatements: that renders COMMENT ON TABLE, which Postgres
	// refuses for a view ("... is not a table") — the relation-level comment
	// needs its own COMMENT ON VIEW, and only that one line differs; a
	// column's own comment uses the same COMMENT ON COLUMN either kind of
	// relation accepts.
	if t.Comment() != "" {
		b.WriteString("\n" + commentOnView(t.Name(), t.Comment()))
	}
	for _, f := range t.StoredFields() {
		if d := f.Desc(); d.Comment != "" {
			b.WriteString("\n" + commentOnColumn(t.Name(), d.Name, d.Comment))
		}
	}
	return b.String()
}

func dropView(t *schema.TableDef) string {
	return "DROP VIEW IF EXISTS " + quoteIdent(t.Name()) + ";"
}

func addColumn(table string, d *schema.FieldDesc, opts diffOptions) (string, error) {
	def, err := columnDef(d, opts)
	if err != nil {
		return "", err
	}
	return "ALTER TABLE " + quoteIdent(table) + " ADD COLUMN " + def + ";", nil
}

func dropColumn(table, column string) string {
	return "ALTER TABLE " + quoteIdent(table) + " DROP COLUMN " + quoteIdent(column) + ";"
}

func alterColumn(table, column, action string) string {
	return "ALTER TABLE " + quoteIdent(table) + " ALTER COLUMN " + quoteIdent(column) +
		" " + action + ";"
}

// Renames. Each of these touches the catalog only: no table is rewritten, no
// index is rebuilt, and nothing is locked for longer than the statement takes.
// That is the whole reason for preferring them over a drop and an add.

func renameTable(from, to string) string {
	return "ALTER TABLE " + quoteIdent(from) + " RENAME TO " + quoteIdent(to) + ";"
}

func renameColumn(table, from, to string) string {
	return "ALTER TABLE " + quoteIdent(table) + " RENAME COLUMN " +
		quoteIdent(from) + " TO " + quoteIdent(to) + ";"
}

// renameConstraint renames a constraint. Postgres does not do this for you when
// the table or the column a constraint is named after is renamed, so a schema
// whose names are derived from the new name needs it said explicitly.
func renameConstraint(table, from, to string) string {
	return "ALTER TABLE " + quoteIdent(table) + " RENAME CONSTRAINT " +
		quoteIdent(from) + " TO " + quoteIdent(to) + ";"
}

func renameIndex(from, to string) string {
	return "ALTER INDEX " + quoteIdent(from) + " RENAME TO " + quoteIdent(to) + ";"
}

func addConstraint(table string, c constraint) string {
	return "ALTER TABLE " + quoteIdent(table) + " ADD CONSTRAINT " + quoteIdent(c.name) +
		" " + c.def + ";"
}

func dropConstraint(table, name string) string {
	return "ALTER TABLE " + quoteIdent(table) + " DROP CONSTRAINT " + quoteIdent(name) + ";"
}

// addConstraintNotValid adds a constraint without checking the rows already
// there. New and updated rows are enforced from this moment; the existing ones
// are proven separately by validateConstraint.
//
// Postgres accepts NOT VALID for CHECK and FOREIGN KEY only. A UNIQUE or a
// PRIMARY KEY is enforced by an index, and there is no way to build one without
// reading every row, so those have no lock-brief form of this shape.
func addConstraintNotValid(table string, c constraint) string {
	return strings.TrimSuffix(addConstraint(table, c), ";") + " NOT VALID;"
}

func validateConstraint(table, name string) string {
	return "ALTER TABLE " + quoteIdent(table) + " VALIDATE CONSTRAINT " + quoteIdent(name) + ";"
}

// notNullCheckName names the temporary constraint that carries the scan for a
// SET NOT NULL out from under the ACCESS EXCLUSIVE lock.
//
// It deliberately does not end in _not_null: Postgres 18 gives a column's real
// NOT NULL constraint exactly that name, and the SET NOT NULL this sequence
// exists to serve would collide with it.
func notNullCheckName(table, column string) string {
	return table + "_" + column + "_notnull_check"
}

// notNullCheck is the constraint that proves a column holds no NULLs. It is
// added NOT VALID, validated under a weak lock, and dropped once SET NOT NULL
// has used it — so that the table ends up exactly as the single statement would
// have left it, with no constraint of this package's invention left behind.
func notNullCheck(table, column string) constraint {
	return constraint{
		name: notNullCheckName(table, column),
		def:  "CHECK (" + quoteIdent(column) + " IS NOT NULL)",
	}
}

// indexDef reduces an index to a comparable string, so that a changed index is
// recognised as one thing rather than as an unrelated drop and create.
func indexDef(idx schema.Index) string {
	// The operator classes and the storage parameters are part of the index's
	// identity, not decoration: a vector index under a different class answers
	// a different question, and one built with different parameters is a
	// different index. Leaving them out here made a changed index compare equal
	// and stay as it was (issue #53).
	classes := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		classes = append(classes, c+":"+idx.Opclasses[c])
	}
	// The sort order per column, for the same reason as the classes: an index
	// on the same columns in a different order is a different index, and the
	// one backing a list's default ordering is unusable in any other (issue
	// #64). Fingerprinted through Suffix rather than the struct so that two
	// spellings of the same order — `{Desc: true}` and `{Desc: true, Nulls:
	// NullsFirst}` — do not propose replacing each other.
	orders := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		orders = append(orders, c+":"+idx.Orders[c].Suffix())
	}
	return fmt.Sprintf("unique=%t method=%q columns=%s classes=%s orders=%s with=%q where=%q",
		idx.Unique, idx.Method, strings.Join(idx.Columns, ","),
		strings.Join(classes, ","), strings.Join(orders, ","),
		storageParameters(idx.With), idx.Where)
}

// Explaining a rebuild whose statement looks identical to what is already
// there.
//
// A declared expression is compared with an introspected one as text, and
// Postgres does not hand back the text it was given. shadow.Normalize is what
// puts the declared side through the same rendering; a caller diffing against
// introspect.Registry without making that call gets a diff proposing to replace
// a CHECK or rebuild a partial index with a statement it cannot tell apart from
// the one already in effect. Issues #24, #56 and #63 were all reported from
// that position, and each took someone measuring both sides to work out that
// the difference was whitespace.
//
// The two functions below recognise that shape so the change can say so. They
// are the heuristic comparison shadow/normalize.go rejects, and they are safe
// here for one reason: nothing branches on them. The drop and the add are
// already decided, by the exact comparison, before either is asked; all they do
// is choose a sentence to attach. A wrong answer here is a misleading comment
// on a correct migration, where a wrong answer there would be a schema edit
// that never reaches the database.
//
// The sentence is worded to survive being wrong about it. Reducing
// `(a OR b) AND c` and `a OR (b AND c)` alike is exactly why this cannot decide
// equality — and "these differ only in parenthesisation" is a true statement
// about that pair too, which is why the comment says that rather than "these
// are the same expression".

// onlyThePredicateFormattingDiffers reports whether two indexes of the same
// name are identical apart from a predicate that differs only in spacing,
// parentheses and casts.
func onlyThePredicateFormattingDiffers(a, b schema.Index) bool {
	if a.Where == b.Where || a.Where == "" || b.Where == "" {
		return false
	}
	// Everything else identical, or the predicate is not the story.
	bare := func(idx schema.Index) schema.Index { idx.Where = ""; return idx }
	if indexDef(bare(a)) != indexDef(bare(b)) {
		return false
	}
	return reduceExpression(a.Where) == reduceExpression(b.Where)
}

// onlyTheDefinitionFormattingDiffers is the same question for a constraint
// replaced under its own name.
//
// Both sides must be hand-written CHECKs, which is exactly the set
// shadow.Normalize rewrites. Everything else a constraint can be — a primary
// key, a unique, a foreign key, the CHECK an enum column renders — is built
// from column names and values on both sides rather than carried through as
// text, so a difference between two of those is never formatting, and saying it
// might be would point the reader at a normalisation step that does not touch
// them.
func onlyTheDefinitionFormattingDiffers(a, b constraint) bool {
	if !a.handWritten || !b.handWritten || a.def == b.def {
		return false
	}
	return reduceExpression(a.def) == reduceExpression(b.def)
}

// castSuffix matches the `::type` Postgres renders onto a literal. It stops at
// the first word, so a two-word type name such as `::double precision` is only
// half removed — which loses a hint rather than inventing one, and inventing
// one is the failure worth avoiding.
var castSuffix = regexp.MustCompile(`::"?[a-zA-Z_][a-zA-Z0-9_]*"?(\s*\[\s*\])?`)

// reduceExpression strips what Postgres's rendering adds and a person does not
// write: parentheses, casts on literals, and its own spacing. Only ever used to
// explain a diff — see the comment above onlyThePredicateFormattingDiffers.
func reduceExpression(s string) string {
	s = castSuffix.ReplaceAllString(s, "")
	s = strings.NewReplacer("(", " ", ")", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// formattingNote is the clause both cases attach, written once so that a reader
// who has seen it on an index recognises it on a constraint. what names the
// thing that differs, in the words the declaration uses for it.
func formattingNote(what string) string {
	return " (the " + what + " differs from the existing one only in spacing, " +
		"parentheses or casts; if this change comes back every run, the declared " +
		"schema has not been through shadow.Normalize)"
}

// renamedIndex returns the index as it will read once cols have been renamed.
// The covered columns are a list of names and are mapped exactly; a partial
// index's Where is hand-written SQL and is left alone for the same reason a
// hand-written CHECK is (see constraint.renamed).
func renamedIndex(idx schema.Index, cols map[string]string) schema.Index {
	if len(cols) == 0 {
		return idx
	}
	out := idx
	out.Columns = make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		out.Columns[i] = rename(cols, c)
	}
	// The per-column maps are keyed by column name, so a rename that missed
	// them would silently drop an operator class or a sort order — which is
	// the same index being replaced by a different one under a clean rename.
	out.Opclasses = renameKeys(idx.Opclasses, cols)
	out.Orders = renameKeys(idx.Orders, cols)
	return out
}

// renameKeys rebuilds a per-column map under the new column names.
func renameKeys[V any](m map[string]V, cols map[string]string) map[string]V {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[rename(cols, k)] = v
	}
	return out
}

// createIndex renders CREATE INDEX. concurrent is decided by the caller: an
// index on a table created in the same migration needs no CONCURRENTLY,
// because there is nothing to lock it against and requiring it would force the
// migration into a second file for no benefit.
func createIndex(t *schema.TableDef, idx schema.Index, concurrent bool) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if idx.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if concurrent {
		b.WriteString("CONCURRENTLY ")
	}
	b.WriteString(quoteIdent(idx.Name) + " ON " + quoteIdent(t.Name()))
	if idx.Method != "" {
		b.WriteString(" USING " + idx.Method)
	}
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = quoteIdent(c)
		// The operator class follows the column, unquoted: it is a type name
		// in the catalog rather than a user identifier, and pgvector's classes
		// are the reason this is not optional (issue #53).
		if class := idx.Opclasses[c]; class != "" {
			cols[i] += " " + class
		}
		// And then the sort order, which follows the class — the order
		// Postgres itself renders them in, so a declared index and the one
		// pg_get_indexdef hands back read the same.
		cols[i] += idx.Orders[c].Suffix()
	}
	b.WriteString(" (" + strings.Join(cols, ", ") + ")")
	if with := storageParameters(idx.With); with != "" {
		b.WriteString(" WITH (" + with + ")")
	}
	if idx.Where != "" {
		b.WriteString(" WHERE " + idx.Where)
	}
	b.WriteString(";")
	return b.String()
}

// storageParameters renders an index's WITH clause in sorted key order.
//
// A number is written bare and anything else is quoted, which is what Postgres
// hands back through reloptions and therefore what makes the value survive a
// round trip: `m=16` comes back as `m=16`, and `buffering=auto` as
// `buffering=auto` whether it was written quoted or not.
func storageParameters(with map[string]string) string {
	if len(with) == 0 {
		return ""
	}
	keys := make([]string, 0, len(with))
	for k := range with {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" = "+storageValue(with[k]))
	}
	return strings.Join(parts, ", ")
}

func storageValue(v string) string {
	if v == "" {
		return "''"
	}
	for i := 0; i < len(v); i++ {
		if (v[i] < '0' || v[i] > '9') && v[i] != '.' && v[i] != '-' {
			return sqlString(v)
		}
	}
	return v
}

func dropIndex(name string, concurrent bool) string {
	if concurrent {
		return "DROP INDEX CONCURRENTLY " + quoteIdent(name) + ";"
	}
	return "DROP INDEX " + quoteIdent(name) + ";"
}

// createUniqueIndexConcurrently builds the index that will back a unique
// constraint, under the lock a concurrent build takes rather than the one
// ADD CONSTRAINT would take to build the same index itself.
//
// The index is given the constraint's own name, because ADD CONSTRAINT ...
// USING INDEX renames the index to match the constraint — giving it the right
// name up front means nothing is renamed and the end state is exactly what a
// plain ADD CONSTRAINT would have produced.
func createUniqueIndexConcurrently(table, name string, columns []string) string {
	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = quoteIdent(c)
	}
	return "CREATE UNIQUE INDEX CONCURRENTLY " + quoteIdent(name) + " ON " +
		quoteIdent(table) + " (" + strings.Join(cols, ", ") + ");"
}

// dropIndexConcurrentlyIfExists reverses a concurrent build.
//
// IF EXISTS because the index may not be there to drop: reversing the migration
// that adopted it as a constraint has already taken it, since dropping a unique
// constraint drops the index enforcing it. Both orders have to work — rolling
// back one file, or rolling back both.
func dropIndexConcurrentlyIfExists(name string) string {
	return "DROP INDEX CONCURRENTLY IF EXISTS " + quoteIdent(name) + ";"
}

// addConstraintUsingIndex adopts an already-built unique index as the
// constraint it enforces. This is a catalog write: the index exists, and
// nothing is read to make the constraint out of it.
func addConstraintUsingIndex(table string, c constraint) string {
	kind := "UNIQUE"
	if c.pk {
		kind = "PRIMARY KEY"
	}
	return "ALTER TABLE " + quoteIdent(table) + " ADD CONSTRAINT " + quoteIdent(c.name) +
		" " + kind + " USING INDEX " + quoteIdent(c.name) + c.deferrable + ";"
}

// excludeCovers lists the table's own columns an exclusion names, so the diff
// can recognise one that depends on a column the same migration is adding.
//
// Matched by name against the table's columns rather than parsed out of the
// element list, because the elements are expressions — tstzrange(starts_at,
// ends_at) names two columns inside a function call — and a parser here would
// be a second, worse copy of one Postgres already has. Over-reporting is the
// safe direction: a column named coincidentally by a literal only delays the
// constraint until after the column exists, which it would anyway.
func excludeCovers(t *schema.TableDef, e schema.Exclusion) []string {
	body := e.Elements + " " + e.Where
	var out []string
	for _, f := range t.StoredFields() {
		if containsWord(body, f.Desc().Name) {
			out = append(out, f.Desc().Name)
		}
	}
	return out
}

// containsWord reports whether name appears in s as a whole identifier, so that
// "id" does not match "coach_id".
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
