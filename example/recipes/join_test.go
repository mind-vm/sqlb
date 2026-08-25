package recipes_test

import (
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Join takes the table, an alias and the ON predicate — built from EqField,
// since both sides are columns. The alias may be empty, in which case the table
// name is the alias.
//
// A join changes which rows come back, not which columns: the projection is
// still the model's. Use Expand instead when the point is to *carry* the other
// row in the response; see expand_test.go.
func Example_joinToFilterByAnotherTable() {
	show(sqlb.Query[recipes.Post]().
		Join("authors", "a", sqlb.F("a.id").EqField(sqlb.F("posts.author_id"))).
		Where(sqlb.F("a.name").Contains("ada")).
		Select(sqlb.F("posts.id"), sqlb.F("posts.title")))
	// Output:
	// SELECT "posts"."id", "posts"."title" FROM "posts" JOIN "authors" AS "a" ON "a"."id" = "posts"."author_id" WHERE "a"."name" ILIKE $1
	// args: [%ada%]
}

// LeftJoin keeps rows with no match, which is what "and how many comments does
// each have, including none" needs. The aggregate then counts a column of the
// joined table rather than rows, so a post with no comments counts 0 instead
// of 1.
func Example_joinLeftWithAggregate() {
	show(sqlb.Query[recipes.Post]().
		LeftJoin("comments", "c", sqlb.F("c.post_id").EqField(sqlb.F("posts.id"))).
		Select(sqlb.F("posts.id"), sqlb.CountOf(sqlb.F("c.id")).As("comments")).
		GroupBy(sqlb.F("posts.id")))
	// Output:
	// SELECT "posts"."id", count("c"."id") AS "comments" FROM "posts" LEFT JOIN "comments" AS "c" ON "c"."post_id" = "posts"."id" GROUP BY "posts"."id"
}

// As aliases the model's own table, which is what a self-join needs: both sides
// are the same table, so at least one of them must be called something else.
//
// EqField is the only column-to-column comparison there is. Anything else —
// `earlier.published_at < p.published_at` — needs RawPred, for the reason
// raw_test.go gives.
func Example_joinSelf() {
	show(sqlb.Query[recipes.Post]().
		As("p").
		Join("posts", "sibling", sqlb.F("sibling.author_id").EqField(sqlb.F("p.author_id"))).
		Where(sqlb.Not(sqlb.F("sibling.id").EqField(sqlb.F("p.id")))).
		Select(sqlb.F("p.id"), sqlb.Sel(sqlb.F("sibling.id").Column()).As("sibling_id")))
	// Output:
	// SELECT "p"."id", "sibling"."id" AS "sibling_id" FROM "posts" AS "p" JOIN "posts" AS "sibling" ON "sibling"."author_id" = "p"."author_id" WHERE NOT ("sibling"."id" = "p"."id")
}

// Qualify attaches a table to a column reference after the fact, for building
// predicates in a helper that does not know which alias it will be used under.
func Example_joinQualifyAColumn() {
	published := func(table string) sqlb.Pred {
		return sqlb.F("status").Qualify(table).Eq("published")
	}

	showWhere(sqlb.Query[recipes.Post]().As("p").Where(published("p")))
	// Output:
	// WHERE "p"."status" = $1
	// args: [published]
}
