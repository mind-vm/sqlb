package rest_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// txRecorder is a Publisher that also implements rest.TxPublisher, which is the
// shape outbox.Outbox has and the one PublishChanges asserts for.
//
// Record issues a sentinel statement on the transaction it finds on the context.
// That is what makes "inside the transaction" a testable claim rather than a
// documented intention: the statement lands in the fake's log, and the log says
// whether it came before the COMMIT.
type txRecorder struct {
	mu        sync.Mutex
	recorded  []rest.Event
	published []rest.Event
	sawTx     bool
	err       error
}

func (r *txRecorder) Publish(events ...rest.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published = append(r.published, events...)
}

func (r *txRecorder) Record(ctx context.Context, events ...rest.Event) error {
	r.mu.Lock()
	r.recorded = append(r.recorded, events...)
	err := r.err
	r.mu.Unlock()

	tx, inTx := sqlb.TxFrom(ctx)
	if inTx {
		r.mu.Lock()
		r.sawTx = true
		r.mu.Unlock()
		if _, execErr := tx.Exec(ctx, "OUTBOX APPEND"); execErr != nil {
			return execErr
		}
	}
	return err
}

func (r *txRecorder) state() ([]rest.Event, []rest.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rest.Event(nil), r.recorded...), append([]rest.Event(nil), r.published...), r.sawTx
}

func recording(t *testing.T, db *fakeDB, opts rest.Options, rec *txRecorder) humatest.TestAPI {
	t.Helper()
	scoped := sqlb.NewRegistry()
	if err := rest.PublishChanges[Post](scoped, rec); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	return mount(t, sqlb.New(db.db).WithHooks(scoped), opts)
}

// The assertion that makes the swap from a Broker to an outbox a swap: the same
// PublishChanges call finds Record and uses it, and the event is written before
// the commit rather than after it.
func TestTxPublisherRecordsInsideTheWritingTransaction(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	rec := &txRecorder{}
	api := recording(t, db, postOptions(), rec)

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	recorded, published, sawTx := rec.state()
	if len(recorded) != 1 || recorded[0].Key != "p1" || recorded[0].Op != rest.Created {
		t.Errorf("recorded = %+v, want one create of p1", recorded)
	}
	if len(published) != 0 {
		t.Errorf("published = %+v, want nothing: a TxPublisher in a transaction records instead", published)
	}
	if !sawTx {
		t.Error("Record ran without the writing transaction on its context")
	}

	stmts := db.statements()
	appended, commit := indexOf(stmts, "OUTBOX APPEND"), indexOf(stmts, "COMMIT")
	if appended < 0 || commit < 0 || appended > commit {
		t.Errorf("statements = %v, want the outbox append before the COMMIT", stmts)
	}
}

// The other half of the durability claim, and the reason Record returns an error
// at all: an event that could not be recorded takes the write down with it.
// Otherwise the row exists and no subscriber will ever be told, which is the
// failure the whole feed exists to prevent, arrived at from the other side.
func TestTxPublisherFailureRollsTheWriteBack(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	rec := &txRecorder{err: errors.New("the outbox is unavailable")}
	api := recording(t, db, postOptions(), rec)

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code == http.StatusCreated {
		t.Fatalf("status = %d, want a failure: a change nobody can be told about is not a change", resp.Code)
	}

	stmts := db.statements()
	if len(stmts) == 0 || stmts[len(stmts)-1] != "ROLLBACK" {
		t.Errorf("statements = %v, want them to end in ROLLBACK", stmts)
	}
	if indexOf(stmts, "COMMIT") >= 0 {
		t.Errorf("statements = %v, want no COMMIT", stmts)
	}
}

// ADR-0016: the guard is proven by both directions. With transactions off there
// is no transaction to record into, so the same publisher takes the Publish path
// — which is at-most-once and says so, and is exactly what a Broker would have
// done.
func TestTxPublisherFallsBackToPublishWithoutATransaction(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	opts := postOptions()
	opts.DisableTransactions = true
	rec := &txRecorder{}
	api := recording(t, db, opts, rec)

	resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.Code, resp.Body)
	}

	recorded, published, _ := rec.state()
	if len(recorded) != 0 {
		t.Errorf("recorded = %+v, want nothing: there was no transaction to record into", recorded)
	}
	if len(published) != 1 || published[0].Key != "p1" {
		t.Errorf("published = %+v, want the create to have taken the fallback", published)
	}
}

// And the plain Publisher is untouched: it still announces after the commit,
// which is what keeps this an additive interface rather than a change of
// behaviour for everyone already using a Broker.
func TestPlainPublisherStillAnnouncesAfterTheCommit(t *testing.T) {
	db := newFakeDB(t, reply{cols: postCols(), rows: rowsOf(postRow("p1", "Hello"))})
	api, rec := publishing(t, db, postOptions())

	if resp := api.Post("/posts", map[string]any{"org_id": "acme", "title": "Hello", "body": "b"}); resp.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body)
	}

	if got := rec.events(); len(got) != 1 {
		t.Fatalf("events = %+v, want one", got)
	}
	stmts := db.statements()
	if indexOf(stmts, "COMMIT") < 0 {
		t.Errorf("statements = %v, want a COMMIT", stmts)
	}
	if indexOf(stmts, "OUTBOX APPEND") >= 0 {
		t.Errorf("statements = %v, want no outbox append from a plain Publisher", stmts)
	}
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if strings.Contains(s, needle) {
			return i
		}
	}
	return -1
}
