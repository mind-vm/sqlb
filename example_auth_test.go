package sqlb_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/mind-vm/sqlb"
)

// Verifier is deliberately blind to the request: Verify(ctx, cred) answers
// "who is calling" from a credential and nothing else, so a verifier is not
// coupled to transport details it has no business reading.
//
// A multi-tenant application needs a second answer — which workspace, and what
// role there — and it usually arrives in a header alongside the bearer token.
// That resolution cannot live inside the Verifier, so it is a second
// middleware, chained after the first: read back the principal Middleware
// attached, enrich it, attach it again. The seam is the same one Middleware
// itself uses, and both stages are small enough to read.
//
// The order matters and is the whole point: enrichment runs only on a request
// whose credential already verified, so a forged X-Workspace-Id header reaches
// nothing. The claims it lands on came from Verify, not from the wire.
func ExampleMiddleware_enrichment() {
	type claims struct {
		Subject   string
		Workspace string
		Role      string
	}

	// Stage one: identity, from the credential alone.
	identity := sqlb.Middleware[claims](
		sqlb.VerifierFunc[claims](func(ctx context.Context, cred string) (claims, error) {
			if cred != "good-token" {
				return claims{}, errors.New("no such token")
			}
			return claims{Subject: "user-1"}, nil
		}),
		sqlb.BearerToken,
	)

	// Stage two: everything identity alone cannot answer. It refuses rather
	// than passing an unenriched principal through, for the reason a hook must
	// fail closed — a missing membership is not "no restriction".
	roleOf := func(ctx context.Context, subject, workspace string) (string, error) {
		if subject == "user-1" && workspace == "ws-7" {
			return "editor", nil
		}
		return "", errors.New("not a member")
	}
	enrich := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := sqlb.PrincipalFrom[claims](r.Context())
			if !ok {
				http.Error(w, "no principal", http.StatusUnauthorized)
				return
			}
			ws := r.Header.Get("X-Workspace-Id")
			role, err := roleOf(r.Context(), c.Subject, ws)
			if err != nil {
				http.Error(w, "not a member of that workspace", http.StatusForbidden)
				return
			}
			c.Workspace, c.Role = ws, role
			next.ServeHTTP(w, r.WithContext(sqlb.WithPrincipal(r.Context(), c)))
		})
	}

	// A hook reads one principal type and cannot tell which stage filled it in,
	// which is what keeps the two-stage shape an application concern.
	handler := identity(enrich(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := sqlb.PrincipalFrom[claims](r.Context())
		fmt.Printf("subject=%s workspace=%s role=%s\n", c.Subject, c.Workspace, c.Role)
	})))

	call := func(token, workspace string) {
		r := httptest.NewRequest(http.MethodGet, "/cards", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("X-Workspace-Id", workspace)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			fmt.Println("status", w.Code)
		}
	}

	call("good-token", "ws-7")
	call("good-token", "ws-9") // verified, but not a member: 403, not 401
	call("bad-token", "ws-7")  // never reaches enrichment at all

	// Output:
	// subject=user-1 workspace=ws-7 role=editor
	// status 403
	// status 401
}
