package sqlb_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
)

// tracer wraps an Executor to observe every statement sqlb runs.
//
// This is why sqlb ships no tracing API: Executor is two methods, so the seam
// already exists, and a wrapper reaches OpenTelemetry, slog or a test double
// without sqlb taking a dependency on any of them.
type tracer struct {
	inner sqlb.Executor
	log   func(op, query string, args []any, dur time.Duration, err error)
}

func (t tracer) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	rows, err := t.inner.Query(ctx, q, args...)
	t.log("query", q, args, time.Since(start), err)
	return rows, err
}

func (t tracer) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	tag, err := t.inner.Exec(ctx, q, args...)
	t.log("exec", q, args, time.Since(start), err)
	return tag, err
}

func TestExecutorWrappingObservesEveryStatement(t *testing.T) {
	h := newHarness(t, []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"},
		[][]any{{"u1", "a@example.com", "Ada", nil, "acme", "", time.Time{}}})
	defer h.close()

	var seen []string
	db := tracer{inner: h.db, log: func(op, q string, args []any, dur time.Duration, err error) {
		seen = append(seen, fmt.Sprintf("%s args=%d err=%v", op, len(args), err))
	}}

	ctx := context.Background()
	if _, err := sqlb.Query[User]().Where(sqlb.F("org_id").Eq("acme")).All(ctx, db); err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, err := sqlb.DeleteRows[User]().Where(sqlb.F("id").Eq("u1")).Exec(ctx, db); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	want := []string{"query args=1 err=<nil>", "exec args=1 err=<nil>"}
	if len(seen) != len(want) {
		t.Fatalf("observed %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, seen[i], want[i])
		}
	}

	// The wrapper sees the compiled SQL, which is the thing a filter produced
	// and the thing an EXPLAIN would be run against.
	if h.lastQuery() == "" {
		t.Error("the wrapper should have passed statements through to the executor")
	}
}
