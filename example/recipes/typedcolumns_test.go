package recipes_test

import (
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Status is a named string type, which is how an enum column reaches Go.
type Status string

// The four statuses the schema declares.
const (
	StatusDraft     Status = "draft"
	StatusReview    Status = "review"
	StatusPublished Status = "published"
)

// A typed column facade, of the kind `sqlb generate` emits next to the models.
// F("status") takes any value; PostCols.Status takes a Status, so a typo is a
// compile error instead of a query that returns nothing.
//
// The three types are not interchangeable, and the difference is the point:
//
//	Col[T]        comparison and ordering
//	TextCol[T]    Col, plus the pattern operators
//	ArrayCol[E]   containment only — no ordering, no patterns
//
// None of them embeds Field. Embedding would promote every operator onto every
// column, so Contains would be callable on an integer — which compiles, reaches
// Postgres and fails there.
var PostCols = struct {
	ID          sqlb.Col[string]
	Title       sqlb.TextCol[string]
	Status      sqlb.Col[Status]
	ViewCount   sqlb.Col[int64]
	Tags        sqlb.ArrayCol[string]
	PublishedAt sqlb.Col[time.Time]
}{
	ID:          sqlb.Typed[string]("id"),
	Title:       sqlb.TextColumn[string]("title"),
	Status:      sqlb.Typed[Status]("status"),
	ViewCount:   sqlb.Typed[int64]("view_count"),
	Tags:        sqlb.ArrayColumn[string]("tags"),
	PublishedAt: sqlb.Typed[time.Time]("published_at"),
}

// The same query as the untyped one, with the comparands checked. Eq(42) on a
// Status column, Contains on ViewCount and Has on Title all fail to compile —
// which is the whole return on generating the facade.
func Example_typedColumnsCheckComparands() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		PostCols.Status.OneOf(StatusPublished, StatusReview),
		PostCols.Title.Contains("postgres"),
		PostCols.ViewCount.Gte(100),
		PostCols.Tags.Has("go"),
	))
	// Output:
	// WHERE ((("status" IN ($1, $2)) AND ("title" ILIKE $3)) AND ("view_count" >= $4)) AND ($5 = ANY("tags"))
	// args: [published review %postgres% 100 go]
}

// A hidden column has no entry in the generated facade at all, so a predicate
// against one does not compile rather than being refused at runtime. That is
// the same guarantee the REST layer makes, moved to build time for the caller
// who has a compiler.
func Example_typedColumnsOmitHiddenOnes() {
	// AuthorCols would have no PasswordHash field. The untyped escape hatch
	// still exists, and still runs — Go code was never the thing being
	// restricted:
	showWhere(sqlb.Query[recipes.Author]().Where(sqlb.F("password_hash").Eq("...")))
	// Output:
	// WHERE "password_hash" = $1
	// args: [...]
}

// Field is the way back out, for the operators a typed column deliberately does
// not carry. It is an escape hatch rather than a workaround: the typed surface
// covers what is checkable, and this covers the rest.
func Example_typedColumnsEscapeHatch() {
	showWhere(sqlb.Query[recipes.Post]().Where(
		PostCols.PublishedAt.Field().Between(
			time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		),
	))
	// Output:
	// WHERE "published_at" BETWEEN $1 AND $2
	// args: [2026-01-01 00:00:00 +0000 UTC 2026-06-30 00:00:00 +0000 UTC]
}

// Ordering and qualification work the same way, so a typed column is usable
// everywhere an untyped one is.
func Example_typedColumnsOrderAndQualify() {
	show(sqlb.Query[recipes.Post]().
		As("p").
		Select(PostCols.ID.Qualify("p").Field()).
		OrderBy(PostCols.ViewCount.Desc(), PostCols.ID.Asc()))
	// Output:
	// SELECT "p"."id" FROM "posts" AS "p" ORDER BY "view_count" DESC, "id" ASC
}
