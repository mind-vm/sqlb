package fxkit

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb"
)

// Unscoped is the handle with no hooks on it.
//
// It exists for the jobs that cannot be scoped because they run before there
// is anything to scope by: provisioning tenants at boot, and resolving a
// credential to the id the hooks then filter on.
//
// It is a distinct type rather than a flag on the scoped handle, and that is
// the point: a flag is something a caller passes, and the set of callers
// allowed to pass it is exactly the thing being controlled. A type is also
// grep-able the same way the example's `name:"unscoped"` tag was, and cannot
// be misspelled in a string only fx.ValidateApp would catch. Grep for
// fxkit.Unscoped to see every consumer.
type Unscoped struct {
	*sqlb.DB
}

// newUnscoped is the hookless handle.
//
// The Migrated parameter is not used. It is here because this handle is how
// boot-time provisioning reaches the database, and a query against a table
// that does not exist yet is the failure it rules out.
//
// WithHooks on a fresh registry rather than sqlb.New alone: New resolves
// against the process-wide default registry, which nothing in the application
// registers into but a library in some future dependency might. An empty
// registry says "no rules apply here" out loud.
func newUnscoped(pool *pgxpool.Pool, _ Migrated) Unscoped {
	return Unscoped{DB: sqlb.New(pool).WithHooks(sqlb.NewRegistry())}
}

type scopedParams struct {
	fx.In

	Unscoped Unscoped
	Sets     []HookSet    `group:"fxkit.hooks"`
	Log      *slog.Logger `optional:"true"`
}

// newScoped is the handle everything else uses: the same connection, with
// every module's rules attached.
//
// The registry is built here, from the value group, rather than by a function
// each module calls at init time. That is what makes two servers in one test
// binary independent — each fx app builds its own registry — and it is why
// this constructor can report which module's registration failed.
func newScoped(p scopedParams) (*sqlb.DB, error) {
	// Sorted so the boot log reads the same way twice. Hook registration is
	// order-independent — a BeforeQuery hook adds a predicate, and predicates
	// commute — so this is for the reader, not for correctness.
	ordered := append([]HookSet(nil), p.Sets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Module < ordered[j].Module })

	reg := sqlb.NewRegistry()
	names := make([]string, 0, len(ordered))
	for _, set := range ordered {
		if set.Register == nil {
			return nil, fmt.Errorf("fxkit: the %q hook set has no Register function", set.Module)
		}
		if err := set.Register(reg); err != nil {
			return nil, fmt.Errorf("fxkit: registering %s hooks: %w", set.Module, err)
		}
		names = append(names, set.Module)
	}

	logger(p.Log).Info("fxkit: hooks registered", "modules", names)
	return p.Unscoped.WithHooks(reg), nil
}
