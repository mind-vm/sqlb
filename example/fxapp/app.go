// Package fxapp is the composition: the list of modules this server is made
// of, and nothing else.
//
// It is a library rather than a main so that the tests build the same
// application the binary builds. A demo whose tests exercise a different
// assembly than the one that ships is testing the tests — and with a container
// the risk is sharper than usual, because the assembly *is* the program.
//
// # What to read, and in what order
//
//	noteschema/schema.go   the source of truth: two tables, one of them a tenant
//	platform.go            the fxkit glue, fed by this application's configuration
//	store/module.go        the generated resources, mounted on the scoped handle
//	notes/hooks.go         the space boundary, one registration per statement kind
//	app_test.go            the claims above, asserted — including the one that fails
package fxapp

import (
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb/example/fxapp/access"
	"github.com/mind-vm/sqlb/example/fxapp/notes"
	"github.com/mind-vm/sqlb/example/fxapp/spaces"
	"github.com/mind-vm/sqlb/example/fxapp/store"
)

// Modules is the whole application.
//
// The order of this list does not matter, and that is worth stating rather
// than leaving to be discovered: fx resolves by type, so what runs before what
// is decided by the parameters each constructor declares. The list is grouped
// and commented for a reader, not for the container.
//
// What the grouping does say is where the seams are. Platform knows nothing
// about notes or spaces and would be the same in any application; the rest is
// this one. That is the split a platform repository makes into two modules —
// see the studio-apps/core layout, where the first half is an
// appbase.Standard() every product composes and the second is the product.
// The glue packages this example used to hand-write (dbbase, sqlbkit, httpkit)
// are now one package, fxkit, and platform.go is what is left: the
// configuration boundary. fxkit was briefly a published module and is not one
// any more — ADR-0044 has the reversal, and fxkit/doc.go states the four
// obligations that make a copy of it correct.
func Modules() fx.Option {
	return fx.Options(
		// Platform: the logger and the fxkit glue — pool, migrations, the
		// scoped and unscoped handles, chi + Huma, and the four value groups
		// the modules below contribute to.
		Platform(),

		// This application. The schema's generated surface, who may speak for
		// a space, the tenant, and the feature.
		store.Module,
		access.Module,
		spaces.Module,
		notes.Module,
	)
}

// Run boots the application and blocks until it is signalled.
//
// It is the entire body of cmd/server's main. A binary that needed a flag of
// its own would call fx.New(Modules(), ...) directly rather than growing an
// argument here.
func Run(opts ...fx.Option) {
	fx.New(append([]fx.Option{Modules()}, opts...)...).Run()
}
