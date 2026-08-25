package sqlb_test

import (
	"context"
	"testing"

	"github.com/mind-vm/sqlb"
)

// TestPrincipalRoundTrip covers the seam's contract: stored as any type, found
// by that type, and both failure modes — nothing stored, wrong type — give the
// same answer.
func TestPrincipalRoundTrip(t *testing.T) {
	type spaceID string

	ctx := context.Background()
	if _, ok := sqlb.PrincipalFrom[spaceID](ctx); ok {
		t.Fatal("an empty context reported a principal")
	}

	ctx = sqlb.WithPrincipal(ctx, spaceID("acme"))
	got, ok := sqlb.PrincipalFrom[spaceID](ctx)
	if !ok || got != "acme" {
		t.Fatalf("PrincipalFrom = %q, %v; want acme, true", got, ok)
	}

	// A hook asking for a type the middleware did not store must get the same
	// answer as no principal at all — the two failure modes are one,
	// deliberately.
	if _, ok := sqlb.PrincipalFrom[int](ctx); ok {
		t.Fatal("a principal of a different type was found")
	}
}

// TestPrincipalIsNotInheritedAcrossTypes pins the property the seam trades for
// its simplicity: an interface value and its concrete type are different keys
// here, so middleware storing a concrete type and a hook asking for an
// interface do not meet. It is asserted rather than assumed because the
// alternative — a reflective search for an assignable type — is the design
// this seam rejected, and a future edit that "fixes" this test would be
// adopting it.
func TestPrincipalIsNotInheritedAcrossTypes(t *testing.T) {
	type tenant struct{ ID string }

	ctx := sqlb.WithPrincipal(context.Background(), tenant{ID: "acme"})

	if _, ok := sqlb.PrincipalFrom[any](ctx); !ok {
		t.Fatal("any did not match a stored principal; the seam stores it as any")
	}
	if _, ok := sqlb.PrincipalFrom[*tenant](ctx); ok {
		t.Fatal("a pointer type matched a value the middleware stored by value")
	}
}

// TestPrincipalNestsLikeAContext covers the case a middleware chain produces:
// an inner value shadows an outer one, and the outer context is unchanged.
func TestPrincipalNestsLikeAContext(t *testing.T) {
	type spaceID string

	outer := sqlb.WithPrincipal(context.Background(), spaceID("acme"))
	inner := sqlb.WithPrincipal(outer, spaceID("globex"))

	if got, _ := sqlb.PrincipalFrom[spaceID](inner); got != "globex" {
		t.Errorf("inner principal = %q; want globex", got)
	}
	if got, _ := sqlb.PrincipalFrom[spaceID](outer); got != "acme" {
		t.Errorf("outer principal = %q; want acme — WithPrincipal mutated its parent", got)
	}
}
