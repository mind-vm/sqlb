package sqlb_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
)

// statements returns the recorded log, with the SQL reduced to its first word
// so assertions read as the shape of the unit of work.
func (h *harness) statements() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.log))
	for i, q := range h.log {
		out[i], _, _ = strings.Cut(strings.TrimSpace(q), " ")
	}
	return out
}

// lastSelect returns the most recent SELECT, skipping the transaction markers
// that lastQuery would otherwise return.
func (h *harness) lastSelect(t *testing.T) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.log) - 1; i >= 0; i-- {
		if strings.HasPrefix(h.log[i], "SELECT") {
			return h.log[i]
		}
	}
	t.Fatalf("no SELECT was recorded, log = %v", h.log)
	return ""
}

func sameStatements(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("statements = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("statements = %v, want %v", got, want)
		}
	}
}

// txHarness builds a harness whose canned rows satisfy an insert's RETURNING.
func txHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, []string{"id", "email", "name", "age", "org_id", "password_hash", "created_at"},
		[][]any{{"u1", "a@b.c", "Ada", nil, "org1", "", time.Unix(0, 0).UTC()}})
	t.Cleanup(h.close)
	return h
}

// --- the handle -------------------------------------------------------------

func TestDBIsItselfAnExecutor(t *testing.T) {
	// The whole reason the handle is additive: it goes wherever an Executor
	// went, so adopting it does not touch call sites.
	h := txHarness(t)
	var _ sqlb.Executor = sqlb.New(h.db)

	db := sqlb.New(h.db)
	if _, err := sqlb.Query[User]().All(context.Background(), db); err != nil {
		t.Fatalf("All through a *DB: %v", err)
	}
	if got := h.lastQuery(); !strings.HasPrefix(got, "SELECT") {
		t.Errorf("query = %q, want a SELECT", got)
	}
}

func TestWithTxCommitsOnSuccess(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		u := User{Email: "a@b.c"}
		_, err := sqlb.InsertRows(&u).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	sameStatements(t, h.statements(), []string{"BEGIN", "INSERT", "COMMIT"})
}

func TestWithTxRollsBackOnError(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	sentinel := errors.New("domain rule said no")
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		u := User{Email: "a@b.c"}
		if _, err := sqlb.InsertRows(&u).One(ctx, tx); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the caller's error unwrapped", err)
	}
	sameStatements(t, h.statements(), []string{"BEGIN", "INSERT", "ROLLBACK"})
}

// A panic must not leave the transaction open. It must also still reach the
// caller — swallowing it would turn a bug into a silent rollback.
func TestWithTxRollsBackOnPanicAndReRaises(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Error("panic did not reach the caller")
			}
		}()
		_ = db.WithTx(context.Background(), func(context.Context, *sqlb.DB) error {
			panic("boom")
		})
	}()

	sameStatements(t, h.statements(), []string{"BEGIN", "ROLLBACK"})
}

// Nesting joins the outer transaction rather than opening a second one, so a
// function that opens a transaction stays callable from inside one.
func TestNestedWithTxJoinsTheOuterTransaction(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if !tx.InTx() {
			t.Error("InTx() = false inside WithTx")
		}
		return tx.WithTx(ctx, func(ctx context.Context, inner *sqlb.DB) error {
			u := User{Email: "a@b.c"}
			_, err := sqlb.InsertRows(&u).One(ctx, inner)
			return err
		})
	})
	if err != nil {
		t.Fatalf("nested WithTx: %v", err)
	}
	sameStatements(t, h.statements(), []string{"BEGIN", "INSERT", "COMMIT"})
}

// An inner error rolls back the whole unit of work, not just the inner part.
// This is the consequence of joining, and it is worth pinning.
func TestNestedErrorRollsBackTheWhole(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		u := User{Email: "a@b.c"}
		if _, err := sqlb.InsertRows(&u).One(ctx, tx); err != nil {
			return err
		}
		return tx.WithTx(ctx, func(context.Context, *sqlb.DB) error {
			return errors.New("inner failed")
		})
	})
	if err == nil {
		t.Fatal("expected the inner error to surface")
	}
	sameStatements(t, h.statements(), []string{"BEGIN", "INSERT", "ROLLBACK"})
}

func TestWithTxNeedsAnExecutorThatCanBegin(t *testing.T) {
	h := txHarness(t)
	// A tracer wrapper implements Executor and nothing else, which is the
	// common case and exactly when the error has to explain itself.
	db := sqlb.New(execOnly{inner: h.db})

	err := db.WithTx(context.Background(), func(context.Context, *sqlb.DB) error { return nil })
	if err == nil {
		t.Fatal("expected WithTx to refuse an executor that cannot begin")
	}
	for _, want := range []string{"BeginTx", "only implements Executor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(h.statements()) != 0 {
		t.Errorf("nothing should have run, got %v", h.statements())
	}
}

// Asking for stricter isolation inside an existing transaction cannot be
// honoured. Ignoring it would leave the caller believing it had a guarantee it
// does not have, so it is refused.
func TestNestedIsolationRequestIsRefused(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		return tx.WithTxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable},
			func(context.Context, *sqlb.DB) error { return nil })
	})
	if err == nil {
		t.Fatal("expected the nested isolation request to be refused")
	}
	if !strings.Contains(err.Error(), "outermost") {
		t.Errorf("error %q should say where to request it instead", err)
	}
	sameStatements(t, h.statements(), []string{"BEGIN", "ROLLBACK"})
}

func TestBeginFailureIsReported(t *testing.T) {
	h := txHarness(t)
	h.txErr = errors.New("connection refused")
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(context.Context, *sqlb.DB) error {
		t.Error("the function should not run when BEGIN fails")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "beginning transaction") {
		t.Fatalf("error = %v, want it to name the failing step", err)
	}
}

func TestCommitFailureReachesTheCaller(t *testing.T) {
	h := txHarness(t)
	h.commitErr = errors.New("serialization failure")
	db := sqlb.New(h.db)

	err := db.WithTx(context.Background(), func(context.Context, *sqlb.DB) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "committing transaction") {
		t.Fatalf("error = %v, want the commit failure surfaced", err)
	}
}

// --- scoped hooks -----------------------------------------------------------

// The point of putting the registry on the handle: two handles can disagree
// about the domain rules without either of them being the process default.
func TestHooksAreScopedToTheHandlesRegistry(t *testing.T) {
	h := txHarness(t)

	scoped := sqlb.NewRegistry()
	sqlb.On[User](scoped).BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
		q.Where(sqlb.F("org_id").Eq("org-scoped"))
		return nil
	})

	// The default registry has no such hook, so the same query differs by
	// handle alone.
	plain := sqlb.New(h.db)
	if _, err := sqlb.Query[User]().All(context.Background(), plain); err != nil {
		t.Fatalf("All: %v", err)
	}
	// org_id is in every select list, so the tell is the WHERE the hook adds.
	if got := h.lastSelect(t); strings.Contains(got, "WHERE") {
		t.Errorf("default registry applied a scoped hook: %s", got)
	}

	tenant := plain.WithHooks(scoped)
	if _, err := sqlb.Query[User]().All(context.Background(), tenant); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := h.lastSelect(t); !strings.Contains(got, `WHERE "org_id" =`) {
		t.Errorf("scoped hook did not apply: %s", got)
	}
}

func TestWithHooksSurvivesIntoTheTransaction(t *testing.T) {
	h := txHarness(t)

	scoped := sqlb.NewRegistry()
	sqlb.On[User](scoped).BeforeQuery(func(_ context.Context, q *sqlb.Builder[User]) error {
		q.Where(sqlb.F("org_id").Eq("org-scoped"))
		return nil
	})

	db := sqlb.New(h.db).WithHooks(scoped)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if tx.Hooks() != scoped {
			t.Error("the transaction handle lost its registry")
		}
		_, err := sqlb.Query[User]().All(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if got := h.lastSelect(t); !strings.Contains(got, `WHERE "org_id" =`) {
		t.Errorf("scoped hook did not apply inside the transaction: %s", got)
	}
}

// The sharper half of the finding this closes: a hook could not previously
// tell it was inside a unit of work, so it could not read uncommitted rows
// written earlier in that unit.
func TestTxFromLetsAHookJoinTheUnitOfWork(t *testing.T) {
	h := txHarness(t)

	scoped := sqlb.NewRegistry()
	var sawTx, ranAtAll bool
	sqlb.On[User](scoped).BeforeCreate(func(ctx context.Context, _ *User) error {
		ranAtAll = true
		tx, ok := sqlb.TxFrom(ctx)
		if !ok {
			return nil
		}
		sawTx = tx.InTx()
		// Reading through the handle the hook was given reaches the
		// uncommitted rows; reading through the pool would not.
		_, err := sqlb.Query[User]().Limit(1).All(ctx, tx)
		return err
	})

	db := sqlb.New(h.db).WithHooks(scoped)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		u := User{Email: "a@b.c"}
		_, err := sqlb.InsertRows(&u).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if !ranAtAll {
		t.Fatal("the BeforeCreate hook did not run")
	}
	if !sawTx {
		t.Error("TxFrom did not reach the hook, so it cannot join the unit of work")
	}
	// The hook's own SELECT lands inside the transaction, between BEGIN and
	// COMMIT, which is the whole point.
	sameStatements(t, h.statements(), []string{"BEGIN", "SELECT", "INSERT", "COMMIT"})
}

func TestTxFromIsAbsentOutsideATransaction(t *testing.T) {
	if _, ok := sqlb.TxFrom(context.Background()); ok {
		t.Error("TxFrom found a transaction in a bare context")
	}
}

// --- after-commit callbacks -------------------------------------------------

func TestAfterCommitRunsOnceCommitted(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	var order []string
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if err := tx.AfterCommit(func(context.Context) error {
			order = append(order, "first")
			return nil
		}); err != nil {
			return err
		}
		if err := tx.AfterCommit(func(context.Context) error {
			order = append(order, "second")
			return nil
		}); err != nil {
			return err
		}
		order = append(order, "inside")
		u := User{Email: "a@b.c"}
		_, err := sqlb.InsertRows(&u).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	want := []string{"inside", "first", "second"}
	sameStatements(t, order, want)
	sameStatements(t, h.statements(), []string{"BEGIN", "INSERT", "COMMIT"})
}

// The whole point of the hook: a side effect the outside world can see must not
// fire for a write that never happened.
func TestAfterCommitDoesNotRunOnRollback(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	fired := false
	err := db.WithTx(context.Background(), func(_ context.Context, tx *sqlb.DB) error {
		if err := tx.AfterCommit(func(context.Context) error {
			fired = true
			return nil
		}); err != nil {
			return err
		}
		return errors.New("aborted")
	})
	if err == nil {
		t.Fatal("expected the caller's error")
	}
	if fired {
		t.Error("an after-commit callback fired for a transaction that rolled back")
	}
}

func TestAfterCommitDoesNotRunWhenCommitFails(t *testing.T) {
	h := txHarness(t)
	h.commitErr = errors.New("serialization failure")
	db := sqlb.New(h.db)

	fired := false
	err := db.WithTx(context.Background(), func(_ context.Context, tx *sqlb.DB) error {
		return tx.AfterCommit(func(context.Context) error {
			fired = true
			return nil
		})
	})
	if err == nil {
		t.Fatal("expected the commit failure")
	}
	if fired {
		t.Error("an after-commit callback fired although the commit failed")
	}
}

// A failing callback must be distinguishable from a failing unit of work: the
// row is durable either way, and retrying the write would double it.
func TestAfterCommitFailureIsDistinguishable(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	boom := errors.New("broker unreachable")
	ran := 0
	err := db.WithTx(context.Background(), func(_ context.Context, tx *sqlb.DB) error {
		if err := tx.AfterCommit(func(context.Context) error { ran++; return boom }); err != nil {
			return err
		}
		// Independent side effects: one failing must not cancel the rest.
		return tx.AfterCommit(func(context.Context) error { ran++; return nil })
	})
	if !errors.Is(err, sqlb.ErrAfterCommit) {
		t.Fatalf("error = %v, want it to wrap ErrAfterCommit", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the callback's own error reachable", err)
	}
	if ran != 2 {
		t.Errorf("ran %d callbacks, want 2 — one failing should not skip the others", ran)
	}
	// The commit still happened, which is what the sentinel exists to say.
	sameStatements(t, h.statements(), []string{"BEGIN", "COMMIT"})
}

// Registering from a hook is the intended use, and the hook only has a context.
func TestAfterCommitFromAHook(t *testing.T) {
	h := txHarness(t)

	scoped := sqlb.NewRegistry()
	published := 0
	sqlb.On[User](scoped).AfterCreate(func(ctx context.Context, _ *User) error {
		return sqlb.AfterCommit(ctx, func(context.Context) error {
			published++
			return nil
		})
	})

	db := sqlb.New(h.db).WithHooks(scoped)
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		u := User{Email: "a@b.c"}
		_, err := sqlb.InsertRows(&u).One(ctx, tx)
		if published != 0 {
			t.Error("the callback fired before the commit")
		}
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if published != 1 {
		t.Errorf("published %d times, want 1", published)
	}
}

// A callback must not find a live handle: the transaction it would name has
// already committed, so anything it ran there would be outside the unit of
// work while looking like it was inside it.
func TestAfterCommitCallbackSeesNoTransaction(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	sawTx := true
	err := db.WithTx(context.Background(), func(_ context.Context, tx *sqlb.DB) error {
		return tx.AfterCommit(func(ctx context.Context) error {
			_, sawTx = sqlb.TxFrom(ctx)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if sawTx {
		t.Error("the callback found a committed transaction in its context")
	}
}

// Callbacks registered by an inner block belong to the one transaction, and
// drain once, when the outermost commit lands.
func TestAfterCommitFromNestedBlockDrainsOnce(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	var order []string
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if err := tx.AfterCommit(func(context.Context) error {
			order = append(order, "outer")
			return nil
		}); err != nil {
			return err
		}
		return tx.WithTx(ctx, func(_ context.Context, inner *sqlb.DB) error {
			return inner.AfterCommit(func(context.Context) error {
				order = append(order, "inner")
				return nil
			})
		})
	})
	if err != nil {
		t.Fatalf("nested WithTx: %v", err)
	}
	sameStatements(t, order, []string{"outer", "inner"})
	sameStatements(t, h.statements(), []string{"BEGIN", "COMMIT"})
}

// An inner rollback discards the outer block's callbacks too, because joining
// means there was only ever one transaction to be after.
func TestAfterCommitDiscardedWhenNestedBlockFails(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	fired := false
	err := db.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		if err := tx.AfterCommit(func(context.Context) error {
			fired = true
			return nil
		}); err != nil {
			return err
		}
		return tx.WithTx(ctx, func(context.Context, *sqlb.DB) error {
			return errors.New("inner failed")
		})
	})
	if err == nil {
		t.Fatal("expected the inner error")
	}
	if fired {
		t.Error("the outer block's callback fired although the transaction rolled back")
	}
}

// "After commit" has no meaning when sqlb does not own the commit. Refusing is
// the accurate answer, and it says what to do instead.
func TestAfterCommitRefusedOutsideATransaction(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	err := db.AfterCommit(func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("expected AfterCommit to refuse a handle with no transaction")
	}
	if !strings.Contains(err.Error(), "WithTx") {
		t.Errorf("error %q should name WithTx", err)
	}

	if err := sqlb.AfterCommit(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("expected the context form to refuse a bare context")
	}
}

func TestAfterCommitRefusedOnceDrained(t *testing.T) {
	h := txHarness(t)
	db := sqlb.New(h.db)

	var escaped *sqlb.DB
	if err := db.WithTx(context.Background(), func(_ context.Context, tx *sqlb.DB) error {
		escaped = tx
		return nil
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	err := escaped.AfterCommit(func(context.Context) error {
		t.Error("a callback registered after the drain still ran")
		return nil
	})
	if err == nil {
		t.Fatal("expected AfterCommit to refuse a transaction that had already committed")
	}
}

// execOnly implements Executor and nothing else, standing in for the tracer
// wrapper the README documents.
type execOnly struct{ inner sqlb.Executor }

func (e execOnly) Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) {
	return e.inner.Query(ctx, q, args...)
}

func (e execOnly) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	return e.inner.Exec(ctx, q, args...)
}
