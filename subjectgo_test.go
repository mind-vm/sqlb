package sqlb_test

// The shapes docs/special-cases-subject-go.md counted, pinned at the level
// where the answer is decided: what sqlb writes.
//
// Half of these pass because a capability exists and nothing demonstrated it —
// `ForUpdate` is the clearest case, present since builder.go was written and
// walked by no test. The other half pass because `Raw` is the answer today, and
// those are the interesting ones: a gap that lives in a document is a gap
// somebody has to remember, and a gap that lives in a test is one that breaks
// the build the day it closes. Each of those tests says which combinator would
// replace it, so closing the gap means deleting a named thing rather than
// discovering why a golden string moved.
//
// Nothing here runs against a database. pgtest/subjectgo_test.go is the other half:
// the same shapes, judged by Postgres, including the two claims — the bind
// parameter ceiling and the cursor held across a reorder — that only a real
// server can settle.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
)

// subjectEvent is the corpus's analytics_events, trimmed to the columns one
// dashboard row reads.
type subjectEvent struct {
	ID        string    `db:"id" sqlb:"pk,default"`
	OrgID     string    `db:"org_id" sqlb:"filter"`
	PageURL   string    `db:"page_url" sqlb:"filter,sort"`
	EventType string    `db:"event_type" sqlb:"filter"`
	UserID    string    `db:"user_id" sqlb:"filter"`
	Duration  *int64    `db:"duration_ms" sqlb:"filter"`
	CreatedAt time.Time `db:"created_at" sqlb:"sort,readonly,default"`
}

func (subjectEvent) TableName() string { return "analytics_events" }

// subjectTask is the corpus's tasks table as the reorder path sees it: a
// sort key that is nullable, and a creation time to break its ties.
type subjectTask struct {
	ID        string    `db:"id" sqlb:"pk,default"`
	OrgID     string    `db:"org_id" sqlb:"filter"`
	ProjectID string    `db:"project_id" sqlb:"filter"`
	Position  *string   `db:"position" sqlb:"sort"`
	CreatedAt time.Time `db:"created_at" sqlb:"sort,readonly,default"`
}

func (subjectTask) TableName() string { return "tasks" }

// subjectSubmission is the forms shape: the field definitions live on the
// parent, the values live in a jsonb object here, and neither set of keys is
// known when this struct is written.
type subjectSubmission struct {
	ID     string `db:"id" sqlb:"pk,default"`
	OrgID  string `db:"org_id" sqlb:"filter"`
	FormID string `db:"form_id" sqlb:"filter"`
	Values string `db:"custom_field_values"`
}

func (subjectSubmission) TableName() string { return "submissions" }

// subjectChunk has ten columns and no defaults, which is the corpus's own chunk
// table and the arithmetic behind the bind parameter ceiling below.
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

// A dashboard row is several differently-filtered counts over one scan, and
// sqlb has no combinator for the filter.
//
// `COUNT(*) FILTER (WHERE …)` is 15 occurrences in the second corpus and the
// compiler already renders one — expand.go caps a collection with it — so what
// is missing is public surface, not machinery: `Selection.Filter(Pred)`, which
// would let this whole projection be written with Count(), CountDistinct() and
// a predicate built the same way the WHERE clause is.
//
// Until it exists the aggregate is verbatim SQL, which costs three things this
// test makes visible: the predicate is a string rather than a Pred, so no hook
// and no scope can reach it; the column names are interpolated by hand rather
// than validated against the model; and the alias is inside the fragment.
func TestAFilteredCountIsOnlyReachableThroughRaw(t *testing.T) {
	q := sqlb.Query[subjectEvent]().
		Select(
			sqlb.F("page_url"),
			sqlb.Count().As("total"),
			sqlb.CountDistinct(sqlb.F("user_id")).As("unique_users"),
			sqlb.RawSel(`count(*) FILTER (WHERE "event_type" = ?)`, "pageview").As("pageviews"),
			sqlb.RawSel(`count(*) FILTER (WHERE "event_type" = ?)`, "click").As("clicks"),
		).
		Where(sqlb.F("org_id").Eq("acme")).
		GroupBy(sqlb.F("page_url")).
		OrderBy(sqlb.OrderByDesc(sqlb.Raw{SQL: "total"})).
		Limit(20)

	got, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "page_url", count(*) AS "total", count(DISTINCT "user_id") AS "unique_users", ` +
		`count(*) FILTER (WHERE "event_type" = $1) AS "pageviews", ` +
		`count(*) FILTER (WHERE "event_type" = $2) AS "clicks" ` +
		`FROM "analytics_events" WHERE "org_id" = $3 GROUP BY "page_url" ORDER BY total DESC LIMIT 20`
	if got != want {
		t.Errorf("dashboard row\n got: %s\nwant: %s", got, want)
	}
	// The point of the fragment is the predicate, and the values inside it are
	// still bound. A Raw that interpolated them would be the same query and a
	// different security posture, so this is worth an assertion of its own.
	if len(args) != 3 || args[0] != "pageview" || args[1] != "click" || args[2] != "acme" {
		t.Errorf("args = %#v, want the two event types bound before the tenant", args)
	}
	if strings.Contains(got, "'pageview'") {
		t.Error("the event type reached the SQL text")
	}
}

// The empty-set trap and the filtered aggregate compose, and the composition
// is the reason `Filter` belongs on Selection rather than on Count.
//
// COUNT over no rows is 0, so a filtered count needs nothing. SUM, AVG, MIN and
// MAX over no rows are NULL — which is the shape the exchange report hit — and
// a filtered SUM is exactly a SUM over no rows whenever the filter matches
// nothing, so a dashboard makes the trap *more* likely rather than less. The
// fix that exists is Coalesce, and it takes an Expr, which is why the fragment
// has to be unwrapped with .Expr() here and would not have to be if the filter
// were a combinator.
func TestAFilteredSumOverAnEmptyRangeStillNeedsCoalesce(t *testing.T) {
	filtered := sqlb.RawSel(`sum("duration_ms") FILTER (WHERE "event_type" = ?)`, "pageview")

	q := sqlb.Query[subjectEvent]().
		Select(sqlb.Coalesce(filtered.Expr(), sqlb.Raw{SQL: "0"}).As("dwell_ms")).
		Where(sqlb.F("org_id").Eq("acme"))

	got, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT coalesce(sum("duration_ms") FILTER (WHERE "event_type" = $1), 0) AS "dwell_ms" ` +
		`FROM "analytics_events" WHERE "org_id" = $2`
	if got != want {
		t.Errorf("coalesced aggregate\n got: %s\nwant: %s", got, want)
	}
	if len(args) != 2 {
		t.Errorf("args = %#v, want the filter value and the tenant", args)
	}
}

// `DISTINCT ON` is reachable, and only because the projection is written
// verbatim before the compiler has an opinion about it.
//
// One occurrence per corpus, both "first row per group". Distinct() renders the
// standard `SELECT DISTINCT`, which is a different operator: it de-duplicates
// whole rows and cannot say which row of a group wins. Postgres's form is a
// modifier on the select list, so a RawSel in first position lands in the right
// place — and the ORDER BY it requires has to agree with it, which nothing
// checks. That is the whole gap: it works, and it works by coincidence of where
// the fragment is written.
func TestDistinctOnIsReachableOnlyByWritingTheProjectionVerbatim(t *testing.T) {
	q := sqlb.Query[subjectEvent]().
		Select(
			sqlb.RawSel(`DISTINCT ON ("page_url") "page_url"`),
			sqlb.F("event_type"),
			sqlb.F("created_at"),
		).
		Where(sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("page_url").Asc(), sqlb.F("created_at").Desc())

	got, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT DISTINCT ON ("page_url") "page_url", "event_type", "created_at" ` +
		`FROM "analytics_events" WHERE "org_id" = $1 ` +
		`ORDER BY "page_url" ASC, "created_at" DESC`
	if got != want {
		t.Errorf("distinct on\n got: %s\nwant: %s", got, want)
	}

	// And the other half, so the two are not confused: Distinct() is the
	// standard operator and says nothing about which row of a group survives.
	plain, _, err := sqlb.Query[subjectEvent]().Distinct().Select(sqlb.F("page_url")).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if plain != `SELECT DISTINCT "page_url" FROM "analytics_events"` {
		t.Errorf("Distinct() renders %s", plain)
	}
}

// A patch-semantics upsert — keep what the caller did not send — has no
// spelling, and it is the same gap as the arithmetic upsert wearing a
// different disguise.
//
// 6 of the second corpus's 18 upserts are
// `SET col = COALESCE(EXCLUDED.col, table.col)`. OnConflictUpdate copies
// `EXCLUDED.<col>` and nothing else, so a NULL in the proposed row overwrites a
// value that was there. The missing surface is an expression per update
// column — the same one `SET n = t.n + EXCLUDED.n` needs — and this test exists
// to hold the current rendering still while that is decided.
func TestAnUpsertCopiesTheProposedRowAndCannotDoAnythingElseToIt(t *testing.T) {
	row := &subjectChunk{ID: "c1", OrgID: "acme", DocID: "d1", Ordinal: 1,
		Content: "hello", Tokens: 2, Hash: "h", Source: "upload", Lang: "en", Revision: 1}

	got, _, err := sqlb.InsertRows(row).
		OnConflictUpdate([]string{"document_id", "ordinal"}, "content", "tokens", "content_hash").
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const set = `ON CONFLICT ("document_id", "ordinal") DO UPDATE SET ` +
		`"content" = EXCLUDED."content", "tokens" = EXCLUDED."tokens", "content_hash" = EXCLUDED."content_hash"`
	if !strings.Contains(got, set) {
		t.Errorf("upsert\n got: %s\nwant it to contain: %s", got, set)
	}
	// The two forms the corpus writes and this cannot: patch semantics, and the
	// counter. Neither is a missing operator — both are the same missing thing,
	// which is an expression on the right of the assignment.
	if strings.Contains(got, "coalesce") {
		t.Error("OnConflictUpdate has grown expression support; this test should become its assertion")
	}
}

// InsertRows renders one VALUES list with one bind per column per row, and the
// ceiling on that is the wire protocol's, exactly.
//
// A Postgres bind message carries an int16 parameter count, so a statement
// holds at most 65,535 of them. At ten columns that is 6,553 rows. Every test
// anyone writes is below it and the first large document in production is above
// it.
//
// This test used to pin the *absence* of the check, and said what closing it
// would take: refusing here, naming the row count, the column count and the
// maximum, is the ADR-0011 answer, while batching inside a transaction is the
// larger option and changes atomicity so it cannot be done quietly. That is
// what was built, so the test is now its assertion rather than its description.
// Both directions are asserted, because a refusal that also refuses the largest
// working batch would be a regression wearing a check's clothes (ADR-0016).
func TestBulkInsertIsRefusedInTermsOfRowsAtTheBindParameterCeiling(t *testing.T) {
	const (
		maxParams = 65535
		columns   = 10
		maxRows   = maxParams / columns // 6553
	)

	if got := len(sqlb.ModelOf[subjectChunk]().Columns); got != columns {
		t.Fatalf("subjectChunk has %d columns, want %d — the arithmetic below is about the shape", got, columns)
	}

	rows := func(n int) []*subjectChunk {
		out := make([]*subjectChunk, n)
		for i := range out {
			out[i] = &subjectChunk{ID: "c", OrgID: "acme", DocID: "d", Ordinal: int64(i),
				Content: "chunk", Tokens: 1, Hash: "h", Source: "upload", Lang: "en", Revision: 1}
		}
		return out
	}

	// The last statement the protocol carries. 65,530 parameters.
	_, args, err := sqlb.InsertRows(rows(maxRows)...).SQL()
	if err != nil {
		t.Fatalf("the largest batch that fits was refused: %v", err)
	}
	if got, want := len(args), maxRows*columns; got != want {
		t.Fatalf("bound %d parameters for %d rows, want %d", got, maxRows, want)
	}

	// One row more. 65,540 parameters, and the statement never leaves sqlb.
	_, _, err = sqlb.InsertRows(rows(maxRows + 1)...).SQL()
	if err == nil {
		t.Fatal("a batch of 65,540 parameters compiled; the ceiling is no longer checked")
	}
	// The units are the whole point. pgx's own message counts parameters, which
	// is not what anybody wrote — a caller who inserted 6,554 rows has to divide
	// to find out what happened.
	for _, want := range []string{"6554 rows", "10 columns", "65535", "insert at most 6553 rows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q, so it does not name what the caller did: %v", want, err)
		}
	}
}

// A drag-and-drop reorder assigns different values to many rows in one
// statement, and Update has no values-list form.
//
// Set and SetExpr write one value to every matched row, which is the correct
// shape for "archive these" and the wrong one for "here is each row's new
// position". The corpus writes the difference as a join against two unnested
// arrays, and every piece of that is present except the statement: the arrays
// are two ordinary bind parameters, and the lock the write runs under is the
// ForUpdate the next test walks. `sqlb.UpdateFrom[T]` would be the write-side
// twin of the multi-row insert that already exists.
//
// So this test pins two things: what Update does write, and that the arrays the
// hand-written statement leans on need no help to be bound.
func TestABulkRepositionIsOneValuePerStatementOrItIsHandWritten(t *testing.T) {
	// What the builder can say: one position for every matching row.
	got, args, err := sqlb.UpdateRows[subjectTask]().
		Set("position", "a0").
		Where(sqlb.F("project_id").Eq("p1"), sqlb.F("org_id").Eq("acme")).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `UPDATE "tasks" SET "position" = $1 WHERE ("project_id" = $2) AND ("org_id" = $3) ` +
		`RETURNING "id", "org_id", "project_id", "position", "created_at"`
	if got != want {
		t.Errorf("scalar update\n got: %s\nwant: %s", got, want)
	}
	if len(args) != 3 {
		t.Errorf("args = %#v", args)
	}

	// What a reorder needs, and where it has to be written instead. The two
	// arrays are one bind each, so the statement stays one round trip whether
	// it moves three rows or three thousand — which is the entire argument,
	// since the transaction holding this write is also holding a FOR UPDATE
	// over every sibling.
	//
	// They are bound as the slices themselves. Under database/sql this needed
	// sqlb.EncodeArray, a public function whose whole job was rendering
	// `{t3,t1,t2}` because the driver had no other spelling; pgx encodes a
	// slice as an array, so the function is gone and nothing replaced it
	// (ADR-0040).
	ids := []string{"t3", "t1", "t2"}
	positions := []string{"a0", "a1", "a2"}
	_, args, err = sqlb.Query[subjectTask]().
		Select(sqlb.F("id")).
		Where(sqlb.RawPred(`"id" = ANY(?) AND "position" = ANY(?)`, ids, positions)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 2 {
		t.Fatalf("bound %d parameters for two arrays, want 2: %#v", len(args), args)
	}
	if got, ok := args[0].([]string); !ok || len(got) != 3 {
		t.Errorf("the id array reached the driver as %#v, want the []string itself", args[0])
	}
	if got, ok := args[1].([]string); !ok || len(got) != 3 {
		t.Errorf("the position array reached the driver as %#v, want the []string itself", args[1])
	}
}

// `FOR UPDATE` has been in the builder since it was written and no test walks
// it. Both of the second corpus's two uses guard a reorder, which is this
// statement: read the siblings in their current order, hold them, write the new
// order, commit.
//
// The ordering is the one the corpus writes, and it is not decoration. A
// position column that is nullable sorts NULLs last under ASC, which puts every
// unpositioned row at the end of the list a user is about to drag inside;
// NULLS FIRST is what makes the normalisation pass — assign a key to everything
// that has none — read in the same order the user sees.
func TestAReorderHoldsItsSiblingsWithForUpdate(t *testing.T) {
	got, args, err := sqlb.Query[subjectTask]().
		Select(sqlb.F("id"), sqlb.F("position")).
		Where(sqlb.F("project_id").Eq("p1"), sqlb.F("org_id").Eq("acme")).
		OrderBy(sqlb.F("position").Asc().NullsFirst(), sqlb.F("created_at").Desc()).
		ForUpdate().
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "id", "position" FROM "tasks" WHERE ("project_id" = $1) AND ("org_id" = $2) ` +
		`ORDER BY "position" ASC NULLS FIRST, "created_at" DESC FOR UPDATE`
	if got != want {
		t.Errorf("reorder select\n got: %s\nwant: %s", got, want)
	}
	if len(args) != 2 {
		t.Errorf("args = %#v", args)
	}
}

// The forms shape: a filter into a jsonb document, where the key is data.
//
// The first corpus found 96 jsonb-declaring lines and zero filters into them,
// which read as permission not to build `->>` and `@>`. The second corpus
// filters into them about twenty times, because the customer defines the
// fields — so the set of legal keys is a query result, different per tenant,
// changing while the server runs.
//
// Today the accessor is RawPred and the type is a Cast over a Raw. The property
// that must survive whatever replaces them is the one this test asserts: the
// key is a *bind parameter*. `->> $1` is legal Postgres and a key is a value,
// so there is no reason for it ever to be interpolated — and a fuzz corpus of
// keys is the check filter/fuzz_test.go already runs for the column grammar.
//
// The `??` is not a typo. Postgres spells "does this key exist" as the `?`
// operator, which is also sqlb's placeholder, so the compiler takes a doubled
// one as an escape. That collision is the one piece of this shape that no
// document mentions and nothing else tests, and it is exactly what a
// hand-written custom-field filter would trip over first.
func TestAJSONKeyIsBoundAndNeverInterpolated(t *testing.T) {
	hostile := []string{
		"severity",
		"a'b",
		`a"b`,
		"a--b",
		"key->>'other'",
		"'; DROP TABLE submissions; --",
		"",
	}

	for _, key := range hostile {
		t.Run(key, func(t *testing.T) {
			q := sqlb.Query[subjectSubmission]().
				Select(sqlb.F("id")).
				Where(
					sqlb.F("org_id").Eq("acme"),
					// The key on both sides: once to test the field is
					// present, once to read it.
					sqlb.RawPred(`"custom_field_values" ?? ?`, key),
					sqlb.RawPred(`("custom_field_values" ->> ?)::int >= ?`, key, 3),
				)

			got, args, err := q.SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if key != "" && strings.Contains(got, key) {
				t.Fatalf("the key reached the SQL text: %s", got)
			}
			if !strings.Contains(got, `"custom_field_values" ? $2`) {
				t.Errorf("the doubled placeholder did not survive as the key-exists operator: %s", got)
			}
			if len(args) != 4 {
				t.Fatalf("args = %#v, want the tenant, the key twice and the bound minimum", args)
			}
			if args[1] != key || args[2] != key {
				t.Errorf("args = %#v, want the key carried through unchanged", args)
			}
		})
	}
}

// And the failure the escape exists to prevent, pinned so that removing it is
// not silent: a single `?` is a placeholder, so the natural spelling of the
// key-exists operator is an argument-count error rather than a query.
//
// It fails loudly, which is the right outcome and not an obvious one — the
// alternative, splicing the key into the text to avoid the collision, is how
// this becomes an injection.
func TestASingleQuestionMarkIsAPlaceholderAndNotTheJSONOperator(t *testing.T) {
	_, _, err := sqlb.Query[subjectSubmission]().
		Where(sqlb.RawPred(`"custom_field_values" ? ?`, "severity")).
		SQL()
	if err == nil {
		t.Fatal("expected an argument-count error, got a compiled statement")
	}
	if !strings.Contains(err.Error(), "placeholders") {
		t.Errorf("error = %v, want it to name the placeholder mismatch", err)
	}
}

// The typed half of the same shape, which is what makes the analytics queries
// expressible: a JSON path accessor with a declared type.
//
// `C.Values.Key("severity").AsInt()` would render this. Cast takes any Expr, so
// the rendering already composes — what is missing is a Field that knows it is
// a JSON path, which is also what would let the REST grammar validate the key
// against a set the application computed.
func TestATypedJSONAccessorComposesOutOfCastAndRaw(t *testing.T) {
	severity := sqlb.Cast{
		Inner: sqlb.Raw{SQL: `"custom_field_values" ->> ?`, Args: []any{"severity"}},
		Type:  "int",
	}

	got, args, err := sqlb.Query[subjectSubmission]().
		Select(sqlb.Sel(severity).As("severity"), sqlb.Count().As("n")).
		Where(sqlb.F("org_id").Eq("acme")).
		GroupByExpr(severity).
		OrderBy(sqlb.OrderByDesc(sqlb.Raw{SQL: "n"})).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT ("custom_field_values" ->> $1)::int AS "severity", count(*) AS "n" ` +
		`FROM "submissions" WHERE "org_id" = $2 ` +
		`GROUP BY ("custom_field_values" ->> $3)::int ORDER BY n DESC`
	if got != want {
		t.Errorf("typed accessor\n got: %s\nwant: %s", got, want)
	}
	if len(args) != 3 || args[0] != "severity" || args[2] != "severity" {
		t.Errorf("args = %#v, want the key bound in the projection and again in the grouping", args)
	}
}

// The 108 optional-predicate guards, and the reason the number is worth
// quoting: the guard is not the cost.
//
// `(sqlc.narg('x')::uuid IS NULL OR col = x)` appears 108 times in 4,378 lines
// of the second corpus's SQL. Twelve sit in a List/Count pair where both copies
// must be edited together, and the application carries an architecture test
// whose entire reason to exist is that three such pairs had already drifted.
//
// A query that is a value cannot drift, because there is one of it. This test
// is that claim reduced to its smallest form: build the filters once, ask the
// same builder for rows and for a count, and require the two WHERE clauses to
// be the same text.
func TestAListAndItsCountCannotDrift(t *testing.T) {
	// Two harnesses because they answer different shapes: the list scans rows
	// of the model, the count scans one number. What is shared is the builder,
	// which is the whole point.
	list := newHarness(t, []string{"id", "org_id", "page_url", "event_type", "user_id", "duration_ms", "created_at"}, nil)
	defer list.close()
	counter := newHarness(t, []string{"count"}, [][]any{{int64(0)}})
	defer counter.close()

	// Every one of these is optional, and every one of them is one line. If is
	// the zero-Pred idiom: a false condition contributes nothing at all rather
	// than a predicate that is always true.
	params := struct {
		pageURL   string
		eventType string
		userID    string
		since     time.Time
	}{pageURL: "/pricing", userID: "u1"}

	q := sqlb.Query[subjectEvent]().Where(
		sqlb.F("org_id").Eq("acme"),
		sqlb.If(params.pageURL != "", sqlb.F("page_url").Eq(params.pageURL)),
		sqlb.If(params.eventType != "", sqlb.F("event_type").Eq(params.eventType)),
		sqlb.If(params.userID != "", sqlb.F("user_id").Eq(params.userID)),
		sqlb.If(!params.since.IsZero(), sqlb.F("created_at").Gte(params.since)),
	)

	ctx := context.Background()
	if _, err := q.OrderBy(sqlb.F("created_at").Desc()).Limit(20).All(ctx, list.db); err != nil {
		t.Fatalf("All: %v", err)
	}
	listSQL := list.lastQuery()
	if _, err := q.Count(ctx, counter.db); err != nil {
		t.Fatalf("Count: %v", err)
	}
	count := counter.lastQuery()

	// The two unset filters contributed nothing, which is the half of the
	// idiom that a hand-written guard cannot do: there is no `$3 IS NULL OR`
	// left in the plan for Postgres to work around.
	const where = ` WHERE (("org_id" = $1) AND ("page_url" = $2)) AND ("user_id" = $3)`
	if !strings.HasSuffix(count, where) {
		t.Fatalf("count\n got: %s\nwant it to end with: %s", count, where)
	}
	if !strings.Contains(listSQL, where+" ORDER BY") {
		t.Fatalf("list\n got: %s\nwant it to contain: %s", listSQL, where)
	}
	if strings.Contains(listSQL, "IS NULL OR") {
		t.Error("an unset filter left a guard in the statement")
	}
}
