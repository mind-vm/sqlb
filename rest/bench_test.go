package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb/internal/pgfake"
	"github.com/mind-vm/sqlb/rest"
)

// What a generated list endpoint costs above the database, against the handler
// somebody would have written instead.
//
// pgtest/bench_test.go measures the engine against hand-written pgx and finds
// single-digit percent. That number says nothing about this package, which does
// the work the engine does not: parsing the filter grammar, resolving a
// projection, and serialising a page under it. This is that half, and it was
// worth measuring because it turned out to be the larger one.
//
// # Reading these fairly
//
// Both arms read the same canned result set through the same fake, so the
// driver and the database cancel out and what is left is CPU this package
// spends per request. Nothing here is a throughput number: there is no
// connection, no network and no query plan, which are what a real endpoint is
// mostly waiting for.
//
// The hand-written arm is the floor, and deliberately so. It knows its two
// parameters at compile time, appends to a statement it mostly wrote in
// advance, scans by hand and marshals the struct directly — so it also has no
// ?select, no ?cursor, no capability checking and no way to add a filter
// without editing Go. That is the trade being priced, not a bug in the
// comparison.
//
// The third arm exists because the obvious reading of a gap this size is "the
// framework". It is the same hand-written work behind huma, and it lands on the
// bare handler, which is what says the cost is this package's own.
//
// # What it caught
//
// Serialising a page allocated three times over, and 83% of the garbage on the
// response path was in row.MarshalJSON:
//
//   - json.Marshal quoted a *constant* key once per column per row — 400 times
//     for the page below. The keys are now rendered at registration.
//   - json.Marshal copied every value into a slice the buffer appended and then
//     dropped. Values now go through an encoder bound to that buffer.
//   - every row built and filled a buffer of its own, which encoding/json then
//     copied out and discarded. A page now shares one; see rowWriter.
//
// Together: 1,776 allocations to 279, and 173µs to 120µs, for a response that
// is byte-identical down to the escaping. The arms are kept so that it stays
// fixed.
//
// One of the two costs that reading called encoding/json's own floor turned
// out not to be one. time.Time.MarshalJSON allocates a slice per value for the
// encoder to copy out and drop, which is a floor of the Marshaler interface
// rather than of JSON: a timestamp appended straight into the page's buffer
// costs nothing and renders the same bytes. That is 279 allocations to 229 —
// exactly the fifty rows — and about 7% of the request. See rowWriter.timestamp.
//
// What is left is the boxing reflect does to hand a column value to the
// encoder, which is the same cost and would want the same treatment: a typed
// appender per kind. It is unfinished because a value, unlike a timestamp,
// needs the escaping encoding/json would have applied.

const benchPageRows = 50

// benchDB answers every statement with the same canned page. It is not the
// fakeDB the tests use: that one records statements and takes a *testing.T,
// and both would be measured here.
type benchDB struct {
	cols []string
	rows [][]any
}

func (d *benchDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &pgfake.Rows{Cols: d.cols, Data: d.rows}, nil
}

func (d *benchDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SELECT 0"), nil
}

func benchPage(n int) *benchDB {
	rows := make([][]any, n)
	at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for i := range rows {
		rows[i] = []any{
			"p" + strconv.Itoa(i), "acme", "Title " + strconv.Itoa(i),
			"body text goes here", "excerpt text", "draft", int64(i), at,
		}
	}
	return &benchDB{cols: postCols(), rows: rows}
}

// benchTarget filters on two columns and sorts on a third, which is the request
// the resource exists to serve. A bare list would measure less of the parser
// than any real client exercises.
const benchTarget = "/posts?status=eq.draft&view_count=gte.3&sort=-created_at&per_page=50"

func BenchmarkListRequest(b *testing.B) {
	db := benchPage(benchPageRows)

	b.Run("sqlb", func(b *testing.B) {
		mux := http.NewServeMux()
		api := humago.New(mux, huma.DefaultConfig("Bench", "1.0.0"))
		// Writes are not registered, so the transaction the resource would
		// otherwise insist on is not a shape this fake has to grow.
		err := rest.Resource[Post, PostCreate, PostUpdate](api, db, rest.Options{
			Path: "/posts", Name: "post", Ops: rest.OpList,
			DefaultPageSize: benchPageRows, MaxPageSize: 100,
			DisableTransactions: true,
		})
		if err != nil {
			b.Fatalf("mounting the resource: %v", err)
		}
		benchServe(b, mux)
	})

	b.Run("huma+handwritten", func(b *testing.B) {
		mux := http.NewServeMux()
		api := humago.New(mux, huma.DefaultConfig("Bench", "1.0.0"))
		registerHandwritten(api, db)
		benchServe(b, mux)
	})

	b.Run("handwritten", func(b *testing.B) {
		benchServe(b, handwrittenHandler(db))
	})
}

// benchServe issues the same request at h for the whole run. The request is
// built once because parsing it is not what is being compared.
func benchServe(b *testing.B, h http.Handler) {
	b.Helper()
	req := httptest.NewRequest(http.MethodGet, benchTarget, nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
	}
}

// --- the hand-written arm --------------------------------------------------

const benchSelect = `SELECT id, org_id, title, body, excerpt, status, view_count, created_at FROM posts`

type benchBody struct {
	Items   []Post `json:"items"`
	Page    int    `json:"page"`
	PerPage int    `json:"per_page"`
	HasMore bool   `json:"has_more"`
}

// benchQuery is the statement the hand-written arms build, and the scan back
// out of it. Shared by both so the two differ only in how they got their
// parameters and how they wrote their response.
func benchQuery(ctx context.Context, db *benchDB, status, viewCount, sort string, perPage int) (benchBody, error) {
	var (
		sb   strings.Builder
		args []any
		n    int
	)
	sb.WriteString(benchSelect)
	where := func(frag string, v any) {
		n++
		if n == 1 {
			sb.WriteString(" WHERE ")
		} else {
			sb.WriteString(" AND ")
		}
		sb.WriteString(frag)
		sb.WriteString("$" + strconv.Itoa(n))
		args = append(args, v)
	}
	if v, ok := strings.CutPrefix(status, "eq."); ok {
		where(`status = `, v)
	}
	if v, ok := strings.CutPrefix(viewCount, "gte."); ok {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return benchBody{}, err
		}
		where(`view_count >= `, parsed)
	}
	switch sort {
	case "-created_at":
		sb.WriteString(` ORDER BY created_at DESC`)
	case "created_at":
		sb.WriteString(` ORDER BY created_at ASC`)
	}
	n++
	// One row past the page, the way the generated handler learns there is
	// another one without counting.
	sb.WriteString(` LIMIT $` + strconv.Itoa(n))
	args = append(args, perPage+1)

	rows, err := db.Query(ctx, sb.String(), args...)
	if err != nil {
		return benchBody{}, err
	}
	defer rows.Close()

	out := make([]Post, 0, perPage+1)
	for rows.Next() {
		var p Post
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Title, &p.Body, &p.Excerpt,
			&p.Status, &p.ViewCount, &p.CreatedAt); err != nil {
			return benchBody{}, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return benchBody{}, err
	}
	hasMore := len(out) > perPage
	if hasMore {
		out = out[:perPage]
	}
	return benchBody{Items: out, Page: 1, PerPage: perPage, HasMore: hasMore}, nil
}

// handwrittenHandler reads its parameters off the URL and encodes the struct
// directly, which is the whole point: no projection, so no per-column work.
func handwrittenHandler(db *benchDB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		perPage := benchPageRows
		if v := q.Get("per_page"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p < 1 || p > 100 {
				http.Error(w, "bad per_page", http.StatusBadRequest)
				return
			}
			perPage = p
		}
		body, err := benchQuery(r.Context(), db,
			q.Get("status"), q.Get("view_count"), q.Get("sort"), perPage)
		if err != nil {
			http.Error(w, "query", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// benchInput is the typed parameter struct a huma handler declares, which is
// where huma does its share of the per-request work.
type benchInput struct {
	Status    string `query:"status"`
	ViewCount string `query:"view_count"`
	Sort      string `query:"sort"`
	PerPage   int    `query:"per_page" default:"50"`
}

type benchOutput struct{ Body benchBody }

func registerHandwritten(api huma.API, db *benchDB) {
	huma.Register(api, huma.Operation{
		OperationID: "list-posts-handwritten",
		Method:      http.MethodGet,
		Path:        "/posts",
	}, func(ctx context.Context, in *benchInput) (*benchOutput, error) {
		body, err := benchQuery(ctx, db, in.Status, in.ViewCount, in.Sort, in.PerPage)
		if err != nil {
			return nil, huma.Error400BadRequest("bad request")
		}
		return &benchOutput{Body: body}, nil
	})
}
