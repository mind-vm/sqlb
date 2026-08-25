// Package spaces owns the tenant: the rows, the slug-to-id directory the
// hooks resolve a request against, and the scoping rule for the spaces table
// itself.
//
// The notes module depends on this one, which is the shape a tenant module has
// in every application that has one. What it does *not* do is depend back:
// nothing here knows that notes exist.
package spaces

import (
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
)

var Module = fx.Module("spaces",
	fx.Provide(
		// The directory is built from the unscoped handle, because resolving
		// which space a request speaks for is the question the scope is the
		// answer to. Asking it through the scoped handle would be circular,
		// and fx would say so — a cycle is a boot failure here, not a
		// deadlock at 3am. fxkit.Unscoped is a type rather than a name tag,
		// so the constructor's signature says which handle it takes.
		NewDirectory,
	),
	fxkit.ProvideHooks(provideHooks),
)
