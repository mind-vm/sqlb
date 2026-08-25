package notes

import (
	"context"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
	"github.com/mind-vm/sqlb/example/fxapp/spaces"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// provideHooks is the space boundary for notes.
//
// It works because BeforeQuery is handed the *query*. A hook that could only
// veto would have to be given the rows, which means fetching them first; a
// hook that is given the builder adds a predicate, and the database never sees
// the other space's rows at all. That is also why the generated handlers need
// to know nothing about spaces: they call sqlb.Query[T], and so does the
// hand-written endpoint next door.
func provideHooks(dir *spaces.Directory) fxkit.HookSet {
	return fxkit.HookSet{
		Module: "notes",
		Register: func(reg *sqlb.Registry) error {
			hooks := sqlb.On[store.Note](reg)

			// Reads: every SELECT, whether it came from GET /notes, from
			// ?expand=notes on a space, or from the aggregate in insights.go.
			hooks.BeforeQuery(func(ctx context.Context, q *sqlb.Builder[store.Note]) error {
				space, err := dir.Current(ctx)
				if err != nil {
					return err
				}
				q.Where(store.NoteCols.SpaceID.Eq(space))
				return nil
			})

			// Writes are a separate registration because they are a separate
			// statement: BeforeQuery constrains what a request can *see*, and
			// says nothing about what it can overwrite by id.
			//
			// The predicate is added rather than checked, so a PATCH naming a
			// note in another space matches no rows and comes back 404 — the
			// same answer an id that never existed gets, which is the answer
			// it should get. A check would have to read the row first and then
			// choose between 403 and 404; adding the predicate makes the
			// question not arise.
			hooks.BeforeUpdate(func(ctx context.Context, u *sqlb.Update[store.Note]) error {
				space, err := dir.Current(ctx)
				if err != nil {
					return err
				}
				u.Where(store.NoteCols.SpaceID.Eq(space))
				return nil
			})

			hooks.BeforeDelete(func(ctx context.Context, d *sqlb.Delete[store.Note]) error {
				space, err := dir.Current(ctx)
				if err != nil {
					return err
				}
				d.Where(store.NoteCols.SpaceID.Eq(space))
				return nil
			})

			// Creates: the column the client is not allowed to assert is
			// stamped from the verified key. space_id is ReadOnly in the
			// schema, so it is absent from the generated create body — this
			// hook is not overriding a value the caller sent, it is supplying
			// the only value there is.
			hooks.BeforeCreate(func(ctx context.Context, n *store.Note) error {
				space, err := dir.Current(ctx)
				if err != nil {
					return err
				}
				n.SpaceID = space
				return nil
			})
			return nil
		},
	}
}
