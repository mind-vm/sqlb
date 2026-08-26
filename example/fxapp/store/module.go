// Package store is the generated data layer for the fxapp example.
//
// Every file here is generated except module.go, which is what is left once
// the mechanical part is gone: naming the fx module and composing the one
// value FxModule — also generated, alongside wiring_gen.go — already builds
// from noteschema/sqlb.go (ADR-0059).
//
// module.go used to carry provideMigrations and provideOperations — the
// migration history wrapped for fxkit's group, and the resource mount wrapped
// for fxkit's other one, both properties of the schema and neither more than a
// fixed shape around what noteschema/sqlb.go already configures. FxModule now
// generates that shape.
package store

import "go.uber.org/fx"

var Module = fx.Module("store", FxModule)
