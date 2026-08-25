package app_test

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb/example/tasks/app"
)

// The change feed, tested where its one real risk lives.
//
// A generated read is confined by a BeforeQuery hook. An invalidation is not: it
// is published by a write, so nothing in hooks.go runs on it, and the only thing
// standing between Alice's row ids and Bob's browser is the filter in events.go.
// These tests are about that filter, and they are over real HTTP against real
// Postgres because a stream is the one thing an in-process handler call cannot
// stand in for.

// liveServer is newServer over a real listener. The change feed needs one: a
// httptest.ResponseRecorder buffers until the handler returns, and this handler
// does not return.
func liveServer(t *testing.T, db *pgxpool.Pool) *httptest.Server {
	t.Helper()
	srv, err := app.New(app.Config{
		DB:     db,
		Secret: secret,
		Log:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("assembling the server: %v", err)
	}
	t.Cleanup(srv.Close)

	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

// The claim the example is arranged around, on the one path that does not get
// it from the hooks: two tenants, one broker, and neither one's stream carries
// the other's row ids.
func TestChangeFeedIsConfinedToTheWorkspace(t *testing.T) {
	ts := liveServer(t, freshDB(t))
	alice := account(t, ts.Config.Handler, "alice@example.com", "Acme")
	bob := account(t, ts.Config.Handler, "bob@example.com", "Globex")

	aliceFeed := subscribe(t, ts, alice.token)
	bobFeed := subscribe(t, ts, bob.token)

	aliceList := alice.listID("Acme work")
	aliceTask := alice.taskID(aliceList, "Acme secret", nil)

	// Alice is told about her own list and task, in the order she wrote them.
	if got := aliceFeed.nextChange(t); got.Table != "lists" || got.Key != aliceList {
		t.Errorf("Alice's first event = %+v, want the list she created", got)
	}
	if got := aliceFeed.nextChange(t); got.Table != "tasks" || got.Key != aliceTask {
		t.Errorf("Alice's second event = %+v, want the task she created", got)
	}

	// Bob is told nothing — and the proof that this is a filter rather than a
	// stream that never worked is below: his own write comes through.
	bobFeed.quiet(t, 300*time.Millisecond)

	bobList := bob.listID("Globex work")
	if got := bobFeed.nextChange(t); got.Table != "lists" || got.Key != bobList {
		t.Errorf("Bob's first event = %+v, want the list he created", got)
	}

	// And Alice is not told about Bob's, which is the same assertion from the
	// other side: her stream has nothing left in it.
	aliceFeed.quiet(t, 300*time.Millisecond)
}

// A delete in this application is an UPDATE — deletes.go stamps deleted_at —
// so it reaches the feed with the row's key rather than as a bare table
// invalidation. The client refetches that id, the read hooks filter the row
// out, and it disappears.
func TestChangeFeedCarriesASoftDelete(t *testing.T) {
	ts := liveServer(t, freshDB(t))
	alice := account(t, ts.Config.Handler, "alice@example.com", "Acme")

	list := alice.listID("Acme work")
	task := alice.taskID(list, "Doomed", nil)

	// Subscribing after the writes, so the only events are the delete's.
	feed := subscribe(t, ts, alice.token)

	alice.delete("/tasks/" + task).expect(http.StatusNoContent)

	got := feed.nextChange(t)
	if got.Table != "tasks" || got.Key != task {
		t.Fatalf("the delete's event = %+v, want the task's key", got)
	}
	if got.Op != "update" {
		t.Errorf("op = %q, want update — a soft delete is an UPDATE, and saying delete would claim the row is gone", got.Op)
	}

	// The row it points at is gone from the caller's view, which is what the
	// client's refetch will find.
	alice.get("/tasks/" + task).expect(http.StatusNotFound)
}

// The stream is not in auth.Middleware's exception list, so it is refused
// without a token like everything else. Proven in both directions: the same
// request with a token opens.
func TestChangeFeedNeedsAToken(t *testing.T) {
	ts := liveServer(t, freshDB(t))
	alice := account(t, ts.Config.Handler, "alice@example.com", "Acme")

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /events: status %d, want 401", resp.StatusCode)
	}

	subscribe(t, ts, alice.token) // opens, or fails the test
}

// The endpoint is in the document, so a generated client learns the event
// shapes rather than that the response is text.
func TestChangeFeedIsDocumented(t *testing.T) {
	ts := liveServer(t, freshDB(t))
	alice := account(t, ts.Config.Handler, "alice@example.com", "Acme")

	doc := alice.get("/openapi.json").expect(http.StatusOK)
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(doc.Body, &spec); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	op, ok := spec.Paths["/events"]["get"]
	if !ok {
		t.Fatalf("the document does not describe GET /events")
	}
	if _, ok := op.Responses["200"].Content["text/event-stream"]; !ok {
		t.Errorf("GET /events is not documented as an event stream")
	}
}

// feed reads change events off a live SSE connection.
type feed struct {
	frames chan map[string]string
}

// changeEvent is the wire shape, decoded. It is spelled out here rather than
// reusing rest.Event so that a change to that struct's tags shows up as a
// failing assertion in the consumer rather than as a silent agreement.
type changeEvent struct {
	Table string `json:"table"`
	Key   string `json:"key"`
	Op    string `json:"op"`
}

// subscribe opens the stream and waits for it to be established, which the
// endpoint signals by committing its headers and sending the retry hint before
// any event. Returning only after that is what lets a caller write immediately
// and still expect the resulting event.
func subscribe(t *testing.T, ts *httptest.Server, token string) *feed {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // the body is the stream; the reader below closes it
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("opening the stream: status %d", resp.StatusCode)
	}

	f := &feed{frames: make(chan map[string]string, 32)}
	opened := make(chan struct{})
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(f.frames)
		sc := bufio.NewScanner(resp.Body)
		cur := map[string]string{}
		first := true
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if first {
					// The opening frame: retry hint and a comment, no data.
					close(opened)
					first = false
				} else if len(cur) > 0 {
					f.frames <- cur
				}
				cur = map[string]string{}
				continue
			}
			name, value, found := strings.Cut(line, ":")
			if !found || name == "" {
				continue // a comment line: the opener, or a heartbeat
			}
			cur[name] = strings.TrimPrefix(value, " ")
		}
	}()

	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never opened")
	}
	return f
}

func (f *feed) nextChange(t *testing.T) changeEvent {
	t.Helper()
	select {
	case frame, open := <-f.frames:
		if !open {
			t.Fatal("the stream ended while a change was expected")
		}
		if frame["event"] != "change" {
			t.Fatalf("got a %q event, want change: %v", frame["event"], frame)
		}
		var e changeEvent
		if err := json.Unmarshal([]byte(frame["data"]), &e); err != nil {
			t.Fatalf("decoding %q: %v", frame["data"], err)
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a change event")
		return changeEvent{}
	}
}

// quiet asserts that nothing arrives within d. It is the half of every
// isolation claim that the positive assertions cannot make.
func (f *feed) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case frame, open := <-f.frames:
		if !open {
			t.Fatal("the stream ended, want it open and quiet")
		}
		t.Fatalf("an event arrived that this subscriber should not see: %v", frame)
	case <-time.After(d):
	}
}
