package recipes_test

import (
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// The comparison operators, which are the ones with no surprises:
// Eq, Neq, Gt, Gte, Lt, Lte. Every operand becomes a bind parameter.
func Example_operatorsComparison() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.F("view_count").Gte(100),
		sqlb.F("view_count").Lt(10_000),
		sqlb.F("status").Neq("draft"),
	))
	// Output:
	// WHERE (("view_count" >= $1) AND ("view_count" < $2)) AND ("status" <> $3)
	// args: [100 10000 draft]
}

// Contains escapes LIKE wildcards, so a user typing "100%" searches for that
// literal string instead of matching every row. StartsWith and EndsWith escape
// them too; all three are case-insensitive.
//
// Like does not escape, because a pattern is the point of it. Use Like only for
// patterns your own code wrote.
func Example_operatorsTextSearch() {
	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("title").Contains("100%")))
	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("title").StartsWith("Getting")))
	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("title").Like("Chapter _:%")))
	// Output:
	// WHERE "title" ILIKE $1
	// args: [%100\%%]
	// WHERE "title" ILIKE $1
	// args: [Getting%]
	// WHERE "title" LIKE $1
	// args: [Chapter _:%]
}

// OneOf is an IN list, and its empty case is the one worth knowing: it matches
// nothing, because that is what `in ()` means. The alternative — quietly
// dropping the predicate — turns an empty permission set into "may see
// everything", which is the shape a lot of authorisation bugs take.
//
// NotOneOf is its negation, and an empty NotOneOf excludes nothing.
func Example_operatorsInList() {
	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("status").OneOf("published", "review")))

	var allowed []any // the caller may see no statuses at all
	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("status").OneOf(allowed...)))
	// Output:
	// WHERE "status" IN ($1, $2)
	// args: [published review]
	// WHERE false
}

// Null tests are their own operators, because `= NULL` is not one. IsNull and
// NotNull are available on any column; whether a *REST request* may ask for
// them depends on the Go field being a pointer, which is how sqlb knows the
// column is nullable.
func Example_operatorsNull() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		sqlb.F("published_at").NotNull(),
		sqlb.F("deleted_at").IsNull(),
	))
	// Output:
	// WHERE ("published_at" IS NOT NULL) AND ("deleted_at" IS NULL)
}

// Between is a closed interval — both ends included — and NotBetween excludes
// one. It is two bind parameters rather than a range type, so it works for
// timestamps, numbers and anything else the column's type compares.
func Example_operatorsBetween() {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)

	showWhere(sqlb.Query[recipes.Post]().Where(sqlb.F("published_at").Between(from, to)))
	// Output:
	// WHERE "published_at" BETWEEN $1 AND $2
	// args: [2026-01-01 00:00:00 +0000 UTC 2026-12-31 23:59:59 +0000 UTC]
}

// EqField compares two columns instead of a column and a value. It is what a
// join condition is made of, and what a self-referential comparison needs —
// neither of which can be spelled with Eq, since a value there would be bound
// as a parameter rather than read as a column.
func Example_operatorsColumnToColumn() {
	showWhere(sqlb.Query[recipes.Post]().
		Where(sqlb.F("posts.author_id").EqField(sqlb.F("comments.author_id"))))
	// Output:
	// WHERE "posts"."author_id" = "comments"."author_id"
}
