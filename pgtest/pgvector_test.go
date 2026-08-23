package pgtest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/schema"
	"github.com/jryannel/sqlb/sqlbtest"
)

// The three physical claims ADR-0026 rests on, asked of pgvector rather than of
// its documentation.
//
// That record says outright that testing them is the first work, *before* the
// DSL, and holds itself at Low confidence until they are measured — because its
// sharpest claims are about how Postgres behaves rather than about what sqlb
// should render, and ADR-0025 is the precedent for a green suite asserting what
// somebody expected. Nothing here exercises sqlb at all. It exists to tell a
// design decision whether the failure it is designed around is real.
//
// All three are. The first is silent, which is what makes it the one the DSL is
// designed around.
//
// # These tests assert a mechanism, not a recall figure
//
// This is the hard-won part of the file and the thing to preserve. The first
// version measured recall over uniform-random vectors and found a filtered
// search returning 6 of 10, and 0 of 10 once the filter was selective. Rewritten
// with a deterministic corpus so the gate would not flake, the same query
// returned 10 of 10 — and an IVFFlat index built on an empty table, which had
// returned nothing, answered in full.
//
// Neither run was wrong. Recall depends on the corpus, on the planner's costing,
// and on HNSW's own build randomness — an index built twice over identical rows
// is not the same graph. So a test asserting "the filtered search returns fewer
// than k" is a coin toss dressed as a gate, and a number quoted from one is a
// number about that corpus and nothing else.
//
// What is stable is the *mechanism*: the index chooses candidates, the filter
// runs over them, and whatever it discards is gone with no error. So the corpus
// below is arranged to force it — the rows the filter admits are placed
// deliberately far from the probe — which demonstrates the failure without
// pretending to measure how often it bites. ADR-0026 needs to know the failure
// is real and invisible. It does not need a recall figure, and this is not a
// place one could honestly be produced.

// vectorEnv names the pgvector-enabled Postgres these tests run against, kept
// separate from SQLB_TEST_POSTGRES for the reason vectorDB gives below: the
// rest of this module asserts against a server as it ships, and that claim is
// only exact if the server really has no extension available to it.
//
// The image is still pinned, in compose.yaml and in the workflow, for the
// reason it always was: a test that measures what an extension does is only
// meaningful if it says which version. It is pinned there rather than here
// because that is now where the server is started.
const vectorEnv = "SQLB_TEST_PGVECTOR"

// vectorDB returns a pool onto a database of its own on the pgvector server,
// with the extension installed.
//
// Separate from the suite's own Postgres rather than shared: the rest of these
// tests assert that sqlb's DDL applies to Postgres *as it ships*, and
// freshStockDB means that literally. Running them against an image carrying an
// extension would weaken a claim that is currently exact.
func vectorDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sqlbtest.Fresh(t, sqlbtest.DSN(t, vectorEnv, "run `mise run pg-up` first"),
		withBootstrap(
			sqlbtest.MaxConns(poolSize),
			// Statement caching off, which is not a detail. Every measurement below
			// is the same SQL under different session settings, and pgx's default
			// mode prepares and caches a plan per connection keyed on the text
			// alone. The first version of this file measured an exact search,
			// cached its plan, and then got that plan back for the "indexed" query
			// — reporting a perfect result for the query the file exists to show
			// failing.
			//
			// Worth carrying into ADR-0026 rather than leaving here: a search
			// operation that sets hnsw.* per statement has this problem too. The
			// GUC changes and the cached plan does not.
			sqlbtest.Configure(func(cfg *pgxpool.Config) {
				cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
			}),
			// The extension ADR-0026 orders ahead of every table. Here it doubles
			// as the assertion that the image is the one this file thinks it is.
			// The tests that exercise sqlb's own DDL drop it again first, so that
			// what runs is what a project's first migration would run.
			sqlbtest.Extensions("vector"),
		)...)
}

// vectorDim is small because nothing here depends on the width. A real embedding
// is 768 or 1,536; the arrangement of the rows is what these tests are about,
// and eight dimensions make the fixture fast and the failures readable.
const vectorDim = 8

// vectorRows is the corpus size. Enough that an HNSW scan stops visiting every
// row, which is the precondition for the whole failure mode.
const vectorRows = 20000

// probeLiteral is the vector every search below looks for neighbours of. Its
// direction is what "near" and "far" are measured against, so the fixture can
// place a row where it needs to without computing anything.
func probeLiteral() string {
	parts := make([]string, vectorDim)
	for i := range parts {
		parts[i] = "1"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// seedForcedCorpus builds the adversarial arrangement: twenty thousand rows
// around the probe's direction, and fifty rows — the only ones the filter admits
// — pointing the opposite way.
//
// This is a fixture, not a simulation. Real embeddings are not arranged like
// this. The point is that *any* neighbour search around the probe draws its
// candidates from the near cluster and none from the far one, whatever graph the
// index build happened to produce — so the filter discards everything and the
// query comes back short, on every machine and every run. It turns a
// probabilistic effect into a demonstration.
func seedForcedCorpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	mustExecPool(t, pool, fmt.Sprintf(`
		CREATE TABLE docs (
			id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant    text NOT NULL,
			embedding vector(%d) NOT NULL
		)`, vectorDim))

	// The near cluster: all components positive, with a small per-row and
	// per-component jitter so the rows are close to the probe and distinct from
	// each other.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO docs (tenant, embedding)
		SELECT 'bulk',
		       (SELECT array_agg(1::float8 + ((i %% 97)::float8 / 10000) + (d::float8 / 100000))
		        FROM generate_series(1, %d) d)::vector
		FROM generate_series(1, %d) i`, vectorDim, vectorRows-50)); err != nil {
		t.Fatalf("seeding the near cluster: %v", err)
	}

	// The far rows: the negated direction, which is the greatest cosine distance
	// from the probe there is.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO docs (tenant, embedding)
		SELECT 'rare',
		       (SELECT array_agg(-1::float8 - ((i %% 97)::float8 / 10000) - (d::float8 / 100000))
		        FROM generate_series(1, %d) d)::vector
		FROM generate_series(1, 50) i`, vectorDim)); err != nil {
		t.Fatalf("seeding the far rows: %v", err)
	}
	mustExecPool(t, pool, `ANALYZE docs`)
}

// TestFilteredANNSearchSilentlyReturnsLessThanItWasAsked is ADR-0026's first
// physical claim, and the one the whole "declared filters" regime exists to
// prevent.
//
// An HNSW scan chooses its candidates first and the WHERE runs over them, so a
// filter the index does not know about throws away part of the answer — here,
// all of it. The query does not fail. It returns fewer rows than it was asked
// for, in the right order, with nothing to say anything is missing. That is a
// difference no test asserting on rows can see, and no user can either.
func TestFilteredANNSearchSilentlyReturnsLessThanItWasAsked(t *testing.T) {
	t.Parallel()
	pool := vectorDB(t)
	seedForcedCorpus(t, pool)
	mustExecPool(t, pool, `CREATE INDEX docs_hnsw ON docs USING hnsw (embedding vector_cosine_ops)`)
	mustExecPool(t, pool, `ANALYZE docs`)
	probe := probeLiteral()

	const want = 10

	// The control: the rows exist, and exact search finds them.
	var matching int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM docs WHERE tenant = 'rare'`).Scan(&matching); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if matching < want {
		t.Fatalf("only %d rows match the filter, so this cannot tell under-return from an empty corner", matching)
	}
	exact := countNeighbours(t, pool, probe, `tenant = 'rare'`, want, "SET enable_indexscan = off")
	if exact != want {
		t.Fatalf("exact search returned %d of %d, so the fixture is wrong before the claim is tested", exact, want)
	}

	// The same question with the index available. The planner's own choice is a
	// separate finding, below; here the question is what the index scan does
	// when it is the plan.
	ann := countNeighbours(t, pool, probe, `tenant = 'rare'`, want, "SET enable_seqscan = off")
	if ann == want {
		t.Fatalf("the filtered ANN search returned all %d rows, so this fixture no longer forces the mechanism "+
			"ADR-0026 is designed around — find out what changed before relaxing the record", want)
	}
	t.Logf("exact search returned %d of %d; the same query through the HNSW index returned %d, and reported no error",
		exact, want, ann)

	// And the plan that explains it: candidates from the index, the filter
	// applied afterwards, the discarded rows counted where only EXPLAIN sees
	// them.
	plan := explainVectorAnalyze(t, pool, fmt.Sprintf(
		`SELECT id FROM docs WHERE tenant = 'rare' ORDER BY embedding <=> '%s'::vector LIMIT %d`, probe, want),
		"SET enable_seqscan = off")
	if !strings.Contains(plan, "docs_hnsw") || !strings.Contains(plan, "Rows Removed by Filter") {
		t.Errorf("the plan no longer shows a filter running over index candidates, which is the mechanism this "+
			"test exists to pin:\n%s", plan)
	}
}

// TestIterativeScanIsADataDependentMitigation measures pgvector 0.8's answer to
// the above, because ADR-0026 cites it as converting under-recall into latency
// and a regime has been proposed on that framing.
//
// Against this corpus it works completely: the search that returned nothing
// returns all ten. Against uniform-random vectors an earlier version of this
// file measured it moving six rows to seven, with EXPLAIN still reporting a
// single index search. Both are one corpus each, which is the point — and it is
// why this test reports rather than asserts.
//
// What follows for the record is the weaker and defensible statement: iterative
// scan is a real mitigation whose benefit is data-dependent. A regime may offer
// it. A regime may not describe it as the thing that makes a filtered search
// correct, because on some data it is and on other data it is not, and the
// difference is not visible from the schema.
func TestIterativeScanIsADataDependentMitigation(t *testing.T) {
	t.Parallel()
	pool := vectorDB(t)
	seedForcedCorpus(t, pool)
	mustExecPool(t, pool, `CREATE INDEX docs_hnsw ON docs USING hnsw (embedding vector_cosine_ops)`)
	mustExecPool(t, pool, `ANALYZE docs`)
	probe := probeLiteral()

	const want = 10
	for _, mode := range []string{"off", "strict_order", "relaxed_order"} {
		got := countNeighbours(t, pool, probe, `tenant = 'rare'`, want,
			"SET enable_seqscan = off",
			"SET hnsw.iterative_scan = "+mode)
		t.Logf("hnsw.iterative_scan = %-13s → %d of %d rows", mode, got, want)
	}
}

// TestThePlannerMayDeclineTheANNIndex is not one of ADR-0026's three claims, and
// is the finding that surprised this file most.
//
// Under a selective filter the planner may cost the ANN index above a sequential
// scan and choose the scan — which returns the exact answer. So the failure
// above is conditional on a planning decision, which is worse rather than
// better: the same query returns complete results on one database and silently
// partial ones on another, according to statistics nobody is watching.
//
// It asserts nothing, because which way the planner goes is exactly what is not
// stable. It reports, so that a reader of the log can see which regime was in
// force when the numbers above were produced.
func TestThePlannerMayDeclineTheANNIndex(t *testing.T) {
	t.Parallel()
	pool := vectorDB(t)
	seedForcedCorpus(t, pool)
	mustExecPool(t, pool, `CREATE INDEX docs_hnsw ON docs USING hnsw (embedding vector_cosine_ops)`)
	mustExecPool(t, pool, `ANALYZE docs`)
	probe := probeLiteral()

	plan := explainVector(t, pool, fmt.Sprintf(
		`SELECT id FROM docs WHERE tenant = 'rare' ORDER BY embedding <=> '%s'::vector LIMIT 10`, probe))
	if strings.Contains(plan, "docs_hnsw") {
		t.Logf("left to itself the planner chose the ANN index, so the silent under-return above is what this "+
			"query does by default:\n%s", plan)
		return
	}
	t.Logf("left to itself the planner declined the ANN index and scanned, so this query is exact here and "+
		"would not be on a database whose statistics differ:\n%s", plan)
}

// TestAMismatchedOpclassFallsBackToASequentialScan is the second physical claim:
// an index built for one metric does not serve a query using another, and
// Postgres does not say so.
//
// This is the mistake ADR-0026 refuses at build time by putting the metric on
// the index declaration. The cost of not refusing it is not a wrong answer — the
// rows are correct — it is a latency graph nobody can explain, which is the kind
// of failure that survives every test a project writes.
//
// Unlike the first claim this one is exact and stable: it is a property of the
// operator classes, not of the data.
func TestAMismatchedOpclassFallsBackToASequentialScan(t *testing.T) {
	t.Parallel()
	pool := vectorDB(t)
	seedForcedCorpus(t, pool)
	mustExecPool(t, pool, `CREATE INDEX docs_hnsw ON docs USING hnsw (embedding vector_cosine_ops)`)
	mustExecPool(t, pool, `ANALYZE docs`)
	probe := probeLiteral()

	// The operator the index was built for.
	plan := explainVector(t, pool, fmt.Sprintf(
		`SELECT id FROM docs ORDER BY embedding <=> '%s'::vector LIMIT 10`, probe))
	if !strings.Contains(plan, "docs_hnsw") {
		t.Fatalf("the cosine operator did not use the cosine index, so the fixture proves nothing:\n%s", plan)
	}

	// The ones it was not. No error, no warning: a sequential scan.
	for _, op := range []string{"<->", "<#>"} {
		plan := explainVector(t, pool, fmt.Sprintf(
			`SELECT id FROM docs ORDER BY embedding %s '%s'::vector LIMIT 10`, op, probe))
		if strings.Contains(plan, "docs_hnsw") {
			t.Errorf("%s used an index built vector_cosine_ops, which would mean the metric is not part of the "+
				"index after all and ADR-0026's build-time refusal is unnecessary:\n%s", op, plan)
			continue
		}
		if !strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s neither used the index nor fell back to a sequential scan:\n%s", op, plan)
		}
	}
}

// TestIVFFlatOnAnEmptyTableWarnsAtBuildTime is the third physical claim, and the
// one the measurement changed most.
//
// The claim is that an IVFFlat index built on an empty table is useless, which
// matters because a Diff-generated CREATE INDEX runs at exactly that moment.
// Measured, the recall consequence is real and data-dependent — one corpus
// returned nothing at all, another answered in full — so this test does not gate
// on it, for the reason the file header gives.
//
// What it does gate on is the part that is exact, and that ADR-0026 does not
// mention: pgvector *says so*, at build time, as a NOTICE. That is a signal a
// migration runner could turn into a refusal, which is a cheaper answer than
// anything in the DSL — and every runner this project knows of discards notices.
func TestIVFFlatOnAnEmptyTableWarnsAtBuildTime(t *testing.T) {
	t.Parallel()
	pool := vectorDB(t)
	ctx := context.Background()
	mustExecPool(t, pool, fmt.Sprintf(`
		CREATE TABLE later (
			id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			embedding vector(%d) NOT NULL
		)`, vectorDim))

	notice := execCapturingNotices(t, pool,
		`CREATE INDEX later_ivf ON later USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100)`)
	if !strings.Contains(strings.ToLower(notice), "little data") {
		t.Errorf("pgvector no longer warns when an ivfflat index is built on an empty table; it said %q. That "+
			"removes the one signal a migration runner could act on, and ADR-0026 should stop counting on it.",
			notice)
	}
	t.Logf("CREATE INDEX on the empty table said: %s", notice)

	// The recall consequence, reported rather than asserted — see above.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO later (embedding)
		SELECT (SELECT array_agg(1::float8 + ((i %% 97)::float8 / 10000) + (d::float8 / 100000))
		        FROM generate_series(1, %d) d)::vector
		FROM generate_series(1, %d) i`, vectorDim, vectorRows)); err != nil {
		t.Fatalf("filling the table after the index: %v", err)
	}
	mustExecPool(t, pool, `ANALYZE later`)

	got := countNeighboursIn(t, pool, "later", probeLiteral(), "", 10, "SET enable_seqscan = off")
	t.Logf("an unfiltered top-10 over %d rows through an IVFFlat index built on the empty table returned %d",
		vectorRows, got)
}

// countNeighbours runs a top-k similarity search over docs and reports how many
// rows came back.
//
// The settings are SET LOCAL inside a transaction, which is load-bearing rather
// than tidy. A plain SET persists on the pooled connection after it goes back,
// so the first version of this file measured an exact search with
// enable_indexscan off, handed the connection back still carrying it, and then
// measured the "indexed" search on a connection that could not use an index —
// reporting a perfect result for the query this file exists to show failing.
func countNeighbours(t *testing.T, pool *pgxpool.Pool, probe, where string, k int, settings ...string) int {
	t.Helper()
	return countNeighboursIn(t, pool, "docs", probe, where, k, settings...)
}

func countNeighboursIn(t *testing.T, pool *pgxpool.Pool, table, probe, where string, k int, settings ...string) int {
	t.Helper()
	ctx := context.Background()
	tx := beginWithSettings(t, pool, settings)
	defer func() { _ = tx.Rollback(ctx) }()

	clause := ""
	if where != "" {
		clause = " WHERE " + where
	}
	var n int
	err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(*) FROM (SELECT id FROM %s%s ORDER BY embedding <=> '%s'::vector LIMIT %d) t`,
		table, clause, probe, k)).Scan(&n)
	if err != nil {
		t.Fatalf("similarity search: %v", err)
	}
	return n
}

// beginWithSettings opens a transaction and applies the settings to it alone.
func beginWithSettings(t *testing.T, pool *pgxpool.Pool, settings []string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, s := range settings {
		local := strings.Replace(s, "SET ", "SET LOCAL ", 1)
		if _, err := tx.Exec(ctx, local); err != nil {
			t.Fatalf("%s: %v", local, err)
		}
	}
	return tx
}

// explainVector returns the plan for a statement, as the planner would choose
// it. The vector literal is stripped out: a probe renders as a wall of digits
// and makes a failure message unreadable.
func explainVector(t *testing.T, pool *pgxpool.Pool, stmt string, settings ...string) string {
	t.Helper()
	return explainWith(t, pool, "EXPLAIN (COSTS OFF) "+stmt, settings...)
}

// explainVectorAnalyze runs the statement, so the plan carries the row counts
// the first claim is about — "Rows Removed by Filter" appears only under
// ANALYZE, and it is the number that shows where the answer went.
func explainVectorAnalyze(t *testing.T, pool *pgxpool.Pool, stmt string, settings ...string) string {
	t.Helper()
	return explainWith(t, pool, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+stmt, settings...)
}

func explainWith(t *testing.T, pool *pgxpool.Pool, stmt string, settings ...string) string {
	t.Helper()
	ctx := context.Background()
	tx := beginWithSettings(t, pool, settings)
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, stmt)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("reading the plan: %v", err)
		}
		if i := strings.Index(line, "'["); i >= 0 {
			line = line[:i] + "'[…]'::vector)"
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	return b.String()
}

// execCapturingNotices runs a statement on a connection of its own with a notice
// handler attached, and returns what Postgres said outside the result.
//
// A connection of its own because a pool hands out whichever it likes and the
// handler is per-connection; pgx has no pool-wide hook a test can read back.
func execCapturingNotices(t *testing.T, pool *pgxpool.Pool, stmt string) string {
	t.Helper()
	ctx := context.Background()

	cfg, err := pgx.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parsing the connection string: %v", err)
	}
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		notices = append(notices, n.Message)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting with a notice handler: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, stmt); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, strings.TrimSpace(stmt))
	}
	return strings.Join(notices, "; ")
}

func mustExecPool(t *testing.T, pool *pgxpool.Pool, stmt string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, strings.TrimSpace(stmt))
	}
}

// --- the column ------------------------------------------------------------
//
// Everything above measures pgvector. What follows measures sqlb's vector
// column against it: the DDL it renders, the codec it registers, the round trip
// through introspect, and a similarity search built from Near.
//
// This is the increment ADR-0026 calls complete on its own — a declared column
// and an exact search, with no index kind, no metric declaration and no REST
// search operation. Those are the second decision, taken when a corpus outgrows
// an exact scan.

type Embedded struct {
	ID    string      `db:"id" sqlb:"pk,default"`
	Body  string      `db:"body"`
	Embed sqlb.Vector `db:"embedding" sqlb:"hidden"`
}

func (Embedded) TableName() string { return "embedded" }

func embeddedSchema() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("embedded",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("body"),
		schema.Vector("embedding", 4),
	)
	return r
}

// codecPool is a pool with the vector codec registered, which is what makes
// values move in binary rather than as text.
func codecPool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(base.Config().ConnString())
	if err != nil {
		t.Fatalf("parsing the connection string: %v", err)
	}
	cfg.AfterConnect = sqlb.RegisterVectorType
	// Statement caching off for the same reason the measurement pool has it
	// off: these tests set session state per connection.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening a pool with the vector codec: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The generated DDL applies, which is the claim a golden test cannot make: the
// extension statement runs first, the dimension reaches the column, and
// Postgres accepts the whole thing.
func TestVectorSchemaAppliesAndRoundTrips(t *testing.T) {
	t.Parallel()
	base := vectorDB(t)
	// The extension is created by the migration rather than by the harness, so
	// what runs here is what a project's first migration would run.
	mustExecPool(t, base, `DROP EXTENSION IF EXISTS vector`)

	target := embeddedSchema()
	applySchema(t, base, target)

	// Reading it back closes the loop ADR-0014 is about: a schema rendered,
	// applied, and imported must diff to nothing. A vector column that
	// introspect could not name would show up here as a proposal to drop the
	// embedding.
	current := importRegistry(t, base)
	changes := diff(t, current, target)
	if len(changes) != 0 {
		var b strings.Builder
		for _, c := range changes {
			fmt.Fprintf(&b, "-- %s\n%s\n", c.Comment, c.Up)
		}
		t.Errorf("the schema does not round-trip; importing it and diffing proposes:\n%s", b.String())
	}

	// And the imported column is Hidden, which the constructor sets and which
	// an adopted database must not lose — otherwise importing a pgvector schema
	// starts serialising embeddings into REST responses.
	imported := current.Get("embedded")
	if imported == nil {
		t.Fatal("the table did not import")
	}
	f := imported.Field("embedding")
	if f == nil {
		t.Fatal("the embedding column did not import")
	}
	d := f.Desc()
	if d.Type != schema.TypeVector || d.Dim != 4 {
		t.Errorf("imported as %s(%d), want vector(4)", d.Type, d.Dim)
	}
	if !d.Hidden {
		t.Error("an imported vector column is not Hidden, so an adopted database would serialise its embeddings")
	}
}

// A vector written through sqlb comes back as the same vector, in binary.
//
// The values are the ones a float codec gets wrong — a component that is not
// exactly representable, a negative, a zero — because "it round-trips" over
// [1,2,3] would pass with a codec that silently went through float64.
func TestVectorValuesRoundTripThroughTheCodec(t *testing.T) {
	t.Parallel()
	base := vectorDB(t)
	mustExecPool(t, base, `DROP EXTENSION IF EXISTS vector`)
	applySchema(t, base, embeddedSchema())

	pool := codecPool(t, base)
	db := sqlb.New(pool)
	ctx := context.Background()

	want := sqlb.Vector{0.1, -2.5, 0, 3.4028235e38}
	row := &Embedded{Body: "first", Embed: want}
	if _, err := sqlb.InsertRows(row).One(ctx, db); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := sqlb.Query[Embedded]().One(ctx, db)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !reflect.DeepEqual(got.Embed, want) {
		t.Errorf("embedding = %v, want %v", got.Embed, want)
	}

	// The insert wrote the value back into the caller's struct, as every insert
	// does — so the codec ran in both directions on the same statement.
	if !reflect.DeepEqual(row.Embed, want) {
		t.Errorf("the returned embedding = %v, want %v", row.Embed, want)
	}
}

// Near produces a search Postgres runs, and the rows come back in the order the
// scores say they are in.
//
// The ordering assertion is the point. A score computed one way and an ordering
// computed another is the failure the handle exists to make impossible, and it
// is invisible in any test that checks only which rows came back.
func TestNearSearchesAgainstARealDatabase(t *testing.T) {
	t.Parallel()
	base := vectorDB(t)
	mustExecPool(t, base, `DROP EXTENSION IF EXISTS vector`)
	applySchema(t, base, embeddedSchema())

	pool := codecPool(t, base)
	db := sqlb.New(pool)
	ctx := context.Background()

	// Three rows at known angles from the probe: identical, orthogonal, and
	// opposite. Cosine similarity is then 1, 0 and -1, which is arithmetic
	// rather than a measurement.
	rows := []*Embedded{
		{Body: "same", Embed: sqlb.Vector{1, 0, 0, 0}},
		{Body: "orthogonal", Embed: sqlb.Vector{0, 1, 0, 0}},
		{Body: "opposite", Embed: sqlb.Vector{-1, 0, 0, 0}},
	}
	if _, err := sqlb.InsertRows(rows...).Exec(ctx, db); err != nil {
		t.Fatalf("insert: %v", err)
	}

	type hit struct {
		Body       string  `db:"body"`
		Similarity float64 `db:"similarity"`
	}
	near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector{1, 0, 0, 0})
	hits, err := sqlb.Collect[hit](ctx, db, sqlb.Query[Embedded]().
		Select(sqlb.F("body"), near.Similarity()).
		OrderBy(near.Nearest()))
	if err != nil {
		t.Fatalf("similarity search: %v", err)
	}

	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3: %+v", len(hits), hits)
	}
	if hits[0].Body != "same" || hits[len(hits)-1].Body != "opposite" {
		t.Errorf("hits are not ordered nearest first: %+v", hits)
	}
	// Larger is closer, which is what "similarity, not distance" means.
	for i := 1; i < len(hits); i++ {
		if hits[i].Similarity > hits[i-1].Similarity {
			t.Errorf("hit %d scores above the one before it, so the ordering and the score disagree: %+v", i, hits)
		}
	}
	if d := hits[0].Similarity - 1; d > 1e-6 || d < -1e-6 {
		t.Errorf("an identical vector scored %v, want 1", hits[0].Similarity)
	}

	// The threshold uses the same score, so it agrees with the ordering by
	// construction rather than by the caller keeping two expressions in step.
	above, err := sqlb.Collect[hit](ctx, db, sqlb.Query[Embedded]().
		Select(sqlb.F("body"), near.Similarity()).
		Where(near.AtLeast(0.5)).
		OrderBy(near.Nearest()))
	if err != nil {
		t.Fatalf("thresholded search: %v", err)
	}
	if len(above) != 1 || above[0].Body != "same" {
		t.Errorf("AtLeast(0.5) returned %+v, want only the identical row", above)
	}
}

// The column refuses a vector of the wrong width, and the refusal is Postgres's
// rather than something sqlb had to check.
//
// Worth pinning because it is the guarantee the declaration buys: the dimension
// is part of the type, so a model swap that changes the width fails loudly on
// the first write instead of storing vectors that mean nothing.
func TestAWrongWidthVectorIsRefused(t *testing.T) {
	t.Parallel()
	base := vectorDB(t)
	mustExecPool(t, base, `DROP EXTENSION IF EXISTS vector`)
	applySchema(t, base, embeddedSchema())

	db := sqlb.New(codecPool(t, base))
	_, err := sqlb.InsertRows(&Embedded{Body: "wrong", Embed: sqlb.Vector{1, 2}}).One(context.Background(), db)
	if err == nil {
		t.Fatal("a two-component vector was accepted into a vector(4) column")
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("the refusal does not mention the dimension: %v", err)
	}
}
