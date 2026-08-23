package sqlb

import (
	"context"
	"fmt"
)

// Subquery is a query that can stand inside another statement's expression.
//
// [Builder] is the only implementation, and the interface exists so that a
// predicate over one model can name a query over another without this package
// growing a second type parameter: `Query[Author]().Where(F("id").InQuery(q))`
// has no room to say what q selects from.
//
// The methods are unexported, so the set of implementations is closed. That is
// the same closure [Expr] has and it is load-bearing for the same reason: a
// subquery compiles into the surrounding statement's compiler, sharing its bind
// numbering, and a type outside this package could not participate in that.
//
// # Hooks
//
// A subquery does not run its model's BeforeQuery hooks, for the reason
// [Builder.SQL] does not either: hooks apply when a query runs, and a nested
// SELECT is not run, it is compiled. That would make a scope predicate silently
// absent from exactly the position where its absence is invisible — inside
// someone else's WHERE clause — so it is refused rather than dropped. Resolve
// the inner query first and pass the result:
//
//	sub, err := sqlb.Query[Post]().Select(sqlb.F("author_id")).Resolved(ctx, db)
//	authors, err := sqlb.Query[Author]().Where(sqlb.F("id").InQuery(sub)).All(ctx, db)
//
// A model with no registered hook needs none of this, which is the common case
// and costs one map lookup.
type Subquery interface {
	// Err is [Builder.Err]: a subquery that failed to build fails the statement
	// it was nested in rather than compiling to something else.
	Err() error

	// compileSub renders the SELECT into the surrounding statement's compiler,
	// which is what makes the bind numbering continuous across the nesting.
	compileSub(c *compiler)

	// subProjection reports how many columns the SELECT projects and the table
	// it selects from, which is what an error about the wrong width has to name.
	subProjection() (int, string)

	// subUnresolved names the model whose reads are confined by a hook this
	// query has not run, or "" when there is nothing outstanding.
	subUnresolved(ctx context.Context, exec Executor) (string, error)

	// walkSubqueries visits the queries nested inside this one, so that a scope
	// two levels down is checked rather than assumed.
	walkSubqueries(w *subqueryWalk)
}

// SubqueryExpr nests a SELECT inside an expression, parenthesised.
//
// Columns is the width the surrounding operator requires, or zero when any
// width will do. `IN` compares a single column, and a subquery projecting a
// whole model against it fails in Postgres with a complaint about record types
// that names neither the query nor the fix — so the width is checked here,
// where the model is known.
type SubqueryExpr struct {
	Query   Subquery
	Columns int
}

func (SubqueryExpr) exprNode() {}

// Exists is `EXISTS (q)`: whether the subquery returns any row at all.
//
// What q projects does not matter to EXISTS, and it is not narrowed here — a
// caller who minds that the default projection names every column can say
// Select(sqlb.RawSel("1")), and one who does not is paying for a projection
// Postgres does not evaluate.
func Exists(q Subquery) Pred {
	return pred(Unary{Op: "EXISTS", Operand: SubqueryExpr{Query: q}})
}

// NotExists is `NOT EXISTS (q)`.
func NotExists(q Subquery) Pred { return Not(Exists(q)) }

// InQuery is `f IN (q)`, matching against the single column q selects.
//
// It is [Field.OneOf] over a set the database computes rather than one the
// caller enumerates, and the difference is not only convenience: a list of
// values costs a bind parameter each, so the set an application can pass has a
// ceiling (see maxBindParams) that a subquery does not.
func (f Field) InQuery(q Subquery) Pred {
	return pred(Binary{Op: "IN", Left: f.Column(), Right: SubqueryExpr{Query: q, Columns: 1}})
}

// NotInQuery is `f NOT IN (q)`.
//
// SQL's three-valued logic applies as it always does: if q returns a NULL, the
// test is unknown for every row and the predicate matches nothing. Confine q so
// it cannot, or use [NotExists], which does not have this shape.
func (f Field) NotInQuery(q Subquery) Pred { return Not(f.InQuery(q)) }

// compileSub renders this query as a nested SELECT.
//
// The qualification is unconditional, unlike [Builder.compile]'s, which sets it
// only for a statement that joins. Nesting is the other way an unqualified name
// becomes ambiguous: the outer statement may already have named a base table,
// and `WHERE id IN (SELECT id FROM …)` reaching resolution rules rather than
// saying which table it meant is how a subquery over the same table as its
// parent silently becomes correlated.
func (b *Builder[T]) compileSub(c *compiler) {
	defer c.qualifyTo(b.from())()
	b.compile(c)
}

func (b *Builder[T]) subProjection() (int, string) {
	if len(b.sel) > 0 {
		return len(b.sel), b.model.Table
	}
	// The default projection, counted the way compileProjection writes it: a
	// computed column is out unless this query opted into it.
	n := 0
	for _, col := range b.model.Columns {
		if col.Computed() && !b.computed[col.Name] {
			continue
		}
		n++
	}
	return n, b.model.Table
}

// subUnresolved asks whether this query would run confined if it were run now.
//
// The question is answered by running the hooks rather than by finding a
// registration, and the difference is not pedantic: hooksFor materialises an
// empty hook set for every model it is asked about, so a registry that has seen
// one query over T holds an entry for T whether or not anything was ever
// registered on it. Testing for the entry would refuse every nested query in
// any application that uses hooks at all.
//
// Running them is also what makes a released scope behave correctly: a handle
// that dropped a named scope with WithoutScope yields no predicates here and is
// not refused, because for that handle there is nothing to be missing.
func (b *Builder[T]) subUnresolved(ctx context.Context, exec Executor) (string, error) {
	if b.resolved {
		return "", nil
	}
	scoper := registryOf(exec).scoperFor(b.model.Type)
	if scoper == nil {
		return "", nil
	}
	preds, err := scoper.queryScope(ctx, releasedFrom(exec))
	if err != nil {
		return "", fmt.Errorf("sqlb: running %s's query hooks for a nested query: %w",
			b.model.Type.Name(), err)
	}
	if len(preds) == 0 {
		return "", nil
	}
	return b.model.Type.Name(), nil
}

// walkSubqueries visits every expression this query holds. Everything that can
// carry a nested SELECT is listed, because a clause left out here is one whose
// subquery would skip the scope check — the walk fails open by omission, so it
// enumerates rather than sampling.
func (b *Builder[T]) walkSubqueries(w *subqueryWalk) {
	for _, p := range b.filters() {
		w.pred(p)
	}
	for _, p := range b.having {
		w.pred(p)
	}
	for _, j := range b.joins {
		w.pred(j.on)
	}
	for _, s := range b.sel {
		w.expr(s.expr)
	}
	for _, g := range b.groups {
		w.expr(g)
	}
	for _, o := range b.orders {
		w.expr(o.expr)
	}
}

// subqueryWalk collects the queries nested anywhere in a statement.
type subqueryWalk struct {
	found []Subquery
	// seen stops a query that reaches itself from recursing forever. Nothing
	// stops a caller writing q.Where(F("id").InQuery(q)) — it is a value, and
	// values can be handed back to themselves — and this walk would follow the
	// cycle before the compiler ever got the chance to refuse it.
	seen map[Subquery]bool

	// authored is the set of nested queries the statement already carried
	// before its hooks ran. Anything found outside it was added by a hook, and
	// the refusal for that case is a different one — see check.
	//
	// nil means the caller did not ask the question, and everything is treated
	// as authored, which is the message this refusal has always given.
	//
	// A Subquery is safe as a map key because the interface's methods are
	// unexported, so every implementation is a *Builder[T] and every value is a
	// pointer. seen above already depends on this.
	authored map[Subquery]bool

	// hook and owner name the registration and the model, for the refusal that
	// has to tell a reader where the predicate came from and what to change.
	hook  string
	owner string
}

// set is the walk's result as a lookup, for handing to a later walk as its
// authored set.
func (w *subqueryWalk) set() map[Subquery]bool {
	out := make(map[Subquery]bool, len(w.found))
	for _, q := range w.found {
		out[q] = true
	}
	return out
}

// authoredIn is set for a statement whose clauses are already in hand, which is
// the shape [Update] and [Delete] reach this by.
func authoredIn(preds []Pred, exprs []Expr) map[Subquery]bool {
	var w subqueryWalk
	for _, p := range preds {
		w.pred(p)
	}
	for _, e := range exprs {
		w.expr(e)
	}
	return w.set()
}

func (w *subqueryWalk) pred(p Pred) {
	if !p.IsZero() {
		w.expr(p.Expr())
	}
}

// expr descends one expression node.
//
// Column, Field, Param, sharedParam and ConflictRef hold no nested expression.
// Raw holds SQL text rather than nodes, so a subquery hand-written inside one
// is invisible here — which is what Raw means everywhere else in this package:
// its contents are not validated.
func (w *subqueryWalk) expr(e Expr) {
	switch n := e.(type) {
	case SubqueryExpr:
		if n.Query == nil {
			return
		}
		if w.seen == nil {
			w.seen = make(map[Subquery]bool, 1)
		}
		if w.seen[n.Query] {
			return
		}
		w.seen[n.Query] = true
		w.found = append(w.found, n.Query)
		n.Query.walkSubqueries(w)
	case List:
		for _, item := range n.Items {
			w.expr(item)
		}
	case Binary:
		w.expr(n.Left)
		w.expr(n.Right)
	case Unary:
		w.expr(n.Operand)
	case BetweenExpr:
		w.expr(n.Operand)
		w.expr(n.Lo)
		w.expr(n.Hi)
	case Call:
		for _, a := range n.Args {
			w.expr(a)
		}
	case Cast:
		w.expr(n.Inner)
	}
}

// check refuses the statement if any nested query would run without the hooks
// that confine its model's reads.
//
// Two refusals, for one condition, because the fix is not the same in both
// places. Resolving the inner query first is the answer for a subquery the
// caller wrote, and it needs an Executor — which a hook does not have. A hook
// is handed the statement and nothing else, so at the one place this is most
// likely to be hit (inside a scoping rule, which is exactly where a predicate
// reaching another table lives) the advice given was true-sounding and
// inapplicable, and the reporter of #288 arrived at the real answer by trial
// and error instead (denormalising the column, which was the better schema
// anyway).
func (w *subqueryWalk) check(ctx context.Context, exec Executor) error {
	for _, q := range w.found {
		name, err := q.subUnresolved(ctx, exec)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		if w.authored != nil && !w.authored[q] {
			return fmt.Errorf(
				"sqlb: a registered %s hook added a predicate nesting a query over %s, whose "+
					"reads are confined by a query hook that a nested SELECT does not run. "+
					"Resolving the inner query first is the fix elsewhere and is not one here: "+
					"a hook is handed the statement and no executor to resolve with. Either "+
					"denormalise onto %s the column this rule needs, so it becomes a plain "+
					"predicate, or register the rule on %s instead, where its reads are already "+
					"confined when they are issued",
				w.hookName(), name, w.ownerName(), name)
		}
		return fmt.Errorf(
			"sqlb: this statement nests a query over %s, whose reads are confined by a "+
				"registered query hook that a nested SELECT does not run; resolve it first — "+
				"sub, err := sqlb.Query[%s]()….Resolved(ctx, db) — and nest the result",
			name, name)
	}
	return nil
}

// hookName and ownerName degrade to something readable rather than to an empty
// string, so a call site that reaches check without naming itself produces a
// clumsy sentence rather than a broken one.
func (w *subqueryWalk) hookName() string {
	if w.hook == "" {
		return "query"
	}
	return w.hook
}

func (w *subqueryWalk) ownerName() string {
	if w.owner == "" {
		return "the table being confined"
	}
	return w.owner
}

// guardNested is the check as a statement's clauses reach it.
//
// authored is what the statement carried before its hooks ran, or nil when the
// caller has not distinguished; hook and owner name the registration and the
// model for the refusal that needs them.
func guardNested(ctx context.Context, exec Executor, preds []Pred, exprs []Expr,
	authored map[Subquery]bool, hook, owner string) error {
	w := subqueryWalk{authored: authored, hook: hook, owner: owner}
	for _, p := range preds {
		w.pred(p)
	}
	for _, e := range exprs {
		w.expr(e)
	}
	return w.check(ctx, exec)
}

// guardFrom is guardNested's counterpart for [Update.From]: the CTE's query is
// compiled straight into the surrounding statement rather than run, so a model
// with a registered BeforeQuery hook needs the same refusal a nested subquery
// gets, for the reason [Subquery]'s own doc comment gives — and any subquery
// nested inside *that* query needs the same check applied one level down.
func guardFrom(ctx context.Context, exec Executor, name string, q Subquery) error {
	if q == nil {
		return nil
	}
	unresolved, err := q.subUnresolved(ctx, exec)
	if err != nil {
		return err
	}
	if unresolved != "" {
		return fmt.Errorf(
			"sqlb: From(%q) is over %s, whose reads are confined by a registered query hook "+
				"that a CTE source does not run; resolve it first — "+
				"sub, err := sqlb.Query[%s]()….Resolved(ctx, db) — and pass the result to From",
			name, unresolved, unresolved)
	}
	var w subqueryWalk
	q.walkSubqueries(&w)
	return w.check(ctx, exec)
}

// subquery renders a nested SELECT, checking that it is the shape the
// surrounding operator can use.
func (c *compiler) subquery(n SubqueryExpr) {
	if n.Query == nil {
		c.fail("sqlb: nil subquery")
		return
	}
	if err := n.Query.Err(); err != nil {
		c.fail("sqlb: nested query: %w", err)
		return
	}
	// A subquery that reaches itself would otherwise recurse until the stack
	// gives out, which reports a crash rather than the mistake. The depth is
	// far above any legible query and exists only to turn that into an error.
	if c.depth >= maxSubqueryDepth {
		c.fail("sqlb: nested queries are %d deep, which is past the point of a mistake; "+
			"a query nested inside itself is the usual cause", c.depth)
		return
	}
	if n.Columns > 0 {
		if got, table := n.Query.subProjection(); got != n.Columns {
			c.fail("sqlb: this nested query selects %d columns of %s and is compared against %d; "+
				"narrow it with Select — for example Select(sqlb.F(%q))",
				got, table, n.Columns, table+"_id")
			return
		}
	}
	c.depth++
	defer func() { c.depth-- }()
	c.write("(")
	n.Query.compileSub(c)
	c.write(")")
}

// maxSubqueryDepth is a backstop against a cycle, not a budget. Postgres has no
// nesting limit worth mirroring and no real query comes near this.
const maxSubqueryDepth = 32
