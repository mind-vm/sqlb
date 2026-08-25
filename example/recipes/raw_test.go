package recipes_test

import (
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// RawPred is verbatim SQL with its own bind parameters, written as `?`
// placeholders which the compiler renumbers into $1, $2 alongside everything
// else. Use it for expressions the builder cannot model — and only for those:
// its contents are not validated, so a value that reached it by concatenation
// is an injection.
//
// The values still go through `?`. That is the discipline the escape hatch
// keeps: raw *structure*, never raw values.
func Example_rawPredicate() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.F("org_id").Eq("acme"),
		sqlb.RawPred(`to_tsvector('english', "body") @@ plainto_tsquery('english', ?)`, "index scan"),
	))
	// Output:
	// WHERE ("org_id" = $1) AND (to_tsvector('english', "body") @@ plainto_tsquery('english', $2))
	// args: [acme index scan]
}

// The comparison EqField does not cover. Only equality has a column-to-column
// form, so an inequality between two columns is raw — which is a small enough
// gap that closing it with a partial operator set would be worse than naming it.
func Example_rawColumnComparison() {
	showWhere(sqlb.Query[recipes.Post]().
		As("p").
		Join("posts", "sibling", sqlb.F("sibling.author_id").EqField(sqlb.F("p.author_id"))).
		Where(sqlb.RawPred(`"sibling"."published_at" < "p"."published_at"`)))
	// Output:
	// WHERE "sibling"."published_at" < "p"."published_at"
}

// The mistake this replaces. Eq and its siblings bind their operand as a value,
// so passing a Field to one sends the *field* to the driver as a parameter
// rather than comparing columns. It compiles; it is wrong at runtime.
func Example_rawWhatEqDoesWithAField() {
	sql, args, err := sqlb.Query[recipes.Post]().
		Where(sqlb.F("published_at").Lt(sqlb.F("created_at"))).
		SQL()
	if err != nil {
		panic(err)
	}
	showContains(sql, `"published_at" < $1`)
	showContains(sql, `"published_at" < "created_at"`)
	showArgCount(args) // the Field went to the driver as one
	// Output:
	// mentions "published_at" < $1: true
	// mentions "published_at" < "created_at": false
	// 1 bind parameter
}

// RawSel is the same escape hatch in the projection, and Raw is the expression
// form that SetExpr and GroupByExpr take.
func Example_rawSelection() {
	show(sqlb.Query[recipes.Post]().
		Select(
			sqlb.F("status"),
			sqlb.RawSel(`percentile_cont(?) WITHIN GROUP (ORDER BY "view_count")`, 0.95).As("p95"),
		).
		GroupBy(sqlb.F("status")))
	// Output:
	// SELECT "status", percentile_cont($1) WITHIN GROUP (ORDER BY "view_count") AS "p95" FROM "posts" GROUP BY "status"
	// args: [0.95]
}

// Cast emits its type name verbatim, so it must never come from user input.
// The value beside it is still bound.
func Example_rawCast() {
	show(sqlb.Query[recipes.Post]().
		Select(sqlb.Sel(sqlb.F("metadata").Cast("text")).As("metadata_text")).
		Where(sqlb.F("id").Eq("p1")))
	// Output:
	// SELECT "metadata"::text AS "metadata_text" FROM "posts" WHERE "id" = $1
	// args: [p1]
}
