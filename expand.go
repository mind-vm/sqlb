package sqlb

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Expansion: one query, one extra column per relation.
//
// `?expand=list` becomes a LEFT JOIN and a `json_build_object` over the target's
// columns, aliased into a result column the scanner recognises:
//
//	SELECT "tasks"."id", …,
//	       CASE WHEN "__ex_list"."id" IS NULL THEN NULL
//	            ELSE json_build_object('id', "__ex_list"."id", …) END AS "__expand_list"
//	FROM "tasks"
//	LEFT JOIN "lists" AS "__ex_list" ON "__ex_list"."id" = "tasks"."list_id"
//
// # Why one query rather than two
//
// The obvious alternative is to read the page, collect the foreign keys and
// issue a second `WHERE id IN (…)`. It avoids the join and it is what an ORM
// without a query builder usually does. It also cannot be made consistent: the
// second query runs at a later snapshot, so a row can vanish between them and a
// caller gets a null expansion for a reference the database still holds. Inside
// one statement that cannot happen.
//
// # Why json_build_object rather than the target's columns
//
// Joining the columns in directly would mean aliasing every one of them to
// avoid collisions — `tasks.id` and `lists.id` are both `id` — and then
// unaliasing them at scan time. Building an object instead means the target
// arrives as one value, and scanning it is a json.Unmarshal into a type that
// already knows its own shape.
//
// The columns are listed explicitly rather than using `row_to_json(t.*)`,
// because `Hidden` has to hold across a join. A hidden column on the target is
// hidden when the target is expanded, and `row_to_json` of the whole row would
// quietly carry a password hash into a response.
//
// # Why the base table is qualified
//
// Both arguments above are settled; a third was not knowable until Postgres saw
// the SQL. Once a second table is in the statement, an unqualified column is
// ambiguous rather than merely unclear — the compiler resolves bare names to the
// base table for a joined query, and only for a joined query. See compiler.column.
//
// ADR-0025 records all three, and the reason the third one is the useful one.
//
// # The reverse direction is a subquery, not a join
//
// `?expand=tasks` on a list is many rows rather than one, and a join cannot
// carry it: joining a collection multiplies the base rows, so the page's row
// count would depend on the data, and two expanded collections would multiply
// each other. Each collection is a correlated subquery in the projection
// instead, so n of them compose by addition. It is still one statement, so the
// snapshot argument above holds unchanged. ADR-0022 has the rest, including why
// the value is an envelope rather than a bare array.
//
// # An expansion carries the target's query hooks
//
// A BeforeQuery hook on the target runs. `Query[Task]().Expand("list")` joins
// `lists` on the foreign key *and* on whatever predicates List's hooks add, so
// a tenant scope or a soft-delete filter registered against List confines the
// expanded row as well as List's own endpoint.
//
// This did not use to be true, and the reason it now is worth stating, because
// the two obstacles were real. A hook is
// func(context.Context, *Builder[List]) error and this code holds a *Model
// reached through a relation, with no static type to instantiate a Builder of:
// the registry therefore stores a type-erased view of each hook set, created
// where the type is still known (see queryScoper in hooks.go). And a hook
// writing sqlb.F("org_id") wrote a bare column, which inside a join would
// resolve to the *parent* table — so every predicate is rewritten onto the join
// alias before it is spliced in (see qualify.go).
//
// Three consequences follow from the second half:
//
//   - A predicate this package cannot requalify with certainty — RawPred, or a
//     column qualified with a table the expansion did not join — fails the
//     query rather than being dropped. A dropped scope predicate is the leak
//     this closes, arriving silently by another route.
//   - Only the hook's *predicates* are read. It runs against a throwaway
//     builder, so a limit, an ordering or a projection it sets has no effect
//     here; a collection's order and cap belong to the schema.
//   - The predicates are resolved on the execution path, not at Expand(),
//     because a scope reads its tenant from the context. `SQL()` renders the
//     builder as it stands — which is the contract it has always had, since the
//     parent's own hooks do not run at build time either.
//
// The scope lands in the join's ON clause for a forward expansion and in the
// subquery's WHERE for a collection, and the placement is load-bearing in both:
// in WHERE a forward scope would drop the parent row rather than null its
// expansion, and in ON a collection scope would count children toward has_more
// that the caller may never fetch.
//
// # Scoped now reaches an expansion
//
// [ADR-0030] makes `Scoped` an obligation: a table declaring that its rows are
// confined will not mount a REST resource until a hook exists to confine them.
// That check is about the target's own endpoint, and for a while the hook it
// proved existed was precisely the one an expansion did not call — so a
// declaration that read as a boundary was not one across a join.
//
// It is now. The hook that satisfies the mount check is the hook the join
// carries, so the declaration means the same thing in both places.
//
// A composite foreign key carrying the confining column — the arrangement
// `example/tasks` uses, where tasks reference `(workspace_id, list_id)` against
// `lists (workspace_id, id)` — is still worth reaching for, and is now a
// belt-and-braces measure rather than the only thing holding. It makes a
// cross-tenant reference unrepresentable in the data rather than merely
// unreachable through the query, which is a stronger property and the one that
// still holds if someone writes the query by hand.
//
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required

// expandPrefix marks a result column as an expanded relation. It is not a legal
// column name in any schema this generates, so it cannot collide with one.
const expandPrefix = "__expand_"

// expandAlias is the table alias the target is joined under, and — for a
// collection — the alias of the child table inside the subquery.
func expandAlias(name string) string { return "__ex_" + name }

// expandRowsAlias is the alias of the capped, ordered child rows a collection
// aggregates over.
func expandRowsAlias(name string) string { return "__rows_" + name }

// Expand resolves the named relations inline, one LEFT JOIN each.
//
// Names are relation names, not column names: `Expand("list")`, not
// `Expand("list_id")`. An unknown name fails the builder rather than being
// ignored, because a silently dropped expansion answers the request with a 200
// and a missing field.
//
// Expanding is additive and idempotent: naming the same relation twice joins it
// once.
//
// An expanded row carries every column of the target that is not hidden. Use
// [Builder.ExpandOnly] to carry fewer.
func (b *Builder[T]) Expand(names ...string) *Builder[T] {
	for _, name := range names {
		if name == "" {
			continue
		}
		if contains(b.expand, name) {
			continue
		}
		rel := b.model.Relation(name)
		if rel == nil {
			return b.fail("cannot expand %q: %s has no such relation%s",
				name, b.model.Type.Name(), didYouMean(b.model.RelationNames()))
		}
		if _, err := rel.Target(); err != nil {
			return b.Fail(err)
		}
		b.expand = append(b.expand, name)
	}
	return b
}

// ExpandOnly resolves a relation carrying only the named columns of the target.
//
// It is [Builder.Expand] with a narrower row: `ExpandOnly("author", "id",
// "name")` joins the author and puts two keys in the expanded object rather
// than all of them. Naming the same relation again replaces the previous
// selection, so a narrowing cannot accumulate into the full row by accident.
//
// # Why this narrows and cannot widen
//
// It takes columns off an expanded row and can put nothing on one. Hidden stays
// hidden, a computed column stays absent for the reason writeRowObject gives,
// and the cap and ordering of a collection stay where the schema declared them
// ([ADR-0022]): those are what stop a response's size being a function of data
// nobody bounded, and a per-query lever over them would be a request deciding
// how much work it costs to answer. What a caller may decide is how much of
// each row they are willing to pay to carry, which is the same shape as
// [Builder.WithComputed] — opt-in, per query, narrowing only.
//
// This is a Go API rather than a request parameter. The wire shape of an
// expansion is derived from the schema, and a client asking for fewer keys
// would make one endpoint answer with rows of varying shape
// ([ADR-0039](https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#a-schema-edit-is-an-api-edit)).
//
// [ADR-0022]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#references-declare-their-inverse
func (b *Builder[T]) ExpandOnly(name string, columns ...string) *Builder[T] {
	if b.Expand(name); b.err != nil {
		return b
	}
	if len(columns) == 0 {
		return b.fail("ExpandOnly(%q) names no columns; use Expand(%q) for the whole row", name, name)
	}
	rel := b.model.Relation(name)
	target, err := rel.Target()
	if err != nil {
		return b.Fail(err)
	}
	only := make(map[string]bool, len(columns))
	for _, col := range columns {
		info := target.Column(col)
		switch {
		case info == nil:
			return b.fail("ExpandOnly(%q) names %q, which is not a column of %s (have: %s)",
				name, col, target.Table, strings.Join(target.ColumnNames(), ", "))
		case info.Hidden || info.WriteOnly:
			// Refused rather than skipped. A hidden or write-only column dropped
			// quietly would read as "this expansion carries what I asked for"
			// right up until someone tried to use the key that is not there.
			return b.fail("ExpandOnly(%q) names %q, which %s never serves; an expanded row never carries it",
				name, col, target.Table)
		case info.Computed():
			return b.fail("ExpandOnly(%q) names %q, which %s computes; an expanded row carries stored "+
				"columns only, and its derived ones are answered by its own endpoint",
				name, col, target.Table)
		}
		only[col] = true
	}
	// The primary key is not added back. "Only" means only, and the two places
	// the expansion needs the key — the NULL test that tells an absent related
	// row from a row of nulls, and a collection's ordering — reference the
	// joined column in SQL rather than reading it out of the row object, so
	// leaving it out costs the caller a key they did not ask for and nothing
	// else.
	if b.expandOnly == nil {
		b.expandOnly = make(map[string]map[string]bool, 1)
	}
	b.expandOnly[name] = only
	return b
}

// Expanded reports the relations this query will resolve.
func (b *Builder[T]) Expanded() []string { return append([]string(nil), b.expand...) }

// resolveExpansionScopes collects each expanded target's BeforeQuery predicates
// and requalifies them onto the join alias.
//
// It runs where the parent's own hooks run — on the execution path, with a
// context — because a scope predicate reads its tenant from that context. A
// builder compiled without it renders the join unscoped, which is the same
// contract SQL() has always had for the parent's hooks: hooks apply when the
// query runs, not when it is built.
//
// A target with no registered hook contributes nothing and costs one map
// lookup, so the common case is unaffected.
func (b *Builder[T]) resolveExpansionScopes(ctx context.Context, exec Executor) error {
	if len(b.expand) == 0 {
		return nil
	}
	reg := registryOf(exec)
	for _, name := range b.expand {
		rel := b.model.Relation(name)
		if rel == nil {
			continue // already reported by Expand
		}
		target, err := rel.Target()
		if err != nil {
			return err
		}
		scoper := reg.scoperFor(target.Type)
		if scoper == nil {
			continue
		}
		preds, err := scoper.queryScope(ctx, releasedFrom(exec))
		if err != nil {
			return fmt.Errorf("sqlb: running %s's query hooks for expansion %q: %w",
				target.Type.Name(), name, err)
		}
		if len(preds) == 0 {
			continue
		}
		qualified, err := qualifyPreds(preds, target, expandAlias(name))
		if err != nil {
			return fmt.Errorf("%w (expanding %q on %s)", err, name, b.model.Type.Name())
		}
		if b.expandScope == nil {
			b.expandScope = make(map[string][]Pred, len(b.expand))
		}
		b.expandScope[name] = qualified
	}
	return nil
}

// scopeFor returns the resolved predicates confining one expanded relation.
func (b *Builder[T]) scopeFor(name string) []Pred { return b.expandScope[name] }

// compileExpansions writes the joins. Called while compiling FROM, so the
// aliases exist by the time the projection references them.
//
// A collection contributes nothing here. Joining one would multiply the base
// rows — a page's row count would become a function of the data, and two
// expanded collections would produce a cross product of each other — so a
// collection is a correlated subquery in the projection instead. ADR-0022.
func (b *Builder[T]) compileExpansions(c *compiler) {
	for _, name := range b.expand {
		rel := b.model.Relation(name)
		if rel.Collection {
			continue
		}
		target, err := rel.Target()
		if err != nil {
			c.fail("%s", err)
			return
		}
		if target.PK == nil {
			c.fail("cannot expand %q: %s has no primary key to join on",
				name, target.Type.Name())
			return
		}
		if rel.Reverse && b.model.PK == nil {
			c.fail("cannot expand %q: %s has no primary key for its children to point at",
				name, b.model.Type.Name())
			return
		}

		alias := expandAlias(name)
		c.write(" LEFT JOIN ")
		c.ident(target.Table)
		c.write(" AS ")
		c.ident(alias)
		c.write(" ON ")
		if rel.Reverse {
			// The mirror image of the forward case, and of the correlated
			// WHERE compileCollection writes for a capped collection: the
			// foreign key lives on the target, not on this row.
			c.column(Column{Table: alias, Name: rel.FK.Name})
			c.write(" = ")
			c.column(Column{Table: b.from(), Name: b.model.PK.Name})
		} else {
			c.column(Column{Table: alias, Name: target.PK.Name})
			c.write(" = ")
			c.column(Column{Table: b.from(), Name: rel.FK.Name})
		}

		// The target's scope goes in ON rather than in WHERE, and the
		// difference is the whole behaviour of a LEFT JOIN: in WHERE it would
		// discard the parent rows whose target is out of scope, turning the
		// expansion into a filter on the list being paged. In ON those parents
		// are returned with a null expansion, which is what "there is a related
		// row and it is not yours to see" should look like.
		for _, p := range b.scopeFor(name) {
			if p.IsZero() {
				continue
			}
			c.write(" AND ")
			c.operand(p.Expr())
		}
	}
}

// compileExpansionSelections writes one JSON column per expanded relation.
func (b *Builder[T]) compileExpansionSelections(c *compiler) {
	for _, name := range b.expand {
		rel := b.model.Relation(name)
		target, err := rel.Target()
		if err != nil {
			c.fail("%s", err)
			return
		}
		alias := expandAlias(name)

		c.write(", ")
		if rel.Collection {
			b.compileCollection(c, name, rel, target)
			c.write(" AS ")
			c.ident(expandPrefix + name)
			continue
		}

		// A LEFT JOIN that matched nothing produces a row of NULLs, and
		// json_build_object over those yields an object full of nulls rather
		// than a null. The caller asked whether there is a related row; an
		// object of nulls answers "yes, and it is empty", which is wrong.
		c.write("CASE WHEN ")
		c.column(Column{Table: alias, Name: target.PK.Name})
		c.write(" IS NULL THEN NULL ELSE ")
		writeRowObject(c, target, alias, b.expandOnly[name])
		c.write(" END AS ")
		c.ident(expandPrefix + name)
	}
}

// writeRowObject builds one target row as a JSON object.
//
// The columns are listed rather than using row_to_json(t.*), because Hidden has
// to hold across an expansion in either direction: a hidden column on the
// target is hidden when the target is expanded, and row_to_json of the whole
// row would quietly carry a password hash into a response.
// only, when non-nil, is the set of columns the caller narrowed the row to with
// ExpandOnly. It can only remove columns: Hidden and Computed are still skipped
// below whatever it names, because ExpandOnly refuses those names outright and
// this stays correct if it ever stops.
func writeRowObject(c *compiler, target *Model, alias string, only map[string]bool) {
	c.write("json_build_object(")
	first := true
	for _, col := range target.Columns {
		if col.Hidden || col.WriteOnly {
			continue
		}
		if only != nil && !only[col.Name] {
			continue
		}
		// A computed column is SQL text written against the target's own table,
		// and here the target is joined under an alias. Rewriting a raw fragment
		// onto that alias is precisely what qualify.go refuses to do for a
		// RawPred, and for the same reason: text cannot be requalified with
		// certainty, and a fragment silently resolving to the wrong table is
		// worse than an absent key. So an expanded row carries the target's
		// stored columns; its derived ones are answered by its own endpoint.
		if col.Computed() {
			continue
		}
		if !first {
			c.write(", ")
		}
		first = false
		c.write(sqlStringLiteral(col.Name) + ", ")
		writeRowObjectValue(c, col, alias)
	}
	c.write(")")
}

// writeRowObjectValue writes one column's value into the row object, casting
// where Postgres's JSON form is not the form the Go field decodes.
//
// A date is the case that found this. json_build_object serialises a date
// column as "2026-07-01", and the Go field for it is a time.Time, which
// encoding/json parses strictly as RFC 3339 — so expanding a relation whose
// target had a date column answered 500 (#84).
//
// The cast is on the SQL side rather than the decode side because the two
// representations were already inconsistent, and the expansion held the side
// nothing expected: a *direct* read of the same column scans through pgx into a
// time.Time and Go marshals it RFC 3339, which is what both generated clients
// document receiving. So this does not choose a wire format, it stops the
// expansion from contradicting the one already in effect.
//
// AT TIME ZONE 'UTC' rather than ::timestamptz, and that is the whole
// correctness of it: ::timestamptz resolves through the session's TimeZone, so
// under TimeZone=Europe/Berlin the date 2026-07-01 would come back as
// 2026-06-30T22:00:00Z and the column would lose a day. UTC midnight is what a
// direct read produces.
func writeRowObjectValue(c *compiler, col *ColumnInfo, alias string) {
	if col.PGType != pgTypeDate {
		c.column(Column{Table: alias, Name: col.Name})
		return
	}
	c.write("(")
	c.column(Column{Table: alias, Name: col.Name})
	c.write("::timestamp AT TIME ZONE 'UTC')")
}

// pgTypeDate is schema.TypeDate's value. It is a literal rather than an import
// because this package does not depend on the schema package — the type
// arrives as text through the struct tag, and the tag is the contract.
const pgTypeDate = "date"

// sqlStringLiteral renders s as a single-quoted SQL string.
//
// The keys of the JSON object are the only place in this package where a name
// reaches SQL as text rather than as a quoted identifier, so it is the only
// place a quote in a name could close the literal early. Column names come from
// struct tags or Describe and are the developer's own, so this is not a path
// user input reaches — but "not reachable by a request" is a weaker property
// than "cannot be malformed", and the second one costs a doubling.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// compileCollection writes the reverse direction: the children that point back
// at this row, capped, ordered, and told whether there were more.
//
//	(SELECT json_build_object(
//	          'items',    coalesce(json_agg("__rows_tasks"."o" ORDER BY "__rows_tasks"."n")
//	                               FILTER (WHERE "__rows_tasks"."n" <= 50), '[]'::json),
//	          'has_more', count(*) > 50)
//	   FROM (SELECT json_build_object(…) AS "o",
//	                row_number() OVER (ORDER BY …) AS "n"
//	           FROM "tasks" AS "__ex_tasks"
//	          WHERE "__ex_tasks"."list_id" = "lists"."id"
//	          ORDER BY … LIMIT 51) AS "__rows_tasks")
//
// One row past the cap is fetched so that count(*) can answer has_more without
// a second aggregate over the whole child table, and the FILTER drops it again
// so it is never returned. The ORDER BY appears twice on purpose: the inner one
// decides which rows the LIMIT keeps, the window one decides the order they are
// aggregated in, and neither implies the other.
func (b *Builder[T]) compileCollection(c *compiler, name string, rel *RelationInfo, target *Model) {
	if target.PK == nil {
		c.fail("cannot expand %q: %s has no primary key to order its rows by",
			name, target.Type.Name())
		return
	}

	if b.model.PK == nil {
		c.fail("cannot expand %q: %s has no primary key for its children to point at",
			name, b.model.Type.Name())
		return
	}

	alias := expandAlias(name)
	rows := expandRowsAlias(name)
	capped := strconv.Itoa(rel.Cap())

	// The order is made total by the primary key, because under a cap a
	// non-total order does not merely reshuffle the result — it decides which
	// children the caller never sees, and decides it differently each run.
	order := func() {
		if rel.Order != "" {
			c.column(Column{Table: alias, Name: rel.Order})
			if rel.OrderDesc {
				c.write(" DESC")
			}
			c.write(", ")
		}
		c.column(Column{Table: alias, Name: target.PK.Name})
		if rel.OrderDesc {
			c.write(" DESC")
		}
	}

	c.write("(SELECT json_build_object('items', coalesce(json_agg(")
	c.column(Column{Table: rows, Name: "o"})
	c.write(" ORDER BY ")
	c.column(Column{Table: rows, Name: "n"})
	c.write(") FILTER (WHERE ")
	c.column(Column{Table: rows, Name: "n"})
	c.write(" <= " + capped + "), '[]'::json), 'has_more', count(*) > " + capped + ")")

	c.write(" FROM (SELECT ")
	writeRowObject(c, target, alias, b.expandOnly[name])
	c.write(" AS ")
	c.ident("o")
	c.write(", row_number() OVER (ORDER BY ")
	order()
	c.write(") AS ")
	c.ident("n")

	c.write(" FROM ")
	c.table(target.Table)
	c.write(" AS ")
	c.ident(alias)
	c.write(" WHERE ")
	c.column(Column{Table: alias, Name: rel.FK.Name})
	c.write(" = ")
	// The correlated reference. It names the base table explicitly rather than
	// relying on the statement's default qualifier, because inside this
	// subquery an unqualified name resolves to the child table first.
	c.column(Column{Table: b.from(), Name: b.model.PK.Name})

	// The child's own scope. Here it belongs in WHERE rather than beside a
	// join condition: the subquery *is* the collection, so a row it must not
	// return is a row that must not be counted either — has_more counts what
	// this WHERE admits, and a scope in the wrong place would report children
	// the caller may never fetch.
	for _, p := range b.scopeFor(name) {
		if p.IsZero() {
			continue
		}
		c.write(" AND ")
		c.operand(p.Expr())
	}

	c.write(" ORDER BY ")
	order()
	// Rendered as a literal rather than a bind parameter, for the reason
	// Limit and Offset already are: a plan should not have to guess at it.
	c.write(" LIMIT " + strconv.Itoa(rel.Cap()+1) + ") AS ")
	c.ident(rows)
	c.write(")")
}

// scanExpansion decodes one expanded relation into the row being built.
//
// A NULL arrives as a nil pointer field, which is the honest representation of
// "the reference is null" and of "the row it pointed at is gone".
func scanExpansion(rv reflect.Value, rel *RelationInfo, raw []byte) error {
	field, ok := fieldByIndexAlloc(rv, rel.Index)
	if !ok {
		return fmt.Errorf("sqlb: cannot reach field %s to expand %q", rel.Field, rel.Name)
	}
	if len(raw) == 0 || string(raw) == "null" {
		field.SetZero()
		return nil
	}

	// Decoded into the field's own type rather than into rel.Elem, because the
	// two differ for a collection: the field holds a Collection[T] and Elem is
	// the T inside it.
	ft := field.Type()
	pointer := ft.Kind() == reflect.Pointer
	if pointer {
		ft = ft.Elem()
	}
	target := reflect.New(ft)
	if err := json.Unmarshal(raw, target.Interface()); err != nil {
		return fmt.Errorf("sqlb: decoding expanded %q: %w", rel.Name, err)
	}
	if pointer {
		field.Set(target)
		return nil
	}
	field.Set(target.Elem())
	return nil
}

// fieldByIndexAlloc walks to a field, allocating nil embedded pointers on the
// way, because the destination is being written rather than read.
func fieldByIndexAlloc(v reflect.Value, index []int) (reflect.Value, bool) {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}, false
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct || x >= v.NumField() {
			return reflect.Value{}, false
		}
		v = v.Field(x)
	}
	if !v.CanSet() {
		return reflect.Value{}, false
	}
	return v, true
}

// didYouMean renders the available names for a rejection, following ADR-0011:
// a refusal should say what would have worked.
func didYouMean(names []string) string {
	if len(names) == 0 {
		return " (it declares none)"
	}
	return " (expandable: " + strings.Join(names, ", ") + ")"
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
