package sqlb_test

import (
	"fmt"

	"github.com/mind-vm/sqlb"
)

// Article is the model these examples query. The `db` tags name the columns and
// the `sqlb` tags declare what the REST layer may do with them; both are what
// codegen writes from a schema declaration.
type Article struct {
	ID        string `db:"id" sqlb:"pk,default"`
	Title     string `db:"title" sqlb:"filter,search,sort"`
	Status    string `db:"status" sqlb:"filter,sort"`
	ViewCount int64  `db:"view_count" sqlb:"filter,sort"`
	OrgID     string `db:"org_id" sqlb:"filter"`
}

func (Article) TableName() string { return "articles" }

// A query is a value. SQL renders the statement and its bind parameters without
// running anything, which is the inspection point: log it, diff it in a test, or
// paste it into EXPLAIN.
func ExampleQuery() {
	q := sqlb.Query[Article]().
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("view_count").Desc()).
		Limit(10)

	sql, args, err := q.SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" WHERE "status" = $1 ORDER BY "view_count" DESC LIMIT 10
	// published
}

// Because the query is a value rather than a statement, a predicate can be added
// on a branch. This is the case static query generators cannot express, and the
// reason sqlb exists.
func ExampleBuilder_Where_conditional() {
	search := "postgres" // in a handler this came from the request

	q := sqlb.Query[Article]().Where(sqlb.F("status").Eq("published"))
	if search != "" {
		q = q.Where(sqlb.F("title").Contains(search))
	}

	sql, args, _ := q.SQL()
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" WHERE ("status" = $1) AND ("title" ILIKE $2)
	// published %postgres%
}

// If drops the predicate when its condition does not hold, so an optional filter
// needs no surrounding statement. The zero Pred it returns is skipped by Where.
func ExampleIf() {
	minViews := int64(0) // not supplied by this request

	sql, args, _ := sqlb.Query[Article]().
		Where(
			sqlb.F("status").Eq("published"),
			sqlb.If(minViews > 0, sqlb.F("view_count").Gte(minViews)),
		).
		SQL()

	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" WHERE "status" = $1
	// published
}

// Contains escapes LIKE wildcards, so a user typing "100%" searches for that
// literal string instead of matching every row. Use Like only for patterns your
// own code wrote.
func ExampleField_Contains() {
	sql, args, _ := sqlb.Query[Article]().
		Where(sqlb.F("title").Contains("100%")).
		SQL()

	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" WHERE "title" ILIKE $1
	// %100\%%
}

// Or groups alternatives into a single predicate, which Where then conjoins with
// the rest. Values never reach the SQL text: every one becomes a bind parameter.
func ExampleOr() {
	sql, args, _ := sqlb.Query[Article]().
		Where(
			sqlb.F("org_id").Eq("acme"),
			sqlb.Or(
				sqlb.F("status").Eq("published"),
				sqlb.F("status").Eq("review"),
			),
		).
		SQL()

	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" WHERE ("org_id" = $1) AND (("status" = $2) OR ("status" = $3))
	// acme published review
}

// Select replaces the default projection of every mapped column, and aggregates
// carry an alias that the destination type's `db` tag matches. Collect scans
// such a result into a type other than the model.
func ExampleBuilder_Select_aggregate() {
	sql, _, _ := sqlb.Query[Article]().
		Select(sqlb.F("status"), sqlb.Sum(sqlb.F("view_count")).As("views")).
		GroupBy(sqlb.F("status")).
		OrderBy(sqlb.F("status").Asc()).
		SQL()

	fmt.Println(sql)
	// Output:
	// SELECT "status", sum("view_count") AS "views" FROM "articles" GROUP BY "status" ORDER BY "status" ASC
}

// Page is 1-based offset pagination. Limit and offset render as literals rather
// than bind parameters so the planner can see them; both are validated ints, so
// there is no injection surface.
func ExampleBuilder_Page() {
	sql, _, _ := sqlb.Query[Article]().
		OrderBy(sqlb.F("id").Asc()).
		Page(3, 20).
		SQL()

	fmt.Println(sql)
	// Output:
	// SELECT "articles"."id", "articles"."title", "articles"."status", "articles"."view_count", "articles"."org_id" FROM "articles" ORDER BY "id" ASC LIMIT 20 OFFSET 40
}

// An update or delete with no Where is rejected rather than run, because
// rewriting every row is almost never what was meant. Everything confirms it
// when it is.
func ExampleErrUnscoped() {
	_, _, err := sqlb.UpdateRows[Article]().Set("status", "archived").SQL()
	fmt.Println(err)

	sql, _, _ := sqlb.UpdateRows[Article]().Set("status", "archived").Everything().SQL()
	fmt.Println(sql)
	// Output:
	// sqlb: statement would affect every row; add a Where clause or call Everything to confirm
	// UPDATE "articles" SET "status" = $1 RETURNING "id", "title", "status", "view_count", "org_id"
}
