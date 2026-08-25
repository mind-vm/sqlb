package codegen

// The statement half of the exit: one file of SQL text and pgx calls, with
// nothing between a reader and the query that runs.
//
// Everything here is written out per table rather than derived at run time,
// which is the whole point — a query builder that assembled these strings would
// be a smaller sqlb, and the person reading this file wanted to stop reading a
// query builder.

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// projected are the columns a SELECT reads back, in declaration order.
//
// A computed column is projected as its expression, aliased to its name — the
// exit keeps derived values, because they were only ever SQL. The one exception
// is a computed column that declared `Needs`: its expression takes a bind that
// came from the request, and there is no hook here to supply it, so it is left
// out of the projection and its field stays at its zero value. The README says
// so by name.
func projected(t *schema.TableDef) []*schema.Field {
	var out []*schema.Field
	for _, f := range t.Fields() {
		if d := f.Desc(); d.Computed() && len(d.Needs) > 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}

// selectList is the projection as SQL.
func selectList(t *schema.TableDef) string {
	parts := make([]string, 0, len(t.Fields()))
	for _, f := range projected(t) {
		d := f.Desc()
		if d.Computed() {
			parts = append(parts, fmt.Sprintf("%s AS %s", parenthesised(d.Expr), quoteSQLIdent(d.Name)))
			continue
		}
		parts = append(parts, quoteSQLIdent(d.Name))
	}
	return strings.Join(parts, ", ")
}

// writableColumns are the columns a statement may set: everything the table
// stores, minus the ones the database owns.
func writableColumns(t *schema.TableDef) []*schema.Field {
	var out []*schema.Field
	for _, f := range t.StoredFields() {
		if d := f.Desc(); d.ReadOnly || d.PrimaryKey {
			continue
		}
		out = append(out, f)
	}
	return out
}

func quoteSQLIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// parenthesised wraps an expression unless one pair already encloses the whole
// of it, so that what surrounds it in the projection cannot change what it
// means — and so that an expression the author already wrapped does not come
// out wearing two pairs.
func parenthesised(expr string) string {
	s := strings.TrimSpace(expr)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		depth := 0
		wrapped := true
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 && i < len(s)-1 {
				wrapped = false
			}
		}
		if wrapped && depth == 0 {
			return s
		}
	}
	return "(" + s + ")"
}

// ejectStore emits the column descriptors, the projections, the scanners and
// the statements.
func ejectStore(opts EjectOptions, tables []*schema.TableDef) ([]byte, error) {
	b := new(bytes.Buffer)
	fmt.Fprintln(b, `
// The statements. Each one is written out: the SQL is a string you can read,
// paste into psql, and change. What varies per request — the WHERE, the ORDER
// BY and the paging — is assembled by the helpers in support.go, which is the
// smallest amount of assembly a filterable list endpoint can be written with.`)

	for _, t := range tables {
		typeName := TypeName(t)
		lower := unexportedGoName(typeName)

		fmt.Fprintf(b, "\n// %s is the table %s maps to.\n", lower+"Table", typeName)
		fmt.Fprintf(b, "const %sTable = %q\n", lower, t.Name())

		fmt.Fprintf(b, "\n// %sSelect is the projection: every column, in declaration order.\n", lower)
		fmt.Fprintf(b, "const %sSelect = %q\n", lower, selectList(t))

		ejectColumnTable(b, t, lower, opts.Registry.Wire())
		ejectScanner(b, t, typeName, lower)
		ejectList(b, t, typeName, lower)
		ejectGet(b, t, typeName, lower)
		ejectInsert(b, t, typeName, lower)
		ejectUpdate(b, t, typeName, lower)
		ejectDelete(b, t, typeName, lower)
	}
	return ejectFile("store.go", opts.pkg(), b)
}

// ejectColumnTable emits what a request is allowed to name.
//
// Capabilities were opt-in in the schema and they stay opt-in here: a column
// that never declared Filterable is not filterable in the exit either, and the
// rejection names the ones that are. That is the one piece of sqlb's behaviour
// this file reproduces rather than drops, because it is a security property —
// a column excluded from the filter grammar cannot be probed through it.
//
// Each entry carries both of the column's names when the schema's WireCase
// makes them differ. Nothing on the request path may compute a spelling — the
// exit does not import the schema package and there is nothing there to compute
// it with — so the mapping is data, written here once and read by support.go
// (ADR-0036's amendment, which the generated models already follow).
func ejectColumnTable(b *bytes.Buffer, t *schema.TableDef, lower string, wire schema.WireCase) {
	fmt.Fprintf(b, "\n// %sColumns is what a request may name, and for what.\n", lower)
	fmt.Fprintf(b, "var %sColumns = []Column{\n", lower)
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Hidden || d.WriteOnly {
			// A hidden column has no spelling a request could use, in either
			// direction. A write-only one still has a spelling in the create
			// and update bodies — this table is the read-side filter/sort/
			// search/select vocabulary only, so it stays out of here either
			// way.
			continue
		}
		// Omitted when the two names are the same, which under the default
		// WireCase is every column: Column.wire falls back to Name, and a
		// `Wire: "title"` beside `Name: "title"` on every row is noise in a
		// file meant to be read.
		spelling := ""
		if w := wire.WireName(d.Name); w != d.Name {
			spelling = fmt.Sprintf("Wire: %q, ", w)
		}
		fmt.Fprintf(b, "\t{Name: %q, %sFilterable: %t, Sortable: %t, Searchable: %t, Parse: %s},\n",
			d.Name, spelling, d.Filterable, d.Sortable, d.Searchable, ejectParser(d))
	}
	fmt.Fprintln(b, "}")
}

// ejectParser names the support.go function that turns a query-string value
// into something pgx can bind for this column.
func ejectParser(d *schema.FieldDesc) string {
	if d.Array {
		// An array filter was `has`/`hasany`/`hasall` over a GIN index, which
		// the exit does not carry. Parsing one as text keeps the column
		// nameable in a sort without pretending the containment operators are
		// still there.
		return "ParseText"
	}
	switch d.Type {
	case schema.TypeSmallInt, schema.TypeInt, schema.TypeBigInt:
		return "ParseInt"
	case schema.TypeReal, schema.TypeFloat, schema.TypeNumeric:
		return "ParseFloat"
	case schema.TypeBool:
		return "ParseBool"
	case schema.TypeTimestamp, schema.TypeDate, schema.TypeTime:
		return "ParseTime"
	default:
		return "ParseText"
	}
}

func ejectScanner(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	fmt.Fprintf(b, "\n// scan%s reads one row of %sSelect.\n", typeName, lower)
	fmt.Fprintf(b, "func scan%s(s scanner) (%s, error) {\n", typeName, typeName)
	fmt.Fprintf(b, "\tvar row %s\n", typeName)
	fmt.Fprintln(b, "\terr := s.Scan(")
	for _, f := range projected(t) {
		fmt.Fprintf(b, "\t\t&row.%s,\n", GoName(f.Desc().Name))
	}
	fmt.Fprintln(b, "\t)")
	fmt.Fprintln(b, "\treturn row, err")
	fmt.Fprintln(b, "}")
}

func ejectList(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	fmt.Fprintf(b, `
// List%s reads a page. It returns one row more than asked for when there is
// one, which is how the handler answers has_more without a second count.
func List%s(ctx context.Context, db DB, q Query) ([]%s, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %%s FROM %%s", %sSelect, quoteIdent(%sTable))
	writeWhere(&sb, args, q.Where)
	writeOrder(&sb, q.Order)
	writeLimit(&sb, q.Limit, q.Offset)

	rows, err := db.Query(ctx, sb.String(), args.values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []%s
	for rows.Next() {
		row, err := scan%s(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Count%s is ?count=exact: the size of the matching set, which costs a second
// query over the same predicate.
func Count%s(ctx context.Context, db DB, where []Condition) (int64, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT count(*) FROM %%s", quoteIdent(%sTable))
	writeWhere(&sb, args, where)

	var n int64
	err := db.QueryRow(ctx, sb.String(), args.values...).Scan(&n)
	return n, err
}
`, typeName, typeName, typeName, lower, lower, typeName, typeName, typeName, typeName, lower)
}

func ejectGet(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	if t.PrimaryKey() == nil && !ejectSingleton(t) {
		fmt.Fprintf(b, "\n// %s has no primary key, so there is no by-id read to eject.\n", t.Name())
		return
	}
	doc := `
// Get%s reads one row by primary key. The extra conditions are whatever
// confines this table — a tenant, a soft delete — and they are part of the
// lookup rather than a check afterwards, so a row outside them is a 404 and not
// a 403 that confirms it exists.`
	if ejectSingleton(t) {
		doc = `
// Get%s reads the caller's one row. There is no id: the conditions are the
// whole address, which is why the resource this came from could not be mounted
// without a hook to supply them.`
	}
	fmt.Fprintf(b, doc+`
func Get%s(ctx context.Context, db DB, %swhere []Condition) (%s, error) {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "SELECT %%s FROM %%s", %sSelect, quoteIdent(%sTable))
	writeWhere(&sb, args, %s)

	row, err := scan%s(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, ErrNotFound
		}
		return row, err
	}
	return row, nil
}
`, typeName, typeName, ejectIDParam(t), typeName, lower, lower, ejectKeyed(t), typeName)
}

// ejectSingleton reports whether the table's resource is the caller's one row,
// in which case every ejected operation drops its id and is addressed by the
// confining conditions alone (#166).
func ejectSingleton(t *schema.TableDef) bool {
	return t.Rest() != nil && t.Rest().Ops.Has(schema.OpSingleton)
}

// ejectIDParam is the id parameter a store function takes, empty for a
// singleton.
func ejectIDParam(t *schema.TableDef) string {
	if ejectSingleton(t) {
		return ""
	}
	return "id any, "
}

// ejectKeyed renders the condition list a statement runs under: the key
// predicate and the caller's conditions, or — for a singleton — the caller's
// alone.
func ejectKeyed(t *schema.TableDef) string {
	if ejectSingleton(t) {
		return "where"
	}
	return fmt.Sprintf("append([]Condition{{Column: %q, Op: OpEq, Value: id}}, where...)", t.PrimaryKey().Desc().Name)
}

func ejectInsert(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	writable := writableColumns(t)
	if len(writable) == 0 {
		fmt.Fprintf(b, "\n// %s has no writable column, so there is no insert to eject.\n", t.Name())
		return
	}
	order := make([]string, 0, len(writable))
	for _, f := range writable {
		order = append(order, fmt.Sprintf("%q", f.Desc().Name))
	}

	fmt.Fprintf(b, `
// Insert%s writes one row and reads it back, so database defaults and computed
// columns arrive without a second query.
//
// The column order is the schema's, not the map's: a generated statement whose
// text depends on map iteration is a statement that cannot be diffed.
func Insert%s(ctx context.Context, db DB, values map[string]any) (%s, error) {
	order := []string{%s}

	var cols, holes []string
	args := new(args)
	for _, name := range order {
		v, ok := values[name]
		if !ok {
			continue
		}
		cols = append(cols, quoteIdent(name))
		holes = append(holes, args.add(v))
	}

	var sb strings.Builder
	if len(cols) == 0 {
		fmt.Fprintf(&sb, "INSERT INTO %%s DEFAULT VALUES", quoteIdent(%sTable))
	} else {
		fmt.Fprintf(&sb, "INSERT INTO %%s (%%s) VALUES (%%s)",
			quoteIdent(%sTable), strings.Join(cols, ", "), strings.Join(holes, ", "))
	}
	fmt.Fprintf(&sb, " RETURNING %%s", %sSelect)

	return scan%s(db.QueryRow(ctx, sb.String(), args.values...))
}
`, typeName, typeName, typeName, strings.Join(order, ", "), lower, lower, lower, typeName)
}

func ejectUpdate(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	writable := writableColumns(t)
	if (t.PrimaryKey() == nil && !ejectSingleton(t)) || len(writable) == 0 {
		return
	}
	order := make([]string, 0, len(writable))
	for _, f := range writable {
		if f.Desc().Immutable {
			// Settable once, at create. Leaving it out of this list is what
			// makes that hold in the exit.
			continue
		}
		order = append(order, fmt.Sprintf("%q", f.Desc().Name))
	}

	fmt.Fprintf(b, `
// Update%s writes the named columns of one row and reads the row back. An
// empty change set is the caller's mistake rather than a statement with no SET
// clause, which Postgres will not parse.
func Update%s(ctx context.Context, db DB, %schanges map[string]any, where []Condition) (%s, error) {
	order := []string{%s}

	var sets []string
	args := new(args)
	for _, name := range order {
		v, ok := changes[name]
		if !ok {
			continue
		}
		sets = append(sets, quoteIdent(name)+" = "+args.add(v))
	}
	var zero %s
	if len(sets) == 0 {
		return zero, ErrNoChanges
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "UPDATE %%s SET %%s", quoteIdent(%sTable), strings.Join(sets, ", "))
	writeWhere(&sb, args, %s)
	fmt.Fprintf(&sb, " RETURNING %%s", %sSelect)

	row, err := scan%s(db.QueryRow(ctx, sb.String(), args.values...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, ErrNotFound
		}
		return zero, err
	}
	return row, nil
}
`, typeName, typeName, ejectIDParam(t), typeName, strings.Join(order, ", "), typeName, lower, ejectKeyed(t), lower, typeName)
}

func ejectDelete(b *bytes.Buffer, t *schema.TableDef, typeName, lower string) {
	if t.PrimaryKey() == nil && !ejectSingleton(t) {
		return
	}
	fmt.Fprintf(b, `
// Delete%s removes one row, and reports ErrNotFound rather than success when
// the id matched nothing the conditions admit.
func Delete%s(ctx context.Context, db DB, %swhere []Condition) error {
	var sb strings.Builder
	args := new(args)
	fmt.Fprintf(&sb, "DELETE FROM %%s", quoteIdent(%sTable))
	writeWhere(&sb, args, %s)

	tag, err := db.Exec(ctx, sb.String(), args.values...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
`, typeName, typeName, ejectIDParam(t), lower, ejectKeyed(t))
}
