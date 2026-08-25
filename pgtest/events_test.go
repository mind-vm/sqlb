package pgtest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// Note is the smallest resource that exercises the change feed: a
// database-generated key, so the event has to carry what Postgres decided
// rather than what the request sent.
type Note struct {
	ID   int64  `db:"id" json:"id" sqlb:"pk,default,filter,readonly"`
	Body string `db:"body" json:"body" sqlb:"filter"`
}

func (Note) TableName() string { return "notes" }

type NoteCreate struct {
	Body string `json:"body"`
}

func (c NoteCreate) Row() (*Note, error) { return &Note{Body: c.Body}, nil }

// notesServer is the whole path under test: real Postgres, real transaction,
// generated handlers, and the stream on the same mux.
func notesServer(t *testing.T) (*httptest.Server, *rest.Broker) {
	t.Helper()
	pool := freshStockDB(t)
	// The unique constraint is DEFERRABLE INITIALLY DEFERRED on purpose. It is
	// what gives this package a rollback that happens *after* a successful
	// INSERT — the insert lands, the AfterCreate hook runs, and COMMIT is what
	// fails. Nothing in the rest package's fake driver can produce that
	// ordering, and it is the exact ordering a phantom event needs.
	mustExec(t, pool, `
		CREATE TABLE notes (
			id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			body text   NOT NULL,
			CONSTRAINT notes_body_key UNIQUE (body) DEFERRABLE INITIALLY DEFERRED
		)
	`)

	broker := rest.NewBroker(rest.BrokerOptions{})
	t.Cleanup(broker.Close)

	// A registry scoped to this test rather than the process default, so a
	// second test in this package does not inherit the publisher.
	scoped := sqlb.NewRegistry()
	if err := rest.PublishChanges[Note](scoped, broker); err != nil {
		t.Fatalf("registering the publisher: %v", err)
	}
	db := sqlb.New(pool).WithHooks(scoped)

	srv := rest.NewServer(rest.Config{Title: "Notes", Version: "1.0.0"})
	if err := rest.Resource[Note, NoteCreate, rest.None[Note]](srv.API, db, rest.Options{
		Path: "/notes",
		Name: "note",
		Ops:  rest.OpCreate | rest.OpRead | rest.OpDelete | rest.OpList,
	}); err != nil {
		t.Fatalf("mounting the resource: %v", err)
	}
	if err := rest.Events(srv.API, rest.EventsOptions{Source: broker}); err != nil {
		t.Fatalf("mounting the events endpoint: %v", err)
	}

	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts, broker
}

// The fake driver in the rest package proves the hook fires and the event is
// registered for after the commit. Only a real Postgres proves the rest of it:
// that the commit actually happens before the fan-out, that the id in the event
// is the one the database generated, and that a rolled-back write reaches no
// subscriber at all.
func TestChangeFeedFromAGeneratedWrite(t *testing.T) {
	t.Parallel()
	ts, broker := notesServer(t)

	events := openEventStream(t, ts.URL+"/events")
	waitUntil(t, "the subscription to register", func() bool { return broker.Subscribers() == 1 })

	status, body := postJSON(t, ts.URL+"/notes", map[string]any{"body": "hello"})
	if status != http.StatusCreated {
		t.Fatalf("POST /notes: status %d: %s", status, body)
	}
	var created Note
	decodeInto(t, body, &created)
	if created.ID == 0 {
		t.Fatal("the database generated no id, so the event has nothing to carry")
	}

	got := events.nextChange(t)
	want := rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Created}
	if got != want {
		t.Errorf("event = %+v, want %+v", got, want)
	}

	// And the row the event points at is fetchable, which is the entire
	// contract: the client refetches through the ordinary endpoint.
	fetched := getJSON(t, ts.URL+"/notes/"+got.Key)
	if fetched["body"] != "hello" {
		t.Errorf("refetching the key from the event gave %v", fetched)
	}
}

// A delete announces the row it removed, key included — which needs real
// Postgres, because the key is one the database returned from a DELETE …
// RETURNING that only exists because the publisher registered a row-taking hook
// (#144), and it has to survive the same commit boundary as the created event.
func TestChangeFeedOnDelete(t *testing.T) {
	t.Parallel()
	ts, broker := notesServer(t)

	status, body := postJSON(t, ts.URL+"/notes", map[string]any{"body": "doomed"})
	if status != http.StatusCreated {
		t.Fatalf("POST /notes: status %d: %s", status, body)
	}
	var created Note
	decodeInto(t, body, &created)

	// Subscribing after the create, so the only event in this stream is the
	// delete.
	events := openEventStream(t, ts.URL+"/events")
	waitUntil(t, "the subscription to register", func() bool { return broker.Subscribers() == 1 })

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/notes/"+itoa(created.ID), nil)
	if err != nil {
		t.Fatalf("building the delete: %v", err)
	}
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_ = del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status %d", del.StatusCode)
	}

	got := events.nextChange(t)
	if want := (rest.Event{Table: "notes", Key: itoa(created.ID), Op: rest.Deleted}); got != want {
		t.Errorf("event = %+v, want %+v", got, want)
	}
}

// The guard that makes the feed worth trusting, proven where only Postgres can
// prove it: the INSERT succeeds, the hook runs and queues the event, and then
// COMMIT fails on a deferred constraint. A feed that published from inside the
// mutation would announce a note that does not exist, and a client refetching
// on it would read nothing and cache the contradiction.
//
// Three writes, of which the middle one aborts. The assertion is on the second
// event's *key*, and that choice is load-bearing: the aborted insert consumes an
// identity value, so if it published, the second event would carry that row's
// key rather than the third write's. Asserting only on the stream position would
// not catch it, because the phantom event would occupy exactly the position the
// third write's event is expected at.
func TestChangeFeedIsSilentWhenTheCommitFails(t *testing.T) {
	t.Parallel()
	ts, broker := notesServer(t)

	events := openEventStream(t, ts.URL+"/events")
	waitUntil(t, "the subscription to register", func() bool { return broker.Subscribers() == 1 })

	if status, body := postJSON(t, ts.URL+"/notes", map[string]any{"body": "taken"}); status != http.StatusCreated {
		t.Fatalf("first POST: status %d: %s", status, body)
	}
	if got := events.nextChange(t); events.idOfLast() != "1" || got.Op != rest.Created {
		t.Fatalf("first event = %+v at id %q, want a create at 1", got, events.idOfLast())
	}

	// Same body. The insert is accepted, and the deferred constraint rejects it
	// at COMMIT.
	doomed, _ := postJSON(t, ts.URL+"/notes", map[string]any{"body": "taken"})
	if doomed == http.StatusCreated {
		t.Fatal("the duplicate was accepted; the constraint is not doing its job and this test proves nothing")
	}

	status, body := postJSON(t, ts.URL+"/notes", map[string]any{"body": "fine"})
	if status != http.StatusCreated {
		t.Fatalf("third POST: status %d: %s", status, body)
	}
	var survived Note
	decodeInto(t, body, &survived)

	got := events.nextChange(t)
	if want := (rest.Event{Table: "notes", Key: itoa(survived.ID), Op: rest.Created}); got != want {
		t.Errorf("the event after the aborted write = %+v, want %+v — the abort published a note that does not exist", got, want)
	}
	if id := events.idOfLast(); id != "2" {
		t.Errorf("that event has stream id %q, want 2", id)
	}
}

// eventStream reads change events off a live SSE connection.
type eventStream struct {
	frames chan map[string]string
	lastID string
}

func openEventStream(t *testing.T, url string) *eventStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the body is the stream; the reader below closes it
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("opening the stream: status %d", resp.StatusCode)
	}

	s := &eventStream{frames: make(chan map[string]string, 16)}
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		cur := map[string]string{}
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(cur) > 0 {
					s.frames <- cur
				}
				cur = map[string]string{}
				continue
			}
			name, value, found := strings.Cut(line, ":")
			if !found || name == "" {
				continue // a comment: the retry hint's opener, or a heartbeat
			}
			cur[name] = strings.TrimPrefix(value, " ")
		}
	}()
	return s
}

// nextChange returns the payload of the next `change` event, failing the test on
// anything else so that a reset does not read as a missing event.
func (s *eventStream) nextChange(t *testing.T) rest.Event {
	t.Helper()
	for {
		select {
		case f, open := <-s.frames:
			if !open {
				t.Fatal("the stream ended while a change was expected")
			}
			if f["data"] == "" {
				continue
			}
			if f["event"] != "change" {
				t.Fatalf("got a %q event, want change: %v", f["event"], f)
			}
			s.lastID = f["id"]
			var e rest.Event
			if err := json.Unmarshal([]byte(f["data"]), &e); err != nil {
				t.Fatalf("decoding %q: %v", f["data"], err)
			}
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for a change event")
		}
	}
}

func (s *eventStream) idOfLast() string { return s.lastID }

// postJSON sends a create and reads the whole response, so the caller is left
// with a status and bytes rather than a body it has to remember to close.
func postJSON(t *testing.T, url string, body map[string]any) (int, []byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response to POST %s: %v", url, err)
	}
	return resp.StatusCode, read
}

func decodeInto(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	return out
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
