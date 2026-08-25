// Package codegen renders a schema declaration into Go source.
//
// It is driven from a small program in the target project rather than by a CLI
// that compiles your schema behind your back, because the schema is ordinary Go
// and the simplest way to read it is to import it:
//
//	//go:generate go run ./gen
//
//	// gen/main.go
//	package main
//
//	import (
//	    _ "myapp/billing/schema"   // registers its tables
//	    "github.com/mind-vm/sqlb/codegen"
//	    "github.com/mind-vm/sqlb/schema"
//	)
//
//	func main() {
//	    codegen.Must(codegen.Generate(codegen.Options{
//	        Registry: schema.DefaultRegistry(),
//	        Dir:      "billing",
//	        Package:  "billing",
//	    }))
//	}
//
// Output is deterministic: tables are sorted, columns keep declaration order,
// and every file is run through go/format, so a generator bug that produces
// invalid Go fails here rather than at the consumer's next build.
//
// Several further artefacts are opt-in, and all are emitted into the
// repository that consumes them rather than published: TSDir writes a typed
// TypeScript client (ADR-0028), DartDir writes a typed Dart client for a
// Flutter app (ADR-0031), CLIDir writes a cobra command-line client
// (ADR-0029), and WiringMigrations/WiringOperations write the fx wiring —
// value-group providers for the migration history and the resource mount,
// composed as one `fx.Option` a hand-written module joins — for a project
// assembled with uber-go/fx (ADR-0059). Each belongs to a toolchain this
// module does not have, which is why none is emitted unless asked for, and
// why asking costs the consuming repository a gate rather than this one.
package codegen
