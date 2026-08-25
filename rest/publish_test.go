package rest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// recorder is a Publisher that remembers, so a test can assert on what a write
// announced without standing up a stream.
type recorder struct {
	mu   sync.Mutex
	seen []rest.Event
}

func (r *recorder) Publish(events ...rest.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, events...)
}

func (r *recorder) events() []rest.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rest.Event(nil), r.seen...)
}

// publishing mounts the Post resource with its writes announced to a recorder,
// against a hook registry scoped to this test so registrations do not leak into
// the next one.
func publishing(t *testing.T, db *fakeDB, opts rest.Options) (humatest.TestAPI, *recorder) {
	t.Helper()
	rec := &recorder{}
	scoped := sqlb.NewRegistry()
	if err := rest.PublishChanges[Post](scoped, rec); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	return mount(t, sqlb.New(db.db).WithHooks(scoped), opts), rec
}

func TestPublishOnCreate(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api, rec := publishing(t, db, postOptions())

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	want := []rest.Event{{Table: "posts", Key: "p1", Op: rest.Created}}
	assertEvents(t, rec.events(), want)
}

// The key comes off the stored row rather than off the request, which is the
// only way a database-generated id can appear in the event at all.
func TestPublishCarriesTheStoredKey(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("generated-by-postgres", "Hello"))})
	api, rec := publishing(t, db, postOptions())

	if resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body)
	}
	got := rec.events()
	if len(got) != 1 || got[0].Key != "generated-by-postgres" {
		t.Errorf("events = %+v, want the key the database returned", got)
	}
}

func TestPublishOnUpdate(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "New"))})
	api, rec := publishing(t, db, postOptions())

	resp := api.Patch("/posts/p1", map[string]any{"title": "New"})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}
	assertEvents(t, rec.events(), []rest.Event{{Table: "posts", Key: "p1", Op: rest.Updated}})
}

// A delete announces the row it removed, key and all.
//
// It used to announce the table and nothing else, because sqlb's AfterDelete
// hook is handed a count rather than the rows. AfterDeleteRows is what closed
// that (#144): a subscriber keyed on the row now has something to key on, and a
// tenant filter has a scope to compare.
func TestPublishOnDeleteNamesTheRow(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api, rec := publishing(t, db, postOptions())

	resp := api.Delete("/posts/p1")
	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	assertEvents(t, rec.events(), []rest.Event{{Table: "posts", Key: "p1", Op: rest.Deleted}})
}

// And the statement that made that possible is the one that went to the
// database: registering the row-taking hook is what puts RETURNING on a DELETE.
//
// Asserted here rather than only in the engine's tests because this is the path
// that pays for it — a project mounting PublishChanges opts into the scan for
// every delete of that model, and a regression that dropped the clause would
// turn every delete event keyless again while every test above still passed on
// the fake's canned reply.
func TestPublishMakesADeleteReturnItsRows(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api, _ := publishing(t, db, postOptions())

	if resp := api.Delete("/posts/p1"); resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.Code, resp.Body)
	}
	var found bool
	for _, q := range db.statements() {
		if strings.HasPrefix(q, "DELETE") && strings.Contains(q, "RETURNING") {
			found = true
		}
	}
	if !found {
		t.Errorf("no DELETE … RETURNING was sent, so the event could not have named a row: %v", db.statements())
	}
}

// The guard that makes the feed worth trusting: a write that rolls back
// announces nothing. A client refetching on a phantom event would read the row
// unchanged and cache the contradiction.
func TestPublishIsSuppressedByARollback(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})

	rec := &recorder{}
	scoped := sqlb.NewRegistry()
	// Registered first, so its after-commit callback is queued before the hook
	// that aborts. If publication were not tied to the commit, this is exactly
	// the ordering that would leak a phantom event.
	if err := rest.PublishChanges[Post](scoped, rec); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	sqlb.On[Post](scoped).AfterCreate(func(context.Context, *Post) error {
		return errors.New("a validation the database could not express")
	})

	api := mount(t, sqlb.New(db.db).WithHooks(scoped), postOptions())
	if resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"}); resp.Code == http.StatusCreated {
		t.Fatalf("the aborted write reported 201: %s", resp.Body)
	}
	if got := rec.events(); len(got) != 0 {
		t.Errorf("a rolled-back write published %+v", got)
	}
}

// A delete that matched nothing changed nothing, so every subscriber refetching
// a collection identical to the one it holds is pure waste.
//
// Two paths reach that, and only one of them is the hook's own check. The REST
// handler reports a miss as an error from inside the unit of work, so the
// transaction rolls back and the callback is discarded before the count is ever
// consulted. A delete issued directly commits, and there the count is the only
// thing standing between a no-op and a fan-out.
func TestPublishSkipsADeleteThatMatchedNothing(t *testing.T) {
	t.Run("through the REST handler, which answers 404", func(t *testing.T) {
		db := newFakeDB(t, reply{cols: postCols()})
		api, rec := publishing(t, db, postOptions())

		if resp := api.Delete("/posts/p1"); resp.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body)
		}
		if got := rec.events(); len(got) != 0 {
			t.Errorf("a delete that matched nothing published %+v", got)
		}
	})

	t.Run("through a direct delete, which commits", func(t *testing.T) {
		db := newFakeDB(t, reply{cols: postCols()})
		rec := &recorder{}
		scoped := sqlb.NewRegistry()
		if err := rest.PublishChanges[Post](scoped, rec); err != nil {
			t.Fatalf("registering the publisher: %v", err)
		}

		handle := sqlb.New(db.db).WithHooks(scoped)
		err := handle.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
			n, err := sqlb.DeleteRows[Post]().Where(sqlb.F("id").Eq("nobody")).Exec(ctx, tx)
			if err != nil {
				return err
			}
			if n != 0 {
				t.Fatalf("the fake reported %d rows deleted, want 0", n)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("the delete failed: %v", err)
		}
		if got := rec.events(); len(got) != 0 {
			t.Errorf("a committed delete that matched nothing published %+v", got)
		}
	})
}

// Under autocommit there is no commit left to be after — the statement is
// already durable when the hook runs — so the event still goes out. A resource
// that opted out of transactions should not silently lose its change feed too.
func TestPublishUnderAutocommit(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	opts := postOptions()
	opts.DisableTransactions = true
	api, rec := publishing(t, db, opts)

	if resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}
	assertEvents(t, rec.events(), []rest.Event{{Table: "posts", Key: "p1", Op: rest.Created}})
}

func TestPublishChangesRejectsANilPublisher(t *testing.T) {
	if err := rest.PublishChanges[Post](sqlb.NewRegistry(), nil); err == nil {
		t.Error("PublishChanges accepted a nil Publisher")
	}
	if err := rest.PublishChanges[Post](nil, &recorder{}); err == nil {
		t.Error("PublishChanges accepted a nil registry")
	}
}

// A write that is not a REST request publishes too. This is the reason the
// registration is on hooks rather than on the handlers: the background job and
// the admin script are exactly the writes a client would otherwise never hear
// about.
func TestPublishFromAWriteThatIsNotARequest(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})

	rec := &recorder{}
	scoped := sqlb.NewRegistry()
	if err := rest.PublishChanges[Post](scoped, rec); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}

	handle := sqlb.New(db.db).WithHooks(scoped)
	err := handle.WithTx(context.Background(), func(ctx context.Context, tx *sqlb.DB) error {
		_, err := sqlb.InsertRows(&Post{OrgID: "acme", Title: "Hello"}).One(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	assertEvents(t, rec.events(), []rest.Event{{Table: "posts", Key: "p1", Op: rest.Created}})
}

func assertEvents(t *testing.T, got, want []rest.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("published %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A model that declares a scope column carries its value on every event, off
// the wire, so that a Filter can answer "is this event mine" without the
// endpoint knowing what a tenant is.
func TestPublishCarriesTheDeclaredScope(t *testing.T) {
	db := newFakeDB(t, reply{
		cols: []string{"id", "org_id", "title", "deleted_at"},
		rows: [][]any{{"s1", "acme", "Hello", nil}},
	})

	rec := &recorder{}
	reg := sqlb.NewRegistry()
	if err := rest.PublishChanges[Scoped](reg, rec); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	// The obligations the mount check requires of a Scoped model. Their bodies
	// are irrelevant here — what is under test is the event, not the boundary.
	sqlb.On[Scoped](reg).
		BeforeQuery(func(context.Context, *sqlb.Builder[Scoped]) error { return nil }).
		BeforeCreate(func(context.Context, *Scoped) error { return nil }).
		BeforeUpdate(func(context.Context, *sqlb.Update[Scoped]) error { return nil }).
		BeforeDelete(func(context.Context, *sqlb.Delete[Scoped]) error { return nil })

	_, api := humatest.New(t, huma.DefaultConfig("Test", "1.0.0"))
	if err := rest.Resource[Scoped, scopedCreate, scopedUpdate](
		api, sqlb.New(db.db).WithHooks(reg), scopedOptions()); err != nil {
		t.Fatalf("mounting: %v", err)
	}

	if resp := api.Post("/scoped", map[string]any{"title": "Hello"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	got := rec.events()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1: %+v", len(got), got)
	}
	if got[0].Scope != "acme" {
		t.Errorf("Scope = %q, want the org_id the row came back with", got[0].Scope)
	}

	// And it stays off the wire: a subscriber gains nothing from being told its
	// own tenant id, and the wire is the expensive half to change.
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("encoding the event: %v", err)
	}
	if strings.Contains(string(encoded), "acme") {
		t.Errorf("the scope reached the wire: %s", encoded)
	}
}

// A model with no scope column publishes an empty one rather than failing, so
// that a single-tenant application needs no scope declaration to use the feed.
func TestPublishLeavesTheScopeEmptyWhenNoneIsDeclared(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api, rec := publishing(t, db, postOptions())

	if resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body)
	}
	if got := rec.events(); len(got) != 1 || got[0].Scope != "" {
		t.Errorf("events = %+v, want one with an empty Scope — Post declares no scope column", got)
	}
}
