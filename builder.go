package sqlb

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Builder is a SELECT statement under construction against model T.
//
// Its methods mutate the builder in place and return it, so a query can be
// assembled across branches without reassignment gymnastics and hooks can
// amend a query they are handed. Use Clone before sharing a partially built
// query between goroutines or request scopes.
type Builder[T any] struct {
	model    *Model
	dialect  Dialect
	alias    string
	sel      []Selection
	distinct bool
	joins    []joinClause
	where    []Pred
	// seek is the keyset boundary, held apart from where because it is
	// pagination rather than selection: countSQL drops it, exactly as it drops
	// LIMIT and OFFSET. See cursor.go.
	seek   Pred
	groups []Expr
	having []Pred
	orders []Order
	expand []string
	// expandOnly narrows an expanded row to the columns a caller named, one set
	// per relation. An absent entry means the whole row, which is what Expand
	// alone asks for. See ExpandOnly.
	expandOnly map[string]map[string]bool
	// expandScope holds the target's BeforeQuery predicates for each expanded
	// relation, already requalified onto the join alias. It is filled on the
	// execution path rather than at Expand(), because the predicates depend on
	// the context a hook reads its tenant from — the same reason the parent's
	// own hooks do not run at build time either. See expand.go.
	expandScope map[string][]Pred
	// binds are the values a computed column's Needs names, one shared slot per
	// key so that a viewer named in the projection, the WHERE and the ORDER BY
	// is sent to the database once. See Bind.
	binds map[string]*sharedValue
	// computed names the derived columns this query is willing to pay for.
	// Empty means none: a computed column is opt-in, because it is declared on
	// the model and wanted by one caller (#92). See WithComputed.
	computed map[string]bool
	limit    *int
	offset   *int
	lock     string
	// resolved records that this query has run its model's BeforeQuery hooks,
	// which is what makes it safe to nest inside another statement. See
	// [Subquery]. It survives Clone because the predicates the hooks added do.
	resolved bool
	err      error
	// withName and withQuery are the single named CTE With adds, mirroring
	// Update.fromName/fromQuery. See With.
	withName  string
	withQuery Subquery
}

type joinClause struct {
	kind  string // "JOIN", "LEFT JOIN", ...
	table string
	alias string
	on    Pred
}

// Query starts a SELECT against the table mapped by T.
func Query[T any]() *Builder[T] {
	m := ModelOf[T]()
	m.markInUse()
	return &Builder[T]{model: m, alias: m.Table}
}

// Model returns the reflected model the query runs against.
func (b *Builder[T]) Model() *Model { return b.model }

// Err returns the first error recorded while building, if any. Terminal
// methods return it too, so checking it explicitly is optional.
func (b *Builder[T]) Err() error { return b.err }

// Fail records err and returns the builder, so a package outside sqlb can put
// a query into the same error state the builder uses internally rather than
// having to break the fluent chain with its own error return. Only the first
// error is kept, matching fail.
//
// filter.Apply is the motivating caller: it assembles a builder from a parsed
// request and needs somewhere to put "this request is valid but I cannot
// express it".
func (b *Builder[T]) Fail(err error) *Builder[T] {
	if b.err == nil {
		b.err = err
	}
	return b
}

func (b *Builder[T]) fail(format string, args ...any) *Builder[T] {
	if b.err == nil {
		b.err = fmt.Errorf(format, args...)
	}
	return b
}

// Clone returns an independent copy, so a base query can be reused as the
// starting point for several derived ones.
func (b *Builder[T]) Clone() *Builder[T] {
	c := *b
	c.sel = append([]Selection(nil), b.sel...)
	c.joins = append([]joinClause(nil), b.joins...)
	c.where = append([]Pred(nil), b.where...)
	c.groups = append([]Expr(nil), b.groups...)
	c.having = append([]Pred(nil), b.having...)
	c.orders = append([]Order(nil), b.orders...)
	c.expand = append([]string(nil), b.expand...)
	if b.expandScope != nil {
		c.expandScope = make(map[string][]Pred, len(b.expandScope))
		for k, v := range b.expandScope {
			c.expandScope[k] = append([]Pred(nil), v...)
		}
	}
	if b.expandOnly != nil {
		c.expandOnly = make(map[string]map[string]bool, len(b.expandOnly))
		for k, v := range b.expandOnly {
			cols := make(map[string]bool, len(v))
			for col := range v {
				cols[col] = true
			}
			c.expandOnly[k] = cols
		}
	}
	if b.computed != nil {
		c.computed = make(map[string]bool, len(b.computed))
		for k := range b.computed {
			c.computed[k] = true
		}
	}
	if b.binds != nil {
		// The map is copied and the slots are not: rebinding a key on the copy
		// must not reach the original, while a value already bound is the same
		// parameter in both, which is what makes it one placeholder.
		c.binds = make(map[string]*sharedValue, len(b.binds))
		for k, v := range b.binds {
			c.binds[k] = v
		}
	}
	if b.limit != nil {
		v := *b.limit
		c.limit = &v
	}
	if b.offset != nil {
		v := *b.offset
		c.offset = &v
	}
	return &c
}

// UseDialect overrides the dialect for this query.
func (b *Builder[T]) UseDialect(d Dialect) *Builder[T] {
	b.dialect = d
	return b
}

// WithComputed adds the named computed columns to the projection.
//
// Computed columns are opt-in. They are declared on the model, which is shared,
// and wanted by one caller — so projecting them by default charged every read
// of the model for a list screen's aggregates, and a column carrying a Needs
// bind made unrelated reads fail outright:
//
//	sqlb.Query[Project]().Where(sqlb.F("id").Eq(id)).One(ctx, db)
//	// used to answer: computed column "is_starred" needs the "viewer" bind
//
// That query is asking whether a row exists. It has no viewer and should not
// need one, and before this it had no way to say so (#92).
//
//	sqlb.Query[Project]().
//	    WithComputed("total_tasks", "is_starred").
//	    Bind("viewer", actor.ID)
//
// Naming a column the model does not have, or one it stores rather than
// computes, fails the query — a silent no-op would leave the caller believing
// they had asked for a value that is about to arrive as the zero value.
//
// Selecting a computed column explicitly with [Builder.Select] works too and
// does not need this; WithComputed is for keeping the default projection and
// adding to it. For a REST resource the equivalent is rest.Options.Computed.
func (b *Builder[T]) WithComputed(names ...string) *Builder[T] {
	for _, name := range names {
		col := b.model.Column(name)
		switch {
		case col == nil:
			b.fail("sqlb: WithComputed(%q): not a column of %s (have: %s)",
				name, b.model.Table, strings.Join(b.model.ColumnNames(), ", "))
			return b
		case !col.Computed():
			b.fail("sqlb: WithComputed(%q): %s stores that column rather than computing it; "+
				"it is already in the projection", name, b.model.Table)
			return b
		}
		if b.computed == nil {
			b.computed = make(map[string]bool, len(names))
		}
		b.computed[name] = true
	}
	return b
}

// Bind supplies the value a computed column declared it needs.
//
// A computed column whose expression takes a bind is answered per request — is
// this row starred *by the caller* — so the value cannot be in the schema and
// has to arrive with the query:
//
//	sqlb.On[Project](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[Project]) error {
//	    q.Bind("viewer", memberFrom(ctx))
//	    return nil
//	})
//
// The value binds once however many times the expression is rendered: the
// projection, a filter on the column and an ordering by it all resolve to the
// same placeholder.
//
// Binding a key no computed column names is harmless and does nothing. The
// failure worth catching is the other one — a column whose bind never arrives —
// and it is caught twice: the query fails rather than rendering NULL, and
// [rest.Resource] refuses at startup to mount a resource with no hook to supply
// it (ADR-0030, ADR-0041).
func (b *Builder[T]) Bind(key string, value any) *Builder[T] {
	if b.binds == nil {
		b.binds = make(map[string]*sharedValue, 1)
	}
	b.binds[key] = &sharedValue{value: value}
	return b
}

// Bound reports the binds this query carries, for a caller inspecting one.
func (b *Builder[T]) Bound() []string {
	out := make([]string, 0, len(b.binds))
	for k := range b.binds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// computedSet is the model's derived columns as they stand for this query,
// with whatever binds it carries. Nil when the model computes nothing, which
// is the common case and costs one length check.
func (b *Builder[T]) computedSet() *computedSet {
	set := computedSetOf(b.model)
	if set == nil {
		return nil
	}
	set.binds = b.binds
	return set
}

// As aliases the table, which is required for self-joins.
func (b *Builder[T]) As(alias string) *Builder[T] {
	b.alias = alias
	return b
}

// Select appends to the projection. Without any call the query selects every
// mapped column of T. Use ClearSelect to start the projection over.
func (b *Builder[T]) Select(items ...Selectable) *Builder[T] {
	for _, it := range items {
		b.sel = append(b.sel, it.selection())
	}
	return b
}

// ClearSelect discards the projection built so far, so the next Select starts
// from nothing rather than adding to it.
func (b *Builder[T]) ClearSelect() *Builder[T] {
	b.sel = nil
	return b
}

// Distinct adds DISTINCT to the projection.
func (b *Builder[T]) Distinct() *Builder[T] {
	b.distinct = true
	return b
}

// Where conjoins predicates. Zero predicates are skipped, so conditional
// filters need no surrounding if statement.
func (b *Builder[T]) Where(preds ...Pred) *Builder[T] {
	for _, p := range preds {
		if !p.IsZero() {
			b.where = append(b.where, p)
		}
	}
	return b
}

// Join adds an inner join. Pass an empty alias to use the table name.
func (b *Builder[T]) Join(table, alias string, on Pred) *Builder[T] {
	return b.join("JOIN", table, alias, on)
}

// LeftJoin adds a left outer join.
func (b *Builder[T]) LeftJoin(table, alias string, on Pred) *Builder[T] {
	return b.join("LEFT JOIN", table, alias, on)
}

// RightJoin adds a right outer join.
func (b *Builder[T]) RightJoin(table, alias string, on Pred) *Builder[T] {
	return b.join("RIGHT JOIN", table, alias, on)
}

// FullJoin adds a full outer join.
func (b *Builder[T]) FullJoin(table, alias string, on Pred) *Builder[T] {
	return b.join("FULL JOIN", table, alias, on)
}

// CrossJoin adds a cross join: every row of table paired with every row this
// statement already has. There is no ON condition — a cross join is the one
// join kind where "no predicate" is the point rather than a mistake, which is
// why it does not go through join(), whose zero-on check exists precisely to
// catch that mistake for every other kind.
func (b *Builder[T]) CrossJoin(table, alias string) *Builder[T] {
	b.joins = append(b.joins, joinClause{kind: "CROSS JOIN", table: table, alias: alias})
	return b
}

// With adds a single named CTE ahead of the statement: `WITH name AS (query)
// SELECT …`. Reference it like any other table — Join(name, alias, on) or
// LeftJoin(name, alias, on) — the same way [Update.From]'s CTE is joined
// against the table being updated rather than replacing it.
//
// This is not a general CTE facility, for the same reason [Update.From]'s doc
// comment gives: a second With call replaces the first rather than adding a
// second CTE, and there is no WITH RECURSIVE. It exists so a SELECT can name
// and reuse a subquery result the way UPDATE already can, without the
// surrounding statement needing to say what several arbitrary CTEs would mean
// together.
//
// query compiles straight into the surrounding statement, sharing its bind
// numbering, rather than being run — so it needs to arrive already resolved:
// call query.Resolved(ctx, db) first if its model has a registered
// BeforeQuery hook, and pass the result. An unresolved query is refused by
// [Builder.Resolved] rather than silently compiled with its scope missing,
// for the reason [Subquery]'s own doc comment gives.
func (b *Builder[T]) With(name string, query Subquery) *Builder[T] {
	if name == "" {
		return b.fail("sqlb: With needs a CTE name")
	}
	if query == nil {
		return b.fail("sqlb: With(%q) needs a query", name)
	}
	if err := query.Err(); err != nil {
		return b.fail("sqlb: With(%q): %s", name, err)
	}
	b.withName = name
	b.withQuery = query
	return b
}

func (b *Builder[T]) join(kind, table, alias string, on Pred) *Builder[T] {
	if on.IsZero() {
		return b.fail("sqlb: %s %s has no ON condition, which would produce a cross join", kind, table)
	}
	b.joins = append(b.joins, joinClause{kind: kind, table: table, alias: alias, on: on})
	return b
}

// GroupBy groups by the given columns.
//
// A grouped result is usually not the model's shape — the aggregate the query
// exists for has no field on T — so it is read with [Collect] rather than with
// [Builder.All]:
//
//	type PerOrg struct {
//	    OrgID string `db:"org_id"`
//	    Open  int64  `db:"open"`
//	}
//	rows, err := sqlb.Collect[PerOrg](ctx, db,
//	    sqlb.Query[Thread]().
//	        Where(sqlb.F("status").OneOf("open", "escalated")).
//	        GroupBy(sqlb.F("org_id")).
//	        Select(sqlb.F("org_id"), sqlb.Count().As("open")))
//
// Query hooks run either way, so a confined table stays confined through an
// aggregate — which is the whole reason to reach for this rather than dropping
// to [DB.Query] and hand-writing the SQL, where nothing obliges the tenant
// predicate and a wrong number looks like a right one (#306).
//
// Calling All on a grouped query is refused when the projection carries
// something T cannot hold, rather than discarding it: the discarded column is
// the aggregate, so the rows would come back the right length and zero where
// the number should be.
func (b *Builder[T]) GroupBy(fields ...Field) *Builder[T] {
	for _, f := range fields {
		b.groups = append(b.groups, f.Column())
	}
	return b
}

// GroupByExpr groups by arbitrary expressions. See [Builder.GroupBy] for how a
// grouped result is read.
func (b *Builder[T]) GroupByExpr(exprs ...Expr) *Builder[T] {
	b.groups = append(b.groups, exprs...)
	return b
}

// Having filters grouped rows. See [Builder.GroupBy] for how a grouped result
// is read.
func (b *Builder[T]) Having(preds ...Pred) *Builder[T] {
	for _, p := range preds {
		if !p.IsZero() {
			b.having = append(b.having, p)
		}
	}
	return b
}

// OrderBy appends ordering terms.
func (b *Builder[T]) OrderBy(orders ...Order) *Builder[T] {
	b.orders = append(b.orders, orders...)
	return b
}

// OrderColumns names the columns the query orders by, in order, skipping any
// term that orders by an expression rather than a column.
//
// filter.Apply is the motivating caller: it owns the projection and has to
// cover whatever the ordering ended up being, including the tiebreaker Stable
// appended, so that a cursor can be read off the last row.
func (b *Builder[T]) OrderColumns() []string {
	out := make([]string, 0, len(b.orders))
	for _, o := range b.orders {
		if col, ok := o.expr.(Column); ok {
			out = append(out, col.Name)
		}
	}
	return out
}

// Limit caps the number of rows returned. A negative limit is an error rather
// than a silent no-op, since it usually means an unchecked computed value.
func (b *Builder[T]) Limit(n int) *Builder[T] {
	if n < 0 {
		return b.fail("sqlb: negative limit %d", n)
	}
	b.limit = &n
	return b
}

// Offset skips rows.
func (b *Builder[T]) Offset(n int) *Builder[T] {
	if n < 0 {
		return b.fail("sqlb: negative offset %d", n)
	}
	b.offset = &n
	return b
}

// Page applies offset pagination. Pages are 1-based.
func (b *Builder[T]) Page(number, size int) *Builder[T] {
	if number < 1 {
		return b.fail("sqlb: page number %d is below 1", number)
	}
	if size < 1 {
		return b.fail("sqlb: page size %d is below 1", size)
	}
	return b.Limit(size).Offset((number - 1) * size)
}

// ForUpdate takes row locks for the duration of the transaction.
func (b *Builder[T]) ForUpdate() *Builder[T] {
	b.lock = "FOR UPDATE"
	return b
}

// ForShare takes shared row locks.
func (b *Builder[T]) ForShare() *Builder[T] {
	b.lock = "FOR SHARE"
	return b
}

// SkipLocked skips rows already locked, for queue-style consumers. It has no
// effect without ForUpdate or ForShare.
func (b *Builder[T]) SkipLocked() *Builder[T] {
	if b.lock == "" {
		return b.fail("sqlb: SkipLocked requires ForUpdate or ForShare")
	}
	b.lock += " SKIP LOCKED"
	return b
}

// SQL compiles the query to SQL text and its bind parameters. It is the
// inspection point: log it, diff it in tests, or paste it into EXPLAIN.
//
// What it renders is what *this builder* holds, which on a model with
// BeforeQuery hooks is not the statement that reaches the wire: a hook amends a
// clone on the exec path, so a tenant predicate registered once for every read
// of the model is absent here. Use [Builder.Resolved] first for the resolved
// text — that is the form to splice into raw SQL, and the form to assert a scope
// against (#153).
func (b *Builder[T]) SQL() (string, []any, error) {
	if b.err != nil {
		return "", nil, b.err
	}
	c := newCompiler(b.dialect)
	b.compile(c)
	return c.result()
}

func (b *Builder[T]) compile(c *compiler) {
	// The CTE compiles first, so its own binds take the earlier positions and
	// the rest of the statement's binds continue the numbering rather than the
	// two colliding — the same reason [Update.SQL] orders it first. compileSub
	// shares this compiler for exactly that reason; see [Subquery].
	if b.withQuery != nil {
		c.write("WITH ")
		c.ident(b.withName)
		c.write(" AS (")
		b.withQuery.compileSub(c)
		c.write(") ")
	}
	// Once a second table is in the statement, an unqualified column is a
	// coin toss Postgres refuses to make. Everything a caller can name — the
	// projection, a filter, a sort — is a column of T, so T's table is the
	// answer.
	if b.joined() {
		defer c.qualifyTo(b.from())()
	}
	// A derived column resolves under this statement's table and under a bare
	// name, which is how every term a caller can write reaches it.
	defer c.withComputed(b.from(), b.computedSet())()

	c.write("SELECT ")
	if b.distinct {
		c.write("DISTINCT ")
	}
	b.compileProjection(c)
	b.compileExpansionSelections(c)

	c.write(" FROM ")
	c.table(b.model.Table)
	if b.alias != "" && b.alias != b.model.Table {
		c.write(" AS ")
		c.ident(b.alias)
	}

	for _, j := range b.joins {
		c.write(" ")
		c.write(j.kind)
		c.write(" ")
		c.table(j.table)
		if j.alias != "" && j.alias != j.table {
			c.write(" AS ")
			c.ident(j.alias)
		}
		if !j.on.IsZero() {
			c.write(" ON ")
			c.expr(j.on.Expr())
		}
	}
	b.compileExpansions(c)

	if preds := b.filters(); len(preds) > 0 {
		c.write(" WHERE ")
		c.predicates(preds)
	}

	if len(b.groups) > 0 {
		c.write(" GROUP BY ")
		for i, g := range b.groups {
			if i > 0 {
				c.write(", ")
			}
			c.expr(g)
		}
	}

	if len(b.having) > 0 {
		c.write(" HAVING ")
		c.predicates(b.having)
	}

	if len(b.orders) > 0 {
		c.write(" ORDER BY ")
		c.orders(b.orders)
	}

	// Limit and offset are rendered as literals, not bind parameters, so that
	// the planner can see them. Both are ints validated above, so there is no
	// injection surface.
	if b.limit != nil {
		c.write(" LIMIT ")
		c.write(strconv.Itoa(*b.limit))
	}
	if b.offset != nil {
		c.write(" OFFSET ")
		c.write(strconv.Itoa(*b.offset))
	}
	if b.lock != "" {
		c.write(" ")
		c.write(b.lock)
	}
}

func (b *Builder[T]) compileProjection(c *compiler) {
	if len(b.sel) == 0 {
		first := true
		for _, col := range b.model.Columns {
			// A computed column is declared on the model and wanted by one
			// caller, so the default projection leaves it out: otherwise a
			// correlated subquery declared for a list screen is attached to
			// every read of the model, including an existence check by id, and
			// one carrying a Needs bind makes those reads fail outright (#92).
			// WithComputed is how a caller asks for it.
			if col.Computed() && !b.computed[col.Name] {
				continue
			}
			if !first {
				c.write(", ")
			}
			first = false
			c.column(Column{Table: b.alias, Name: col.Name})
			// An expression has no name of its own, and the scan matches result
			// columns to fields by name. Aliasing it back to the column it was
			// declared as is what makes `(due_date < current_date)` arrive as
			// is_overdue.
			if col.Computed() {
				c.write(" AS ")
				c.ident(col.Name)
			}
		}
		return
	}
	for i, s := range b.sel {
		if i > 0 {
			c.write(", ")
		}
		c.expr(s.expr)
		alias := s.alias
		if alias == "" {
			// Same reason as above, for the projection filter.Apply builds:
			// it names columns, including derived ones, as plain F(name).
			alias = c.derivedAlias(s.expr)
		}
		if alias != "" {
			c.write(" AS ")
			c.ident(alias)
		}
	}
}

// countSQL compiles the row count for the query, ignoring projection, ordering
// and pagination. Grouped and distinct queries are wrapped instead, since for
// them the number of rows is not the number of rows the FROM clause produces.
func (b *Builder[T]) countSQL() (string, []any, error) {
	if b.err != nil {
		return "", nil, b.err
	}
	c := newCompiler(b.dialect)

	// A grouped query counts groups. A distinct one counts the rows left after
	// duplicates are removed, which is what All returns and therefore what a
	// count has to agree with — dropping the DISTINCT and counting the rows
	// underneath answers a question nobody asked, and answers it too high.
	if len(b.groups) > 0 || b.distinct {
		inner := b.Clone()
		inner.orders = nil
		inner.limit, inner.offset = nil, nil
		inner.seek = Pred{}
		inner.lock = ""
		// The projection stays exactly as it would run. DISTINCT is defined
		// over it, so narrowing it here — including dropping the expansions,
		// which the unwrapped path below does drop — could only change the
		// answer. A caller who wants the cheaper count can ask for it without
		// the expansion.
		alias := "grouped"
		if b.distinct {
			alias = "distinct_rows"
		}
		c.write("SELECT count(*) FROM (")
		inner.compile(c)
		c.write(") AS ")
		c.ident(alias)
		return c.result()
	}

	counted := b.Clone()
	counted.sel = []Selection{Sel(Call{Name: "count", Star: true})}
	counted.orders = nil
	counted.limit, counted.offset = nil, nil
	// A count is the size of the matching set, not of what is left to page
	// through. Keeping the boundary would make ?count=exact shrink as a client
	// paged, which is a worse answer than no count at all.
	counted.seek = Pred{}
	counted.lock = ""
	// An expansion never changes how many rows match: a forward one joins on
	// the target's primary key, and a collection is a subquery in the
	// projection, which a count does not read. Counting is the one place the
	// expanded row is not looked at, so dropping them keeps ?count=exact from
	// paying for work whose result is discarded — and a collection is the
	// expensive kind, one subquery per row.
	counted.expand = nil
	counted.compile(c)
	return c.result()
}

// ErrNotFound is returned by One when the query matches no rows. It is a
// sentinel so that HTTP handlers can map it to 404 without inspecting text.
var ErrNotFound = errors.New("sqlb: no rows matched")

// filters is everything that reaches the WHERE clause: the caller's predicates
// and, last, the keyset boundary. The boundary goes last so that the SQL reads
// in the order it was asked for, with paging as a suffix.
func (b *Builder[T]) filters() []Pred {
	if b.seek.IsZero() {
		return b.where
	}
	out := make([]Pred, 0, len(b.where)+1)
	out = append(out, b.where...)
	return append(out, b.seek)
}

// from is the name the base table is referenced by: its alias if it has one,
// otherwise the table itself. Expansion joins qualify the foreign key with it.
func (b *Builder[T]) from() string {
	if b.alias != "" {
		return b.alias
	}
	return b.model.Table
}

// joined reports whether the statement brings in a second table, by an explicit
// join or by an expansion. It is what decides whether the default projection
// has to be qualified.
func (b *Builder[T]) joined() bool { return len(b.joins) > 0 || len(b.expand) > 0 }
