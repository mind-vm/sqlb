package recipes_test

import (
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/recipes"
)

// Expand resolves a declared relation inline, one LEFT JOIN each, and the
// joined row arrives as its own value on the model rather than as extra columns
// spliced into it. That is the difference from Join: a join changes which rows
// match, an expansion changes what each row carries.
//
// Names are relation names, not column names — `Expand("author")`, not
// `Expand("author_id")`. An unknown name fails the query rather than being
// ignored, because a silently dropped expansion answers the request with a 200
// and a missing field.
//
// The joined row arrives as one JSON value in a column of its own, so the row
// stays exactly as wide as the model however many relations were asked for.
// That is what lets ?select and ?expand coexist: a projection names columns of
// Post, and an expansion is not one of them.
func Example_expandAForwardRelation() {
	show(sqlb.Query[recipes.Post]().
		Expand("author").
		Where(sqlb.F("status").Eq("published")).
		Limit(2))
	// Output:
	// SELECT "posts"."id", "posts"."org_id", "posts"."author_id", "posts"."title", "posts"."body", "posts"."status", "posts"."view_count", "posts"."tags", "posts"."metadata", "posts"."published_at", "posts"."deleted_at", "posts"."created_at", CASE WHEN "__ex_author"."id" IS NULL THEN NULL ELSE json_build_object('id', "__ex_author"."id", 'org_id', "__ex_author"."org_id", 'name', "__ex_author"."name", 'email', "__ex_author"."email") END AS "__expand_author" FROM "posts" LEFT JOIN "authors" AS "__ex_author" ON "__ex_author"."id" = "posts"."author_id" WHERE "posts"."status" = $1 LIMIT 2
	// args: [published]
}

// A hidden column has no spelling anywhere, and that holds through an
// expansion too: Author.PasswordHash is absent from the object above. This is
// the property that makes expansion safe to expose over HTTP — the joined
// model's own capabilities travel with it rather than being re-decided at the
// join.
func Example_expandRespectsTheTargetsCapabilities() {
	sql, _, err := sqlb.Query[recipes.Post]().Expand("author").SQL()
	if err != nil {
		panic(err)
	}
	showContains(sql, "password_hash")
	showContains(sql, "'email'")
	// Output:
	// mentions password_hash: false
	// mentions 'email': true
}

// Expanding is additive and idempotent, so naming the same relation twice joins
// it once — which matters because the caller and a BeforeQuery hook can both
// ask for it without coordinating.
func Example_expandIsIdempotent() {
	q := sqlb.Query[recipes.Post]().Expand("author").Expand("author")
	showExpanded(q.Expanded())
	// Output:
	// expanded: [author]
}

// A relation the model never declared is an error on the builder, and it names
// what would have been accepted. Terminal methods return it, so it cannot be
// missed by a caller who did not check Err.
func Example_expandUnknownRelationFails() {
	_, _, err := sqlb.Query[recipes.Post]().Expand("publisher").SQL()
	showError(err)
	// Output:
	// cannot expand "publisher": Post has no such relation (expandable: author)
}
