package sqlb_test

import (
	"context"
	"testing"

	"github.com/mind-vm/sqlb"
)

// TestCreateInputRoundTrip covers the seam's contract, which is the
// principal's: stored as any type, found by that type, and both failure modes —
// nothing stored, wrong type — answered the same way.
func TestCreateInputRoundTrip(t *testing.T) {
	type createChildInput struct{ Pin string }

	ctx := context.Background()
	if _, ok := sqlb.CreateInputFrom[createChildInput](ctx); ok {
		t.Fatal("an empty context reported a create input")
	}

	ctx = sqlb.WithCreateInput(ctx, createChildInput{Pin: "4242"})
	got, ok := sqlb.CreateInputFrom[createChildInput](ctx)
	if !ok || got.Pin != "4242" {
		t.Fatalf("CreateInputFrom = %+v, %v; want the input, true", got, ok)
	}

	// A hook asking for a type no handler stored gets the same answer as one
	// asking on a context that carries nothing. It has to: the alternative is a
	// hook that distinguishes "the wrong request" from "no request", which is a
	// hook coupled to which caller ran.
	if _, ok := sqlb.CreateInputFrom[struct{ Token string }](ctx); ok {
		t.Fatal("an input of a different type was found")
	}
}

// TestCreateInputNestsLikeAContext is the case a create inside a create
// produces: the inner value shadows the outer one, and the outer context is
// unchanged. A BeforeCreate hook that inserts a second model is exactly this,
// and it is the one place the seam could hand a hook the wrong request's input.
func TestCreateInputNestsLikeAContext(t *testing.T) {
	type input struct{ Pin string }

	outer := sqlb.WithCreateInput(context.Background(), input{Pin: "4242"})
	inner := sqlb.WithCreateInput(outer, input{Pin: "1234"})

	if got, _ := sqlb.CreateInputFrom[input](inner); got.Pin != "1234" {
		t.Errorf("inner input = %+v; want the inner one", got)
	}
	if got, _ := sqlb.CreateInputFrom[input](outer); got.Pin != "4242" {
		t.Errorf("outer input = %+v; WithCreateInput mutated its parent", got)
	}
}

// The two seams are separate keys. A hook reading one must not be handed the
// other, whichever order the middleware and the handler ran in.
func TestCreateInputAndPrincipalDoNotCollide(t *testing.T) {
	type claims struct{ Subject string }

	ctx := sqlb.WithPrincipal(context.Background(), claims{Subject: "u1"})
	ctx = sqlb.WithCreateInput(ctx, claims{Subject: "from-the-body"})

	if got, _ := sqlb.PrincipalFrom[claims](ctx); got.Subject != "u1" {
		t.Errorf("principal = %+v; a request body overwrote what middleware verified", got)
	}
	if got, _ := sqlb.CreateInputFrom[claims](ctx); got.Subject != "from-the-body" {
		t.Errorf("create input = %+v", got)
	}
}
