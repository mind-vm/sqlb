package app_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/example/tasks/app"
	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// The comment hooks register an AfterCommit callback, and the promise attached
// to it is that the callback runs if and only if the write it follows was
// committed. sqlb's own db_test.go proves that for the mechanism; these two
// prove it for this application's hooks, which is a different claim — they are
// the ones that decide what gets registered and when.
//
// The engine tests cover the mechanism. What they cannot cover is whether
// *these* hooks put the callback in the right place relative to the write.

// recorder is a slog.Handler that keeps what it is handed, so a test can assert
// on a side effect whose only observable form is a log line.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Message)
	return nil
}

func (r *recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *recorder) WithGroup(string) slog.Handler      { return r }

func (r *recorder) count(msg string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// TestAfterCommitIsDiscardedWhenTheUnitOfWorkFails is the case the HTTP path
// cannot reach.
//
// A request refused by BeforeCreate never gets as far as registering a
// callback, so "no notification" there proves only that the hook stopped early.
// The interesting case is the other one: the insert succeeded, AfterCreate ran,
// the counter moved and the callback was registered — and then the unit of work
// failed anyway. Everything has to come back, including the announcement.
//
// The failure is forced rather than provoked through an endpoint, because
// nothing in this application fails at that point on purpose. Forcing it is the
// only way to assert the guarantee rather than assume it.
func TestAfterCommitIsDiscardedWhenTheUnitOfWorkFails(t *testing.T) {
	db := freshDB(t)
	server := newServer(t, db)

	// Set the row up over HTTP, so the fixture is built the way the application
	// builds one.
	alice := account(t, server, "alice@example.com", "Acme")
	list := alice.listID("Backlog")
	task := alice.taskID(list, "Needs discussion", nil)

	me := alice.get("/auth/me").expect(http.StatusOK).item()
	user := me["user"].(map[string]any)
	workspace := me["workspace"].(map[string]any)

	// A handle carrying this application's real hooks, with somewhere to record
	// the notification they emit.
	log := &recorder{}
	hooks := app.Register(slog.New(log))
	handle := sqlb.New(db).WithHooks(hooks)

	// The hooks read identity from the context, exactly as they do behind the
	// middleware. This is the seam that makes them usable outside HTTP at all.
	ctx := auth.WithClaims(context.Background(), auth.Claims{
		Subject:   user["id"].(string),
		Email:     "alice@example.com",
		Workspace: workspace["id"].(string),
		Role:      auth.RoleOwner,
	})

	boom := errors.New("something after the insert went wrong")
	err := handle.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		if _, err := sqlb.InsertRows(&tasks.Comment{
			TaskID: task,
			Body:   "this must not survive",
		}).One(ctx, tx); err != nil {
			return err
		}
		// At this point AfterCreate has run: the counter moved and the callback
		// is registered. Both are about to be undone.
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx err = %v, want the forced failure", err)
	}

	// The callback was registered and must not have run.
	if n := log.count("comment posted"); n != 0 {
		t.Errorf("the notification fired %d times for a rolled-back write", n)
	}

	// And neither half of the write survived.
	var comments int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM comments`).Scan(&comments); err != nil {
		t.Fatalf("counting comments: %v", err)
	}
	if comments != 0 {
		t.Errorf("%d comments survived the rollback", comments)
	}

	var count int32
	if err := db.QueryRow(context.Background(),
		`SELECT comment_count FROM tasks WHERE id = $1`, task).Scan(&count); err != nil {
		t.Fatalf("reading comment_count: %v", err)
	}
	if count != 0 {
		t.Errorf("comment_count = %d after a rollback, want 0", count)
	}
}

// TestAfterCommitFiresOncePerCommittedComment is the other direction, and the
// one that stops the test above from passing for the wrong reason: a callback
// that never fires at all would satisfy it.
func TestAfterCommitFiresOncePerCommittedComment(t *testing.T) {
	db := freshDB(t)

	log := &recorder{}
	server, err := app.New(app.Config{DB: db, Secret: secret, Log: slog.New(log)})
	if err != nil {
		t.Fatalf("assembling the server: %v", err)
	}

	alice := account(t, server.Handler, "alice@example.com", "Acme")
	list := alice.listID("Backlog")
	task := alice.taskID(list, "Needs discussion", nil)

	for range 3 {
		alice.post("/comments", map[string]any{
			"task_id": task, "body": "real",
		}).expect(http.StatusCreated)
	}
	if n := log.count("comment posted"); n != 3 {
		t.Errorf("the notification fired %d times for 3 committed comments, want 3", n)
	}

	// A request refused before the insert adds nothing, which is what the HTTP
	// path can show: the count stays where it was.
	bob := account(t, server.Handler, "bob@example.com", "Globex")
	bob.post("/comments", map[string]any{"task_id": task, "body": "nope"}).
		expect(http.StatusNotFound)

	if n := log.count("comment posted"); n != 3 {
		t.Errorf("the notification fired %d times after a refused comment, want 3", n)
	}
}
