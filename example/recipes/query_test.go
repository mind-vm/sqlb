package recipes_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// A query is a value, and SQL renders it without running it.
//
// This is the inspection point the rest of these recipes are written against:
// log it, diff it in a test, or paste it into EXPLAIN. Nothing here has touched
// a database.
func Example_queryCompilesWithoutRunning() {
	q := sqlb.Query[recipes.Post]().
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("view_count").Desc()).
		Limit(10)

	sql, args, err := q.SQL()
	if err != nil {
		panic(err)
	}
	fmt.Println(sql)
	fmt.Println("args:", args)
	// Output:
	// SELECT "posts"."id", "posts"."org_id", "posts"."author_id", "posts"."title", "posts"."body", "posts"."status", "posts"."view_count", "posts"."tags", "posts"."metadata", "posts"."published_at", "posts"."deleted_at", "posts"."created_at" FROM "posts" WHERE "status" = $1 ORDER BY "view_count" DESC LIMIT 10
	// args: [published]
}

// Terminal methods run the query, and each says what it expects of the result.
// Choosing the right one is how "exactly one row" becomes an error rather than
// a silently discarded second row.
//
//	All    every matching row
//	First  the first, or ErrNotFound — pair it with OrderBy
//	One    the only one; more than one match is an error
//	Count  how many match, ignoring pagination
//	Exists whether any match, without fetching one
func Example_queryTerminalMethods() {
	db := recordingDB()
	ctx := context.Background()

	posts, err := sqlb.Query[recipes.Post]().Where(sqlb.F("org_id").Eq("acme")).All(ctx, db)
	if err != nil {
		panic(err)
	}
	fmt.Println("all:", len(posts), posts[0].Title)

	n, err := sqlb.Query[recipes.Post]().Count(ctx, db)
	if err != nil {
		panic(err)
	}
	fmt.Println("count:", n)

	_, err = sqlb.Query[recipes.Post]().Where(sqlb.F("id").Eq("nope")).One(ctx, recordingDBWith(postColumns))
	fmt.Println("one, no match:", errors.Is(err, sqlb.ErrNotFound))
	// Output:
	// all: 1 Hello
	// count: 1
	// one, no match: true
}

// Building the query and running it are separate steps, so a base query can be
// built once and reused. Clone is what makes that safe: the builder's methods
// mutate in place, which is what lets a hook amend a query it was handed, and
// means two callers must not share one.
func Example_queryCloneToDerive() {
	base := sqlb.Query[recipes.Post]().Where(sqlb.F("org_id").Eq("acme"))

	drafts := base.Clone().Where(sqlb.F("status").Eq("draft"))
	published := base.Clone().Where(sqlb.F("status").Eq("published"))

	showWhere(drafts)
	showWhere(published)
	showWhere(base) // untouched
	// Output:
	// WHERE ("org_id" = $1) AND ("status" = $2)
	// args: [acme draft]
	// WHERE ("org_id" = $1) AND ("status" = $2)
	// args: [acme published]
	// WHERE "org_id" = $1
	// args: [acme]
}

// Select replaces the default projection of every mapped column. Collect scans
// the result into a type other than the model, which is how a query that no
// longer returns rows of T stays typed.
func Example_queryProjectDifferentType() {
	type titleOnly struct {
		ID    string `db:"id"`
		Title string `db:"title"`
	}

	q := sqlb.Query[recipes.Post]().
		Select(sqlb.F("id"), sqlb.F("title")).
		Where(sqlb.F("status").Eq("published"))

	rows, err := sqlb.Collect[titleOnly](
		context.Background(),
		recordingDBWith([]string{"id", "title"}, []any{"p1", "Hello"}),
		q,
	)
	if err != nil {
		panic(err)
	}
	show(q)
	fmt.Println(rows)
	// Output:
	// SELECT "id", "title" FROM "posts" WHERE "status" = $1
	// args: [published]
	// [{p1 Hello}]
}
