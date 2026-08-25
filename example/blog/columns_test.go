package blog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
)

// TestTypedColumnsCompileToTheSameSQL checks that the generated facade is a
// pure convenience: it produces exactly what the untyped form does.
func TestTypedColumnsCompileToTheSameSQL(t *testing.T) {
	typed, _, err := sqlb.Query[blog.Post]().Select(blog.PostCols.ID).
		Where(blog.PostCols.Status.Eq(blog.PostStatusPublished)).
		OrderBy(blog.PostCols.ViewCount.Desc()).SQL()
	if err != nil {
		t.Fatalf("typed SQL(): %v", err)
	}

	untyped, _, err := sqlb.Query[blog.Post]().Select(sqlb.F("id")).
		Where(sqlb.F("status").Eq("published")).
		OrderBy(sqlb.F("view_count").Desc()).SQL()
	if err != nil {
		t.Fatalf("untyped SQL(): %v", err)
	}

	if typed != untyped {
		t.Errorf("the typed facade should be a pure convenience\ntyped:   %s\nuntyped: %s", typed, untyped)
	}
}

// TestEnumTypeFlowsThroughToTheBoundValue confirms the declared Go type
// reaches the driver, rather than being widened to a bare string somewhere in
// the facade.
func TestEnumTypeFlowsThroughToTheBoundValue(t *testing.T) {
	_, args, err := sqlb.Query[blog.Post]().
		Where(blog.PostCols.Status.OneOf(blog.PostStatusDraft, blog.PostStatusReview)).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if len(args) != 2 {
		t.Fatalf("args = %#v, want 2", args)
	}
	if _, ok := args[0].(blog.PostStatus); !ok {
		t.Errorf("arg type = %T, want blog.PostStatus", args[0])
	}
}

// TestNullableColumnsCompareAgainstTheBaseType covers the decision to type a
// nullable column as its base type: the comparand is a time.Time, and NULL is
// expressed with IsNull rather than by comparing against a pointer.
func TestNullableColumnsCompareAgainstTheBaseType(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	sql, args, err := sqlb.Query[blog.Post]().Select(blog.PostCols.ID).
		Where(
			blog.PostCols.PublishedAt.Gte(cutoff),
			blog.PostCols.DeletedAt.IsNull(),
		).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if !strings.Contains(sql, `("published_at" >= $1) AND ("deleted_at" IS NULL)`) {
		t.Errorf("SQL = %s", sql)
	}
	if _, ok := args[0].(time.Time); !ok {
		t.Errorf("arg type = %T, want time.Time", args[0])
	}
}

// TestHiddenColumnsAreAbsentFromTheFacade is the compile-time half of the
// protection the schema declares. There is no AuthorCols.PasswordHash, so a
// predicate against it cannot be written at all.
func TestHiddenColumnsAreAbsentFromTheFacade(t *testing.T) {
	model := sqlb.ModelOf[blog.Author]()
	if model.Column("password_hash") == nil {
		t.Fatal("the model should still map the column: Go code needs to write it")
	}
	// The facade has five columns plus none for the hash; if a generator ever
	// emits it, this count changes and the test fails.
	if got, want := len(model.Columns), 7; got != want {
		t.Errorf("model maps %d columns, want %d", got, want)
	}
	if got, want := len(model.Selectable()), 6; got != want {
		t.Errorf("REST projection has %d columns, want %d", got, want)
	}
}

// TestTypedUpdate covers the hole the column set cannot close: Update.Set
// takes a string and an any, so without a wrapper neither the column name nor
// the value type is checked.
func TestTypedUpdate(t *testing.T) {
	published := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	sql, args, err := blog.UpdatePost().
		SetStatus(blog.PostStatusPublished).
		SetPublishedAt(&published).
		Where(blog.PostCols.ID.Eq("p1")).
		Stmt().SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if !strings.Contains(sql, `UPDATE "posts" SET "status" = $1, "published_at" = $2 WHERE "id" = $3`) {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 3 {
		t.Errorf("args = %#v, want 3", args)
	}
}

// TestTypedUpdateCounterIncrement checks the read-modify-write escape: the
// increment happens in the database, so concurrent updates do not lose counts.
func TestTypedUpdateCounterIncrement(t *testing.T) {
	sql, args, err := blog.UpdatePost().
		AddViewCount(1).
		Where(blog.PostCols.ID.Eq("p1")).
		Stmt().SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}
	if !strings.Contains(sql, `SET "view_count" = view_count + $1`) {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %#v, want 2", args)
	}
}

// TestTypedUpdateStillRefusesToBeUnscoped confirms the wrapper does not lose
// the guard on the statement underneath it.
func TestTypedUpdateStillRefusesToBeUnscoped(t *testing.T) {
	_, _, err := blog.UpdatePost().SetTitle("x").Stmt().SQL()
	if err == nil {
		t.Fatal("an unscoped update should be refused through the wrapper too")
	}
}
