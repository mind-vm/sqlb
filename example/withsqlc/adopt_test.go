package withsqlc_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/withsqlc/sqlcgen"
	"github.com/mind-vm/sqlb/filter"
	"github.com/mind-vm/sqlb/internal/pgfake"
)

// The load-bearing claim: stock sqlc output carries no db tags, and sqlb maps
// Go field names to columns by snake_case when there is none. If that fallback
// ever changed, the incremental-adoption path in the README would quietly stop
// working, and this is the test that would say so.
//
// sqlc.yaml deliberately leaves emit_db_tags off. Turning it on would make this
// pass for a reason that does not generalise to the sqlc projects people
// already have.
func TestDescribeMapsStockSqlcStructs(t *testing.T) {
	m := sqlb.Describe[sqlcgen.Post]().Model()

	// Derived from the type name, with no TableName method in sight.
	if m.Table != "posts" {
		t.Errorf("table = %q, want %q", m.Table, "posts")
	}

	want := map[string]string{
		"ID":          "id",
		"OrgID":       "org_id", // the case a naive splitter turns into org_i_d
		"AuthorID":    "author_id",
		"Title":       "title",
		"Body":        "body",
		"Status":      "status",
		"ViewCount":   "view_count",
		"PublishedAt": "published_at",
		"CreatedAt":   "created_at",
		"UpdatedAt":   "updated_at",
		"DeletedAt":   "deleted_at",
	}
	if len(m.Columns) != len(want) {
		t.Errorf("mapped %d columns, want %d", len(m.Columns), len(want))
	}
	for _, col := range m.Columns {
		if want[col.Field] != col.Name {
			t.Errorf("field %s maps to column %q, want %q", col.Field, col.Name, want[col.Field])
		}
	}

	// pgtype.Timestamptz is what sqlc emits for a nullable timestamp under
	// pgx, and it is a Scanner/Valuer, so a nullable column needs no special
	// handling on the sqlb side.
	if col := m.Column("published_at"); col == nil {
		t.Error("published_at was not mapped")
	}
}

// Capabilities cannot be read from a struct that never declared them, which is
// the real cost of adopting this way: what the schema DSL states once has to be
// restated here. Describe is where that happens, and it is checked against the
// struct, so a typo fails rather than silently disabling a filter.
func TestCapabilitiesAreDeclaredNotInferred(t *testing.T) {
	// A column nobody declared filterable must not be filterable. Otherwise
	// adopting sqlb over existing structs would widen the API by accident,
	// which is exactly what ADR-0006 exists to prevent.
	fresh := sqlb.Describe[sqlcgen.Org]().Model()
	if col := fresh.Column("slug"); col == nil || col.Filterable {
		t.Error("an undeclared column should not be filterable")
	}
}

// The point of adopting sqlb at all: a filterable list endpoint over structs
// sqlc generated. This is the third row of the table in docs/with-sqlc.md,
// proven rather than asserted.
func TestFilteredListOverSqlcStructs(t *testing.T) {
	sqlb.Describe[sqlcgen.Post]().
		PrimaryKey("id").
		Filterable("status", "author_id", "view_count").
		Sortable("published_at", "view_count").
		Searchable("title", "body").
		ReadOnly("view_count")

	parsed, err := filter.Parse(url.Values{
		"status": {"eq.published"},
		"order":  {"view_count.desc"},
		"limit":  {"10"},
	}, filter.Options{Model: sqlb.ModelOf[sqlcgen.Post]()})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	q := filter.Apply(sqlb.Query[sqlcgen.Post](), parsed)
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{`FROM "posts"`, `WHERE "status" = $1`, `ORDER BY "view_count" DESC`, "LIMIT 10"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL is missing %q:\n%s", want, sql)
		}
	}
	if len(args) != 1 || args[0] != "published" {
		t.Errorf("args = %v, want [published]", args)
	}
}

// docs/with-sqlc.md claims one transaction can carry both sides, and since
// ADR-0040 that claim is about one interface rather than two that happen to
// share a receiver: sqlb.Executor is a subset of DBTX, and a pgx.Tx is both.
// Asserting it here means the document cannot drift from what compiles.
func TestOneTransactionCarriesBothSides(t *testing.T) {
	var tx pgx.Tx

	var _ sqlb.Executor = tx
	var _ sqlcgen.DBTX = tx

	// And the route from a sqlb handle back to it, which is what makes WithTx
	// usable rather than forcing callers to manage the boundary themselves.
	handle := sqlb.New(&pgfake.Tx{})
	if _, ok := handle.Tx(); !ok {
		t.Error("DB.Tx did not reach the pgx.Tx it was built over")
	}

	// A pool is not a transaction, and must not claim to be one.
	if _, ok := sqlb.New(notATx{}).Tx(); ok {
		t.Error("DB.Tx returned a transaction for a pool")
	}
}

// notATx is an Executor that is not a transaction, which is what a pool is from
// sqlb's side of the interface.
type notATx struct{ sqlb.Executor }

// An undeclared capability is still refused, over sqlc structs as over
// generated ones. The guard has to hold on this path too, or adoption would be
// a way around it (ADR-0016: prove it fires).
func TestUndeclaredFilterIsStillRefused(t *testing.T) {
	sqlb.Describe[sqlcgen.Author]().PrimaryKey("id").Filterable("email")

	_, err := filter.Parse(url.Values{
		"password_hash": {"eq.secret"},
	}, filter.Options{Model: sqlb.ModelOf[sqlcgen.Author]()})
	if err == nil {
		t.Fatal("filtering an undeclared column should be refused")
	}
}
