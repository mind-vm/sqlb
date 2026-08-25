package access

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
)

// public paths answer without a key: the liveness probe and the API document.
//
// The list is of exceptions rather than of protected routes, which is the only
// arrangement that survives a resource being added by someone who has not read
// this file. A new endpoint is protected by default; forgetting to add it here
// costs a 401, and forgetting the opposite costs a tenant.
var public = map[string]bool{
	"/health":       true,
	"/openapi.json": true,
	"/openapi.yaml": true,
	"/docs":         true,
	"/docs/":        true,
	"/schemas":      true,
}

// Space is the principal this module stores: the verified slug, as a named
// type so that sqlb.PrincipalFrom[Space] cannot collide with any other
// module's principal. The middleware is the only writer; the hooks read it
// back through SpaceFrom and never learn how it was verified — which is what
// makes this module swappable for a JWT one without touching a hook
// (ADR-0044).
type Space string

// WithSpace returns a context carrying the verified space slug. Exported for
// tests, which need to build a context the hooks accept without going over
// HTTP.
func WithSpace(ctx context.Context, slug string) context.Context {
	return sqlb.WithPrincipal(ctx, Space(slug))
}

// SpaceFrom reports the verified space slug on the context.
//
// The context is the only channel a hook has: sqlb hands a BeforeQuery hook a
// context and a builder, and nothing else. That is why the middleware's whole
// output is one context value.
func SpaceFrom(ctx context.Context) (string, bool) {
	slug, ok := sqlb.PrincipalFrom[Space](ctx)
	return string(slug), ok && slug != ""
}

// Middleware is this module's contribution to the fxkit middleware group.
//
// Exported, and taken by fx as the method expression Config.Middleware, so
// that a test can build the same value the container does rather than a
// near-copy of it.
func (c Config) Middleware() fxkit.MiddlewareSet {
	return fxkit.MiddlewareSet{
		Module: "access",
		// Before anything that reads the space from the context, and after
		// chi's RequestID and Recoverer, which the kit installs itself.
		Order: 10,
		Wrap:  c.wrap,
	}
}

func (c Config) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public[r.URL.Path] || strings.HasPrefix(r.URL.Path, "/docs/") || strings.HasPrefix(r.URL.Path, "/schemas/") {
			next.ServeHTTP(w, r)
			return
		}

		key, ok := bearer(r)
		if !ok {
			unauthorized(w, "no bearer key")
			return
		}
		slug, ok := c.match(key)
		if !ok {
			unauthorized(w, "unknown key")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSpace(r.Context(), slug)))
	})
}

// match finds the space whose key was presented.
//
// Every configured key is compared, and the comparison is constant-time, so
// neither the number of comparisons made nor the time each takes says anything
// about how close a guess was. The obvious version — return on first match —
// leaks the same information more slowly, which is not a difference worth
// having.
func (c Config) match(presented string) (string, bool) {
	var found string
	for slug, secret := range c.Keys {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1 {
			found = slug
		}
	}
	return found, found != ""
}

func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < len("Bearer ") || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	key := strings.TrimSpace(header[7:])
	return key, key != ""
}

// unauthorized answers in the same problem+json shape rest uses for its own
// refusals, so a client has one error format to parse rather than two.
func unauthorized(w http.ResponseWriter, detail string) {
	w.Header().Set("content-type", "application/problem+json")
	w.Header().Set("www-authenticate", `Bearer realm="notes"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"` + detail + `"}`))
}
