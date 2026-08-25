package recipes_test

import (
	"context"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Select replaces the default projection, and an aggregate carries an alias.
// The alias is not decoration: it is what a destination struct's `db` tag
// matches, so the name here and the name there have to agree.
func Example_aggregateGroupBy() {
	show(sqlb.Query[recipes.Post]().
		Select(
			sqlb.F("status"),
			sqlb.Count().As("posts"),
			sqlb.Sum(sqlb.F("view_count")).As("views"),
		).
		Where(sqlb.F("org_id").Eq("acme")).
		GroupBy(sqlb.F("status")).
		OrderBy(sqlb.F("status").Asc()))
	// Output:
	// SELECT "status", count(*) AS "posts", sum("view_count") AS "views" FROM "posts" WHERE "org_id" = $1 GROUP BY "status" ORDER BY "status" ASC
	// args: [acme]
}

// A grouped query no longer returns rows of the model, so Collect scans it into
// the type that does match. This is the whole of "a dashboard query": one
// statement, one destination struct, no per-row loop.
func Example_aggregateCollectIntoAStruct() {
	type statusTotal struct {
		Status string `db:"status"`
		Posts  int64  `db:"posts"`
		Views  int64  `db:"views"`
	}

	q := sqlb.Query[recipes.Post]().
		Select(sqlb.F("status"), sqlb.Count().As("posts"), sqlb.Sum(sqlb.F("view_count")).As("views")).
		GroupBy(sqlb.F("status"))

	db := recordingDBWith(
		[]string{"status", "posts", "views"},
		[]any{"published", int64(2), int64(31)},
		[]any{"draft", int64(1), int64(0)},
	)

	totals, err := sqlb.Collect[statusTotal](context.Background(), db, q)
	if err != nil {
		panic(err)
	}
	for _, t := range totals {
		fmt.Printf("%-9s %d posts, %d views\n", t.Status, t.Posts, t.Views)
	}
	// Output:
	// published 2 posts, 31 views
	// draft     1 posts, 0 views
}

// Having filters the groups, where Where filters the rows. Both take the same
// predicates, which is the payoff of the predicate being a value rather than a
// clause: nothing has to be written twice to be usable in a second position.
func Example_aggregateHaving() {
	show(sqlb.Query[recipes.Post]().
		Select(sqlb.F("author_id"), sqlb.Count().As("posts")).
		Where(sqlb.F("status").Eq("published")).
		GroupBy(sqlb.F("author_id")).
		Having(sqlb.F("count(*)").Gt(5)))
	// Output:
	// SELECT "author_id", count(*) AS "posts" FROM "posts" WHERE "status" = $1 GROUP BY "author_id" HAVING "count(*)" > $2
	// args: [published 5]
}

// The rest of the aggregate vocabulary. CountOf counts non-null values of one
// column, which is a different question from Count's "how many rows"; Coalesce
// is how a sum over no rows becomes 0 instead of NULL.
func Example_aggregateFunctions() {
	show(sqlb.Query[recipes.Post]().
		Select(
			sqlb.CountOf(sqlb.F("published_at")).As("published"),
			sqlb.CountDistinct(sqlb.F("author_id")).As("authors"),
			sqlb.Min(sqlb.F("view_count")).As("least"),
			sqlb.Max(sqlb.F("view_count")).As("most"),
			sqlb.Avg(sqlb.F("view_count")).As("mean"),
		))
	// Output:
	// SELECT count("published_at") AS "published", count(DISTINCT "author_id") AS "authors", min("view_count") AS "least", max("view_count") AS "most", avg("view_count") AS "mean" FROM "posts"
}
