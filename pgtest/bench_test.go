package pgtest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
)

// What sqlb costs over the driver it is now written on, measured rather than
// reasoned about.
//
// [ADR-0040] made the engine pgx-native, and half its argument was performance.
// That half was written from what the protocols do rather than from a number,
// which is the position [ADR-0019] was in before it went and tested a real
// PgBouncer — and two of that record's three claims came back more interesting
// than the documentation predicted. These benchmarks are what settled it: the
// bridge cost ordinary CRUD ~30%, and a wide float array 2.7× time and 21×
// memory, which is the pgvector case with a number attached.
//
// What they measure now is different, and worth keeping for it: sqlb and
// hand-written pgx run on the same driver, so what is left is the builder, the
// hooks and the reflective scan. Any gap here is sqlb's own, with nowhere to
// put the blame.
//
// # Reading these fairly
//
// The pgx side is deliberately the *floor*: a hand-written statement and a
// manual rows.Scan into the same struct, which is the fastest pgx can go. It is
// not what an application would write — pgx.CollectRows with
// RowToStructByName is — and it does none of the work sqlb does around the
// scan (hooks, capability-aware projections, expansion decoding). The
// comparison is therefore biased *against* sqlb on purpose. A benchmark that
// flatters the thing its author is arguing for is worth nothing, and the
// interesting question is not "is sqlb slower than raw SQL" (it is, and so is
// every query builder) but "where is the gap large enough to be an argument".
//
// # What these do not measure
//
// Not pgx versus database/sql in the abstract. Not connection setup, not
// pooling under contention, not concurrency. Every benchmark here is one
// goroutine issuing one statement at a time against a warm local container, so
// they isolate encode/decode and per-statement overhead and nothing else.
//
// Run with:
//
//	mise run bench-pg
//
// [ADR-0040]: ../docs/architecture.md#the-driver-is-a-dependency
// [ADR-0019]: ../docs/architecture.md#pgbouncer-in-the-path

// benchRows is the page size the read benchmarks scan. Chosen to look like a
// list endpoint rather than to flatter either side: large enough that
// per-row decoding dominates per-statement overhead, small enough to be a
// plausible response.
const benchRows = 200

// benchDB returns two handles onto one fresh database: a sqlb handle, and the
// pool underneath it for the hand-written comparison and for seeding outside
// the timed region.
//
// They are the same pool on purpose. Before ADR-0040 they could not be — sqlb
// held a *sql.DB and the comparison held a pgxpool, and the two connection
// paths were half of what was being measured.
func benchDB(b *testing.B) (*sqlb.DB, *pgxpool.Pool) {
	b.Helper()
	pool := freshDB(b)
	return sqlb.New(pool), pool
}

// --- Shape 1: a list of scalars -------------------------------------------
//
// The ordinary case, and the one almost every request is. Nothing here needs a
// codec pgx lacks, so what this measures is sqlb's baseline overhead: building
// the statement, and the reflective scan into a struct.

type BenchScalar struct {
	ID        int64     `db:"id" sqlb:"pk"`
	Name      string    `db:"name"`
	Status    string    `db:"status"`
	Amount    float64   `db:"amount"`
	Active    bool      `db:"active"`
	CreatedAt time.Time `db:"created_at"`
}

func (BenchScalar) TableName() string { return "bench_scalars" }

const benchScalarColumns = `id, name, status, amount, active, created_at`

func BenchmarkListScalars(b *testing.B) {
	ctx := context.Background()
	db, pool := benchDB(b)

	mustExec(b, pool, `
		CREATE TABLE bench_scalars (
			id         bigint PRIMARY KEY,
			name       text NOT NULL,
			status     text NOT NULL,
			amount     double precision NOT NULL,
			active     boolean NOT NULL,
			created_at timestamptz NOT NULL
		)`)
	seedScalars(b, pool, benchRows)

	b.Run("sqlb", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := sqlb.Query[BenchScalar]().OrderBy(sqlb.F("id").Asc()).All(ctx, db)
			if err != nil {
				b.Fatalf("sqlb list: %v", err)
			}
			if len(got) != benchRows {
				b.Fatalf("sqlb list returned %d rows, want %d", len(got), benchRows)
			}
		}
	})

	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := listScalarsPgx(ctx, pool)
			if err != nil {
				b.Fatalf("pgx list: %v", err)
			}
			if len(got) != benchRows {
				b.Fatalf("pgx list returned %d rows, want %d", len(got), benchRows)
			}
		}
	})
}

func listScalarsPgx(ctx context.Context, pool *pgxpool.Pool) ([]BenchScalar, error) {
	rows, err := pool.Query(ctx, `SELECT `+benchScalarColumns+` FROM bench_scalars ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BenchScalar, 0, benchRows)
	for rows.Next() {
		var r BenchScalar
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Amount, &r.Active, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func seedScalars(b *testing.B, pool *pgxpool.Pool, n int) {
	b.Helper()
	var q strings.Builder
	q.WriteString(`INSERT INTO bench_scalars (` + benchScalarColumns + `) VALUES `)
	for i := range n {
		if i > 0 {
			q.WriteString(", ")
		}
		fmt.Fprintf(&q, "(%d, 'name-%d', 'active', %d.5, true, now())", i, i, i)
	}
	mustExec(b, pool, q.String())
}

// --- Shape 2: array columns -----------------------------------------------
//
// The shape that used to cost the most, and now costs nothing extra. Under
// database/sql sqlb wrote and parsed the `{a,b}` literal in Go ([ADR-0033],
// array.go, 449 lines); pgx decodes int8[] and text[] natively, so both arms
// here run the same codec and the remaining gap is the builder's.

type BenchArray struct {
	ID    int64    `db:"id" sqlb:"pk"`
	Tags  []string `db:"tags"`
	Sizes []int64  `db:"sizes"`
}

func (BenchArray) TableName() string { return "bench_arrays" }

// benchArrayLen is the element count per array — a tag list, not a vector.
// Shape 3 is where width is the point.
const benchArrayLen = 8

func BenchmarkListArrays(b *testing.B) {
	ctx := context.Background()
	db, pool := benchDB(b)

	mustExec(b, pool, `
		CREATE TABLE bench_arrays (
			id    bigint PRIMARY KEY,
			tags  text[] NOT NULL,
			sizes bigint[] NOT NULL
		)`)
	seedArrays(b, pool, benchRows)

	b.Run("sqlb", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := sqlb.Query[BenchArray]().OrderBy(sqlb.F("id").Asc()).All(ctx, db)
			if err != nil {
				b.Fatalf("sqlb list: %v", err)
			}
			if len(got) != benchRows || len(got[0].Tags) != benchArrayLen {
				b.Fatalf("sqlb list returned %d rows with %d tags", len(got), len(got[0].Tags))
			}
		}
	})

	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := listArraysPgx(ctx, pool)
			if err != nil {
				b.Fatalf("pgx list: %v", err)
			}
			if len(got) != benchRows || len(got[0].Tags) != benchArrayLen {
				b.Fatalf("pgx list returned %d rows with %d tags", len(got), len(got[0].Tags))
			}
		}
	})
}

func listArraysPgx(ctx context.Context, pool *pgxpool.Pool) ([]BenchArray, error) {
	rows, err := pool.Query(ctx, `SELECT id, tags, sizes FROM bench_arrays ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BenchArray, 0, benchRows)
	for rows.Next() {
		var r BenchArray
		if err := rows.Scan(&r.ID, &r.Tags, &r.Sizes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func seedArrays(b *testing.B, pool *pgxpool.Pool, n int) {
	b.Helper()
	var q strings.Builder
	q.WriteString(`INSERT INTO bench_arrays (id, tags, sizes) VALUES `)
	for i := range n {
		if i > 0 {
			q.WriteString(", ")
		}
		tags := make([]string, benchArrayLen)
		sizes := make([]string, benchArrayLen)
		for k := range benchArrayLen {
			tags[k] = fmt.Sprintf("tag-%d-%d", i, k)
			sizes[k] = fmt.Sprint(i*benchArrayLen + k)
		}
		fmt.Fprintf(&q, "(%d, '{%s}', '{%s}')", i, strings.Join(tags, ","), strings.Join(sizes, ","))
	}
	mustExec(b, pool, q.String())
}

// --- Shape 3: a wide float array, standing in for a vector ----------------
//
// [ADR-0026] specifies sqlb.Vector over pgvector's *text* form, and says why:
// "Executor is database/sql". This is that cost, isolated.
//
// It is real[] rather than vector(1536) because postgres:18-alpine has no
// pgvector extension, and pulling one in to benchmark a codec would be a large
// change to the harness for a number this approximates well: pgvector's element
// type is float4, the width is the width embeddings actually are, and the
// text-versus-binary question is identical. It is a proxy, and calling it a
// pgvector benchmark would overstate it.

type BenchVector struct {
	ID        int64     `db:"id" sqlb:"pk"`
	Embedding []float32 `db:"embedding"`
}

func (BenchVector) TableName() string { return "bench_vectors" }

const (
	benchVectorDims = 1536 // what text-embedding-3-small and friends emit
	benchVectorRows = 50   // a plausible ANN result set
)

func BenchmarkListVectors(b *testing.B) {
	ctx := context.Background()
	db, pool := benchDB(b)

	mustExec(b, pool, `
		CREATE TABLE bench_vectors (
			id        bigint PRIMARY KEY,
			embedding real[] NOT NULL
		)`)
	seedVectors(b, pool, benchVectorRows)

	b.Run("sqlb", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := sqlb.Query[BenchVector]().OrderBy(sqlb.F("id").Asc()).All(ctx, db)
			if err != nil {
				b.Fatalf("sqlb list: %v", err)
			}
			if len(got) != benchVectorRows || len(got[0].Embedding) != benchVectorDims {
				b.Fatalf("sqlb list returned %d rows of %d dims", len(got), len(got[0].Embedding))
			}
		}
	})

	b.Run("pgx", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := listVectorsPgx(ctx, pool)
			if err != nil {
				b.Fatalf("pgx list: %v", err)
			}
			if len(got) != benchVectorRows || len(got[0].Embedding) != benchVectorDims {
				b.Fatalf("pgx list returned %d rows of %d dims", len(got), len(got[0].Embedding))
			}
		}
	})
}

func listVectorsPgx(ctx context.Context, pool *pgxpool.Pool) ([]BenchVector, error) {
	rows, err := pool.Query(ctx, `SELECT id, embedding FROM bench_vectors ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BenchVector, 0, benchVectorRows)
	for rows.Next() {
		var r BenchVector
		if err := rows.Scan(&r.ID, &r.Embedding); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func seedVectors(b *testing.B, pool *pgxpool.Pool, n int) {
	b.Helper()
	elems := make([]string, benchVectorDims)
	for i := range n {
		for k := range benchVectorDims {
			// Deterministic, and spread across the float range so the text form
			// is a realistic width rather than n copies of "0".
			elems[k] = fmt.Sprintf("%.6f", float64((i*benchVectorDims+k)%2000)/1000-1)
		}
		mustExec(b, pool, fmt.Sprintf(
			`INSERT INTO bench_vectors (id, embedding) VALUES (%d, '{%s}')`,
			i, strings.Join(elems, ",")))
	}
}

// --- Shape 4: bulk insert -------------------------------------------------
//
// Three variants, because comparing sqlb against CopyFrom conflates two
// different claims — that the driver is faster, and that sqlb lacks an API.
// Separating them says which one the number is about:
//
//   - sqlb        — InsertRows, which compiles to one multi-row VALUES
//     (mutate.go) with RETURNING over every column.
//   - pgx/values  — the same statement shape by hand, also with RETURNING.
//     The difference against sqlb is the builder, and nothing else.
//   - pgx/copy    — CopyFrom, which sqlb has no API for. ADR-0040 made it
//     reachable through DB.Tx; giving it a builder is a separate question.
//
// CopyFrom returns no rows, and cannot: that is not an oversight in the
// benchmark but part of what the speed buys. sqlb's insert always appends
// RETURNING so that generated ids are written back into the caller's structs,
// which is a feature CopyFrom users give up and re-read separately if they need
// it. Read the copy number as "the ceiling, if you do not need the rows back".

const benchInsertRows = 500

func BenchmarkBulkInsert(b *testing.B) {
	ctx := context.Background()
	db, pool := benchDB(b)

	mustExec(b, pool, `
		CREATE TABLE bench_scalars (
			id         bigint PRIMARY KEY,
			name       text NOT NULL,
			status     text NOT NULL,
			amount     double precision NOT NULL,
			active     boolean NOT NULL,
			created_at timestamptz NOT NULL
		)`)

	rows := make([]BenchScalar, benchInsertRows)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i := range rows {
		rows[i] = BenchScalar{
			ID: int64(i), Name: fmt.Sprintf("name-%d", i), Status: "active",
			Amount: float64(i) + 0.5, Active: true, CreatedAt: at,
		}
	}

	// The table is emptied between iterations so every one inserts into the
	// same shape, and outside the timer so the truncate is not what is measured.
	reset := func(b *testing.B) {
		b.StopTimer()
		mustExec(b, pool, `TRUNCATE bench_scalars`)
		b.StartTimer()
	}

	b.Run("sqlb", func(b *testing.B) {
		ptrs := make([]*BenchScalar, len(rows))
		for i := range rows {
			ptrs[i] = &rows[i]
		}
		b.ReportAllocs()
		for b.Loop() {
			reset(b)
			if _, err := sqlb.InsertRows(ptrs...).Exec(ctx, db); err != nil {
				b.Fatalf("sqlb insert: %v", err)
			}
		}
	})

	b.Run("pgx/values", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			reset(b)
			if err := insertValuesPgx(ctx, pool, rows); err != nil {
				b.Fatalf("pgx values insert: %v", err)
			}
		}
	})

	b.Run("pgx/copy", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			reset(b)
			if err := insertCopyPgx(ctx, pool, rows); err != nil {
				b.Fatalf("pgx copy insert: %v", err)
			}
		}
	})
}

// insertValuesPgx is sqlb's statement written by hand: one multi-row VALUES
// with RETURNING, and the returned rows scanned back.
func insertValuesPgx(ctx context.Context, pool *pgxpool.Pool, in []BenchScalar) error {
	var q strings.Builder
	q.WriteString(`INSERT INTO bench_scalars (` + benchScalarColumns + `) VALUES `)
	args := make([]any, 0, len(in)*6)
	for i, r := range in {
		if i > 0 {
			q.WriteString(", ")
		}
		n := i * 6
		fmt.Fprintf(&q, "($%d, $%d, $%d, $%d, $%d, $%d)", n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, r.ID, r.Name, r.Status, r.Amount, r.Active, r.CreatedAt)
	}
	q.WriteString(` RETURNING ` + benchScalarColumns)

	rows, err := pool.Query(ctx, q.String(), args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r BenchScalar
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Amount, &r.Active, &r.CreatedAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

// insertCopyPgx is the binary bulk load that has no database/sql spelling.
func insertCopyPgx(ctx context.Context, pool *pgxpool.Pool, in []BenchScalar) error {
	_, err := pool.CopyFrom(ctx,
		pgx.Identifier{"bench_scalars"},
		[]string{"id", "name", "status", "amount", "active", "created_at"},
		pgx.CopyFromSlice(len(in), func(i int) ([]any, error) {
			r := in[i]
			return []any{r.ID, r.Name, r.Status, r.Amount, r.Active, r.CreatedAt}, nil
		}))
	return err
}
