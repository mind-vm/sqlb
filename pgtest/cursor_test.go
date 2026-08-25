package pgtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	"github.com/mind-vm/sqlb/schema"

	// Imported for its side effects: declaring a table registers it.
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
)

// Cursor pagination, judged by Postgres rather than by a golden string.
//
// The engine's tests assert the shape of the seek predicate, which proves the
// compiler emits what was intended and nothing about whether that is correct.
// The property that actually matters cannot be checked by looking at SQL at
// all: paging through a result set must visit every row exactly once, in the
// same order the un-paged query produces. That is a claim about what Postgres
// does with the predicate, so Postgres is what decides it here.
//
// The seed data is built to break a naive implementation. view_count has heavy
// ties, so a boundary that only compares the sort column would skip or repeat
// rows within a tie group. published_at is nullable and a third of the rows
// leave it NULL, so the NULLS FIRST and NULLS LAST branches are both walked —
// and a `col > NULL` that quietly evaluates to NULL truncates the walk, which
// shows up here as missing rows rather than as an error.

// seedPosts writes n posts with deliberate ties and NULLs, and returns the
// author they belong to.
func seedPosts(t *testing.T, db *pgxpool.Pool, n int) (orgID, authorID string) {
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

	for i := range n {
		// Five distinct view counts across n rows, so every ordering by
		// view_count has tie groups several pages could fall inside.
		views := i % 5
		// A third of the rows have no publication date. Under ASC they sort
		// last, under DESC they sort first, so both NULL branches are covered
		// by the two orderings the walk exercises.
		var published any
		if i%3 != 0 {
			published = fmt.Sprintf("2026-01-%02dT00:00:00Z", (i%28)+1)
		}
		if _, err := db.Exec(context.Background(),
			`INSERT INTO posts (org_id, author_id, title, body, view_count, published_at)
			 VALUES ($1, $2, $3, 'the body', $4, $5)`,
			orgID, authorID, fmt.Sprintf("Post %02d", i), views, published,
		); err != nil {
			t.Fatalf("inserting post %d: %v", i, err)
		}
	}
	return orgID, authorID
}

// walk pages through the query by cursor and returns the ids it visited, in
// order. It is the operation under test: everything else in this file is a way
// of deciding whether its output is right.
func walk(t *testing.T, ctx context.Context, db sqlb.Executor, order []sqlb.Order, size int) []string {
	t.Helper()

	var ids []string
	var cursor sqlb.Cursor
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("paging did not terminate; the boundary is not advancing")
		}
		q := sqlb.Query[blog.Post]().OrderBy(order...).After(cursor).Limit(size)
		rows, err := q.All(ctx, db)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(rows) == 0 {
			return ids
		}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		if len(rows) < size {
			return ids
		}
		cursor, err = q.CursorFor(rows[len(rows)-1])
		if err != nil {
			t.Fatalf("page %d: CursorFor: %v", page, err)
		}
	}
}

// straight reads the whole result set in one query, which is the answer the
// walk has to agree with.
func straight(t *testing.T, ctx context.Context, db sqlb.Executor, order []sqlb.Order) []string {
	t.Helper()

	rows, err := sqlb.Query[blog.Post]().OrderBy(order...).Stable().All(ctx, db)
	if err != nil {
		t.Fatalf("reading everything: %v", err)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func TestCursorWalkVisitsEveryRowExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	const total = 25
	seedPosts(t, raw, total)

	orderings := []struct {
		name  string
		order []sqlb.Order
	}{
		{"no sort at all, so the key is the whole ordering", nil},
		{"a unique column", []sqlb.Order{sqlb.F("title").Asc()}},
		{"heavy ties, ascending", []sqlb.Order{sqlb.F("view_count").Asc()}},
		{"heavy ties, descending", []sqlb.Order{sqlb.F("view_count").Desc()}},
		{"nullable column ascending, so NULLs sort last", []sqlb.Order{sqlb.F("published_at").Asc()}},
		{"nullable column descending, so NULLs sort first", []sqlb.Order{sqlb.F("published_at").Desc()}},
		{"nullable column with the placement asked for explicitly",
			[]sqlb.Order{sqlb.F("published_at").Desc().NullsLast()}},
		{"mixed directions, which cannot be one row comparison",
			[]sqlb.Order{sqlb.F("view_count").Asc(), sqlb.F("published_at").Desc()}},
		{"ties broken by a second tied column",
			[]sqlb.Order{sqlb.F("view_count").Desc(), sqlb.F("status").Asc()}},
	}

	// Page sizes that do and do not divide the row count, so a boundary landing
	// exactly on the last row of a page is covered as well as one landing
	// inside a tie group.
	for _, size := range []int{1, 4, 5, 7, total} {
		for _, o := range orderings {
			t.Run(fmt.Sprintf("%s/size=%d", o.name, size), func(t *testing.T) {
				want := straight(t, ctx, db, o.order)
				if len(want) != total {
					t.Fatalf("the un-paged read returned %d rows, want %d", len(want), total)
				}

				got := walk(t, ctx, db, o.order, size)
				if len(got) != len(want) {
					t.Fatalf("the walk visited %d rows, want %d", len(got), len(want))
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("row %d of the walk is %s, want %s\n"+
							"the walk and the un-paged read disagree, so the boundary is wrong",
							i, got[i], want[i])
					}
				}
			})
		}
	}
}

// A row inserted ahead of the cursor is read on a later page; one inserted
// behind it is not read again. This is the property offset paging cannot have,
// and the reason to prefer cursors for anything that walks a whole set.
func TestConcurrentInsertsDoNotShiftAPagedWalk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, authorID := seedPosts(t, raw, 10)
	order := []sqlb.Order{sqlb.F("title").Asc()}

	q := sqlb.Query[blog.Post]().OrderBy(order...).Limit(4)
	first, err := q.All(ctx, db)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	cursor, err := q.CursorFor(first[len(first)-1])
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}

	// "Post 00" sorts before everything already read; "Post 99" sorts after
	// everything. Under offset paging the first of these would push a row from
	// page one onto page two, and the client would see it twice.
	for _, title := range []string{"Post 00b", "Post 99"} {
		if _, err := raw.Exec(context.Background(),
			`INSERT INTO posts (org_id, author_id, title, body) VALUES ($1, $2, $3, 'body')`,
			orgID, authorID, title,
		); err != nil {
			t.Fatalf("inserting %s: %v", title, err)
		}
	}

	rest, err := sqlb.Query[blog.Post]().OrderBy(order...).After(cursor).All(ctx, db)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range first {
		seen[r.Title] = true
	}
	for _, r := range rest {
		if seen[r.Title] {
			t.Errorf("%s was returned twice; the walk did not hold its position", r.Title)
		}
		seen[r.Title] = true
	}
	if seen["Post 00b"] {
		t.Error("a row inserted behind the cursor was read again")
	}
	if !seen["Post 99"] {
		t.Error("a row inserted ahead of the cursor was never read")
	}
}

// The reason to emit a row comparison rather than the OR-chain: Postgres can
// answer it from an index without re-checking the rows it returns.
//
// enable_seqscan is turned off because the table here is far too small for the
// planner to choose an index on cost. That makes this a test of whether the
// predicate *can* drive an index seek, which is the property being claimed —
// not of what the planner does at this table size, which is not.
func TestRowComparisonSeekBecomesAnIndexCondition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	seedPosts(t, raw, 25)
	if _, err := raw.Exec(context.Background(),
		`CREATE INDEX posts_views_id ON posts (view_count DESC, id DESC)`,
	); err != nil {
		t.Fatalf("creating the paging index: %v", err)
	}
	if _, err := raw.Exec(context.Background(), `SET enable_seqscan = off`); err != nil {
		t.Fatalf("disabling sequential scans: %v", err)
	}

	order := []sqlb.Order{sqlb.F("view_count").Desc()}
	rows, err := sqlb.Query[blog.Post]().OrderBy(order...).Limit(4).All(ctx, db)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	cursor, err := sqlb.Query[blog.Post]().OrderBy(order...).CursorFor(rows[len(rows)-1])
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}

	q := sqlb.Query[blog.Post]().OrderBy(order...).After(cursor).Limit(4)
	stmt, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(stmt, `("view_count", "id") < ($1, $2)`) {
		t.Fatalf("this ordering should compile to a row comparison, got:\n%s", stmt)
	}

	plan := explainText(t, raw, stmt, args)
	if !strings.Contains(plan, "posts_views_id") {
		t.Fatalf("the paging index was not used:\n%s", plan)
	}
	// Index Cond is the point. The same walk over a Filter would read every row
	// before the boundary and discard it, which is what OFFSET already does.
	if !strings.Contains(plan, "Index Cond:") {
		t.Errorf("the boundary did not become an index condition:\n%s", plan)
	}
	if strings.Contains(plan, "Filter: ((view_count") {
		t.Errorf("the boundary was applied as a filter rather than a seek:\n%s", plan)
	}
}

// explainText returns the plan for a statement, as Postgres prints it.
func explainText(t *testing.T, db *pgxpool.Pool, stmt string, args []any) string {
	t.Helper()

	rows, err := db.Query(context.Background(), "EXPLAIN "+stmt, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var out strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("reading the plan: %v", err)
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	return out.String()
}

// The control for TestConcurrentInsertsDoNotShiftAPagedWalk: the same walk done
// with OFFSET returns a row twice.
//
// It is here for two reasons. It proves the assertion above can fail, so its
// passing means something — the guard is shown to fire, in the spirit of
// ADR-0016. And it is the concrete form of the argument for this whole feature:
// nothing is wrong with the offset query, the client, or the insert. The
// duplicate is what addressing a page by its distance from the start means when
// the start can move.
func TestOffsetPagingRepeatsARowUnderConcurrentInserts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, schema.DefaultRegistry())
	db := sqlb.New(raw)

	orgID, authorID := seedPosts(t, raw, 10)
	order := []sqlb.Order{sqlb.F("title").Asc()}

	first, err := sqlb.Query[blog.Post]().OrderBy(order...).Stable().Limit(4).Offset(0).All(ctx, db)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}

	// One row ahead of everything already read.
	if _, err := raw.Exec(context.Background(),
		`INSERT INTO posts (org_id, author_id, title, body) VALUES ($1, $2, 'Post 00b', 'body')`,
		orgID, authorID,
	); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	second, err := sqlb.Query[blog.Post]().OrderBy(order...).Stable().Limit(4).Offset(4).All(ctx, db)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range first {
		seen[r.Title] = true
	}
	repeated := ""
	for _, r := range second {
		if seen[r.Title] {
			repeated = r.Title
		}
	}
	if repeated == "" {
		t.Fatal("offset paging did not repeat a row, so this control proves nothing " +
			"and the cursor test above is not testing what it claims")
	}
	t.Logf("offset paging returned %q on both pages; the cursor walk does not", repeated)
}
