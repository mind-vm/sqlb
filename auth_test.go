package sqlb_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		wantCred string
		wantOK   bool
	}{
		{"present", "Bearer abc123", "abc123", true},
		{"case-insensitive scheme", "bearer abc123", "abc123", true},
		{"upper scheme", "BEARER abc123", "abc123", true},
		{"missing header", "", "", false},
		{"wrong scheme", "Basic abc123", "", false},
		{"empty token", "Bearer ", "", false},
		{"whitespace-only token", "Bearer    ", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}

			cred, ok := sqlb.BearerToken(r)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if cred != tc.wantCred {
				t.Fatalf("cred = %q, want %q", cred, tc.wantCred)
			}
		})
	}
}

func TestTransientError(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	te := sqlb.TransientError{Err: inner}

	if got := te.Error(); got != inner.Error() {
		t.Fatalf("Error() = %q, want %q", got, inner.Error())
	}
	if !errors.Is(te, inner) {
		t.Fatalf("errors.Is(te, inner) = false, want true (Unwrap must expose Err)")
	}

	var target sqlb.TransientError
	if !errors.As(error(te), &target) {
		t.Fatalf("errors.As did not recognize TransientError")
	}
}

func TestVerifierInterface(t *testing.T) {
	// Compile-time check: VerifierFunc[string] must satisfy Verifier[string].
	var _ sqlb.Verifier[string] = sqlb.VerifierFunc[string](func(ctx context.Context, cred string) (string, error) {
		return cred, nil
	})
}

func TestVerifierFunc(t *testing.T) {
	want := errors.New("no such token")
	var gotCred string
	f := sqlb.VerifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		gotCred = cred
		if cred == "good" {
			return testPrincipal{ID: "user-1"}, nil
		}
		return testPrincipal{}, want
	})

	p, err := f.Verify(context.Background(), "good")
	if err != nil {
		t.Fatalf("Verify(good) returned %v, want nil", err)
	}
	if p.ID != "user-1" {
		t.Fatalf("principal.ID = %q, want user-1", p.ID)
	}
	if gotCred != "good" {
		t.Fatalf("the closure saw cred %q, want good", gotCred)
	}
	if _, err := f.Verify(context.Background(), "bad"); !errors.Is(err, want) {
		t.Fatalf("Verify(bad) returned %v, want %v", err, want)
	}
}

type testPrincipal struct{ ID string }

func TestMiddleware_Success(t *testing.T) {
	v := sqlb.VerifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		if cred != "good-token" {
			t.Fatalf("Verify received %q, want %q", cred, "good-token")
		}
		return testPrincipal{ID: "user-1"}, nil
	})

	var gotPrincipal testPrincipal
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotOK = sqlb.PrincipalFrom[testPrincipal](r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !gotOK {
		t.Fatalf("PrincipalFrom[testPrincipal] found nothing in next's context")
	}
	if gotPrincipal.ID != "user-1" {
		t.Fatalf("principal.ID = %q, want %q", gotPrincipal.ID, "user-1")
	}
}

func TestMiddleware_MissingCredential(t *testing.T) {
	v := sqlb.VerifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		t.Fatal("Verify must not be called when no credential is present")
		return testPrincipal{}, nil
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil) // no Authorization header
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextCalled {
		t.Fatal("next was called despite a missing credential")
	}
	assertProblemJSON(t, w, http.StatusUnauthorized)
}

func TestMiddleware_InvalidCredential(t *testing.T) {
	wantErr := errors.New("signature mismatch")
	v := sqlb.VerifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		return testPrincipal{}, wantErr
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextCalled {
		t.Fatal("next was called despite an invalid credential")
	}
	body := assertProblemJSON(t, w, http.StatusUnauthorized)
	if strings.Contains(body["detail"].(string), wantErr.Error()) {
		t.Fatal("the underlying Verify error must not be echoed in the response body")
	}
}

// TestMiddleware_TransientError covers both shapes a Verifier can return a
// TransientError in. By value is what the doc comment asks for; by pointer is
// what Go's error-wrapping habits produce, and it used to fall through to the
// 401 branch — the exact conflation the type exists to prevent (#278).
func TestMiddleware_TransientError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"by value", sqlb.TransientError{Err: errors.New("dial tcp: i/o timeout")}},
		{"by pointer", &sqlb.TransientError{Err: errors.New("dial tcp: i/o timeout")}},
		{"wrapped by value", fmt.Errorf("verifying: %w", sqlb.TransientError{Err: errors.New("provider 503")})},
		{"wrapped by pointer", fmt.Errorf("verifying: %w", &sqlb.TransientError{Err: errors.New("provider 503")})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transientCase(t, tc.err)
		})
	}
}

func transientCase(t *testing.T, verifyErr error) {
	t.Helper()
	v := sqlb.VerifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		return testPrincipal{}, verifyErr
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (a provider outage must not read as a rejected credential)", w.Code, http.StatusInternalServerError)
	}
	if nextCalled {
		t.Fatal("next was called despite a transient verification failure")
	}
	assertProblemJSON(t, w, http.StatusInternalServerError)
}

// assertProblemJSON checks the response is RFC 9457 problem+json shaped with
// the given status, and returns the decoded body for further assertions.
func assertProblemJSON(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("response body did not decode as JSON: %v", err)
	}
	if got, want := int(body["status"].(float64)), status; got != want {
		t.Fatalf("body[status] = %d, want %d", got, want)
	}
	if _, ok := body["detail"].(string); !ok {
		t.Fatal("body[detail] missing or not a string")
	}
	return body
}
