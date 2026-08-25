package vault_test

// This file is the REST-surface half of what vault_test.go proves at the
// library level, mirroring example/blog/server_test.go: mount the generated
// Register on a real rest.NewServer and assert what a caller over HTTP
// actually receives, rather than what the model says it would receive.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/vault"
	"github.com/mind-vm/sqlb/rest"
)

func newVaultServer(t *testing.T, db *sqlb.DB) http.Handler {
	t.Helper()
	srv := rest.NewServer(rest.Config{Title: "Vault", Version: "1.0.0"})
	if err := vault.Register(srv.API, db); err != nil {
		t.Fatalf("mounting the vault resources: %v", err)
	}
	return srv.Handler
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestGeneratedServerNeverServesThePayload is the REST half of
// TestHiddenColumnsAreAbsentFromTheFacade: a generated GET /secrets response
// carries the owner and the timestamps, and nothing that was ever written
// through Encrypt.
func TestGeneratedServerNeverServesThePayload(t *testing.T) {
	ctx := context.Background()
	pool := freshDatabase(t)
	db := sqlb.New(pool)

	if _, err := vault.Encrypt(ctx, db, "user", "00000000-0000-0000-0000-000000000003",
		[]byte("a payload no response should ever carry")); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	server := newVaultServer(t, db)
	resp := do(t, server, http.MethodGet, "/secrets", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body)
	}

	body := resp.Body.String()
	for _, leak := range []string{"ciphertext", "nonce", "key_version"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response mentions %q, which should never leave the process:\n%s", leak, body)
		}
	}

	var parsed struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decoding %s: %v", resp.Body, err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("items = %v, want 1", parsed.Items)
	}
	for _, key := range []string{"ciphertext", "nonce", "key_version"} {
		if _, present := parsed.Items[0][key]; present {
			t.Errorf("%q is present in the decoded response", key)
		}
	}
	if parsed.Items[0]["owner_kind"] != "user" {
		t.Errorf("owner_kind = %v, want %q", parsed.Items[0]["owner_kind"], "user")
	}
}

// TestNoGeneratedCreateRoute is the write side of the same finding: Ops never
// declared OpCreate, so Register never mounts POST /secrets at all — there is
// no generated body for a request to reach, let alone reject. The census's
// claim was that the create *body* would have nothing to name; measured
// against what codegen does, the whole route is absent, not merely an
// endpoint that accepts a request and writes nothing.
func TestNoGeneratedCreateRoute(t *testing.T) {
	pool := freshDatabase(t)
	db := sqlb.New(pool)
	server := newVaultServer(t, db)

	resp := do(t, server, http.MethodPost, "/secrets",
		strings.NewReader(`{"owner_kind":"user","owner_id":"00000000-0000-0000-0000-000000000004"}`))
	if resp.Code != http.StatusNotFound && resp.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /secrets = %d, want the route to be absent (404 or 405): %s", resp.Code, resp.Body)
	}
}

// TestOpenAPIDocumentNeverMentionsThePayload is the same claim OpenAPI has to
// make good on: a client reading the document to decide what it can send or
// expect finds no trace of the hidden columns anywhere.
func TestOpenAPIDocumentNeverMentionsThePayload(t *testing.T) {
	pool := freshDatabase(t)
	db := sqlb.New(pool)
	server := newVaultServer(t, db)

	resp := do(t, server, http.MethodGet, "/openapi.json", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	body := resp.Body.String()
	for _, leak := range []string{"ciphertext", "nonce", "key_version"} {
		if strings.Contains(body, leak) {
			t.Errorf("the OpenAPI document mentions %q", leak)
		}
	}

	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the document: %v", err)
	}
	path, ok := doc.Paths["/secrets"]
	if !ok {
		t.Fatal("the document has no /secrets")
	}
	if _, present := path["post"]; present {
		t.Error("the document documents a create operation Ops never granted")
	}
	if _, present := path["get"]; !present {
		t.Error("the document is missing the read operation Ops did grant")
	}
}
