package pgtest

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/schema"
)

// Containment against a real Postgres.
//
// The filter package's own tests compile `?metadata=hasdoc.{"lang":"de"}` to
// `"metadata" @> $1::jsonb` and compare it against a string somebody wrote.
// What that cannot check is what Postgres does with it:
//
//   - `@>` is subset containment, not equality. A row whose metadata carries
//     more keys than the filter named must still match, and that is the whole
//     reason a document column is filterable without declaring its keys.
//   - a jsonb column has to come back as a document, not as whatever the driver
//     decided a jsonb column was.
//   - the operator has to be one the declared GIN index can serve.
//
// The last is the one worth having, and the only one here that has been made to
// fail on purpose in the sense ADR-0016 asks for. Swapping `@>` for `=` leaves
// every result-based assertion in this file green — the rows are still right —
// and turns the plan into a Seq Scan. Results cannot tell a correct answer from
// a correct answer that scanned the table; only the plan can.
//
// The `::jsonb` cast is deliberately *not* claimed to be load bearing. It was
// removed to check, and the query still ran: pgx sends the parameter with an
// unspecified type and Postgres infers jsonb from the operator. The cast stays
// because it says what the statement means and costs nothing, not because
// anything here would catch its absence.

// JSONDoc is the model for a table with a document column.
type JSONDoc struct {
	ID       string          `db:"id" sqlb:"pk,default"`
	Title    string          `db:"title" sqlb:"sort"`
	Metadata json.RawMessage `db:"metadata" sqlb:"filter"`
}

func (JSONDoc) TableName() string { return "jsondocs" }

// jsonDocsRegistry declares the table JSONDoc maps to, with the GIN index that makes
// containment an index scan rather than a table scan.
func jsonDocsRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("jsondocs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
		schema.JSON("metadata").Filterable(),
	).AddIndex(schema.Index{Columns: []string{"metadata"}, Method: "gin"})
	return r
}

// seedJSONDocs inserts three rows whose metadata overlaps deliberately: one exact
// match for the filter below, one superset of it, and one that shares the key
// but not the value.
func seedJSONDocs(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	for _, row := range []struct{ title, metadata string }{
		{"exact", `{"lang":"de"}`},
		{"superset", `{"lang":"de","tier":"pro","tags":["urgent"]}`},
		{"same key, other value", `{"lang":"fr"}`},
	} {
		if _, err := db.Exec(context.Background(),
			`INSERT INTO jsondocs (title, metadata) VALUES ($1, $2::jsonb)`, row.title, row.metadata,
		); err != nil {
			t.Fatalf("inserting %q: %v", row.title, err)
		}
	}
}

// jsonDocsQuery parses a query string against the JSONDoc model and applies it.
func jsonDocsQuery(t *testing.T, query string) *sqlb.Builder[JSONDoc] {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("bad test query %q: %v", query, err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[JSONDoc]()})
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	return filter.Apply(sqlb.Query[JSONDoc]().OrderBy(sqlb.F("title").Asc()), q)
}

func TestJSONContainmentRunsAgainstPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, jsonDocsRegistry())
	seedJSONDocs(t, raw)

	docs, err := jsonDocsQuery(t, `metadata=hasdoc.{"lang":"de"}`).All(ctx, sqlb.New(raw))
	if err != nil {
		t.Fatalf("containment filter did not run: %v", err)
	}

	got := make([]string, len(docs))
	for i, d := range docs {
		got[i] = d.Title
	}
	want := []string{"exact", "superset"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("containment matched %v, want %v — a superset must match, and a different value must not", got, want)
	}
}

// A document column has to survive the round trip as a document: the filter is
// only useful if what comes back can be read as JSON rather than as whatever
// the driver decided a jsonb column was.
func TestJSONColumnScansAsADocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, jsonDocsRegistry())
	seedJSONDocs(t, raw)

	docs, err := jsonDocsQuery(t, `metadata=hasdoc.{"tier":"pro"}`).All(ctx, sqlb.New(raw))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d rows, want 1", len(docs))
	}

	var decoded map[string]any
	if err := json.Unmarshal(docs[0].Metadata, &decoded); err != nil {
		t.Fatalf("the metadata column did not scan as JSON: %v (raw: %q)", err, docs[0].Metadata)
	}
	if decoded["tier"] != "pro" {
		t.Errorf("decoded metadata = %v, want tier=pro", decoded)
	}
}

// The claim this test exists for: `@>` is servable by the GIN index the schema
// declared. An operator outside the index's operator class answers the query
// correctly by scanning the table, so results cannot distinguish the two — only
// the plan can.
//
// enable_seqscan is turned off because three rows are far too few for the
// planner to prefer an index on cost. That makes this a test of whether the
// index *can* serve the operator, which is the part that would break, rather
// than of what the planner picks at a size this test does not have.
func TestJSONContainmentCanUseTheGINIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, jsonDocsRegistry())
	seedJSONDocs(t, raw)

	sqlText, args, err := jsonDocsQuery(t, `metadata=hasdoc.{"lang":"de"}`).SQL()
	if err != nil {
		t.Fatalf("SQL(): %v", err)
	}

	// One connection for both statements. SET is per-session, and a pool is
	// free to answer the EXPLAIN on a different connection than it answered
	// the SET on — which would leave seqscan enabled and quietly turn this
	// into the table scan the test exists to rule out.
	conn, err := raw.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("disabling seqscan: %v", err)
	}

	rows, err := conn.Query(ctx, "EXPLAIN "+sqlText, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning the plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}

	if !strings.Contains(plan.String(), "jsondocs_metadata_idx") {
		t.Errorf("containment did not reach the GIN index, so it is a table scan wearing the right answer:\n%s", plan.String())
	}
}

// NullableJSONDoc is the model for a table whose document column may be NULL.
// The pointer is what codegen emits, and TestNullableJSONModelMatchesCodegen
// below is what keeps that claim from drifting: this struct is hand-written, so
// on its own it would keep passing through a codegen regression.
type NullableJSONDoc struct {
	ID       string           `db:"id" sqlb:"pk,default"`
	Title    string           `db:"title" sqlb:"sort"`
	Metadata *json.RawMessage `db:"metadata" sqlb:"filter"`
}

func (NullableJSONDoc) TableName() string { return "nullable_jsondocs" }

func nullableJSONDocsRegistry() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("nullable_jsondocs",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
		schema.JSON("metadata").Nullable().Filterable(),
	)
	return r
}

// A nullable jsonb column has to read back when the column is actually NULL.
//
// Nothing else in the suite scanned a NULL document: the round-trip test
// round-trips DDL rather than values, and every other jsonb test writes the
// column on every row. A schema that defaults such a column to '[]' and always
// writes it never produces the row this test needs, which is how the gap
// survived — the first NULL tends to arrive from a fixture, a backfill, or a
// migration that adds the column to rows that already exist.
//
// Note what this does and does not pin down. It passes with a bare
// json.RawMessage too: pgx scans a NULL document into one as nil, and sqlb
// takes pgx as a dependency (ADR-0040), so the pointer is not what makes this
// work. Under the database/sql executor that preceded pgx it *was* — a named
// type over []byte matches no case in convertAssign, so the read failed with
// "unsupported Scan, storing driver.Value type <nil>". This test is therefore
// coverage of a NULL document, not a regression test for the pointer;
// TestNullableJSONModelMatchesCodegen below is the one that fails without it.
func TestNullableJSONScansWhenTheColumnIsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, nullableJSONDocsRegistry())

	// The row that matters is the one that omits the column entirely. The
	// written row sits beside it so that a failure distinguishes "NULL does not
	// scan" from "this column never scanned at all".
	if _, err := raw.Exec(ctx,
		`INSERT INTO nullable_jsondocs (title, metadata) VALUES ($1, $2::jsonb)`,
		"written", `{"lang":"de"}`,
	); err != nil {
		t.Fatalf("inserting the written row: %v", err)
	}
	if _, err := raw.Exec(ctx,
		`INSERT INTO nullable_jsondocs (title) VALUES ($1)`, "omitted",
	); err != nil {
		t.Fatalf("inserting the row with no metadata: %v", err)
	}

	docs, err := sqlb.Query[NullableJSONDoc]().
		OrderBy(sqlb.F("title").Asc()).
		All(ctx, sqlb.New(raw))
	if err != nil {
		t.Fatalf("reading a table with a NULL document column: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d rows, want 2", len(docs))
	}

	// Ordered by title: "omitted" then "written".
	if docs[0].Metadata != nil {
		t.Errorf("a NULL document scanned as %q, want a nil pointer", *docs[0].Metadata)
	}
	if docs[1].Metadata == nil {
		t.Fatal("the written document scanned as NULL")
	}
	var decoded map[string]any
	if err := json.Unmarshal(*docs[1].Metadata, &decoded); err != nil {
		t.Fatalf("the written document did not scan as JSON: %v", err)
	}
	if decoded["lang"] != "de" {
		t.Errorf("decoded metadata = %v, want lang=de", decoded)
	}
}

// The model above is hand-written, so the round trip only proves that
// *json.RawMessage scans a NULL — not that codegen emits it. This ties the two
// together: the Go type the schema says it will generate for the column has to
// be the type the struct that scans it actually uses.
func TestNullableJSONModelMatchesCodegen(t *testing.T) {
	t.Parallel()
	r := nullableJSONDocsRegistry()
	declared := r.Tables()[0].Field("metadata").Desc().GoType()

	field, ok := reflect.TypeOf(NullableJSONDoc{}).FieldByName("Metadata")
	if !ok {
		t.Fatal("NullableJSONDoc has no Metadata field")
	}
	if got := field.Type.String(); got != declared {
		t.Errorf("codegen emits %s for a nullable jsonb column, but the model that scans it is %s", declared, got)
	}
}

// A nullable document column stays filterable, and containment still reaches
// it. The pointer is a scanning concern; the filter layer looks through it
// (isJSONColumn dereferences), and this is what says so against a real server
// rather than against a reflect test.
func TestNullableJSONIsStillFilterable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := freshDB(t)
	applySchema(t, raw, nullableJSONDocsRegistry())

	for _, row := range []struct {
		title    string
		metadata any
	}{
		{"has doc", `{"lang":"de"}`},
		{"null doc", nil},
	} {
		if _, err := raw.Exec(ctx,
			`INSERT INTO nullable_jsondocs (title, metadata) VALUES ($1, $2::jsonb)`,
			row.title, row.metadata,
		); err != nil {
			t.Fatalf("inserting %q: %v", row.title, err)
		}
	}

	values, err := url.ParseQuery(`metadata=hasdoc.{"lang":"de"}`)
	if err != nil {
		t.Fatalf("bad test query: %v", err)
	}
	q, err := filter.Parse(values, filter.Options{Model: sqlb.ModelOf[NullableJSONDoc]()})
	if err != nil {
		t.Fatalf("a nullable jsonb column should still be filterable: %v", err)
	}

	docs, err := filter.Apply(sqlb.Query[NullableJSONDoc](), q).All(ctx, sqlb.New(raw))
	if err != nil {
		t.Fatalf("containment over a nullable document column: %v", err)
	}
	if len(docs) != 1 || docs[0].Title != "has doc" {
		t.Errorf("containment matched %d row(s), want just the one with a document", len(docs))
	}
}

// The containment operators as an expression, rather than as the thing a query
// string parses into.
//
// Everything above reaches `@>` through filter's `hasdoc`, which means the
// builder's own spelling — sqlb.F("metadata").ContainsJSON(doc) — had no test
// against a server anywhere, and its negation had none at all. That matters
// more than the duplication suggests: containment is the operator whose
// behaviour is least like the one its name implies, and a caller writing it by
// hand is the caller least likely to have a filter test standing behind them.
//
// Shaped after TestArrayOperatorsAgainstPostgres: rows seeded to overlap
// deliberately, and a table of predicates whose matched IDs are the assertion.
// Postgres answers every case; nothing here is read off the rendered SQL.

// JSONContainRow gives the containment cases a table with stable integer keys,
// which the UUID-keyed model above cannot: the assertion is which rows matched,
// so the IDs have to be ones the test wrote. `extra` is the nullable column,
// and it is what makes the three-valued case reachable.
type JSONContainRow struct {
	ID    int64            `db:"id" sqlb:"pk"`
	Doc   json.RawMessage  `db:"doc"`
	Extra *json.RawMessage `db:"extra"`
}

func (JSONContainRow) TableName() string { return "json_contain_rows" }

func jsonContainTable(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := freshDB(t)
	mustExec(t, raw, `
		CREATE TABLE json_contain_rows (
			id    bigint PRIMARY KEY,
			doc   jsonb NOT NULL,
			extra jsonb
		)`)
	return raw
}

// containedIDs runs a predicate and returns the IDs it matched, in order.
func containedIDs(t *testing.T, db *sqlb.DB, pred sqlb.Pred) []int64 {
	t.Helper()
	found, err := sqlb.Query[JSONContainRow]().Where(pred).OrderBy(sqlb.F("id").Asc()).All(context.Background(), db)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var out []int64
	for _, r := range found {
		out = append(out, r.ID)
	}
	return out
}

func TestJSONContainmentOperatorsAgainstPostgres(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := jsonContainTable(t)

	// The rows are chosen so that no single one of them can satisfy two cases
	// by accident: an exact match, a superset of it, a row sharing the key but
	// not the value, a nested document, an array document, and the empty
	// object — which is the row that separates "contains nothing" from
	// "contains everything".
	//
	// Seeded through raw SQL with an explicit cast rather than through
	// InsertRows, for the same reason the inserts above do it: what is under
	// test is the operator, and binding a json.RawMessage as a parameter is a
	// separate question that would fail here wearing this test's name.
	for _, row := range []struct {
		id  int64
		doc string
	}{
		{1, `{"lang":"de"}`},
		{2, `{"lang":"de","tier":"pro","tags":["urgent"]}`},
		{3, `{"lang":"fr"}`},
		{4, `{"owner":{"team":"core","region":"eu"}}`},
		{5, `["a","b"]`},
		{6, `{}`},
	} {
		if _, err := raw.Exec(ctx,
			`INSERT INTO json_contain_rows (id, doc) VALUES ($1, $2::jsonb)`, row.id, row.doc,
		); err != nil {
			t.Fatalf("seeding row %d: %v", row.id, err)
		}
	}
	db := sqlb.New(raw)

	tests := []struct {
		name string
		pred sqlb.Pred
		want []int64
	}{
		// Containment is subset, not equality. Row 2 carries two keys the
		// filter never named and still matches — the property the whole
		// operator exists for, and the one an `=` would silently pass every
		// other assertion in this file without.
		{"subset, not equality", sqlb.F("doc").ContainsJSON(`{"lang":"de"}`), []int64{1, 2}},
		{"every named key must match", sqlb.F("doc").ContainsJSON(`{"lang":"de","tier":"pro"}`), []int64{2}},
		{"a key the exact row lacks", sqlb.F("doc").ContainsJSON(`{"tier":"pro"}`), []int64{2}},
		{"same key, other value", sqlb.F("doc").ContainsJSON(`{"lang":"fr"}`), []int64{3}},
		{"no row has it", sqlb.F("doc").ContainsJSON(`{"lang":"it"}`), nil},

		// Nesting recurses: the inner object is matched by containment too, so
		// naming one of its keys is enough.
		{"nested, partially named", sqlb.F("doc").ContainsJSON(`{"owner":{"team":"core"}}`), []int64{4}},
		{"nested, fully named", sqlb.F("doc").ContainsJSON(`{"owner":{"team":"core","region":"eu"}}`), []int64{4}},
		{"nested, wrong inner value", sqlb.F("doc").ContainsJSON(`{"owner":{"team":"infra"}}`), nil},
		// A nested object is not flattened: the key has to be where the
		// document puts it.
		{"nested key at the top level", sqlb.F("doc").ContainsJSON(`{"team":"core"}`), nil},

		// Array documents are subsets by element, in either position: the
		// column holding an array (row 5) and an array nested under a key
		// (row 2).
		{"array document, one element", sqlb.F("doc").ContainsJSON(`["a"]`), []int64{5}},
		{"array document, both elements", sqlb.F("doc").ContainsJSON(`["a","b"]`), []int64{5}},
		{"array document, an absent element", sqlb.F("doc").ContainsJSON(`["a","c"]`), nil},
		{"array under a key", sqlb.F("doc").ContainsJSON(`{"tags":["urgent"]}`), []int64{2}},
		// The asymmetry worth knowing: a top-level array contains a bare
		// scalar, but an object nested under a key does not get the same
		// treatment — `{"tags":"urgent"}` is a value comparison and fails.
		{"a scalar against an array document", sqlb.F("doc").ContainsJSON(`"a"`), []int64{5}},
		{"a scalar against an array under a key", sqlb.F("doc").ContainsJSON(`{"tags":"urgent"}`), nil},

		// The empty-document constants, confirmed against Postgres rather than
		// reasoned about: every object contains the empty object, and the
		// array document does not — which makes `{}` the one filter whose
		// answer distinguishes the shapes in the column.
		{"the empty object", sqlb.F("doc").ContainsJSON(`{}`), []int64{1, 2, 3, 4, 6}},
		{"the empty array", sqlb.F("doc").ContainsJSON(`[]`), []int64{5}},

		// The column is NOT NULL here, so negation is an exact complement.
		// Every case below is the row set its positive above left behind.
		{"negated subset", sqlb.F("doc").NotContainsJSON(`{"lang":"de"}`), []int64{3, 4, 5, 6}},
		// Not "the key is absent": row 1 holds the key and is excluded, row 3
		// holds the key with another value and is kept.
		{"negation is not absence", sqlb.F("doc").NotContainsJSON(`{"lang":"de","tier":"pro"}`), []int64{1, 3, 4, 5, 6}},
		{"negating a match nothing has", sqlb.F("doc").NotContainsJSON(`{"lang":"it"}`), []int64{1, 2, 3, 4, 5, 6}},
		{"negated empty object", sqlb.F("doc").NotContainsJSON(`{}`), []int64{5}},
		{"negated nested", sqlb.F("doc").NotContainsJSON(`{"owner":{"team":"core"}}`), []int64{1, 2, 3, 5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containedIDs(t, db, tt.pred); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("matched %v, want %v", got, tt.want)
			}
		})
	}
}

// NotContainsJSON is `NOT (...)`, not a complement: a NULL document satisfies
// neither direction, because the comparison under the NOT is NULL and negating
// NULL is NULL. Same answer the negated array operators give on a NULL array,
// and the same trap — the row a caller expected to "not contain the document"
// is simply absent from the result.
//
// Proven against Postgres rather than asserted from the rendered SQL, because
// the claim is entirely about how Postgres evaluates three-valued logic.
func TestNegatedJSONContainmentIsThreeValued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	raw := jsonContainTable(t)

	// `extra` is the nullable document column. Row 2 leaves it NULL. `doc` is
	// NOT NULL, so both rows have to write something there; it is not what is
	// being asked about.
	if _, err := raw.Exec(ctx, `
		INSERT INTO json_contain_rows (id, doc, extra)
		VALUES (1, '{}'::jsonb, '{"lang":"de"}'::jsonb), (2, '{}'::jsonb, NULL)`,
	); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	db := sqlb.New(raw)

	tests := []struct {
		name string
		pred sqlb.Pred
		want []int64
	}{
		{"containment skips the NULL row", sqlb.F("extra").ContainsJSON(`{"lang":"de"}`), []int64{1}},
		// The row that matters: 2 is returned by neither direction.
		{"negation also skips the NULL row", sqlb.F("extra").NotContainsJSON(`{"lang":"de"}`), nil},
		{"negating a document nothing holds", sqlb.F("extra").NotContainsJSON(`{"lang":"fr"}`), []int64{1}},
		// Even the empty object, which every non-NULL document contains, does
		// not reach the NULL row in either direction.
		{"the empty object skips the NULL row", sqlb.F("extra").ContainsJSON(`{}`), []int64{1}},
		{"negated empty object matches nothing", sqlb.F("extra").NotContainsJSON(`{}`), nil},
		// Reaching the NULL rows is a separate condition, spelled as one.
		{
			"isnull beside it reaches the row",
			sqlb.Or(sqlb.F("extra").NotContainsJSON(`{"lang":"de"}`), sqlb.F("extra").IsNull()),
			[]int64{2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containedIDs(t, db, tt.pred); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("matched %v, want %v", got, tt.want)
			}
		})
	}
}
