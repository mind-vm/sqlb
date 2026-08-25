package sqlb_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/filter"
)

var benchQuery = url.Values{
	"org_id": {"eq.acme"},
	"age":    {"gte.18", "lt.65"},
	"name":   {"like.ada*"},
	"sort":   {"-created_at,name"},
	"select": {"id,email,name,age,created_at"},
	"page":   {"3"},
}

func benchOpts() filter.Options { return filter.Options{Model: sqlb.ModelOf[User]()} }

// Parse only: query string -> validated Query.
func BenchmarkFilterParse(b *testing.B) {
	opts := benchOpts()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := filter.Parse(benchQuery, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// Apply + compile: the whole build path once a Query exists.
func BenchmarkApplyAndSQL(b *testing.B) {
	q, err := filter.Parse(benchQuery, benchOpts())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		sql, args, err := filter.Apply(sqlb.Query[User](), q).SQL()
		if err != nil {
			b.Fatal(err)
		}
		_, _ = sql, args
	}
}

// Parse + Apply + compile: everything a list request does before the database.
func BenchmarkParseApplySQL(b *testing.B) {
	opts := benchOpts()
	b.ReportAllocs()
	for b.Loop() {
		q, err := filter.Parse(benchQuery, opts)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := filter.Apply(sqlb.Query[User](), q).SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

// Hand-written builder, no filter package: the compile cost on its own.
func BenchmarkCompileSimple(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _, err := sqlb.Query[User]().
			Where(sqlb.F("org_id").Eq("acme"), sqlb.F("age").Gte(18)).
			OrderBy(sqlb.F("created_at").Desc()).
			Limit(25).SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// Clone alone, since every terminal method starts with one.
func BenchmarkClone(b *testing.B) {
	q := filter.Apply(sqlb.Query[User](), mustParse(b))
	b.ReportAllocs()
	for b.Loop() {
		_ = q.Clone()
	}
}

func mustParse(b *testing.B) *filter.Query {
	b.Helper()
	q, err := filter.Parse(benchQuery, benchOpts())
	if err != nil {
		b.Fatal(err)
	}
	return q
}

// Scan of a 100-row page through the fake result set. The fake's own Scan cost
// is constant, so the number is only useful as a before/after.
func BenchmarkScanPage(b *testing.B) {
	cols := []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"}
	age := int32(33)
	now := time.Now()
	data := make([][]any, 100)
	for i := range data {
		data[i] = []any{"01HQ9ZK4T7X2VF8N3M5R6P0QWE", "ada@example.com", "Ada Lovelace", &age, "acme", "x", now}
	}
	db := &fakeDB{h: &harness{cols: cols, rows: data}}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		rows, err := sqlb.Query[User]().All(ctx, db)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 100 {
			b.Fatalf("got %d rows", len(rows))
		}
	}
}
