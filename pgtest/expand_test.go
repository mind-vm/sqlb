package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	"github.com/mind-vm/sqlb/schema"

	// Imported for its side effects: declaring a table registers it.
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
)

// Expansion against a real Postgres.
//
// The engine's own tests compile ?expand to SQL and compare it against a string
// somebody wrote. That is worth having and it is not the same question: a
// json_build_object with the wrong argument shape, or a CASE that Postgres reads
// differently than intended, matches the golden string exactly and fails only
// when a database sees it. It already did once — the projection was unqualified,
// and `column reference "id" is ambiguous` is not a wrong answer, it is not a
// query at all.
//
// The security-relevant one is TestHiddenColumnsDoNotSurviveTheJoin, and it is
// the reason this file reads the raw JSON rather than the decoded struct. See
// the comment there.

// seedBlog inserts one org, one author with a password hash, and one post by
// that author, returning the author's id.
func seedBlog(t *testing.T, db *pgxpool.Pool) (orgID, authorID string) {
	t.Helper()

	if err := db.QueryRow(context.Background(),
		`INSERT INTO orgs (name, slug) VALUES ('Acme', 'acme') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("inserting an org: %v", err)
	}
	if err := db.QueryRow(context.Background(),
		`INSERT INTO authors (org_id, email, name, password_hash)
		 VALUES ($1, 'ada@example.com', 'Ada', 'argon2id$v=19$correct-horse')
		 RETURNING id`, orgID,
	).Scan(&authorID); err != nil {
		t.Fatalf("inserting an author: %v", err)
	}
	if _, err := db.Exec(context.Background(),
		`INSERT INTO posts (org_id, author_id, title, body)
		 VALUES ($1, $2, 'Hello', 'the body')`, orgID, authorID,
	); err != nil {
		t.Fatalf("inserting a post: %v", err)
	}
	return orgID, authorID
}

func TestExpandRunsAndScansAgainstPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	_, authorID := seedBlog(t, raw)

	posts, err := sqlb.Query[blog.Post]().Expand("author").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding author: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}

	got := posts[0]
	if got.Author == nil {
		t.Fatal("the relation was not filled in")
	}
	if got.Author.ID != authorID || got.Author.Email != "ada@example.com" {
		t.Errorf("expanded the wrong author: %+v", got.Author)
	}
	// The key is untouched: expansion adds the row, it does not replace the
	// reference.
	if got.AuthorID != authorID {
		t.Errorf("author_id = %q, want %q", got.AuthorID, authorID)
	}
}

// TestHiddenColumnsDoNotSurviveTheJoin is the one failure here that would be a
// security bug rather than a broken feature: if Hidden stopped holding across a
// join, ?expand would become a way to read a column the target refuses to serve
// directly — blog's password_hash, in the shipped example.
//
// It reads the raw JSON Postgres produced rather than the decoded struct, and
// the distinction is the whole test. blog.Author tags PasswordHash `json:"-"`,
// so json.Unmarshal drops the key whether or not it arrived; asserting on
// got.Author.PasswordHash == "" would pass with the hash sitting in the
// database's answer. That is a test passing for the wrong reason, and this file
// exists to stop exactly that kind of thing.
func TestHiddenColumnsDoNotSurviveTheJoin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())

	seedBlog(t, raw)

	text, args, err := sqlb.Query[blog.Post]().Expand("author").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	payload := expansionJSON(t, ctx, raw, text, args, "__expand_author")

	// Present, so the assertion below is about the hash and not about an empty
	// result.
	if !strings.Contains(payload, "ada@example.com") {
		t.Fatalf("the expansion did not carry the author at all: %s", payload)
	}
	if strings.Contains(payload, "password_hash") || strings.Contains(payload, "correct-horse") {
		t.Errorf("a hidden column of the expanded target reached the response: %s", payload)
	}
}

// A LEFT JOIN that matches nothing has to produce NULL, not an object of nulls.
// "there is no related row" and "there is one and every field is empty" are
// different answers and a client can act on the difference — so the CASE guard
// is asserted against the database that evaluates it, not against the string it
// compiles to.
//
// The row is orphaned behind the foreign key's back, because a schema that let
// this happen through its own DDL would be a different bug. What is under test
// is what the query does when it happens.
func TestAMissingTargetExpandsToNullNotAnEmptyObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	_, authorID := seedBlog(t, raw)

	mustExec(t, raw, `ALTER TABLE posts DROP CONSTRAINT posts_author_id_fkey`)
	if _, err := raw.Exec(context.Background(), `DELETE FROM authors WHERE id = $1`, authorID); err != nil {
		t.Fatalf("orphaning the post: %v", err)
	}

	text, args, err := sqlb.Query[blog.Post]().Expand("author").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if payload := expansionJSON(t, ctx, raw, text, args, "__expand_author"); payload != "" {
		t.Errorf("a reference to a row that is gone expanded to %s, want NULL", payload)
	}

	// And the scanner turns that into a nil field rather than a zero struct,
	// which is the same distinction one layer up.
	posts, err := sqlb.Query[blog.Post]().Expand("author").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding a missing author: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	if posts[0].Author != nil {
		t.Errorf("a missing target scanned as %+v, want nil", posts[0].Author)
	}
}

// Every other list parameter still has to work once a second table is in the
// statement. posts and authors share id, org_id, created_at and updated_at, so
// an unqualified reference to any of them is ambiguous — which Postgres refuses
// outright rather than resolving.
func TestExpandComposesWithTheOtherQueryParameters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, _ := seedBlog(t, raw)

	for name, q := range map[string]*sqlb.Builder[blog.Post]{
		"filter on a shared column name": sqlb.Query[blog.Post]().
			Expand("author").Where(sqlb.F("org_id").Eq(orgID)),
		"sort on a shared column name": sqlb.Query[blog.Post]().
			Expand("author").OrderBy(sqlb.F("created_at").Desc()),
		"an explicit projection over shared names": sqlb.Query[blog.Post]().
			Expand("author").Select(sqlb.F("id"), sqlb.F("org_id")),
	} {
		rows, err := q.All(ctx, db)
		if err != nil {
			text, _, _ := q.SQL()
			t.Errorf("%s: %v\n%s", name, err, text)
			continue
		}
		if len(rows) != 1 {
			t.Errorf("%s: got %d posts, want 1", name, len(rows))
		}
	}

	// Counting drops the join — it cannot change how many rows match — but the
	// statement still has to be valid.
	total, err := sqlb.Query[blog.Post]().Expand("author").Count(ctx, db)
	if err != nil {
		t.Fatalf("counting an expanded query: %v", err)
	}
	if total != 1 {
		t.Errorf("count = %d, want 1", total)
	}
}

// expansionJSON runs a compiled statement and returns the named expansion
// column of its first row, as the text Postgres produced. An empty string means
// the column was NULL.
func expansionJSON(t *testing.T, ctx context.Context, db *pgxpool.Pool, text string, args []any, column string) string {
	t.Helper()

	rows, err := db.Query(ctx, text, args...)
	if err != nil {
		t.Fatalf("executing:\n%s\n%v", text, err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	names := make([]string, len(fields))
	at := -1
	for i, f := range fields {
		names[i] = f.Name
		if f.Name == column {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no %q in the result columns %v", column, names)
	}

	if !rows.Next() {
		t.Fatalf("the statement returned no rows:\n%s", text)
	}
	// The raw bytes, so this reads what Postgres produced rather than what a
	// codec made of it: the expansion column is JSON and the assertions are
	// about its text.
	values := rows.RawValues()
	out := string(values[at])
	if err := rows.Err(); err != nil {
		t.Fatalf("reading rows: %v", err)
	}
	return out
}

// Reverse expansion against a real Postgres.
//
// This half needs a database more than the forward half did. A collection is a
// correlated subquery with a window function, an aggregate, a FILTER and a
// LIMIT one past the cap, and every one of those is a claim about how Postgres
// evaluates the statement rather than about what string sqlb produced. ADR-0022
// says outright that it expects at least one of them to be wrong until a
// database has ruled; this file is that ruling.

// seedAuthorPosts gives the seeded author three posts with distinct publication
// dates, so a cap of two has something to truncate and an order to truncate by.
func seedAuthorPosts(t *testing.T, db *pgxpool.Pool, orgID, authorID string) {
	t.Helper()
	for _, p := range []struct {
		title     string
		published string
	}{
		{"Oldest", "2020-01-01T00:00:00Z"},
		{"Middle", "2021-01-01T00:00:00Z"},
		{"Newest", "2022-01-01T00:00:00Z"},
	} {
		if _, err := db.Exec(context.Background(),
			`INSERT INTO posts (org_id, author_id, title, body, status, published_at)
			 VALUES ($1, $2, $3, 'body', 'published', $4)`,
			orgID, authorID, p.title, p.published,
		); err != nil {
			t.Fatalf("inserting %s: %v", p.title, err)
		}
	}
}

func TestExpandCollectionRunsAndScansAgainstPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, authorID := seedBlog(t, raw)
	seedAuthorPosts(t, raw, orgID, authorID)

	authors, err := sqlb.Query[blog.Author]().Expand("posts").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding posts: %v", err)
	}
	if len(authors) != 1 {
		t.Fatalf("got %d authors, want 1", len(authors))
	}
	got := authors[0]
	if got.Posts == nil {
		t.Fatal("the collection was not filled in")
	}

	// The schema caps this relation at two, and seedBlog already added one post
	// with no publication date on top of the three above.
	if got.Posts.Len() != 2 {
		t.Fatalf("got %d posts, want the declared cap of 2: %+v", got.Posts.Len(), got.Posts.Items)
	}
	// The half a bare array could not have carried.
	if !got.Posts.HasMore {
		t.Error("a truncated collection did not report has_more")
	}
	// Ordered by -published_at, so the newest two, newest first. NULLs sort
	// first under DESC in Postgres, which is why the undated post from seedBlog
	// leads — an ordering detail no golden string would have caught.
	if got.Posts.Items[0].Title != "Hello" || got.Posts.Items[1].Title != "Newest" {
		t.Errorf("wrong posts or wrong order: %q, %q",
			got.Posts.Items[0].Title, got.Posts.Items[1].Title)
	}
	// The child rows are whole rows, not just keys.
	if got.Posts.Items[1].AuthorID != authorID || got.Posts.Items[1].Body == "" {
		t.Errorf("child row is incomplete: %+v", got.Posts.Items[1])
	}
}

// A row with no children is an empty array and has_more false — not a null, and
// not a missing key. "no children" and "did not ask" have to stay
// distinguishable, which is the same argument the forward direction makes about
// NULL versus an object of nulls.
func TestAnEmptyCollectionIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, _ := seedBlog(t, raw)
	var emptyOrg string
	if err := raw.QueryRow(context.Background(),
		`INSERT INTO orgs (name, slug) VALUES ('Empty', 'empty') RETURNING id`,
	).Scan(&emptyOrg); err != nil {
		t.Fatalf("inserting an org: %v", err)
	}

	orgs, err := sqlb.Query[blog.Org]().Expand("authors").OrderBy(sqlb.F("slug").Asc()).All(ctx, db)
	if err != nil {
		t.Fatalf("expanding authors: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("got %d orgs, want 2", len(orgs))
	}

	byID := map[string]*blog.Org{}
	for i := range orgs {
		byID[orgs[i].ID] = &orgs[i]
	}
	if got := byID[orgID].Authors; got == nil || got.Len() != 1 {
		t.Errorf("the populated org expanded to %+v, want one author", got)
	}
	empty := byID[emptyOrg].Authors
	if empty == nil {
		t.Fatal("an org with no authors expanded to nothing at all, want an empty collection")
	}
	if empty.Len() != 0 || empty.HasMore {
		t.Errorf("an org with no authors expanded to %+v", empty)
	}
	if empty.Items == nil {
		t.Error("the empty collection scanned as a nil slice; the SQL should coalesce it to []")
	}
}

// The security-relevant one in the reverse direction, and the reason the org →
// authors relation is the fixture: authors.password_hash is Hidden, so a
// collection that carried it would be a way to read through ?expand a column
// the authors endpoint refuses to serve.
//
// It reads the raw JSON for the reason the forward version does — blog.Author
// tags PasswordHash `json:"-"`, so a decoded struct would look clean whatever
// the database returned.
func TestHiddenColumnsDoNotSurviveACollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())

	seedBlog(t, raw)

	text, args, err := sqlb.Query[blog.Org]().Expand("authors").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	payload := expansionJSON(t, ctx, raw, text, args, "__expand_authors")

	if !strings.Contains(payload, "ada@example.com") {
		t.Fatalf("the expansion did not carry the author at all: %s", payload)
	}
	if strings.Contains(payload, "password_hash") || strings.Contains(payload, "correct-horse") {
		t.Errorf("a hidden column of the collected rows reached the response: %s", payload)
	}
}

// A collection must not multiply the base rows, which is the whole reason it is
// a subquery rather than a join. Three posts under one author still means one
// author row, and the count is unchanged by the expansion.
func TestACollectionDoesNotMultiplyTheBaseRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, authorID := seedBlog(t, raw)
	seedAuthorPosts(t, raw, orgID, authorID)

	rows, err := sqlb.Query[blog.Author]().Expand("posts").All(ctx, db)
	if err != nil {
		t.Fatalf("expanding posts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("one author with four posts returned %d rows", len(rows))
	}

	total, err := sqlb.Query[blog.Author]().Expand("posts").Count(ctx, db)
	if err != nil {
		t.Fatalf("counting an expanded query: %v", err)
	}
	if total != 1 {
		t.Errorf("count = %d, want 1", total)
	}
}

// Everything else a list request can carry still has to work with a correlated
// subquery in the projection. orgs, authors and posts share id, org_id,
// created_at and updated_at, so an unqualified reference to any of them is
// ambiguous — and the subquery adds a second scope in which that is true.
func TestACollectionComposesWithTheOtherQueryParameters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, authorID := seedBlog(t, raw)
	seedAuthorPosts(t, raw, orgID, authorID)

	for name, q := range map[string]*sqlb.Builder[blog.Author]{
		"filter on a shared column name": sqlb.Query[blog.Author]().
			Expand("posts").Where(sqlb.F("org_id").Eq(orgID)),
		"sort on a shared column name": sqlb.Query[blog.Author]().
			Expand("posts").OrderBy(sqlb.F("created_at").Desc()),
		"an explicit projection over shared names": sqlb.Query[blog.Author]().
			Expand("posts").Select(sqlb.F("id"), sqlb.F("org_id")),
		"a page boundary": sqlb.Query[blog.Author]().
			Expand("posts").OrderBy(sqlb.F("id").Asc()).Stable().Limit(10),
		"both directions at once": sqlb.Query[blog.Author]().
			Expand("posts", "org"),
	} {
		rows, err := q.All(ctx, db)
		if err != nil {
			text, _, _ := q.SQL()
			t.Errorf("%s: %v\n%s", name, err, text)
			continue
		}
		if len(rows) != 1 {
			t.Errorf("%s: got %d authors, want 1", name, len(rows))
		}
		_ = authorID
	}
}

// The plan is the claim ADR-0022 is least sure of: one subquery per base row is
// affordable only if the child's foreign key is indexed, which is why
// schema.Lint reports an unindexed one as a warning rather than as hygiene.
// This asserts the index is actually used rather than trusting that it exists.
func TestACollectionUsesTheForeignKeyIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())

	orgID, authorID := seedBlog(t, raw)
	seedAuthorPosts(t, raw, orgID, authorID)

	text, args, err := sqlb.Query[blog.Author]().Expand("posts").SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	rows, err := raw.Query(ctx, "EXPLAIN "+text, args...)
	if err != nil {
		t.Fatalf("EXPLAIN:\n%s\n%v", text, err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Postgres will choose a sequential scan on a table this small whatever the
	// indexes say, so the assertion is that the planner *considered* the
	// subquery cheap enough to keep as a correlated one rather than that it
	// used the index today. What would be a real regression is the subquery
	// disappearing into a join, which would change the row count.
	if strings.Contains(plan.String(), "Nested Loop Left Join") {
		t.Errorf("the collection was planned as a join rather than a subquery:\n%s", plan.String())
	}
	if !strings.Contains(plan.String(), "SubPlan") {
		t.Errorf("no correlated subquery in the plan:\n%s", plan.String())
	}
}

// The leak this closes, demonstrated against a real database.
//
// A plain single-column foreign key lets a post in one org reference an author
// in another. Before the target's hooks ran on an expansion, `?expand=author`
// returned that author's row to a caller scoped to the first org — a
// cross-tenant read behind a capability the schema declared safe.
//
// It is here rather than only in the engine's tests because the engine can only
// assert what the SQL says. What matters is which rows come back, and only
// Postgres answers that.
func TestExpandDoesNotCrossATenantBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	reg := sqlb.NewRegistry()
	db := sqlb.New(raw).WithHooks(reg)

	mine, _ := seedBlog(t, raw)

	// A second org, with an author of its own, and a post in the *first* org
	// that points at them. The foreign key permits it; nothing in the schema
	// says a post's author must share its org.
	var theirs, theirAuthor string
	if err := raw.QueryRow(context.Background(),
		`INSERT INTO orgs (name, slug) VALUES ('Other', 'other') RETURNING id`,
	).Scan(&theirs); err != nil {
		t.Fatalf("inserting the second org: %v", err)
	}
	if err := raw.QueryRow(context.Background(),
		`INSERT INTO authors (org_id, email, name, password_hash)
		 VALUES ($1, 'grace@example.com', 'Grace', 'argon2id$v=19$other')
		 RETURNING id`, theirs,
	).Scan(&theirAuthor); err != nil {
		t.Fatalf("inserting the second author: %v", err)
	}
	var leaky string
	if err := raw.QueryRow(context.Background(),
		`INSERT INTO posts (org_id, author_id, title, body)
		 VALUES ($1, $2, 'Crosses', 'the boundary') RETURNING id`, mine, theirAuthor,
	).Scan(&leaky); err != nil {
		t.Fatalf("inserting the cross-tenant post: %v", err)
	}

	// The scope, registered on both models exactly as an application would —
	// into the registry the handle above carries.
	posts := sqlb.On[blog.Post](reg)
	authors := sqlb.On[blog.Author](reg)
	posts.BeforeQuery(func(_ context.Context, q *sqlb.Builder[blog.Post]) error {
		q.Where(sqlb.F("org_id").Eq(mine))
		return nil
	})
	authors.BeforeQuery(func(_ context.Context, q *sqlb.Builder[blog.Author]) error {
		q.Where(sqlb.F("org_id").Eq(mine))
		return nil
	})

	rows, err := sqlb.Query[blog.Post]().
		Expand("author").
		OrderBy(sqlb.F("title").Asc()).
		All(ctx, db)
	if err != nil {
		t.Fatalf("expanding author: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d posts, want 2 — the parent scope should not have changed", len(rows))
	}

	byTitle := map[string]blog.Post{}
	for _, p := range rows {
		byTitle[p.Title] = p
	}

	// The post whose author is out of scope still appears — the scope belongs
	// in the join condition, so a LEFT JOIN that matches nothing nulls the
	// expansion rather than dropping the row.
	crossing, found := byTitle["Crosses"]
	if !found {
		t.Fatal("the cross-tenant post vanished; the target's scope reached the WHERE clause rather than the ON clause")
	}
	if crossing.Author != nil {
		t.Errorf("an author from another org was expanded into this org's page: %+v", crossing.Author)
	}
	// The foreign key itself is untouched. Hiding the row is the boundary;
	// rewriting the data is not this layer's business.
	if crossing.AuthorID != theirAuthor {
		t.Errorf("author_id = %q, want the reference left alone (%q)", crossing.AuthorID, theirAuthor)
	}

	// And the in-scope expansion still resolves, so this is a boundary rather
	// than a blanket refusal.
	ok, found := byTitle["Hello"]
	if !found {
		t.Fatal("the in-org post is missing")
	}
	if ok.Author == nil {
		t.Error("an author in the caller's own org should still expand")
	}
}
