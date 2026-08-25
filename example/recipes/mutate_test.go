package recipes_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// InsertRows takes pointers, and the statement always returns the stored rows,
// so database-generated values land back in the caller's structs. A column
// carrying a default is omitted when its Go value is the zero value — which is
// why the empty ID below does not overwrite the key the database generates.
func Example_insertWritesDefaultsBack() {
	post := recipes.Post{OrgID: "acme", AuthorID: "a1", Title: "Hello", Status: "draft"}

	stored, err := sqlb.InsertRows(&post).One(context.Background(), recordingDB())
	if err != nil {
		panic(err)
	}
	fmt.Println("returned:", stored.ID, stored.CreatedAt.Format("2006-01-02"))
	fmt.Println("caller's struct:", post.ID, post.CreatedAt.Format("2006-01-02"))
	// Output:
	// returned: p1 2026-06-01
	// caller's struct: p1 2026-06-01
}

// One statement, several rows. Only and Omit narrow which columns are written,
// for the cases where "every non-zero column" is not what was meant.
func Example_insertManyRows() {
	a := recipes.Post{OrgID: "acme", Title: "First"}
	b := recipes.Post{OrgID: "acme", Title: "Second"}

	show(sqlb.InsertRows(&a, &b).Only("org_id", "title"))
	// Output:
	// INSERT INTO "posts" ("org_id", "title") VALUES ($1, $2), ($3, $4) RETURNING "id", "org_id", "author_id", "title", "body", "status", "view_count", "tags", "metadata", "published_at", "deleted_at", "created_at"
	// args: [acme First acme Second]
}

// Upsert. OnConflictUpdate names the conflicting columns and the ones to
// overwrite from the proposed row; OnConflictDoNothing skips instead.
//
// A skipped row cannot be told apart from its neighbours in what comes back, so
// a do-nothing statement that skipped anything leaves *every* caller struct
// untouched and the returned slice is the only account of what was written.
func Example_insertUpsert() {
	post := recipes.Post{OrgID: "acme", Title: "Hello"}

	show(sqlb.InsertRows(&post).
		Only("org_id", "title", "status").
		OnConflictUpdate([]string{"org_id", "title"}, "status"))
	// Output:
	// INSERT INTO "posts" ("org_id", "title", "status") VALUES ($1, $2, $3) ON CONFLICT ("org_id", "title") DO UPDATE SET "status" = EXCLUDED."status" RETURNING "id", "org_id", "author_id", "title", "body", "status", "view_count", "tags", "metadata", "published_at", "deleted_at", "created_at"
	// args: [acme Hello ]
}

// An update or delete with no Where is refused rather than run, because
// rewriting every row is almost never what was meant. Everything is how a
// caller who did mean it says so — one word, at the call site, in the diff.
func Example_mutateUnscopedIsRefused() {
	_, _, err := sqlb.UpdateRows[recipes.Post]().Set("status", "archived").SQL()
	showError(err)
	fmt.Println("is ErrUnscoped:", errors.Is(err, sqlb.ErrUnscoped))

	showWhere(sqlb.UpdateRows[recipes.Post]().Set("status", "archived").Everything())
	// Output:
	// sqlb: statement would affect every row; add a Where clause or call Everything to confirm
	// is ErrUnscoped: true
	// (no WHERE clause)
}

// SetExpr assigns an expression rather than a value, which is what an increment
// needs: read-modify-write in Go would lose a concurrent update between the
// read and the write.
func Example_updateComputedFromTheCurrentRow() {
	show(sqlb.UpdateRows[recipes.Post]().
		SetExpr("view_count", sqlb.Raw{SQL: `"view_count" + 1`}).
		Where(sqlb.F("id").Eq("p1")))
	// Output:
	// UPDATE "posts" SET "view_count" = "view_count" + 1 WHERE "id" = $1 RETURNING "id", "org_id", "author_id", "title", "body", "status", "view_count", "tags", "metadata", "published_at", "deleted_at", "created_at"
	// args: [p1]
}

// One asserts that exactly one row was affected. The check is on the *result*,
// so an update that matched three rows has already changed all three when the
// error returns — under autocommit that is durable. Inside WithTx the error
// rolls it back, which is what turns "expected one" from a report into a
// refusal.
func Example_updateExactlyOneRow() {
	_, err := sqlb.UpdateRows[recipes.Post]().
		Set("status", "published").
		Where(sqlb.F("id").Eq("p1")).
		One(context.Background(), recordingDB())
	showError(err)
	// Output:
	// (no error)
}

// Delete returns how many rows went, rather than the rows themselves.
func Example_deleteRows() {
	n, err := sqlb.DeleteRows[recipes.Post]().
		Where(sqlb.F("status").Eq("draft"), sqlb.F("org_id").Eq("acme")).
		Exec(context.Background(), recordingDB())
	if err != nil {
		panic(err)
	}
	fmt.Println("deleted:", n)
	fmt.Println(statements()[0])
	// Output:
	// deleted: 1
	// DELETE FROM "posts" WHERE ("status" = $1) AND ("org_id" = $2)
}
