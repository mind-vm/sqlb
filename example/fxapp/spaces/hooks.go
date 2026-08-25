package spaces

import (
	"context"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// provideHooks contributes this module's rule to the registry the kit
// assembles.
//
// One rule, and the schema is what obliges it: spaces.id is declared Scoped,
// so the resource over the table does not mount until a BeforeQuery hook is
// registered for the model (ADR-0030). Deleting this file does not produce a
// server that lists every tenant — it produces a boot that fails naming
// store.Space.
func provideHooks(dir *Directory) fxkit.HookSet {
	return fxkit.HookSet{
		Module: "spaces",
		Register: func(reg *sqlb.Registry) error {
			// Scoped by identity rather than by a foreign key: on this table
			// the row *is* the tenant, so the predicate narrows the primary
			// key. Without it, GET /spaces would list every space in the
			// installation — the hole a "every table has a space_id"
			// convention silently leaves behind at the one table that does
			// not.
			sqlb.On[store.Space](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[store.Space]) error {
				id, err := dir.Current(ctx)
				if err != nil {
					return err
				}
				q.Where(store.SpaceCols.ID.Eq(id))
				return nil
			})
			return nil
		},
	}
}
