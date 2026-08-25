// Package cli's tests are hand-written; cli_gen.go beside them is not.
//
// They exist because the emitter's own tests in codegen assert what was
// *written*, and the claim this package actually makes is about what goes over
// the wire: that the flags compose into the filter grammar the server parses,
// that --all walks by cursor, and that a rejection reaches the operator with
// the allow-list intact. Those are round trips, so they are tested against an
// httptest server rather than a fixture — no Docker, no Postgres, and no
// database in the binary at all, which is the point of a client that speaks
// HTTP.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/example/tasks/cli/client"
)

// run executes one command line against a stub server and returns stdout.
func run(t *testing.T, handler http.HandlerFunc, args ...string) (string, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	var out bytes.Buffer
	root := New(&client.Client{BaseURL: server.URL})
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// emptyPage is the smallest response that satisfies a list command.
func emptyPage(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"items":[],"page":1,"per_page":25,"has_more":false}`))
}

// The flags compose into the grammar filter.Parse reads: a repeated flag
// conjoins its conditions, sort and select are comma-separated in one
// parameter, and per_page is spelled with an underscore however the flag is.
func TestListFlagsEncodeTheFilterGrammar(t *testing.T) {
	var got url.Values
	_, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		emptyPage(w)
	},
		"tasks", "list",
		"--status", "eq.todo",
		"--comment-count", "gte.10", "--comment-count", "lt.100",
		"--sort", "-created_at,title",
		"--select", "id,title",
		"--search", "release",
		"--expand", "list",
		"--per-page", "5",
		"--count",
	)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	for param, want := range map[string]string{
		"status":   "eq.todo",
		"sort":     "-created_at,title",
		"select":   "id,title",
		"search":   "release",
		"expand":   "list",
		"per_page": "5",
		"count":    "exact",
	} {
		if got.Get(param) != want {
			t.Errorf("?%s = %q, want %q (query: %s)", param, got.Get(param), want, got.Encode())
		}
	}
	// Two conditions on one column are two parameters, not one joined value:
	// repeating is what conjoins them.
	if count := got["comment_count"]; len(count) != 2 || count[0] != "gte.10" || count[1] != "lt.100" {
		t.Errorf("repeating a filter flag should repeat the parameter, got %v", count)
	}
	// A page number nobody asked for would override the resource's own default
	// page size with a value the server then has to reinterpret.
	if got.Has("page") {
		t.Errorf("?page should be absent unless --page was passed, got %q", got.Get("page"))
	}
}

// An array column's flag carries the containment grammar, and --help states
// which operators that is — which is the form the guarantee has to take for a
// caller with no compile step (ADR-0029, ADR-0033).
func TestArrayColumnFlag(t *testing.T) {
	var got url.Values
	_, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		emptyPage(w)
	}, "tasks", "list", "--labels", "has.urgent", "--labels", "hasany.backend,infra")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	labels := got["labels"]
	if len(labels) != 2 || labels[0] != "has.urgent" || labels[1] != "hasany.backend,infra" {
		t.Errorf("?labels = %v, want the two conditions as two parameters", labels)
	}

	// The usage names the operators an array takes, and not the ones it does
	// not: a caller reading --help should not be told about `between`.
	usage, err := run(t, func(w http.ResponseWriter, _ *http.Request) { emptyPage(w) },
		"tasks", "list", "--help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"has, hasany, hasall"} {
		if !strings.Contains(usage, want) {
			t.Errorf("--help should name %q for an array column, got:\n%s", want, usage)
		}
	}
}

// --all walks with ?cursor= rather than ?page=, which is what makes the walk
// cost the same at any depth and stops a concurrent insert from making it read
// a row twice.
func TestAllWalksByCursor(t *testing.T) {
	var seen []string
	out, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Query().Get("cursor"))
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"items":[{"id":"a"}],"page":1,"per_page":1,"has_more":true,"next_cursor":"c1"}`))
		case "c1":
			_, _ = w.Write([]byte(`{"items":[{"id":"b"}],"page":1,"per_page":1,"has_more":true,"next_cursor":"c2"}`))
		default:
			_, _ = w.Write([]byte(`{"items":[{"id":"c"}],"page":1,"per_page":1,"has_more":false}`))
		}
	}, "tasks", "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v", err)
	}

	if len(seen) != 3 || seen[1] != "c1" || seen[2] != "c2" {
		t.Errorf("the walk should follow next_cursor, requested cursors %v", seen)
	}

	var page struct {
		Items   []map[string]string `json:"items"`
		HasMore bool                `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("the walk should write one page: %v\n%s", err, out)
	}
	if len(page.Items) != 3 {
		t.Errorf("every row should arrive in one response, got %d: %s", len(page.Items), out)
	}
	if page.HasMore {
		t.Errorf("a completed walk has no more pages: %s", out)
	}
}

// A server that answers with the cursor it was handed would otherwise spin
// forever. ADR-0016: a guard is worth having only if it is proven to fire.
func TestAllRefusesARepeatedCursor(t *testing.T) {
	_, err := run(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"page":1,"per_page":1,"has_more":true,"next_cursor":"stuck"}`))
	}, "tasks", "list", "--all")
	if err == nil {
		t.Fatal("a repeated cursor should stop the walk")
	}
	if !strings.Contains(err.Error(), "repeated cursor") {
		t.Errorf("the error should say why it stopped, got: %v", err)
	}
}

// --all and --page are two answers to the same question, and the second is the
// one cursor paging exists to avoid.
func TestAllRefusesToBeCombinedWithPaging(t *testing.T) {
	_, err := run(t, func(w http.ResponseWriter, _ *http.Request) { emptyPage(w) },
		"tasks", "list", "--all", "--page", "3")
	if err == nil {
		t.Fatal("--all with --page should be refused")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("the error should name the flags, got: %v", err)
	}
}

// Only the flags that were passed are sent. A flag left out and a flag set to
// an empty value must write different SQL, which is why presence is read off
// the flag rather than off the value.
func TestUpdateSendsOnlyTheFlagsThatWerePassed(t *testing.T) {
	var body map[string]any
	var path string
	_, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":"t1"}`))
	}, "tasks", "update", "t1", "--title", "", "--set-null", "due_at")
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if path != "/tasks/t1" {
		t.Errorf("path = %q, want /tasks/t1", path)
	}
	title, ok := body["title"]
	if !ok || title != "" {
		t.Errorf(`--title "" should send an empty string, got %#v`, body)
	}
	// An explicit null, which no value flag can express.
	if due, ok := body["due_at"]; !ok || due != nil {
		t.Errorf("--set-null should send an explicit null, got %#v", body)
	}
	// Everything else stays absent rather than arriving as a zero value.
	if _, ok := body["description"]; ok {
		t.Errorf("a flag that was not passed should not be sent, got %#v", body)
	}
}

// --set-null takes a column, and a name that is not a nullable one is refused
// here, naming the alternatives, rather than sent for the server to reject.
func TestSetNullRefusesAColumnThatIsNotNullable(t *testing.T) {
	_, err := run(t, func(w http.ResponseWriter, _ *http.Request) { emptyPage(w) },
		"tasks", "update", "t1", "--set-null", "title")
	if err == nil {
		t.Fatal("--set-null on a non-nullable column should be refused")
	}
	if !strings.Contains(err.Error(), "allowed:") {
		t.Errorf("the refusal should name what would have been accepted, got: %v", err)
	}
}

// An update naming no column is a round trip that could only succeed by doing
// nothing.
func TestUpdateRefusesAnEmptyPatch(t *testing.T) {
	_, err := run(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an empty patch should not reach the server")
	}, "tasks", "update", "t1")
	if err == nil {
		t.Fatal("an update with no field flags should be refused")
	}
}

// ADR-0011's property, carried to the last consumer: a rejection names what
// would have been accepted, and the CLI prints it rather than flattening the
// problem document to a message.
func TestRejectionPrintsWhatWouldHaveBeenAccepted(t *testing.T) {
	_, err := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"title": "Bad Request",
			"status": 400,
			"detail": "the request could not be understood",
			"errors": [{
				"message": "column is not sortable",
				"location": "query.sort",
				"value": "secret",
				"allowed": ["title", "created_at", "position"]
			}]
		}`))
	}, "tasks", "list", "--sort", "title")
	if err == nil {
		t.Fatal("a 400 should be an error")
	}

	msg := err.Error()
	for _, want := range []string{
		"the request could not be understood",
		"HTTP 400",
		"query.sort: column is not sortable",
		"allowed: title, created_at, position",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the rejection should carry %q, got:\n%s", want, msg)
		}
	}
}

// A delete answers 204. Writing "null" for it would make a shell test for
// emptiness fail.
func TestDeleteWritesNothing(t *testing.T) {
	out, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}, "memberships", "delete", "m1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out != "" {
		t.Errorf("a 204 should print nothing, got %q", out)
	}
}

// The credential is a header, not a query parameter: a token in a URL is
// logged by every proxy between here and the server.
func TestTokenIsSentAsABearerHeader(t *testing.T) {
	var auth string
	var raw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		raw = r.URL.RawQuery
		emptyPage(w)
	}))
	defer server.Close()

	root := New(&client.Client{BaseURL: server.URL, Token: "secret-token"})
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"tasks", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}

	if auth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want a bearer token", auth)
	}
	if strings.Contains(raw, "secret-token") {
		t.Errorf("the token should not reach the query string: %q", raw)
	}
}

// The transport is the seam, and it is a field rather than a decision: a caller
// that sets it never opens a socket. Binding the persistent flags must not
// undo a Client configured in Go, which is what registering a flag default
// does if the default is not the field's own value.
func TestTransportReplacesTheBuiltInOne(t *testing.T) {
	var seen client.Request
	c := &client.Client{
		BaseURL: "https://example.invalid",
		Transport: func(_ context.Context, req client.Request) (json.RawMessage, error) {
			seen = req
			return json.RawMessage(`{"items":[]}`), nil
		},
	}

	root := New(c)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"tasks", "list", "--status", "eq.todo"})
	if err := root.Execute(); err != nil {
		t.Fatalf("list: %v", err)
	}

	if seen.Path != "/tasks" || seen.Query.Get("status") != "eq.todo" {
		t.Errorf("the injected transport should receive the request, got %+v", seen)
	}
	// The flag's default is the field's value, so registration leaves a
	// configured client alone and an actual --base-url still wins.
	if c.BaseURL != "https://example.invalid" {
		t.Errorf("binding a flag should not overwrite a configured field, got %q", c.BaseURL)
	}
}

// A column name may be typed as the schema spells it. An agent reading
// sqlb.json, or an error response, has the snake_case name in hand.
func TestFlagsAcceptTheColumnsOwnSpelling(t *testing.T) {
	var got url.Values
	_, err := run(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		emptyPage(w)
	}, "tasks", "list", "--assignee_id", "isnull")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got.Get("assignee_id") != "isnull" {
		t.Errorf("the snake_case spelling should reach the same parameter, got %s", got.Encode())
	}
}

// Hidden columns are absent from the CLI entirely — not as a filter, not in
// the projection vocabulary, not as a flag on a write. users.password_hash is
// the one this schema has.
func TestHiddenColumnsHaveNoFlag(t *testing.T) {
	out, err := run(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an unknown flag should not reach the server")
	}, "users", "list", "--password-hash", "eq.x")
	if err == nil {
		t.Fatalf("a hidden column should have no flag:\n%s", out)
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("the failure should be an unknown flag, got: %v", err)
	}
}

// #254: a header set on Request is the only way in for what a schema cannot
// derive — tenant selection, a trace id — and it has to win over what Do
// derives on its own, the same way a caller whose auth is a signature rather
// than a bearer token needs to be able to replace Authorization outright.
func TestRequestHeaderOverridesWhatDoDerives(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		emptyPage(w)
	}))
	defer server.Close()

	c := &client.Client{BaseURL: server.URL, Token: "bearer-token"}
	_, err := c.Do(context.Background(), client.Request{
		Method: http.MethodGet,
		Path:   "/tasks",
		Header: http.Header{
			"X-Workspace-Id": []string{"acme"},
			"Authorization":  []string{"Signature abc123"},
		},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	if got.Get("X-Workspace-Id") != "acme" {
		t.Errorf("X-Workspace-Id = %q, want it to reach the wire", got.Get("X-Workspace-Id"))
	}
	if got.Get("Authorization") != "Signature abc123" {
		t.Errorf("Authorization = %q, want the caller's header to replace the derived "+
			"bearer token rather than sit beside it", got.Get("Authorization"))
	}
}

// #254: setting Client.HTTP — the only seam a header had before Request
// carried one — must not silently drop --timeout. A context deadline bounds
// the request regardless of what http.Client is in play.
func TestTimeoutBoundsARequestEvenWithACustomHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			emptyPage(w)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	c := &client.Client{
		BaseURL: server.URL,
		Timeout: 10 * time.Millisecond,
		// A client the caller supplied, with no timeout of its own — the
		// shape #254 reports as the trap: taking the seam that carries a
		// header used to cost the flag silently.
		HTTP: &http.Client{},
	}
	_, err := c.Do(context.Background(), client.Request{Method: http.MethodGet, Path: "/tasks"})
	if err == nil {
		t.Fatal("Do returned no error, so --timeout was silently dropped by the supplied HTTP client")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Do failed for a reason other than the timeout: %v", err)
	}
}

// #257: a hand-written command is only "like a generated one" if it holds the
// client the root's persistent flags are bound to. Constructing its own would
// compile, run, and ignore --base-url, --token and --timeout — a failure that
// looks like a working command right up until the first flag.
//
// Asserted through the flag rather than the field: --base-url is set on the
// root and has to reach a command the root did not generate.
func TestHandWrittenCommandSharesTheRootsConfiguration(t *testing.T) {
	var (
		gotPath  string
		gotAuth  string
		gotBody  map[string]any
		gotQuery url.Values
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.Query()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"tok-1","user_id":"u-1","workspace_id":"w-1","role":"owner"}`))
	}))
	defer server.Close()

	// Exactly what cmd/taskctl/main.go assembles: one client, the generated
	// tree, and the command the generator cannot write.
	c := &client.Client{}
	root := New(c)
	root.AddCommand(NewLoginCommand(c))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"login",
		"--base-url", server.URL, // the root's flag, on a command it did not generate
		"--token", "ignored-for-login",
		"--email", "you@example.com",
		"--password", "correct horse battery staple",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v — output was %q", err, out.String())
	}

	if gotPath != "/auth/login" {
		t.Fatalf("path = %q, want /auth/login; --base-url did not reach the hand-written command", gotPath)
	}
	if gotAuth != "Bearer ignored-for-login" {
		t.Errorf("Authorization = %q, want the root's --token, since one client configures both halves", gotAuth)
	}
	if gotBody["email"] != "you@example.com" || gotBody["password"] != "correct horse battery staple" {
		t.Errorf("body = %v, want the two required flags", gotBody)
	}
	// Absent rather than empty: the server reads a missing workspace as "the
	// oldest membership" and an empty slug as one that matches nothing.
	if _, ok := gotBody["workspace"]; ok {
		t.Errorf("body carries workspace = %v with the flag unset", gotBody["workspace"])
	}
	if len(gotQuery) != 0 {
		t.Errorf("query = %v, want none", gotQuery)
	}

	// Run's conventions, not reimplemented ones: the response is JSON on the
	// command's own writer, indented unless --compact.
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatalf("stdout did not decode as JSON: %v — %q", err, out.String())
	}
	if printed["token"] != "tok-1" {
		t.Errorf("printed token = %v, want tok-1", printed["token"])
	}
	if !strings.Contains(out.String(), "\n  ") {
		t.Errorf("output was not indented, so Run's writeJSON was bypassed: %q", out.String())
	}
}

// The other half of behaving like a generated command: a rejection reaches the
// operator as the server's problem document, with a non-zero exit, rather than
// as a decoding error.
func TestHandWrittenCommandRendersAProblem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"email or password is wrong"}`))
	}))
	defer server.Close()

	c := &client.Client{BaseURL: server.URL}
	root := New(c)
	root.AddCommand(NewLoginCommand(c))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"login", "--email", "you@example.com", "--password", "wrong"})

	err := root.Execute()
	if err == nil {
		t.Fatal("login against a 401 returned no error, so the command would exit 0")
	}
	if !strings.Contains(err.Error(), "email or password is wrong") {
		t.Errorf("error = %v, want the problem document's detail", err)
	}
}
