package rest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// #301: Middleware must wrap whatever mount left on srv.Handler — including
// the documented escape hatch of mount reassigning the field itself — not a
// value read before mount ran. wrapHandler is the seam that keeps that
// ordering out of Serve's control flow and into something this test can
// break on purpose.
func TestWrapHandlerAppliesMiddlewareAfterMount(t *testing.T) {
	var calls []string

	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls = append(calls, "inner")
	})
	// The pattern mount uses today: reassigning srv.Handler itself.
	mounted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "mount")
		inner.ServeHTTP(w, r)
	})
	srv := &Server{Handler: mounted}

	cfg := ServeConfig{Middleware: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "middleware")
			next.ServeHTTP(w, r)
		})
	}}

	wrapHandler(srv, cfg).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"middleware", "mount", "inner"}
	if len(calls) != len(want) {
		t.Fatalf("got %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("got %v, want %v — middleware must run outside whatever mount left on srv.Handler", calls, want)
		}
	}
}

func TestWrapHandlerIsANoOpWithoutMiddleware(t *testing.T) {
	var called bool
	srv := &Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})}

	wrapHandler(srv, ServeConfig{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Fatal("wrapHandler with no Middleware should still serve what mount left on srv.Handler")
	}
}
