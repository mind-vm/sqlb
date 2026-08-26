package sqlb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrUnscoped is returned by Update and Delete when no WHERE clause was given.
// Rewriting or removing every row is almost never intended, so it must be
// requested explicitly with Everything.
var ErrUnscoped = errors.New("sqlb: statement would affect every row; add a Where clause or call Everything to confirm")

// Insert is an INSERT statement over model T.
//
// Columns carrying a database default are omitted when their Go value is the
// zero value, so generated identifiers and timestamps come from the database
// rather than being overwritten with zeroes. The statement always returns the
// inserted rows, so those values land back in the caller's structs.
//
// The rule is per row, including in a multi-row insert: a column no row fills in
// leaves the statement entirely, and in a mixed batch a row that leaves it zero
// gets the DEFAULT keyword in its own tuple. A row's semantics therefore do not
// depend on its batch-mates, which they did until #73.
type Insert[T any] struct {
	model    *Model
	dialect  Dialect
	rows     []*T
	only     map[string]bool
	omit     map[string]bool
	explicit map[string]bool
	computed map[string]bool
	conflict *conflictClause
	err      error
}

type conflictClause struct {
	target   []string
	doUpdate []string
	sets     []conflictSet
}

// conflictSet is one `column = expression` assignment in DO UPDATE, kept in
// declaration order so the rendered statement is stable.
type conflictSet struct {
	column string
	value  Expr
}

// skipsRows reports whether the clause renders DO NOTHING, which is the one
// shape where a row can be absent from RETURNING. The condition is the same one
// SQL renders on, and it is read from the same fields, so the two cannot drift.
func (c *conflictClause) skipsRows() bool {
	return c != nil && len(c.doUpdate) == 0 && len(c.sets) == 0
}

// InsertRows starts an INSERT for one or more rows. The rows are pointers so
// that hooks and returned database values can be written back into them.
func InsertRows[T any](rows ...*T) *Insert[T] {
	m := ModelOf[T]()
	m.markInUse()
	ins := &Insert[T]{model: m, rows: rows}
	if len(rows) == 0 {
		ins.err = errors.New("sqlb: InsertRows called with no rows")
	}
	for _, r := range rows {
		if r == nil {
			ins.err = errors.New("sqlb: InsertRows called with a nil row")
			break
		}
	}
	return ins
}

// Only restricts the insert to the named columns.
func (i *Insert[T]) Only(columns ...string) *Insert[T] {
	i.checkColumns("Only", columns)
	i.only = toSet(columns)
	return i
}

// Explicit says the named columns carry a value the caller meant, so their zero
// is written rather than left to the database default.
//
// It is [Insert.Only]'s effect on the default-omitting rule without Only's
// restriction: the columns not named keep the ordinary behaviour, so a
// BeforeCreate hook can still fill in a column nobody named and a generated id
// still comes from the database. Only cannot serve this case, because naming
// the columns a request carried would drop every column it did not — including
// the ones a hook is about to supply (#314).
//
// The shape that needs it is a column whose default disagrees with its Go zero
// value, which in practice means Bool(...).Default(Value(true)): false is both
// "not set" and the interesting state, and without this the two are the same
// statement.
//
//	sqlb.InsertRows(row).Explicit("active").One(ctx, db)
func (i *Insert[T]) Explicit(columns ...string) *Insert[T] {
	i.checkColumns("Explicit", columns)
	i.explicit = toSet(columns)
	return i
}

// Omit excludes the named columns, leaving them to their database defaults.
func (i *Insert[T]) Omit(columns ...string) *Insert[T] {
	i.checkColumns("Omit", columns)
	i.omit = toSet(columns)
	return i
}

// WithComputed adds the named computed columns to the statement's RETURNING.
//
// It is [Builder.WithComputed] for a write, and the default is the same: none.
// See [writeComputed] for why a write's derived columns are opt-in rather than
// automatic.
func (i *Insert[T]) WithComputed(names ...string) *Insert[T] {
	set, err := writeComputed(i.model, "WithComputed", i.computed, names)
	if err != nil {
		if i.err == nil {
			i.err = err
		}
		return i
	}
	i.computed = set
	return i
}

// checkColumns fails the statement on a name the model does not have.
//
// Update.Set and the conflict target both validate their names, and an
// unvalidated one here fails quietly in the worst way: Only("emial") matches
// nothing, so the column is silently not written, or — if it was the only name
// given — the statement fails with "no columns to write", which names neither
// the typo nor the column it was meant to be.
func (i *Insert[T]) checkColumns(method string, columns []string) {
	for _, name := range columns {
		col := i.model.Column(name)
		if col == nil {
			if i.err == nil {
				i.err = fmt.Errorf("sqlb: %s names %q, which is not a column of %s (have: %s)",
					method, name, i.model.Table, strings.Join(i.model.ColumnNames(), ", "))
			}
			return
		}
		if col.Computed() {
			if i.err == nil {
				i.err = fmt.Errorf("sqlb: %s names %q, which %s computes rather than stores; there is nothing to write to it",
					method, name, i.model.Table)
			}
			return
		}
	}
}

// OnConflictDoNothing makes a conflict on the given columns skip the row
// instead of failing. Skipped rows are simply absent from the result.
//
// Because a skipped row cannot be told apart from its neighbours in what
// comes back, a statement that skips any row leaves every caller struct
// untouched — the returned slice is then the only account of what was
// written. So the terminal is [Insert.Exec], whose empty slice and nil error
// are what "it was already there" looks like.
//
// [Insert.One] is refused after this call rather than answering ErrNotFound on
// the conflict, which is the case an idempotent insert exists to serve (#146).
// If the row itself is wanted whether or not this call created it, the spelling
// is OnConflictUpdate with the target as its own update column — a write that
// changes nothing is still a written row, and a written row is a returned one.
func (i *Insert[T]) OnConflictDoNothing(target ...string) *Insert[T] {
	i.conflict = &conflictClause{target: target}
	return i
}

// OnConflictUpdate upserts: a conflict on target updates the named columns
// from the proposed row. With no update columns it behaves as do-nothing,
// unless [Insert.OnConflictSet] adds an assignment.
//
// Each named column is shorthand for `col = EXCLUDED.col`. For anything else —
// the database clock, an accumulation, a value kept when the proposed one is
// null — see OnConflictSet.
func (i *Insert[T]) OnConflictUpdate(target []string, update ...string) *Insert[T] {
	i.conflict = &conflictClause{target: target, doUpdate: update}
	return i
}

// OnConflictSet assigns an expression to a column in DO UPDATE, for the
// upserts that cannot be spelled by naming a column.
//
//	ins.OnConflictUpdate([]string{"key"}, "payload").
//	    OnConflictSet("updated_at", sqlb.Now()).
//	    OnConflictSet("hits", sqlb.Add(sqlb.Current("hits"), sqlb.Val(1))).
//	    OnConflictSet("note", sqlb.Coalesce(sqlb.Excluded("note"), sqlb.Current("note")).Expr())
//
// Assignments render after the bare columns, in the order declared, and their
// bind parameters are numbered in the same sequence as the VALUES list — so a
// parameterised assignment is an ordinary `$n`, not a separate numbering that
// happens to line up.
//
// A column reference inside the expression must say which side of the conflict
// it means, with [Excluded] or [Current]. Both rows are in scope in DO UPDATE,
// so a bare [Field] is ambiguous, and it is refused rather than resolved to
// whichever side the compiler would have picked (#90). [Raw] is exempt for the
// reason it is always exempt: its contents are not parsed by this package.
//
// Calling it without OnConflictUpdate or OnConflictDoNothing is an error — an
// assignment with no conflict clause has nowhere to go.
func (i *Insert[T]) OnConflictSet(column string, value Expr) *Insert[T] {
	if i.conflict == nil {
		if i.err == nil {
			i.err = fmt.Errorf("sqlb: OnConflictSet(%q) needs a conflict clause; call OnConflictUpdate or OnConflictDoNothing first", column)
		}
		return i
	}
	i.conflict.sets = append(i.conflict.sets, conflictSet{column: column, value: value})
	return i
}

// UseDialect overrides the dialect for this statement.
func (i *Insert[T]) UseDialect(d Dialect) *Insert[T] {
	i.dialect = d
	return i
}

// SQL compiles the statement without running it.
func (i *Insert[T]) SQL() (string, []any, error) {
	if i.err != nil {
		return "", nil, i.err
	}
	cols := i.columns()
	if len(cols) == 0 {
		return "", nil, fmt.Errorf("sqlb: insert into %s has no columns to write", i.model.Table)
	}

	c := newCompiler(i.dialect)
	c.overflow = i.overflowErr(len(cols))
	c.write("INSERT INTO ")
	c.table(i.model.Table)
	c.write(" (")
	for n, col := range cols {
		if n > 0 {
			c.write(", ")
		}
		c.ident(col.Name)
	}
	c.write(") VALUES ")

	for n, row := range i.rows {
		if n > 0 {
			c.write(", ")
		}
		c.write("(")
		rv := reflect.ValueOf(row).Elem()
		for k, col := range cols {
			if k > 0 {
				c.write(", ")
			}
			fv, err := fieldByIndex(rv, col.Index)
			if err != nil {
				return "", nil, err
			}
			// Per row, not per statement. columns() drops a defaulted column
			// only when *every* row leaves it zero, so in a mixed batch the
			// column stays and a zero-valued row used to bind an explicit zero
			// — the same row taking the database's default when inserted alone
			// and writing a zero when inserted beside a non-zero neighbour
			// (#73). Postgres accepts the DEFAULT keyword per position in a
			// multi-row VALUES, which makes the rule the one the doc comment
			// already described.
			if i.takesDefault(col) && fv.IsZero() {
				c.write("DEFAULT")
				continue
			}
			c.bind(fv.Interface())
		}
		c.write(")")
	}

	if i.conflict != nil {
		c.write(" ON CONFLICT")
		if len(i.conflict.target) > 0 {
			c.write(" (")
			for n, name := range i.conflict.target {
				if n > 0 {
					c.write(", ")
				}
				if i.model.Column(name) == nil {
					return "", nil, fmt.Errorf("sqlb: conflict target %q is not a column of %s", name, i.model.Table)
				}
				c.ident(name)
			}
			c.write(")")
		}
		if len(i.conflict.doUpdate) == 0 && len(i.conflict.sets) == 0 {
			c.write(" DO NOTHING")
		} else {
			c.write(" DO UPDATE SET ")
			for n, name := range i.conflict.doUpdate {
				if n > 0 {
					c.write(", ")
				}
				if i.model.Column(name) == nil {
					return "", nil, fmt.Errorf("sqlb: conflict update column %q is not a column of %s", name, i.model.Table)
				}
				c.ident(name)
				c.write(" = EXCLUDED.")
				c.ident(name)
			}
			// Qualified to the target table for the duration of the assignments,
			// so Current renders `"posts"."hits"` rather than a bare `"hits"`.
			// Postgres resolves the bare form to the stored row too, but only
			// because nothing else is in scope under that name — writing it out
			// is what makes the statement say which row it meant.
			restore := c.qualifyTo(i.model.Table)
			for n, set := range i.conflict.sets {
				if n > 0 || len(i.conflict.doUpdate) > 0 {
					c.write(", ")
				}
				if err := i.checkConflictSet(set); err != nil {
					restore()
					return "", nil, err
				}
				c.ident(set.column)
				c.write(" = ")
				c.expr(set.value)
			}
			restore()
		}
	}

	writeReturning(c, i.model, i.computed)
	return c.result()
}

// overflowErr explains a batch too wide for one statement in terms of the batch
// rather than of the protocol.
//
// The ceiling is reported as a row count because rows are what the caller
// controls: they chose the batch size, not the column count. It is derived from
// the values actually bound rather than from rows × columns, because the two
// differ — a column left to its database default writes the DEFAULT keyword and
// binds nothing — and a suggestion computed from the wider figure would name a
// batch size that still does not fit.
//
// Integer division floors, which is the safe direction: the answer is a batch
// that fits, and rounding up would name one that does not.
func (i *Insert[T]) overflowErr(width int) func(int) error {
	rows := len(i.rows)
	return func(need int) error {
		fits := rows * maxBindParams / need
		return fmt.Errorf(
			"sqlb: inserting %d rows into %s binds %d values across %d columns, "+
				"and one statement can carry %d; insert at most %d rows at a time, "+
				"in a transaction if they have to land together",
			rows, i.model.Table, need, width, maxBindParams, fits)
	}
}

// columns picks the columns to write: everything mapped, minus Only/Omit, and
// minus database-defaulted columns still holding their zero value.
func (i *Insert[T]) columns() []*ColumnInfo {
	var out []*ColumnInfo
	for _, col := range i.model.Columns {
		// A computed column has no storage to write to. Naming one explicitly
		// through Only is refused in checkColumns; here it is simply not
		// written, which is what makes an ordinary insert of a struct carrying
		// a derived field work at all.
		if col.Computed() {
			continue
		}
		if i.only != nil && !i.only[col.Name] {
			continue
		}
		if i.omit[col.Name] {
			continue
		}
		if i.takesDefault(col) && i.allZero(col) {
			continue
		}
		out = append(out, col)
	}
	return out
}

// takesDefault reports whether a zero value in this column means "let the
// database decide" rather than "write a zero".
//
// Only when the caller did not name the columns. Only(...) is an explicit list,
// and a column on it was asked for by name — writing its zero is what the caller
// said. Explicit(...) says the same thing about one column without restricting
// the statement to it.
func (i *Insert[T]) takesDefault(col *ColumnInfo) bool {
	return col.HasDefault && i.only == nil && !i.explicit[col.Name]
}

func (i *Insert[T]) allZero(col *ColumnInfo) bool {
	for _, row := range i.rows {
		fv, err := fieldByIndex(reflect.ValueOf(row).Elem(), col.Index)
		if err != nil || !fv.IsZero() {
			return false
		}
	}
	return true
}

// Exec runs the insert, returning the stored rows with database defaults
// applied. The caller's structs are updated in place as well — except when
// ON CONFLICT DO NOTHING skipped a row, in which case none of them are; see
// writeBack for why.
func (i *Insert[T]) Exec(ctx context.Context, db Executor) ([]T, error) {
	hooks := hooksFor[T](db)
	for _, row := range i.rows {
		if err := hooks.runBeforeCreate(ctx, row); err != nil {
			return nil, err
		}
	}

	query, args, err := i.SQL()
	if err != nil {
		return nil, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return nil, err
	}
	stored, err := scanAllClose[T](rows, i.model)
	if err != nil {
		return nil, asConstraintErr(err)
	}

	i.writeBack(stored)
	if err := hooks.runAfterCreate(ctx, stored); err != nil {
		return nil, err
	}
	return stored, nil
}

// writeBack copies the stored rows into the caller's structs, so generated
// ids are visible without reading the returned slice.
//
// A VALUES insert returns at most one row per row written, in the order they
// were written, so equal lengths mean position identifies a row and nothing
// was skipped. A shorter result means ON CONFLICT DO NOTHING dropped one:
// every later stored row then belongs to an earlier struct than its index
// says, and writing positionally hands one row's generated primary key to a
// different row — silently, since both structs look plausible afterwards.
//
// Which row was skipped is not recoverable from the result. RETURNING reports
// only the target table's columns, so no ordinal can be carried through the
// statement to identify them, and matching on the conflict target fails
// exactly when the target is generated rather than supplied. So a short
// result writes nothing at all, and the returned slice — which is complete
// and correct — is the account of what was written. A struct left holding its
// zero value is a caller reading an obvious absence; a struct holding its
// neighbour's identity is a caller reading a lie.
func (i *Insert[T]) writeBack(stored []T) {
	if len(stored) != len(i.rows) {
		return
	}
	for n := range stored {
		*i.rows[n] = stored[n]
	}
}

// One inserts a single row and returns it.
//
// It is refused over ON CONFLICT DO NOTHING. "Give me exactly one row" and "do
// not produce a row on conflict" are a contradiction, and the way it used to
// resolve was the worst available: the conflict — the case the clause was added
// to allow — came back as ErrNotFound, through the same `if err != nil` as
// everything else, from a call whose job was to make the row exist. The failure
// also inverts with state, so a test that inserts into a clean database passes
// and only the second call fails (#146).
func (i *Insert[T]) One(ctx context.Context, db Executor) (T, error) {
	var zero T
	if err := i.refuseSkippingTerminal(); err != nil {
		return zero, err
	}
	stored, err := i.Exec(ctx, db)
	if err != nil {
		return zero, err
	}
	if len(stored) == 0 {
		// Unreachable now that DO NOTHING is refused above, and kept because
		// One's contract is "one row or an error" and a silent index panic is
		// not the way to discover a statement that returned none.
		return zero, ErrNotFound
	}
	return stored[0], nil
}

// refuseSkippingTerminal rejects One over a clause that can return no row.
//
// Refused at the terminal rather than at OnConflictDoNothing, because the
// clause is fine and it is the pairing that is not — and the terminal is the
// call the author is about to get wrong.
func (i *Insert[T]) refuseSkippingTerminal() error {
	if !i.conflict.skipsRows() {
		return nil
	}
	alt := "OnConflictUpdate with the conflict target as its own update column"
	if t := i.conflict.target; len(t) > 0 {
		quoted := make([]string, len(t))
		for n, name := range t {
			quoted[n] = fmt.Sprintf("%q", name)
		}
		list := strings.Join(quoted, ", ")
		alt = fmt.Sprintf("OnConflictUpdate([]string{%s}, %s)", list, list)
	}
	return fmt.Errorf(
		"sqlb: One after OnConflictDoNothing on %s: a skipped insert returns no row, "+
			"so a conflict would answer ErrNotFound — the case the clause exists to allow;\n"+
			"  call Exec instead, whose empty slice and nil error are what \"it was already there\" looks like,\n"+
			"  or %s if the row is wanted whether or not this call created it",
		i.model.Table, alt)
}

// Update is an UPDATE statement over model T.
type Update[T any] struct {
	model    *Model
	dialect  Dialect
	sets     []assignment
	where    []Pred
	all      bool
	computed map[string]bool
	// fromName and fromQuery are set together, by From. fromQuery is nil for
	// every statement that does not call it, which is what SQL and Resolved
	// branch on.
	fromName  string
	fromQuery Subquery
	err       error
}

type assignment struct {
	column string
	value  Expr
}

// UpdateRows starts an UPDATE.
func UpdateRows[T any]() *Update[T] {
	m := ModelOf[T]()
	m.markInUse()
	return &Update[T]{model: m}
}

// Set assigns a value to a column.
func (u *Update[T]) Set(column string, value any) *Update[T] {
	if err := u.writable(column); err != nil {
		return u.fail("%s", err)
	}
	u.sets = append(u.sets, assignment{column: column, value: Param{Value: value}})
	return u
}

// SetExpr assigns an expression, for updates computed from the current row
// such as a counter increment.
func (u *Update[T]) SetExpr(column string, value Expr) *Update[T] {
	if err := u.writable(column); err != nil {
		return u.fail("%s", err)
	}
	u.sets = append(u.sets, assignment{column: column, value: value})
	return u
}

// writable reports why a column cannot be assigned, or nil.
func (u *Update[T]) writable(column string) error {
	col := u.model.Column(column)
	if col == nil {
		return fmt.Errorf("sqlb: %q is not a column of %s", column, u.model.Table)
	}
	if col.Computed() {
		return fmt.Errorf("sqlb: %q is computed by %s rather than stored, so it cannot be assigned; "+
			"set the columns its expression reads instead", column, u.model.Table)
	}
	return nil
}

// Where narrows the affected rows.
func (u *Update[T]) Where(preds ...Pred) *Update[T] {
	for _, p := range preds {
		if !p.IsZero() {
			u.where = append(u.where, p)
		}
	}
	return u
}

// Everything confirms an intentionally unscoped update.
func (u *Update[T]) Everything() *Update[T] {
	u.all = true
	return u
}

// From feeds query into the update as a CTE: `WITH name AS (query) UPDATE …
// FROM name …`. It exists for exactly one shape — the Postgres queue-claim
// idiom, where a batch is selected with ForUpdate().SkipLocked() and the same
// statement both marks and returns the rows it locked, in one round trip
// instead of the two an explicit transaction otherwise needs (#174):
//
//	claimed := sqlb.Query[Job]().Select(sqlb.F("id")).
//	    Where(sqlb.F("status").Eq("pending")).
//	    OrderBy(sqlb.F("id").Asc()).Limit(n).
//	    ForUpdate().SkipLocked()
//
//	sqlb.UpdateRows[Job]().
//	    From("claimed", claimed).
//	    Set("status", "claimed").
//	    Where(sqlb.F("id").EqField(sqlb.F("id").Qualify("claimed"))).
//	    Exec(ctx, db)
//
// This is not a general CTE facility, and building one is deliberately out of
// scope: a `From` on `Update` covers the queue-claim shape, which is the shape
// asked for, without the surrounding statement needing to say what several
// arbitrary CTEs would mean together.
//
// query is compiled straight into the surrounding statement — sharing its bind
// numbering, the same way a nested [Subquery] does — rather than being run, so
// it needs to arrive already resolved: call query.Resolved(ctx, db) first if
// its model has a registered BeforeQuery hook, and pass the result. An
// unresolved query is refused rather than silently compiled with its scope
// missing, for the reason [Subquery]'s own doc comment gives.
//
// name becomes both the CTE's name and, since it brings a second table into
// the statement, what an unqualified column reference in Set, SetExpr, Where
// or RETURNING resolves to when it is not the CTE's — [Field.Qualify] is how a
// predicate names the CTE's own column instead, as the Where above does.
func (u *Update[T]) From(name string, query Subquery) *Update[T] {
	if name == "" {
		return u.fail("sqlb: From needs a CTE name")
	}
	if query == nil {
		return u.fail("sqlb: From(%q) needs a query", name)
	}
	if err := query.Err(); err != nil {
		return u.fail("sqlb: From(%q): %s", name, err)
	}
	u.fromName = name
	u.fromQuery = query
	return u
}

// WithComputed adds the named computed columns to the statement's RETURNING.
//
// It is [Builder.WithComputed] for a write, and the default is the same: none.
// See [writeComputed] for why a write's derived columns are opt-in rather than
// automatic.
func (u *Update[T]) WithComputed(names ...string) *Update[T] {
	set, err := writeComputed(u.model, "WithComputed", u.computed, names)
	if err != nil {
		return u.fail("%s", err)
	}
	u.computed = set
	return u
}

// UseDialect overrides the dialect for this statement.
func (u *Update[T]) UseDialect(d Dialect) *Update[T] {
	u.dialect = d
	return u
}

func (u *Update[T]) fail(format string, args ...any) *Update[T] {
	if u.err == nil {
		u.err = fmt.Errorf(format, args...)
	}
	return u
}

// SQL compiles the statement without running it.
//
// What it renders is what this statement holds. A BeforeUpdate hook — the
// updated_at stamp, the predicate that narrows the affected rows — amends a
// clone on the exec path and is absent here; [Update.Resolved] applies them
// (#153).
func (u *Update[T]) SQL() (string, []any, error) {
	if u.err != nil {
		return "", nil, u.err
	}
	if len(u.sets) == 0 {
		return "", nil, fmt.Errorf("sqlb: update of %s assigns no columns", u.model.Table)
	}
	if len(u.where) == 0 && !u.all {
		return "", nil, ErrUnscoped
	}

	c := newCompiler(u.dialect)
	// A WHERE may name a derived column even though a SET may not.
	defer c.withComputed(u.model.Table, computedSetOf(u.model))()

	// The CTE compiles first, so its own binds — a LIMIT's argument, a WHERE
	// inside the SELECT — take the earlier positions and the outer statement's
	// binds continue the numbering rather than the two colliding. compileSub
	// shares this compiler for exactly that reason; see [Subquery].
	if u.fromQuery != nil {
		c.write("WITH ")
		c.ident(u.fromName)
		c.write(" AS (")
		u.fromQuery.compileSub(c)
		c.write(") ")
	}

	c.write("UPDATE ")
	c.table(u.model.Table)
	c.write(" SET ")

	// From brings a second table into the statement, so from here on an
	// unqualified column is the same ambiguity Builder.compile qualifies
	// against for a join — resolving it to the target table is what a caller
	// meant, the same argument as there. A statement with no From leaves
	// c.base empty and every column renders exactly as it always did.
	if u.fromQuery != nil {
		defer c.qualifyTo(u.model.Table)()
	}

	for n, a := range u.sets {
		if n > 0 {
			c.write(", ")
		}
		c.ident(a.column)
		c.write(" = ")
		c.expr(a.value)
	}
	if u.fromQuery != nil {
		c.write(" FROM ")
		c.ident(u.fromName)
	}
	if len(u.where) > 0 {
		c.write(" WHERE ")
		c.predicates(u.where)
	}
	writeReturning(c, u.model, u.computed)
	return c.result()
}

// Clone returns an independent copy, so a statement can be reused as the
// starting point for several derived ones.
func (u *Update[T]) Clone() *Update[T] {
	c := *u
	c.sets = append([]assignment(nil), u.sets...)
	c.where = append([]Pred(nil), u.where...)
	return &c
}

// Resolved returns a copy of the statement with the BeforeUpdate hooks
// registered for T against db applied — the statement that will actually run,
// which [Update.SQL] on its own is not. It is [Builder.Resolved] for a write,
// and the reason is the same: a hook that stamps a column or narrows the
// affected rows is invisible in the rendered text (#153).
//
// The receiver is untouched, as it is in Exec.
func (u *Update[T]) Resolved(ctx context.Context, db Executor) (*Update[T], error) {
	stmt := u.Clone()
	// What the caller wrote, taken before the hooks can add to it: a nested
	// query that appears only afterwards came from a hook, and the fix for that
	// one is not the fix for this one. See subqueryWalk.check (#288).
	authored := authoredIn(stmt.where, updateSetExprs(stmt))
	if err := hooksFor[T](db).runBeforeUpdate(ctx, stmt, releasedFrom(db)); err != nil {
		return nil, err
	}
	// A WHERE may name a nested query, and one over a confined model has to have
	// run that model's hooks before it can decide which rows this write touches.
	// See [Subquery].
	exprs := updateSetExprs(stmt)
	if err := guardNested(ctx, db, stmt.where, exprs,
		authored, "BeforeUpdate", ModelOf[T]().Type.Name()); err != nil {
		return nil, err
	}
	// From's query is compiled straight into this statement rather than run,
	// the same reason a nested Subquery is; see guardFrom.
	if stmt.fromQuery != nil {
		if err := guardFrom(ctx, db, stmt.fromName, stmt.fromQuery); err != nil {
			return nil, err
		}
	}
	return stmt, nil
}

func (u *Update[T]) resolvedSQL(ctx context.Context, db Executor) (string, []any, error) {
	stmt, err := u.Resolved(ctx, db)
	if err != nil {
		return "", nil, err
	}
	return stmt.SQL()
}

// Exec runs the update and returns the updated rows.
//
// The statement is cloned first, for the reason Builder.All clones: a
// BeforeUpdate hook amends what it is given, and the doc comment's own example
// is one that calls Set. Amending the caller's statement would make a second
// Exec assign updated_at twice and narrow a scoping predicate twice.
func (u *Update[T]) Exec(ctx context.Context, db Executor) ([]T, error) {
	hooks := hooksFor[T](db)
	stmt, err := u.Resolved(ctx, db)
	if err != nil {
		return nil, err
	}
	query, args, err := stmt.SQL()
	if err != nil {
		return nil, err
	}
	rows, err := runQuery(ctx, db, query, args...)
	if err != nil {
		return nil, err
	}
	updated, err := scanAllClose[T](rows, u.model)
	if err != nil {
		return nil, asConstraintErr(err)
	}
	if err := hooks.runAfterUpdate(ctx, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

// One runs an update expected to touch exactly one row.
//
// The check is on the result, so an update matching several rows has already
// changed all of them when the error returns. Under autocommit that is durable;
// inside WithTx the error rolls it back, which is the way to make "expected
// one" a refusal rather than a report.
func (u *Update[T]) One(ctx context.Context, db Executor) (T, error) {
	var zero T
	updated, err := u.Exec(ctx, db)
	if err != nil {
		return zero, err
	}
	switch len(updated) {
	case 0:
		return zero, ErrNotFound
	case 1:
		return updated[0], nil
	default:
		return zero, fmt.Errorf("sqlb: update matched %d rows in %s, expected one; "+
			"they have already been updated — wrap the call in WithTx if the count "+
			"needs to be able to refuse it", len(updated), u.model.Table)
	}
}

// Delete is a DELETE statement over model T.
type Delete[T any] struct {
	model    *Model
	dialect  Dialect
	where    []Pred
	all      bool
	computed map[string]bool
	err      error
	// returning asks for the removed rows back. Not settable by a caller: it is
	// set on the clone [Delete.Exec] runs, when an [Hooks.AfterDeleteRows] hook
	// is registered for T and the rows therefore have somewhere to go. A delete
	// whose rows nothing reads should not pay to scan them.
	returning bool
}

// DeleteRows starts a DELETE.
func DeleteRows[T any]() *Delete[T] {
	m := ModelOf[T]()
	m.markInUse()
	return &Delete[T]{model: m}
}

// Where narrows the affected rows.
func (d *Delete[T]) Where(preds ...Pred) *Delete[T] {
	for _, p := range preds {
		if !p.IsZero() {
			d.where = append(d.where, p)
		}
	}
	return d
}

// Everything confirms an intentionally unscoped delete.
func (d *Delete[T]) Everything() *Delete[T] {
	d.all = true
	return d
}

// WithComputed adds the named computed columns to the statement's RETURNING.
//
// A delete only carries a RETURNING at all when an [Hooks.AfterDeleteRows] hook
// is registered, so this decides what those rows hold. The default is none, as
// it is on the other two writes; see [writeComputed].
func (d *Delete[T]) WithComputed(names ...string) *Delete[T] {
	set, err := writeComputed(d.model, "WithComputed", d.computed, names)
	if err != nil {
		if d.err == nil {
			d.err = err
		}
		return d
	}
	d.computed = set
	return d
}

// UseDialect overrides the dialect for this statement.
func (d *Delete[T]) UseDialect(dl Dialect) *Delete[T] {
	d.dialect = dl
	return d
}

// SQL compiles the statement without running it.
//
// No RETURNING clause, and no BeforeDelete predicate. Both are decided by the
// hooks registered for T against the executor it runs on, and this method has no
// executor — so what it prints is what a delete with no hooks at all sends.
// [Delete.Resolved] takes one and renders the statement that will run. See
// [Hooks.AfterDeleteRows].
func (d *Delete[T]) SQL() (string, []any, error) {
	if d.err != nil {
		return "", nil, d.err
	}
	if len(d.where) == 0 && !d.all {
		return "", nil, ErrUnscoped
	}
	c := newCompiler(d.dialect)
	defer c.withComputed(d.model.Table, computedSetOf(d.model))()
	c.write("DELETE FROM ")
	c.table(d.model.Table)
	if len(d.where) > 0 {
		c.write(" WHERE ")
		c.predicates(d.where)
	}
	if d.returning {
		writeReturning(c, d.model, d.computed)
	}
	return c.result()
}

// Clone returns an independent copy, so a statement can be reused as the
// starting point for several derived ones.
func (d *Delete[T]) Clone() *Delete[T] {
	c := *d
	c.where = append([]Pred(nil), d.where...)
	return &c
}

// Resolved returns a copy of the statement with the BeforeDelete hooks
// registered for T against db applied, and with RETURNING decided — the
// statement that will actually run, which [Delete.SQL] on its own is not. It is
// [Builder.Resolved] for a delete (#153).
//
// The receiver is untouched, as it is in Exec.
func (d *Delete[T]) Resolved(ctx context.Context, db Executor) (*Delete[T], error) {
	hooks := hooksFor[T](db)
	stmt := d.Clone()
	// See [Update.Resolved]: taken before the hooks so a nested query that only
	// appears afterwards can be named as theirs.
	authored := authoredIn(stmt.where, nil)
	if err := hooks.runBeforeDelete(ctx, stmt, releasedFrom(db)); err != nil {
		return nil, err
	}
	// Decided after BeforeDelete, so that a hook registering on first use — which
	// On[T] does — is visible to the statement it is about to affect. It is part
	// of resolving rather than of executing because RETURNING is the difference
	// between a bare DELETE and one that scans every row it removed, which is a
	// difference an inspection exists to show.
	stmt.returning = hooks.wantsDeletedRows()
	// See [Update.Resolved]: a nested query choosing which rows a write removes
	// is the position where a missing scope matters most.
	if err := guardNested(ctx, db, stmt.where, nil,
		authored, "BeforeDelete", ModelOf[T]().Type.Name()); err != nil {
		return nil, err
	}
	return stmt, nil
}

// updateSetExprs is the values an UPDATE assigns, which can each carry a nested
// query of their own. Named because it is now taken twice: once before the
// hooks run to record what the caller wrote, once after to check it.
func updateSetExprs[T any](stmt *Update[T]) []Expr {
	exprs := make([]Expr, 0, len(stmt.sets))
	for _, a := range stmt.sets {
		exprs = append(exprs, a.value)
	}
	return exprs
}

func (d *Delete[T]) resolvedSQL(ctx context.Context, db Executor) (string, []any, error) {
	stmt, err := d.Resolved(ctx, db)
	if err != nil {
		return "", nil, err
	}
	return stmt.SQL()
}

// Exec runs the delete and returns the number of rows removed.
//
// The statement is cloned first, for the reason Update.Exec clones: a
// BeforeDelete hook narrowing the statement must narrow one execution, not
// every later one.
//
// It sends `DELETE … RETURNING` and scans what comes back only when an
// [Hooks.AfterDeleteRows] hook is registered for T. Without one this is a bare
// DELETE and the count is the command tag's, exactly as it always was.
func (d *Delete[T]) Exec(ctx context.Context, db Executor) (int64, error) {
	hooks := hooksFor[T](db)
	stmt, err := d.Resolved(ctx, db)
	if err != nil {
		return 0, err
	}
	query, args, err := stmt.SQL()
	if err != nil {
		return 0, err
	}

	var n int64
	var deleted []T
	if stmt.returning {
		rows, err := runQuery(ctx, db, query, args...)
		if err != nil {
			return 0, err
		}
		if deleted, err = scanAllClose[T](rows, d.model); err != nil {
			return 0, asConstraintErr(err)
		}
		// RETURNING yields one row per row removed, so this is the command tag's
		// count arrived at by the other road rather than a second answer.
		n = int64(len(deleted))
	} else {
		tag, err := db.Exec(ctx, query, args...)
		if err != nil {
			return 0, wrapQueryErr(err, query)
		}
		n = tag.RowsAffected()
	}

	if err := hooks.runAfterDelete(ctx, n); err != nil {
		return 0, err
	}
	if err := hooks.runAfterDeleteRows(ctx, deleted); err != nil {
		return 0, err
	}
	return n, nil
}

// writeComputed validates the names a write asks its RETURNING to evaluate and
// returns the set to hold, leaving the caller's untouched.
//
// A write's computed columns are opt-in, and the default is none. Reads flipped
// to opt-in in #92 — "a computed column is a cost, and one the schema happens to
// declare is not the same thing as one this caller wants to serve" — and the
// write path was simply never revisited, so the same aggregate a read had to ask
// for by name was evaluated by every INSERT and UPDATE of the table whether or
// not anyone read it. Three things came of that, and only the first is about
// cost (#164): a create returned a value that was structurally wrong, because
// the rows the subquery counts are written later in the same transaction; and a
// subquery naming another module's table rode into the RETURNING of every insert,
// so the table could not be written at all unless that module's tables were
// present. Opting in contains both to the caller that wanted the column.
//
// A column carrying [Field.Needs] is refused rather than accepted-and-skipped. A
// parameterised expression needs a bind, and a mutation has nowhere to take one
// from: the value is a property of who is asking, and the hooks a write runs
// receive the row rather than the statement. ADR-0041 decided such a column is
// left out and read back by the next query — so a caller naming one here is
// asking for something no write can produce, and hearing so is better than a
// field that silently arrives holding its zero value.
func writeComputed(m *Model, method string, have map[string]bool, names []string) (map[string]bool, error) {
	if len(names) == 0 {
		return have, nil
	}
	set := make(map[string]bool, len(have)+len(names))
	for name := range have {
		set[name] = true
	}
	for _, name := range names {
		col := m.Column(name)
		switch {
		case col == nil:
			return nil, fmt.Errorf("sqlb: %s(%q): not a column of %s (have: %s)",
				method, name, m.Table, strings.Join(m.ColumnNames(), ", "))
		case !col.Computed():
			return nil, fmt.Errorf("sqlb: %s(%q): %s stores that column rather than computing it; "+
				"a stored column is already in RETURNING", method, name, m.Table)
		case len(col.Needs) > 0:
			return nil, fmt.Errorf("sqlb: %s(%q): %s computes that column from the %s bind, and a write has "+
				"nowhere to take a bind from — the value is a property of who is asking, and the hooks a write "+
				"runs receive the row rather than the statement; read it back with the next query (ADR-0041)",
				method, name, m.Table, quoteList(col.Needs))
		}
		set[name] = true
	}
	return set, nil
}

// quoteList renders a short list of names for an error message.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// writeReturning appends RETURNING over every stored column, so that callers see
// database-generated values without a follow-up read, plus whichever computed
// columns the statement asked for.
//
// Computed columns are not in it by default. See [writeComputed] for the
// argument, and WithComputed on [Insert], [Update] and [Delete] for the opt-in.
//
// A stored column renders through [compiler.column] rather than a bare
// [compiler.ident], so that it picks up c.base when one is set. Every caller
// but a From-bearing [Update] leaves c.base empty and gets exactly the bare
// name this always rendered; [Update.From] brings a second table — the CTE —
// into the statement, and RETURNING can see both of them, so an unqualified
// column already ambiguous in SET and WHERE is the same ambiguity here if the
// CTE happens to project a column of the same name (it does, in the queue-claim
// shape From exists for: the claimed id).
func writeReturning(c *compiler, m *Model, computed map[string]bool) {
	c.write(" RETURNING ")
	first := true
	for _, col := range m.Columns {
		if col.Computed() && !computed[col.Name] {
			continue
		}
		if !first {
			c.write(", ")
		}
		first = false
		if col.Computed() {
			c.computedExpr(col, &computedSet{})
			c.write(" AS ")
			c.ident(col.Name)
			continue
		}
		c.column(Column{Name: col.Name})
	}
}

// scanAllClose scans a RETURNING result set and closes it.
func scanAllClose[T any](rows rowSource, m *Model) ([]T, error) {
	defer rows.Close()
	return scanAll[T](rows, m)
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// checkConflictSet validates one DO UPDATE assignment before it is rendered.
//
// Two things, and both are the same principle the bare-column form already
// follows: a name that is not a column should be an error from this package
// naming the column, not a 42703 from Postgres at request time.
//
// The second is the ambiguity. Inside DO UPDATE both the proposed row and the
// stored one are in scope, so a bare column reference has two readings and SQL
// picks the stored one silently. `count = count + 1` is the shape that makes it
// concrete: it reads like an accumulation and it is one, but a reader cannot
// tell from the Go whether the author meant the stored count or the proposed
// one. Refusing is the only answer that does not quietly choose (#90).
func (i *Insert[T]) checkConflictSet(set conflictSet) error {
	col := i.model.Column(set.column)
	if col == nil {
		return fmt.Errorf("sqlb: OnConflictSet(%q): not a column of %s (have: %s)",
			set.column, i.model.Table, strings.Join(i.model.ColumnNames(), ", "))
	}
	if col.Computed() {
		return fmt.Errorf("sqlb: OnConflictSet(%q): %s computes that column rather than storing it; there is nothing to assign to",
			set.column, i.model.Table)
	}
	if set.value == nil {
		return fmt.Errorf("sqlb: OnConflictSet(%q): no expression; pass sqlb.Val(nil) to assign NULL", set.column)
	}
	return i.checkConflictExpr(set.column, set.value)
}

// checkConflictExpr walks an assignment for column references that do not say
// which side of the conflict they mean, and for names no column answers to.
//
// Raw is not walked, for the reason Raw is never walked: its contents are SQL
// text this package has not parsed, which is what it is for.
func (i *Insert[T]) checkConflictExpr(assigned string, e Expr) error {
	switch n := e.(type) {
	case nil, Param, sharedParam, Raw:
		return nil

	case ConflictRef:
		if i.model.Column(n.name) == nil {
			side := "Current"
			if n.excluded {
				side = "Excluded"
			}
			return fmt.Errorf("sqlb: OnConflictSet(%q): %s(%q) is not a column of %s (have: %s)",
				assigned, side, n.name, i.model.Table, strings.Join(i.model.ColumnNames(), ", "))
		}
		return nil

	case Column:
		return ambiguousConflictRef(assigned, n.Name)
	case Field:
		return ambiguousConflictRef(assigned, n.Name())

	case List:
		for _, item := range n.Items {
			if err := i.checkConflictExpr(assigned, item); err != nil {
				return err
			}
		}
		return nil
	case Binary:
		if err := i.checkConflictExpr(assigned, n.Left); err != nil {
			return err
		}
		return i.checkConflictExpr(assigned, n.Right)
	case Unary:
		return i.checkConflictExpr(assigned, n.Operand)
	case Cast:
		return i.checkConflictExpr(assigned, n.Inner)
	case Call:
		for _, arg := range n.Args {
			if err := i.checkConflictExpr(assigned, arg); err != nil {
				return err
			}
		}
		return nil
	case BetweenExpr:
		for _, part := range []Expr{n.Operand, n.Lo, n.Hi} {
			if err := i.checkConflictExpr(assigned, part); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}

func ambiguousConflictRef(assigned, name string) error {
	return fmt.Errorf(
		"sqlb: OnConflictSet(%q): the reference to %q does not say which row it means; "+
			"inside DO UPDATE both are in scope — write sqlb.Excluded(%q) for the row being "+
			"inserted or sqlb.Current(%q) for the one already stored",
		assigned, name, name, name)
}
