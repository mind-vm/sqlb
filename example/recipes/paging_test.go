package recipes_test

import (
	"context"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Page is 1-based offset pagination. The limit and offset render as literals
// rather than bind parameters so the planner can see them; both are validated
// ints, so there is no injection surface in doing that.
//
// It is the right tool for a page number in a URL and the wrong one for a feed:
// page 500 makes Postgres count past 9,980 rows to discard them.
func Example_pagingByOffset() {
	show(sqlb.Query[recipes.Post]().
		OrderBy(sqlb.F("created_at").Desc()).
		Page(3, 20).
		Select(sqlb.F("id")))
	// Output:
	// SELECT "id" FROM "posts" ORDER BY "created_at" DESC LIMIT 20 OFFSET 40
}

// Stable makes an ordering deterministic by appending the primary key, and
// without it no cursor can exist. `ORDER BY status` leaves rows with equal
// status in whatever order the plan produced, so page 2 may repeat a row from
// page 1 or skip one — and nothing in the result can tell the two apart.
//
// The appended term takes the direction of the last existing one, so
// "newest first" stays newest-first rather than reversing halfway through.
func Example_pagingStableOrdering() {
	show(sqlb.Query[recipes.Post]().
		OrderBy(sqlb.F("created_at").Desc()).
		Stable().
		Select(sqlb.F("id")))
	// Output:
	// SELECT "id" FROM "posts" ORDER BY "created_at" DESC, "id" DESC
}

// A cursor names the position of the last row rather than counting to it, so
// page 500 costs what page 1 costs and a concurrent insert cannot make a client
// read a row twice.
//
// The loop is: run the query, take CursorFor on the last row, hand it back as
// After. A zero cursor is a no-op, so the first request and every one after it
// run the same code.
func Example_pagingByCursor() {
	ctx := context.Background()
	db := recordingDB()

	page := sqlb.Query[recipes.Post]().
		OrderBy(sqlb.F("created_at").Desc()).
		Limit(20)

	rows, err := page.All(ctx, db)
	if err != nil {
		panic(err)
	}

	next, err := page.CursorFor(rows[len(rows)-1])
	if err != nil {
		panic(err)
	}

	// The next request. Same query, plus the cursor the client sent back.
	showWhere(page.Clone().After(next).Select(sqlb.F("id")))
	fmt.Println("zero cursor is the first page:", sqlb.Cursor("").IsZero())
	// Output:
	// WHERE ("created_at", "id") < ($1, $2) ORDER BY "created_at" DESC, "id" DESC LIMIT 20
	// args: [2026-06-01 09:00:00 +0000 UTC p1]
	// zero cursor is the first page: true
}

// A cursor is opaque by intent rather than by encryption. It decodes to the
// ordering columns and the values of the row it was taken from — nothing a
// client could not read off the response it came in — and After checks those
// columns against the ordering the request actually asked for, so an edited
// cursor can only move the boundary along a column the caller was already
// permitted to sort by.
func Example_pagingCursorIsOpaqueNotSecret() {
	ctx := context.Background()

	page := sqlb.Query[recipes.Post]().OrderBy(sqlb.F("created_at").Desc()).Limit(20)
	rows, err := page.All(ctx, recordingDB())
	if err != nil {
		panic(err)
	}
	cursor, err := page.CursorFor(rows[0])
	if err != nil {
		panic(err)
	}

	showDecodedCursor(cursor)
	// Output:
	// {"k":[{"c":"created_at","d":true,"v":"2026-06-01T09:00:00Z"},{"c":"id","d":true,"v":"p1"}]}
}

// After keeps its predicate apart from Where rather than folding it in, so
// Count still answers "how many rows match" rather than "how many are left".
// A total that changed as a client paged would be a worse answer than no total.
func Example_pagingCountIgnoresTheCursor() {
	ctx := context.Background()
	db := recordingDB()

	page := sqlb.Query[recipes.Post]().
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("created_at").Desc())

	rows, err := page.All(ctx, db)
	if err != nil {
		panic(err)
	}
	next, err := page.CursorFor(rows[0])
	if err != nil {
		panic(err)
	}

	if _, err := page.Clone().After(next).Count(ctx, db); err != nil {
		panic(err)
	}
	fmt.Println("count WHERE:", lastWhere())
	// Output:
	// count WHERE: "status" = $1
}
