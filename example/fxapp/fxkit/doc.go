// Package fxkit assembles sqlb inside a [uber-go/fx] application: the pool and
// its lifetime, a migration runner over a value group, the hook registry built
// from what feature modules contribute, the scoped and unscoped handles, and an
// HTTP surface — chi, a Huma API, a server whose lifetime fx manages.
//
// # This is glue to copy, not a library to import
//
// It lives inside the example on purpose. It was briefly a published module,
// github.com/mind-vm/sqlb/sqlbfx, and ADR-0044 records why that was reversed:
// nearly all of it is opinionated wiring — chi for the router, humachi for the
// API, goose for the runner, log/slog for the log — and opinions that
// load-bearing are the wrong thing to publish, because an application on echo
// or golang-migrate can only take them or refuse them, where it could have
// adapted a file it owns. What a published module offers instead is a contract
// that separately-authored modules can compile against; nobody was writing the
// second module, so nothing was buying the compatibility surface, the second
// go.mod and the second release tag that cost.
//
// So: copy this directory and adapt it, keeping the four obligations below. The
// engine keeps the parts that are not opinions — the hooks, the boot refusal,
// and the principal seam.
//
// # What a copy must preserve
//
// These are the properties the example asserts, and the reason this glue is
// worth reading before writing your own. Each is a decision, not a habit:
//
//  1. A refused mount is a boot failure, naming the module. The error sqlb
//     raises for a Scoped resource with no confining hook (ADR-0030) travels
//     out through OperationSet.Register and stops the process. Glue that logs
//     that error instead of returning it turns sqlb's loudest safety check into
//     a warning nobody reads. TestResourcesRefuseToMountWithoutHooks in the
//     example is the assertion, made against a real Postgres.
//
//  2. Migrated is a value, so ordering is a dependency edge. It means "every
//     registered migration set has been applied"; the handles take one; so
//     nothing can query a table that does not exist yet, in any module-list
//     order. Glue that runs migrations in an fx.Invoke and hopes has replaced a
//     guarantee with a race.
//
//  3. Middleware order is an explicit integer, not group arrival order. fx
//     value groups have no defined order, and authentication that runs after
//     the handler it protects is not authentication.
//
//  4. The boot log is deterministic — contributions sorted by module name — so
//     that two boots of the same module list produce the same lines, and a diff
//     between them means something.
//
// # The contract inside this application
//
// Four value-group element types. A feature module provides some subset of them
// and nothing anybody imports, which is what lets app.go be a list of modules:
//
//	fxkit.ProvideHooks(func(dir *spaces.Directory) fxkit.HookSet { ... })
//	fxkit.ProvideMigrations(func() fxkit.MigrationSet { ... })
//	fxkit.ProvideMiddleware(func(cfg Config) fxkit.MiddlewareSet { ... })
//	fxkit.ProvideOperations(func(db *sqlb.DB) fxkit.OperationSet { ... })
//
// The kit is five composable options — Pool, Migrations, Handles, HTTP, and
// Module as the sum. A codebase whose platform layer already owns the pool, the
// migrations and the router takes Handles alone, over the platform's pool, and
// supplies the fact its own runner established: fx.Supply(fxkit.Migrated{}) is
// the application asserting what the kit cannot know.
//
// # What is not here
//
// The principal seam. [github.com/mind-vm/sqlb.WithPrincipal] and
// [github.com/mind-vm/sqlb.PrincipalFrom] are in the engine, because the seam
// between "who is calling" and "what confines the query" turned out not to be
// fx-shaped at all: example/tasks had hand-rolled the same thing with no
// container in sight. Middleware stores the principal, scoping hooks read it
// back by type, and neither end names the other — which is what makes an auth
// mechanism swappable without touching a hook. See access/middleware.go for
// this application's end of it.
//
// Configuration is the application's business. DBConfig and HTTPConfig are
// plain structs the application provides (fx.Supply, or a constructor that
// reads whatever the application reads); the kit reads no environment variable.
// The logger is optional: a *slog.Logger in the graph is used, otherwise
// slog.Default() — the kit never provides one.
//
// [uber-go/fx]: https://github.com/uber-go/fx
package fxkit
