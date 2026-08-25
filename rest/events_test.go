package rest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/rest"
)

// frame is one SSE block: the fields up to a blank line.
type frame struct {
	id       string
	event    string
	data     string
	retry    string
	comments []string
}

func (f frame) isComment() bool { return f.data == "" && f.event == "" }

// stream reads SSE frames off a live connection.
//
// It reads on its own goroutine because the test has to assert on what has
// *not* arrived as well as on what has — a heartbeat that never fires and a
// filtered event that was sent anyway are both silences, and a blocking read
// cannot tell them apart from a slow one.
type stream struct {
	frames chan frame
	cancel context.CancelFunc
	errs   chan error
}

func openStream(t *testing.T, url, lastEventID string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	// nolint:bodyclose — the body outlives this function by design: it is the
	// stream, and it is closed by the reader goroutine below and by every error
	// path here. A defer would close it before the first frame was read.
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose
	if err != nil {
		cancel()
		t.Fatalf("opening the stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("opening the stream: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		_ = resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	s := &stream{frames: make(chan frame, 32), cancel: cancel, errs: make(chan error, 1)}
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(s.frames)
		sc := bufio.NewScanner(resp.Body)
		var cur frame
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				s.frames <- cur
				cur = frame{}
				continue
			}
			name, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch name {
			case "":
				cur.comments = append(cur.comments, value)
			case "id":
				cur.id = value
			case "event":
				cur.event = value
			case "data":
				cur.data = value
			case "retry":
				cur.retry = value
			}
		}
		s.errs <- sc.Err()
	}()

	t.Cleanup(func() { cancel() })
	return s
}

// next returns the next frame of any kind.
func (s *stream) next(t *testing.T) frame {
	t.Helper()
	select {
	case f, open := <-s.frames:
		if !open {
			t.Fatal("the stream ended while a frame was expected")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return frame{}
	}
}

// nextEvent skips comment-only frames, which are the retry hint and the
// heartbeats.
func (s *stream) nextEvent(t *testing.T) frame {
	t.Helper()
	for {
		f := s.next(t)
		if !f.isComment() {
			return f
		}
	}
}

// quiet asserts no event arrives within d. Comment frames do not count.
func (s *stream) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, open := <-s.frames:
			if !open {
				t.Fatal("the stream ended, want it open and quiet")
			}
			if !f.isComment() {
				t.Fatalf("an unexpected event arrived: %+v", f)
			}
		case <-deadline:
			return
		}
	}
}

func (f frame) decode(t *testing.T) rest.Event {
	t.Helper()
	var e rest.Event
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		t.Fatalf("decoding %q: %v", f.data, err)
	}
	return e
}

// eventServer mounts the change feed alone, on the real net/http adapter.
// humatest's recorder cannot stand in here: a stream that is buffered until the
// handler returns is not a stream, and the handler does not return.
func eventServer(t *testing.T, opts rest.EventsOptions) (*httptest.Server, *rest.Broker) {
	t.Helper()
	b := rest.NewBroker(rest.BrokerOptions{History: 8})
	if opts.Source == nil {
		opts.Source = b
	}
	srv := rest.NewServer(rest.Config{Title: "Test", Version: "1.0.0"})
	if err := rest.Events(srv.API, opts); err != nil {
		t.Fatalf("mounting the events endpoint: %v", err)
	}
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	t.Cleanup(b.Close)
	return ts, b
}

func TestEventsStreamsAChange(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{})
	s := openStream(t, ts.URL+"/events", "")

	// The stream commits its headers before anything is published, so a client
	// knows it is connected rather than waiting on the first write.
	if opening := s.next(t); opening.retry == "" {
		t.Errorf("the stream opened without a retry hint: %+v", opening)
	}

	waitFor(t, "the subscription to register", func() bool { return b.Subscribers() == 1 })
	b.Publish(change("posts", "p1", rest.Created))

	f := s.nextEvent(t)
	if f.event != "change" {
		t.Errorf("event = %q, want change", f.event)
	}
	if f.id != "1" {
		t.Errorf("id = %q, want 1", f.id)
	}
	if got := f.decode(t); got != change("posts", "p1", rest.Created) {
		t.Errorf("payload = %+v", got)
	}
}

// The reconnection contract: EventSource resends the last id it saw, and the
// events since then arrive rather than being lost to the disconnection.
func TestEventsResumesFromLastEventID(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{})
	b.Publish(
		change("posts", "p1", rest.Created),
		change("posts", "p2", rest.Updated),
		change("posts", "p3", rest.Deleted),
	)

	s := openStream(t, ts.URL+"/events", "1")

	for _, want := range []rest.Event{
		change("posts", "p2", rest.Updated),
		change("posts", "p3", rest.Deleted),
	} {
		f := s.nextEvent(t)
		if f.event != "change" {
			t.Fatalf("event = %q, want change: %+v", f.event, f)
		}
		if got := f.decode(t); got != want {
			t.Errorf("resumed payload = %+v, want %+v", got, want)
		}
	}
}

// A client away long enough for its position to fall out of the replay history
// is told to refetch, rather than reconnecting into a stream that silently
// starts mid-story.
func TestEventsResetsWhenThePositionCannotBeResumed(t *testing.T) {
	short := rest.NewBroker(rest.BrokerOptions{History: 1})
	t.Cleanup(short.Close)
	ts, _ := eventServer(t, rest.EventsOptions{Source: short})

	for _, key := range []string{"p1", "p2", "p3"} {
		short.Publish(change("posts", key, rest.Created))
	}

	s := openStream(t, ts.URL+"/events", "1") // only event 3 is still held
	f := s.nextEvent(t)
	if f.event != "reset" {
		t.Fatalf("event = %q, want reset: %+v", f.event, f)
	}
	// The reset carries the current position, so the client's next
	// reconnection resumes from where the stream is rather than from the gap.
	if f.id != "3" {
		t.Errorf("reset id = %q, want 3", f.id)
	}
}

// A Last-Event-ID that is not a number is a client with a position we cannot
// identify. Resuming from zero would look exactly like a working stream while
// skipping everything in between.
func TestEventsResetOnUnreadableLastEventID(t *testing.T) {
	ts, _ := eventServer(t, rest.EventsOptions{})
	s := openStream(t, ts.URL+"/events", "not-a-position")

	f := s.nextEvent(t)
	if f.event != "reset" {
		t.Fatalf("event = %q, want reset: %+v", f.event, f)
	}
	var r rest.Reset
	if err := json.Unmarshal([]byte(f.data), &r); err != nil {
		t.Fatalf("decoding the reset: %v", err)
	}
	if r.Reason == "" {
		t.Error("the reset carries no reason")
	}
}

// And the other direction: a readable position on a fresh stream is not a reset.
func TestEventsDoesNotResetAReadablePosition(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{})
	b.Publish(change("posts", "p1", rest.Created))

	s := openStream(t, ts.URL+"/events", "1")
	s.quiet(t, 150*time.Millisecond)
}

func TestEventsHeartbeatsAnIdleStream(t *testing.T) {
	ts, _ := eventServer(t, rest.EventsOptions{Heartbeat: 20 * time.Millisecond})
	s := openStream(t, ts.URL+"/events", "")

	// The opening frame carries the retry hint; what follows on an idle stream
	// is heartbeats, which is what stops an intermediary reclaiming the
	// connection.
	s.next(t)
	f := s.next(t)
	if !f.isComment() {
		t.Fatalf("want a heartbeat comment, got %+v", f)
	}
	if len(f.comments) == 0 {
		t.Errorf("the heartbeat frame carried no comment line: %+v", f)
	}
}

func TestEventsAppliesTheFilter(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{
		Filter: func(_ context.Context, e rest.Event) bool { return e.Table == "posts" },
	})
	s := openStream(t, ts.URL+"/events", "")
	s.next(t)
	waitFor(t, "the subscription to register", func() bool { return b.Subscribers() == 1 })

	b.Publish(
		change("secrets", "s1", rest.Created),
		change("posts", "p1", rest.Created),
	)

	f := s.nextEvent(t)
	if got := f.decode(t); got.Table != "posts" {
		t.Errorf("a filtered-out event reached the subscriber: %+v", got)
	}
	// The filtered event's id is not written, so the client's position is the
	// last one it was actually shown.
	if f.id != "2" {
		t.Errorf("id = %q, want 2 — the id is the source's position, not a per-client count", f.id)
	}
}

// Without a filter every event is delivered, which is the default the Filter
// documentation warns about.
func TestEventsWithoutAFilterDeliversEverything(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{})
	s := openStream(t, ts.URL+"/events", "")
	s.next(t)
	waitFor(t, "the subscription to register", func() bool { return b.Subscribers() == 1 })

	b.Publish(change("secrets", "s1", rest.Created))
	if got := s.nextEvent(t).decode(t); got.Table != "secrets" {
		t.Errorf("payload = %+v, want the secrets event", got)
	}
}

func TestEventsReleasesTheSubscriptionOnDisconnect(t *testing.T) {
	ts, b := eventServer(t, rest.EventsOptions{})
	s := openStream(t, ts.URL+"/events", "")
	s.next(t)
	waitFor(t, "the subscription to register", func() bool { return b.Subscribers() == 1 })

	s.cancel()
	waitFor(t, "the subscription to be released", func() bool { return b.Subscribers() == 0 })
}

func TestEventsRequiresASource(t *testing.T) {
	srv := rest.NewServer(rest.Config{})
	if err := rest.Events(srv.API, rest.EventsOptions{}); err == nil {
		t.Fatal("Events accepted an EventsOptions with no Source")
	}
	if err := rest.Events(srv.API, rest.EventsOptions{Source: rest.NewBroker(rest.BrokerOptions{}), Path: "events"}); err == nil {
		t.Fatal("Events accepted a Path without a leading slash")
	}
}

// The endpoint is in the document, with both event shapes, because a client
// generated from the document should learn what a change looks like rather than
// that the response is text.
func TestEventsIsDocumented(t *testing.T) {
	ts, _ := eventServer(t, rest.EventsOptions{})
	body := get(t, ts, "/openapi.json")

	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	op, ok := doc.Paths["/events"]["get"]
	if !ok {
		t.Fatalf("the document does not describe GET /events: %s", body)
	}
	if _, ok := op.Responses["200"].Content["text/event-stream"]; !ok {
		t.Fatalf("the 200 response is not documented as an event stream: %s", body)
	}
	for _, want := range []string{"Event change", "Event reset"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the document does not carry the %q schema", want)
		}
	}
}
