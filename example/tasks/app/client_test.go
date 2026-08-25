package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/example/tasks/app"
	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// The tests drive the assembled server over HTTP rather than calling handlers,
// because the things being tested live between them: the middleware puts claims
// in the context, the hooks read them, and the generated handlers are the part
// that must not have to know. Calling a handler directly would skip exactly the
// wiring that matters.

// secret is fixed so that a token minted in one place verifies in another. It
// is a test constant and nothing else; the server refuses to start without a
// real one.
var secret = []byte("test-secret-that-is-at-least-32-bytes-long")

func newServer(t *testing.T, db *pgxpool.Pool) http.Handler {
	t.Helper()
	srv, err := app.New(app.Config{
		DB:     db,
		Secret: secret,
		// Discard the log: the comment endpoint writes an AfterCommit line, and
		// a passing test should be quiet.
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("assembling the server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv.Handler
}

// client is a thin wrapper that carries a bearer token.
type client struct {
	t      *testing.T
	h      http.Handler
	token  string
	userID string // set by account(); "" for the anonymous client
}

type response struct {
	t       *testing.T
	Code    int
	Body    []byte
	Headers http.Header
}

func (c *client) do(method, path string, body any) *response {
	c.t.Helper()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("encoding the request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, payload)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return &response{t: c.t, Code: rec.Code, Body: rec.Body.Bytes(), Headers: rec.Header()}
}

func (c *client) get(path string) *response    { return c.do(http.MethodGet, path, nil) }
func (c *client) delete(path string) *response { return c.do(http.MethodDelete, path, nil) }

func (c *client) post(path string, body any) *response {
	return c.do(http.MethodPost, path, body)
}

func (c *client) patch(path string, body any) *response {
	return c.do(http.MethodPatch, path, body)
}

// expect fails with the body when the status is not the one wanted. Printing
// the body matters: every rejection this API produces says what would have been
// accepted, and a bare "got 400, want 200" throws that away.
func (r *response) expect(code int) *response {
	r.t.Helper()
	if r.Code != code {
		r.t.Fatalf("status = %d, want %d: %s", r.Code, code, r.Body)
	}
	return r
}

func (r *response) decode(v any) *response {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("decoding %s: %v", r.Body, err)
	}
	return r
}

// item decodes into a map, for reading one field without declaring a struct.
func (r *response) item() map[string]any {
	r.t.Helper()
	var m map[string]any
	r.decode(&m)
	return m
}

// list decodes a list response.
type listBody struct {
	Items      []map[string]any `json:"items"`
	Page       int              `json:"page"`
	PerPage    int              `json:"per_page"`
	HasMore    bool             `json:"has_more"`
	NextCursor string           `json:"next_cursor"`
	Total      *int             `json:"total"`
}

func (r *response) list() listBody {
	r.t.Helper()
	var body listBody
	r.decode(&body)
	return body
}

// problem is the RFC 9457 document every rejection uses, including the
// `allowed` field that carries what would have worked.
type problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Errors []struct {
		Message  string   `json:"message"`
		Location string   `json:"location"`
		Value    any      `json:"value"`
		Allowed  []string `json:"allowed"`
	} `json:"errors"`
}

func (r *response) problem() problem {
	r.t.Helper()
	var p problem
	r.decode(&p)
	return p
}

// account registers a user and returns a client holding their token.
func account(t *testing.T, h http.Handler, email, workspace string) *client {
	t.Helper()
	anon := &client{t: t, h: h}
	body := anon.post("/auth/register", map[string]any{
		"name":      email,
		"email":     email,
		"password":  "correct-horse-battery-staple",
		"workspace": workspace,
	}).expect(http.StatusCreated).item()

	token, ok := body["token"].(string)
	if !ok || token == "" {
		t.Fatalf("register returned no token: %v", body)
	}
	userID, ok := body["user_id"].(string)
	if !ok || userID == "" {
		t.Fatalf("register returned no user_id: %v", body)
	}
	return &client{t: t, h: h, token: token, userID: userID}
}

// profileID creates a profile for the given user and returns its id.
func (c *client) profileID(userID, bio string) string {
	c.t.Helper()
	body := c.post("/profiles", map[string]any{
		"user_id": userID,
		"bio":     bio,
	}).expect(http.StatusCreated).item()
	return id(c.t, body)
}

// listID creates a list and returns its id.
func (c *client) listID(name string) string {
	c.t.Helper()
	body := c.post("/lists", map[string]any{
		"name":        name,
		"description": "",
	}).expect(http.StatusCreated).item()
	return id(c.t, body)
}

// taskID creates a task and returns its id.
func (c *client) taskID(list, title string, fields map[string]any) string {
	c.t.Helper()
	body := map[string]any{"list_id": list, "title": title, "description": ""}
	for k, v := range fields {
		body[k] = v
	}
	return id(c.t, c.post("/tasks", body).expect(http.StatusCreated).item())
}

func id(t *testing.T, m map[string]any) string {
	t.Helper()
	v, ok := m["id"].(string)
	if !ok || v == "" {
		t.Fatalf("no id in %v", m)
	}
	return v
}

// mustJSON renders a value for a failure message.
func mustJSON(v any) string {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(out)
}

// forgedToken is a well-formed token signed with the wrong key. It exercises
// the signature check rather than the parser, which is the half that matters:
// a malformed token is rejected by accident, a forged one only on purpose.
func forgedToken(t *testing.T) string {
	t.Helper()
	signer, err := auth.NewSigner(
		[]byte("a-different-secret-that-is-also-32-bytes"), "tasks", time.Hour)
	if err != nil {
		t.Fatalf("building the forging signer: %v", err)
	}
	token, err := signer.Sign(auth.Claims{
		Subject:   "00000000-0000-0000-0000-000000000001",
		Workspace: "00000000-0000-0000-0000-000000000002",
		Role:      auth.RoleOwner,
	})
	if err != nil {
		t.Fatalf("signing the forged token: %v", err)
	}
	return token
}
