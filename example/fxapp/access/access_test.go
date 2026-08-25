package access_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mind-vm/sqlb/example/fxapp/access"
)

// These need no database and no container: the configuration and the
// middleware are the two halves of this module that decide something, and
// both are functions of their input.

func TestNewConfigParses(t *testing.T) {
	t.Setenv("FXAPP_SPACE_KEYS", "acme=acme-key-0123456789, globex=globex-key-0123456789")

	cfg, err := access.NewConfig()
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if got := cfg.Slugs(); len(got) != 2 || got[0] != "acme" || got[1] != "globex" {
		t.Fatalf("Slugs() = %v; want [acme globex] in that order", got)
	}
	if cfg.Keys["acme"] != "acme-key-0123456789" {
		t.Fatalf("the acme key round-tripped as %q", cfg.Keys["acme"])
	}
}

// TestNewConfigRefuses proves each guard fires. A guard that has never been
// seen to refuse is a claim, not a check (ADR-0016).
func TestNewConfigRefuses(t *testing.T) {
	for name, value := range map[string]string{
		"unset":       "",
		"no equals":   "acme",
		"empty slug":  "=acme-key-0123456789",
		"empty key":   "acme=",
		"short key":   "acme=too-short",
		"a duplicate": "acme=acme-key-0123456789,acme=other-key-0123456789",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FXAPP_SPACE_KEYS", value)
			if _, err := access.NewConfig(); err == nil {
				t.Fatalf("NewConfig accepted %q", value)
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	t.Setenv("FXAPP_SPACE_KEYS", "acme=acme-key-0123456789,globex=globex-key-0123456789")
	cfg, err := access.NewConfig()
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}

	// The middleware reaches the test through the value group, the same way
	// the kit gets it — so a change to the Order or the Module name is visible
	// here rather than only at boot.
	set := cfg.Middleware()
	if set.Module != "access" {
		t.Fatalf("the set names module %q", set.Module)
	}

	var seen string
	handler := set.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = access.SpaceFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		path   string
		key    string
		status int
		space  string
	}{
		{"a known key names its space", "/notes", "globex-key-0123456789", http.StatusOK, "globex"},
		{"an unknown key is refused", "/notes", "nope", http.StatusUnauthorized, ""},
		{"no key at all is refused", "/notes", "", http.StatusUnauthorized, ""},
		// The liveness probe has to answer while everything else is refusing,
		// and it reaches the handler with no space on the context — which is
		// what every hook fails closed on.
		{"health is public", "/health", "", http.StatusOK, ""},
		{"the document is public", "/openapi.json", "", http.StatusOK, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.key != "" {
				req.Header.Set("authorization", "Bearer "+tc.key)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d", rec.Code, tc.status)
			}
			if seen != tc.space {
				t.Fatalf("the handler saw space %q, want %q", seen, tc.space)
			}
		})
	}
}

// TestKeysAreNotConfusedProves the match is per-key rather than per-prefix: a
// key that is a prefix of another must not open the other's space.
func TestKeysAreNotConfused(t *testing.T) {
	t.Setenv("FXAPP_SPACE_KEYS", "acme=shared-prefix-0123456789,globex=shared-prefix-0123456789-more")
	cfg, err := access.NewConfig()
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}

	var seen string
	handler := cfg.Middleware().Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, _ = access.SpaceFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/notes", nil)
	req.Header.Set("authorization", "Bearer shared-prefix-0123456789-more")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "globex" {
		t.Fatalf("the longer key opened %q", seen)
	}
}
