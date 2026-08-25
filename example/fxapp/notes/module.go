// Package notes owns the note: the rules that confine it to one space, and
// the one endpoint the generator does not write.
//
// It contributes to two value groups and provides nothing anybody imports,
// which is what a feature module usually looks like. Removing it from the
// module list removes its endpoint and its rules together — and the second
// half is why the resources refuse to mount rather than serving unscoped, as
// app_test.go asserts.
package notes

import (
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
)

var Module = fx.Module("notes",
	fxkit.ProvideHooks(provideHooks),
	fxkit.ProvideOperations(provideOperations),
)
