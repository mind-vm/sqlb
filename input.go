package sqlb

import "context"

// The create-input seam: how the part of a request body that is *not* a column
// reaches the hook that has to do something with it.
//
// A BeforeCreate hook is handed a context and the row and nothing else, which
// is what lets one registration apply to every insert of a model. The price is
// the same one the principal pays: whatever the hook needs beyond the row must
// already be in the context, put there by whoever knows. For the principal that
// is the middleware that verified the request; here it is the generated create
// handler, which decoded a body carrying more than the row does.
//
// The case is a create that takes a secret, and it is ordinary — a signup with
// a password, an invite carrying a token, a create whose input names rows of
// another table. The column stores a bcrypt digest; the request sends the
// plaintext. Before this the two workarounds were a WriteOnly `pin_hash`
// property carrying a plaintext PIN, and a column called `pin` holding a hash,
// which is the same lie told to the client or to the DBA (#309).
//
// So the property is declared beside the resource — schema.REST's CreateInput —
// the generated body carries it, and the value arrives here. Nothing in the
// engine inspects it; as with the principal, sqlb only carries it and hands it
// back to whoever asks for that type.

type createInputKey struct{}

// WithCreateInput returns a context carrying in as the create's declared
// non-column input.
//
// The generated create handler calls this, so an application on the REST layer
// does not. What it is for outside that path is a caller reaching the same hook
// from somewhere else — a seeding command, a background job, a test — which has
// to supply the input the hook is written against, because a hook that silently
// tolerates its absence is a hook that writes a row with an empty digest in it.
func WithCreateInput(ctx context.Context, in any) context.Context {
	return context.WithValue(ctx, createInputKey{}, in)
}

// CreateInputFrom returns the create input as a T, and whether one of that type
// was stored.
//
//	sqlb.On[Child](reg).BeforeCreate(func(ctx context.Context, c *Child) error {
//	    in, ok := sqlb.CreateInputFrom[models.CreateChildInput](ctx)
//	    if !ok {
//	        return errors.New("children: a child is created with a PIN")
//	    }
//	    hash, err := bcrypt.GenerateFromPassword([]byte(in.Pin), bcrypt.DefaultCost)
//	    if err != nil {
//	        return err
//	    }
//	    c.PinHash = string(hash)
//	    return nil
//	})
//
// The two failure modes are one answer, as they are for [PrincipalFrom]: no
// input stored and an input of a different type both report false. A hook that
// needs to tell them apart is coupling itself to which caller ran.
//
// What a hook must not do is treat false as "nothing to do". Every insert of
// the model runs it, including one issued by a job that never saw a request, so
// false means the value the row needs was not supplied — and a hook that
// shrugs at that writes exactly the row the declaration exists to prevent, with
// an empty hash in the column that authenticates. Fail closed: return an error.
func CreateInputFrom[T any](ctx context.Context) (T, bool) {
	in, ok := ctx.Value(createInputKey{}).(T)
	return in, ok
}
