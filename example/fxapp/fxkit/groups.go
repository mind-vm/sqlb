package fxkit

import (
	"io/fs"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb"
)

// The value-group names, prefixed so they cannot collide with a group some
// other layer of the application claims — a platform package that already
// consumes group:"migrations" is the case this is written against, and it is
// the reason the bare names are not used even though this kit is now the
// application's own. Modules normally never spell these: the Provide* helpers
// below carry them, which is also what keeps the strings typed in one place.
const (
	GroupHooks      = "fxkit.hooks"
	GroupMigrations = "fxkit.migrations"
	GroupMiddleware = "fxkit.middleware"
	GroupOperations = "fxkit.operations"
)

// HookSet is the value-group element a module contributes to register its
// query and mutation rules.
//
// Register may fail: a rule that cannot be expressed is better reported at
// boot than skipped, and the group's whole purpose is that nobody downstream
// gets a handle until every contributor has had its say.
type HookSet struct {
	// Module names the contributor. It appears in the boot log and in the
	// error when Register fails, which is the difference between "a hook
	// failed" and a file to open.
	Module string

	// Register adds this module's rules to the registry.
	Register func(*sqlb.Registry) error
}

// MigrationSet is the value-group element a module contributes to register
// its migration history.
type MigrationSet struct {
	// Module names the set. It is the prefix of the tracking table
	// (<Module>_schema_migrations), so two modules migrate independently and
	// neither can renumber the other's history.
	Module string

	// FS is the embedded filesystem holding the .sql files.
	FS fs.FS

	// Dir is the path within FS the files live at, "." when they are at the
	// root.
	Dir string
}

// MiddlewareSet is the value-group element a module contributes to wrap every
// request.
type MiddlewareSet struct {
	// Module names the contributor, for the boot log.
	Module string

	// Order decides where in the chain this sits: lower runs first, and ties
	// break on Module so the chain is the same on every boot.
	//
	// An explicit number rather than the order the group happened to arrive
	// in, because value-group order is not something fx promises — and
	// because middleware order is a correctness question. Authentication that
	// ran after the handler would be decoration.
	Order int

	// Wrap is the middleware.
	Wrap func(http.Handler) http.Handler
}

// OperationSet is the value-group element a module contributes to put
// endpoints on the shared API.
//
// Register returning an error is what carries a refused mount out to the
// boot: the generated Register reports a resource whose declared scope has no
// hook behind it (ADR-0030), and this group is how that reaches fx instead of
// being logged and stepped over.
type OperationSet struct {
	// Module names the contributor, for the boot log and the error.
	Module string

	// Register is called once, with the API every module shares.
	Register func(huma.API) error
}

// ProvideHooks provides a constructor whose result joins the hooks group.
// The constructor may take any dependencies; its result must be HookSet.
func ProvideHooks(ctor any) fx.Option {
	return fx.Provide(fx.Annotate(ctor, fx.ResultTags(`group:"`+GroupHooks+`"`)))
}

// ProvideMigrations provides a constructor whose result joins the migrations
// group. The constructor may take any dependencies; its result must be
// MigrationSet.
func ProvideMigrations(ctor any) fx.Option {
	return fx.Provide(fx.Annotate(ctor, fx.ResultTags(`group:"`+GroupMigrations+`"`)))
}

// ProvideMiddleware provides a constructor whose result joins the middleware
// group. The constructor may take any dependencies; its result must be
// MiddlewareSet.
func ProvideMiddleware(ctor any) fx.Option {
	return fx.Provide(fx.Annotate(ctor, fx.ResultTags(`group:"`+GroupMiddleware+`"`)))
}

// ProvideOperations provides a constructor whose result joins the operations
// group. The constructor may take any dependencies; its result must be
// OperationSet.
func ProvideOperations(ctor any) fx.Option {
	return fx.Provide(fx.Annotate(ctor, fx.ResultTags(`group:"`+GroupOperations+`"`)))
}
