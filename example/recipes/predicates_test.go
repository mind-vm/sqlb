package recipes_test

import (
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Where conjoins predicates, and zero predicates are skipped. That is why an
// optional filter needs no surrounding statement: the branch that decides
// whether the filter applies is the branch that already exists.
//
// This is the case static query generators cannot express, and the reason sqlb
// exists.
func Example_predicateAddedOnABranch() {
	search := "postgres" // in a handler, this came from the request

	q := sqlb.Query[recipes.Post]().Where(sqlb.F("status").Eq("published"))
	if search != "" {
		q = q.Where(sqlb.F("title").Contains(search))
	}

	showWhere(q)
	// Output:
	// WHERE ("status" = $1) AND ("title" ILIKE $2)
	// args: [published %postgres%]
}

// If drops the predicate when its condition does not hold, which keeps the
// chain unbroken. It returns a zero Pred, and Where skips those — so the same
// call site reads the same way whether or not the filter was supplied.
func Example_predicateIf() {
	minViews := int64(0) // this request did not ask for a minimum

	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.F("status").Eq("published"),
		sqlb.If(minViews > 0, sqlb.F("view_count").Gte(minViews)),
	))
	// Output:
	// WHERE "status" = $1
	// args: [published]
}

// Or groups alternatives into one predicate, which Where then conjoins with the
// rest. And nests the other way. Values never reach the SQL text: every one
// becomes a bind parameter, which is the whole bind discipline in one sentence.
func Example_predicateOrAnd() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.F("org_id").Eq("acme"),
		sqlb.Or(
			sqlb.F("status").Eq("published"),
			sqlb.And(
				sqlb.F("status").Eq("review"),
				sqlb.F("view_count").Gt(100),
			),
		),
	))
	// Output:
	// WHERE ("org_id" = $1) AND (("status" = $2) OR (("status" = $3) AND ("view_count" > $4)))
	// args: [acme published review 100]
}

// Not negates a predicate rather than requiring a negated operator for every
// comparison. The parenthesisation is explicit, so a negated disjunction means
// what it reads as.
func Example_predicateNot() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.Not(sqlb.Or(
			sqlb.F("status").Eq("draft"),
			sqlb.F("status").Eq("review"),
		)),
	))
	// Output:
	// WHERE NOT (("status" = $1) OR ("status" = $2))
	// args: [draft review]
}

// Predicates are values, so a rule used in several places can be a function.
// Nothing in the type says "predicate about posts", which is deliberate: the
// column names are checked when the statement compiles against a model.
func Example_predicateAsAFunction() {
	// visible is the rule "published, and not soft-deleted", written once.
	visible := func() sqlb.Pred {
		return sqlb.And(
			sqlb.F("status").Eq("published"),
			sqlb.F("deleted_at").IsNull(),
		)
	}

	showWhere(sqlb.Query[recipes.Post]().Where(visible(), sqlb.F("org_id").Eq("acme")))
	// Output:
	// WHERE (("status" = $1) AND ("deleted_at" IS NULL)) AND ("org_id" = $2)
	// args: [published acme]
}

// Building a slice of predicates is the other shape, for when the conditions
// come from a loop rather than from named branches. Where is variadic, so the
// slice goes in whole.
func Example_predicateSlice() {
	requested := map[string]string{"status": "published"} // sorted below for a stable output

	var preds []sqlb.Pred
	for _, name := range []string{"author_id", "org_id", "status"} {
		if v, ok := requested[name]; ok {
			preds = append(preds, sqlb.F(name).Eq(v))
		}
	}

	showWhere(sqlb.Query[recipes.Post]().Where(preds...))
	// Output:
	// WHERE "status" = $1
	// args: [published]
}
