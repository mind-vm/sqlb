package pgtest

// The shapes docs/special-cases-subject-go.md counted, judged by Postgres.
//
// subjectgo_test.go in the root module pins what sqlb *writes* for each of these.
// Four of them cannot be settled that way, because the answer is not a string:
//
//   - a filtered aggregate has to produce the right numbers, and the reason to
//     want one is that the alternative is several statements or a CASE per
//     column, which is where a tenant predicate gets forgotten;
//   - the bind parameter ceiling is the wire protocol's, so only a real driver
//     and server can confirm that the largest batch sqlb accepts is one Postgres
//     actually takes;
//   - a bulk reposition is a claim about one statement doing what N would;
//   - and a cursor held across a reorder either repeats a row or it does not.
//
// The last of those contradicts a sentence in ADR-0027, which is why it is here
// rather than in a document.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
)

// The models are declared here rather than reused from an example because the
// point of the second corpus is that it is a different architecture: one
// application, one large schema. These are its tables, trimmed to the columns
// each shape needs.

type subjectEvent struct {
	ID        string `db:"id" sqlb:"pk"`
	OrgID     string `db:"org_id" sqlb:"filter"`
	PageURL   string `db:"page_url" sqlb:"filter,sort"`
	EventType string `db:"event_type" sqlb:"filter"`
	UserID    string `db:"user_id" sqlb:"filter"`
	Duration  *int64 `db:"duration_ms" sqlb:"filter"`
}

func (subjectEvent) TableName() string { return "analytics_events" }

type subjectChunk struct {
	ID       string `db:"id"`
	OrgID    string `db:"org_id"`
	DocID    string `db:"document_id"`
	Ordinal  int64  `db:"ordinal"`
	Content  string `db:"content"`
	Tokens   int64  `db:"tokens"`
	Hash     string `db:"content_hash"`
	Source   string `db:"source"`
	Lang     string `db:"lang"`
	Revision int64  `db:"revision"`
}

func (subjectChunk) TableName() string { return "chunks" }

type subjectTask struct {
	ID        string    `db:"id" sqlb:"pk"`
	OrgID     string    `db:"org_id" sqlb:"filter"`
	ProjectID string    `db:"project_id" sqlb:"filter"`
	Title     string    `db:"title" sqlb:"sort"`
	Position  string    `db:"position" sqlb:"sort"`
	CreatedAt time.Time `db:"created_at" sqlb:"sort"`
}

func (subjectTask) TableName() string { return "tasks" }

type subjectSubmission struct {
	ID     string `db:"id" sqlb:"pk"`
	OrgID  string `db:"org_id" sqlb:"filter"`
	FormID string `db:"form_id" sqlb:"filter"`
	Values string `db:"custom_field_values"`
}

func (subjectSubmission) TableName() string { return "submissions" }

// A dashboard row is several differently-filtered counts over one scan, and the
// tenant predicate is written once.
//
// That last clause is the argument. Written without FILTER this is either four
// round trips — each of which has to remember `org_id = $1` — or a
// `SUM(CASE WHEN … THEN 1 ELSE 0 END)` per column, which is the same predicate
// copied per column instead of stated once in the WHERE. The seed data has a
// second org in it for exactly that reason: a query that forgets the tenant
// gets a bigger number here rather than the same one.
func TestAFilteredCountCountsWhatItSays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	createSubjectEvents(t, raw)
	db := sqlb.New(raw)

	seedSubjectEvents(t, raw, "acme", "/pricing", 5, 2)
	seedSubjectEvents(t, raw, "acme", "/docs", 1, 4)
	// The other tenant, whose rows must not appear in any of the four numbers.
	seedSubjectEvents(t, raw, "globex", "/pricing", 9, 9)

	type row struct {
		PageURL     string `db:"page_url"`
		Total       int64  `db:"total"`
		UniqueUsers int64  `db:"unique_users"`
		Pageviews   int64  `db:"pageviews"`
		Clicks      int64  `db:"clicks"`
	}

	rows, err := sqlb.Collect[row](ctx, db, sqlb.Query[subjectEvent]().
		Select(
			sqlb.F("page_url"),
			sqlb.Count().As("total"),
			sqlb.CountDistinct(sqlb.F("user_id")).As("unique_users"),
			sqlb.RawSel(`count(*) FILTER (WHERE "event_type" = ?)`, "pageview").As("pageviews"),
			sqlb.RawSel(`count(*) FILTER (WHERE "event_type" = ?)`, "click").As("clicks"),
		).
		Where(sqlb.F("org_id").Eq("acme")).
		GroupBy(sqlb.F("page_url")).
		OrderBy(sqlb.OrderByDesc(sqlb.Raw{SQL: "total"})))
	if err != nil {
		t.Fatalf("the dashboard query: %v", err)
	}

	want := []row{
		{PageURL: "/pricing", Total: 7, UniqueUsers: 5, Pageviews: 5, Clicks: 2},
		{PageURL: "/docs", Total: 5, UniqueUsers: 4, Pageviews: 1, Clicks: 4},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %#v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %#v, want %#v", i, rows[i], want[i])
		}
	}
}

// The empty-set trap, under a filter that matches nothing — which is the
// combination a dashboard produces on its own, without anybody having to pick
// an empty date range.
//
// COUNT over no rows is 0. SUM over no rows is NULL, and a filtered SUM is a
// SUM over no rows whenever the filter excludes everything, so the filter makes
// the trap reachable from live data rather than from an edge case. Coalesce is
// the fix that exists; this is Postgres confirming which half needs it.
func TestAFilteredSumOverNoMatchingRowsIsNullUntilItIsCoalesced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	createSubjectEvents(t, raw)
	db := sqlb.New(raw)

	seedSubjectEvents(t, raw, "acme", "/pricing", 3, 0)

	bare := sqlb.RawSel(`sum("duration_ms") FILTER (WHERE "event_type" = ?)`, "click")

	type nullable struct {
		Dwell *int64 `db:"dwell_ms"`
	}
	got, err := sqlb.Collect[nullable](ctx, db, sqlb.Query[subjectEvent]().
		Select(bare.As("dwell_ms")).
		Where(sqlb.F("org_id").Eq("acme")))
	if err != nil {
		t.Fatalf("the bare sum: %v", err)
	}
	if len(got) != 1 || got[0].Dwell != nil {
		t.Fatalf("a filtered sum matching no rows = %#v, want NULL — if this is 0 the trap has "+
			"been closed somewhere and the Coalesce below is no longer load-bearing", got)
	}

	type counted struct {
		Dwell int64 `db:"dwell_ms"`
	}
	fixed, err := sqlb.Collect[counted](ctx, db, sqlb.Query[subjectEvent]().
		Select(sqlb.Coalesce(bare.Expr(), sqlb.Raw{SQL: "0"}).As("dwell_ms")).
		Where(sqlb.F("org_id").Eq("acme")))
	if err != nil {
		t.Fatalf("the coalesced sum: %v", err)
	}
	if len(fixed) != 1 || fixed[0].Dwell != 0 {
		t.Fatalf("the coalesced sum = %#v, want 0", fixed)
	}
}

// The bind parameter ceiling, measured rather than reasoned about.
//
// A Postgres bind message carries an int16 parameter count, so a statement
// holds at most 65,535 of them. InsertRows renders one VALUES list with one
// bind per column per row and checks nothing, so at the ten columns this
// corpus's chunk table uses the last statement that works has 6,553 rows in it.
//
// The refusal is now sqlb's rather than the driver's, and this is where that is
// worth measuring: the arithmetic says 6,553 rows is the largest batch that
// fits, and only a real server can confirm that the batch sqlb accepts is one
// Postgres actually takes. A ceiling that is one row too generous fails in
// production and nowhere else.
//
// The other direction — that 6,554 rows is refused, in terms of rows — is
// settled without a database in the engine's own suite. What it adds here is
// that the refusal happens *before* the wire: pgx never sees the statement, so
// the message is sqlb's, in the units the caller wrote.
func TestBulkInsertIsRefusedInTermsOfRowsAtTheBindParameterCeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	mustExec(t, raw, `
		CREATE TABLE chunks (
			id            text PRIMARY KEY,
			org_id        text NOT NULL,
			document_id   text NOT NULL,
			ordinal       bigint NOT NULL,
			content       text NOT NULL,
			tokens        bigint NOT NULL,
			content_hash  text NOT NULL,
			source        text NOT NULL,
			lang          text NOT NULL,
			revision      bigint NOT NULL
		)`)
	db := sqlb.New(raw)

	const (
		maxParams = 65535
		columns   = 10
		maxRows   = maxParams / columns // 6553
	)

	rows := func(n, offset int) []*subjectChunk {
		out := make([]*subjectChunk, n)
		for i := range out {
			out[i] = &subjectChunk{
				ID: fmt.Sprintf("c%d", offset+i), OrgID: "acme", DocID: "d1",
				Ordinal: int64(offset + i), Content: "chunk", Tokens: 1,
				Hash: "h", Source: "upload", Lang: "en", Revision: 1,
			}
		}
		return out
	}

	// The last statement the protocol carries. 65,530 parameters.
	if _, err := sqlb.InsertRows(rows(maxRows, 0)...).Exec(ctx, db); err != nil {
		t.Fatalf("inserting %d rows (%d parameters) failed, and it is inside the limit: %v",
			maxRows, maxRows*columns, err)
	}

	// One row more. 65,540 parameters, and sqlb refuses it rather than sending
	// it: the statement never reaches pgx.
	_, err := sqlb.InsertRows(rows(maxRows+1, maxRows)...).Exec(ctx, db)
	if err == nil {
		t.Fatalf("inserting %d rows (%d parameters) succeeded; the ceiling has moved and this "+
			"test is the thing that should say so", maxRows+1, (maxRows+1)*columns)
	}
	t.Logf("the answer at %d parameters: %v", (maxRows+1)*columns, err)

	// In rows, because rows are what the caller chose. pgx's own wording —
	// "extended protocol limited to 65535 parameters" — would leave a caller who
	// inserted 6,554 rows to divide by the column count to find out what
	// happened, and that wording appearing here would mean sqlb had let the
	// batch through after all.
	if !strings.Contains(err.Error(), "insert at most") {
		t.Errorf("the failure does not say how large a batch would work: %v", err)
	}
	if strings.Contains(err.Error(), "extended protocol") {
		t.Errorf("the batch reached the driver, so sqlb's own check did not fire: %v", err)
	}
}

// A drag-and-drop reorder writes different values to many rows in one
// statement, and the write has no builder form.
//
// The read side is sqlb's — ForUpdate over the siblings, in the order the user
// sees them — and the arrays are two ordinary bind parameters. The statement
// between them is hand-written, because Update.Set writes one value to every
// matched row and there is no `UpdateFrom`. The cost of writing it row by row is not only N
// round trips: it is N round trips *while holding the lock the SELECT took*.
func TestABulkRepositionIsOneStatementUnderOneLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	createSubjectTasks(t, raw)
	db := sqlb.New(raw)

	seedSubjectTasks(t, raw, "acme", "p1", 5)

	tx, err := raw.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Read the siblings in the order the user is dragging inside, and hold
	// them. This is the half sqlb writes today.
	siblings, err := sqlb.Query[subjectTask]().
		Where(sqlb.F("project_id").Eq("p1"), sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("position").Asc(), sqlb.F("created_at").Desc()).
		ForUpdate().
		All(ctx, sqlb.New(tx))
	if err != nil {
		t.Fatalf("selecting the siblings for update: %v", err)
	}
	if len(siblings) != 5 {
		t.Fatalf("locked %d siblings, want 5", len(siblings))
	}

	// Reverse them, which is the worst case a normalisation pass produces: every
	// row gets a different new key.
	ids := make([]string, len(siblings))
	positions := make([]string, len(siblings))
	for i, s := range siblings {
		ids[i] = s.ID
		positions[i] = fmt.Sprintf("a%d", len(siblings)-1-i)
	}
	// The half that is hand-written. One statement, two binds, any number of
	// rows — which is what `sqlb.UpdateFrom[T]` would render. The slices go
	// over as text[] with nothing in between: under database/sql this needed
	// sqlb.EncodeArray, and ADR-0040 removed both the need and the function.
	res, err := tx.Exec(ctx, `
		UPDATE tasks SET position = u.position
		FROM (SELECT unnest($1::text[]) AS id, unnest($2::text[]) AS position) u
		WHERE tasks.id = u.id AND tasks.org_id = $3`,
		ids, positions, "acme")
	if err != nil {
		t.Fatalf("the reposition: %v", err)
	}
	if affected := res.RowsAffected(); affected != int64(len(siblings)) {
		t.Fatalf("one statement moved %d rows, want %d", affected, len(siblings))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	after, err := sqlb.Query[subjectTask]().
		Where(sqlb.F("project_id").Eq("p1")).
		OrderBy(sqlb.F("position").Asc()).
		All(ctx, db)
	if err != nil {
		t.Fatalf("reading the new order: %v", err)
	}
	for i := range after {
		want := siblings[len(siblings)-1-i].ID
		if after[i].ID != want {
			t.Fatalf("position %d holds %s, want %s — the order did not reverse", i, after[i].ID, want)
		}
	}
}

// A keyset cursor held across a reorder reads a row twice, and ADR-0027 says it
// cannot.
//
// The ADR's sentence is about *inserts*, and for inserts it is true —
// TestConcurrentInsertsDoNotShiftAPagedWalk is that claim, and it passes. What
// the record does not say is that the guarantee comes from the boundary being a
// position in an ordering: a row that was behind the boundary and stays there is
// never read again. Nothing keeps it there. An UPDATE to a sort column moves a
// row across the boundary, and the client reading page two sees it for the
// second time.
//
// That is not a defect in the cursor. It is what "a page is a position" means
// when the position is computed from mutable data, and it is precisely the
// endpoint a reorder feature adds — so an application with drag-and-drop
// ordering and cursor paging has this, today, and no document mentions it.
//
// The mitigation is not in this test because it is a design question: order by
// something immutable (the key, a creation time), or accept that a reorder
// invalidates outstanding cursors and say so at the API.
func TestAReorderUnderACursorCanRepeatARow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	createSubjectTasks(t, raw)
	db := sqlb.New(raw)

	seedSubjectTasks(t, raw, "acme", "p1", 10)
	order := []sqlb.Order{sqlb.F("position").Asc()}

	q := sqlb.Query[subjectTask]().
		Where(sqlb.F("project_id").Eq("p1")).
		OrderBy(order...).Limit(4)
	first, err := q.All(ctx, db)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("first page has %d rows, want 4", len(first))
	}
	cursor, err := q.CursorFor(first[len(first)-1])
	if err != nil {
		t.Fatalf("CursorFor: %v", err)
	}

	// Somebody drags the first task to the end. It is behind the client's
	// boundary and it is now ahead of it.
	moved := first[0]
	if _, err := raw.Exec(ctx,
		`UPDATE tasks SET position = 'z9' WHERE id = $1`, moved.ID); err != nil {
		t.Fatalf("the reorder: %v", err)
	}

	rest, err := sqlb.Query[subjectTask]().
		Where(sqlb.F("project_id").Eq("p1")).
		OrderBy(order...).After(cursor).All(ctx, db)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range first {
		seen[r.ID] = true
	}
	repeated := false
	for _, r := range rest {
		if seen[r.ID] {
			repeated = true
		}
	}
	if !repeated {
		t.Fatalf("the row moved across the boundary was not read twice.\n\n" +
			"Either the boundary now survives an update to a sort column — in which case " +
			"ADR-0027 has grown a stronger guarantee than it claims and this test should " +
			"assert it — or this test no longer moves the row it thinks it does.")
	}
	t.Logf("task %s was read on page one and again after the reorder", moved.ID)

	// And the other direction, which is the same cause and the worse symptom: a
	// row dragged from ahead of the boundary to behind it is never read at all.
	// Any row of the second page that is not the one already moved will do.
	var skipped subjectTask
	for _, r := range rest {
		if r.ID != moved.ID {
			skipped = r
			break
		}
	}
	if skipped.ID == "" {
		t.Fatal("the second page holds nothing but the row the first reorder moved")
	}
	if _, err := raw.Exec(ctx,
		`UPDATE tasks SET position = 'a0' WHERE id = $1`, skipped.ID); err != nil {
		t.Fatalf("the second reorder: %v", err)
	}
	again, err := sqlb.Query[subjectTask]().
		Where(sqlb.F("project_id").Eq("p1")).
		OrderBy(order...).After(cursor).All(ctx, db)
	if err != nil {
		t.Fatalf("resuming after the second reorder: %v", err)
	}
	for _, r := range again {
		if r.ID == skipped.ID {
			t.Fatalf("task %s is still after the boundary; the second reorder did not move it "+
				"across", skipped.ID)
		}
	}
}

// `DISTINCT ON` is one occurrence per corpus and both are "first row per
// group". It is reachable, and Postgres is what decides whether the way it is
// reachable actually works.
//
// The fragment lands in the projection because it is written first and rendered
// verbatim. What nothing checks is the rule that makes it correct: the leading
// ORDER BY terms must match the DISTINCT ON expressions, or Postgres either
// errors or — worse — returns whichever row it liked. This test asserts the
// working spelling; the ordering agreeing with the grouping is the caller's
// job, which is the argument for a combinator that renders both from one term.
func TestDistinctOnPicksTheFirstRowPerGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	createSubjectEvents(t, raw)
	db := sqlb.New(raw)

	// Two pages, several events each, and the newest of each page is the row
	// that must survive. id sorts in insertion order here, so it stands in for
	// the created_at the corpus orders by.
	for _, e := range []struct{ id, page, kind string }{
		{"e1", "/pricing", "pageview"},
		{"e2", "/pricing", "click"},
		{"e3", "/docs", "pageview"},
		{"e4", "/docs", "click"},
		{"e5", "/docs", "signup"},
	} {
		mustExec(t, raw, fmt.Sprintf(
			`INSERT INTO analytics_events (id, org_id, page_url, event_type, user_id)
			 VALUES ('%s', 'acme', '%s', '%s', 'u1')`, e.id, e.page, e.kind))
	}

	type latest struct {
		PageURL   string `db:"page_url"`
		EventType string `db:"event_type"`
	}
	rows, err := sqlb.Collect[latest](ctx, db, sqlb.Query[subjectEvent]().
		Select(
			sqlb.RawSel(`DISTINCT ON ("page_url") "page_url"`),
			sqlb.F("event_type"),
		).
		Where(sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("page_url").Asc(), sqlb.F("id").Desc()))
	if err != nil {
		t.Fatalf("the distinct-on query: %v", err)
	}

	want := []latest{{"/docs", "signup"}, {"/pricing", "click"}}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %#v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %#v, want %#v", i, rows[i], want[i])
		}
	}
}

// The forms shape, run: a filter into a jsonb document where the key is data,
// including keys chosen to break a query that interpolated them.
//
// This is the check the `forms` example would own. It is here in its smallest
// form because the property does not depend on the example existing: whatever
// spells the accessor later — `C.Values.Key("severity").AsInt()` — has to keep
// the key a bind parameter, and a test that already passes is the cheapest way
// to notice if it stops.
func TestAJSONKeyFilterReachesPostgresAsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	mustExec(t, raw, `
		CREATE TABLE submissions (
			id                   text PRIMARY KEY,
			org_id               text NOT NULL,
			form_id              text NOT NULL,
			custom_field_values  jsonb NOT NULL DEFAULT '{}'::jsonb
		)`)
	db := sqlb.New(raw)

	// One submission per hostile key, each carrying that key with the value 5,
	// plus a decoy that carries a different key entirely.
	hostile := []string{
		"severity",
		"a'b",
		`a"b`,
		"a--b",
		"key->>'other'",
		"'; DROP TABLE submissions; --",
	}
	for i, key := range hostile {
		values, err := oneKeyJSON(key, 5)
		if err != nil {
			t.Fatalf("building the values object: %v", err)
		}
		if _, err := raw.Exec(ctx,
			`INSERT INTO submissions (id, org_id, form_id, custom_field_values)
			 VALUES ($1, 'acme', 'f1', $2::jsonb)`,
			fmt.Sprintf("s%d", i), values); err != nil {
			t.Fatalf("inserting submission %d: %v", i, err)
		}
	}
	mustExec(t, raw, `
		INSERT INTO submissions (id, org_id, form_id, custom_field_values)
		VALUES ('decoy', 'acme', 'f1', '{"unrelated": 5}'::jsonb)`)

	for i, key := range hostile {
		t.Run(key, func(t *testing.T) {
			rows, err := sqlb.Query[subjectSubmission]().
				Where(
					sqlb.F("org_id").Eq("acme"),
					// `??` is a literal question mark, which is Postgres's
					// key-exists operator. A single `?` is sqlb's placeholder.
					sqlb.RawPred(`"custom_field_values" ?? ?`, key),
					sqlb.RawPred(`("custom_field_values" ->> ?)::int >= ?`, key, 3),
				).
				All(ctx, db)
			if err != nil {
				t.Fatalf("filtering on %q: %v", key, err)
			}
			if len(rows) != 1 {
				t.Fatalf("filtering on %q returned %d rows, want the one submission that "+
					"carries it", key, len(rows))
			}
			if want := fmt.Sprintf("s%d", i); rows[0].ID != want {
				t.Errorf("filtering on %q returned %s, want %s", key, rows[0].ID, want)
			}
		})
	}

	// And the table is still there, which is the crude version of the same
	// assertion and the one a reader believes without reading the args.
	var n int
	if err := raw.QueryRow(ctx, `SELECT count(*) FROM submissions`).Scan(&n); err != nil {
		t.Fatalf("counting submissions: %v", err)
	}
	if n != len(hostile)+1 {
		t.Errorf("submissions holds %d rows, want %d", n, len(hostile)+1)
	}
}

// oneKeyJSON builds a one-key JSON object through the encoder, so that a key
// containing a quote is escaped by something that knows how rather than by the
// test.
func oneKeyJSON(key string, value int) (string, error) {
	b, err := json.Marshal(map[string]int{key: value})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func createSubjectEvents(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	mustExec(t, db, `
		CREATE TABLE analytics_events (
			id           text PRIMARY KEY,
			org_id       text NOT NULL,
			page_url     text NOT NULL,
			event_type   text NOT NULL,
			user_id      text NOT NULL,
			duration_ms  bigint
		)`)
}

// seedSubjectEvents writes pageviews and clicks for one page of one org, each from a
// distinct user so that the distinct count differs from the total.
func seedSubjectEvents(t *testing.T, db *pgxpool.Pool, org, page string, pageviews, clicks int) {
	t.Helper()
	write := func(kind string, n int) {
		for i := range n {
			id := fmt.Sprintf("%s-%s-%s-%d", org, strings.TrimPrefix(page, "/"), kind, i)
			if _, err := db.Exec(context.Background(),
				`INSERT INTO analytics_events (id, org_id, page_url, event_type, user_id, duration_ms)
				 VALUES ($1, $2, $3, $4, $5, $6)`,
				id, org, page, kind, fmt.Sprintf("u%d", i), 100+i,
			); err != nil {
				t.Fatalf("inserting a %s: %v", kind, err)
			}
		}
	}
	write("pageview", pageviews)
	write("click", clicks)
}

func createSubjectTasks(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	mustExec(t, db, `
		CREATE TABLE tasks (
			id          text PRIMARY KEY,
			org_id      text NOT NULL,
			project_id  text NOT NULL,
			title       text NOT NULL,
			position    text NOT NULL,
			created_at  timestamptz NOT NULL DEFAULT now()
		)`)
}

// seedSubjectTasks writes n tasks whose positions sort in insertion order, which is
// what a list looks like before anybody has dragged anything.
func seedSubjectTasks(t *testing.T, db *pgxpool.Pool, org, project string, n int) {
	t.Helper()
	for i := range n {
		if _, err := db.Exec(context.Background(),
			`INSERT INTO tasks (id, org_id, project_id, title, position, created_at)
			 VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))`,
			fmt.Sprintf("t%02d", i), org, project, fmt.Sprintf("Task %02d", i),
			fmt.Sprintf("b%02d", i), i,
		); err != nil {
			t.Fatalf("inserting task %d: %v", i, err)
		}
	}
}
