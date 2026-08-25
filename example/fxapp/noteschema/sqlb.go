package noteschema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import (
	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
)

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// example/fxapp is a module of its own, so paths resolve against
// example/fxapp rather than the repository root. The generated code lands in
// store/ rather than beside go.mod, because the module root here is the
// composition — app.go and its test.
//
// store/ also carries the generated fx wiring (ADR-0059) — which is *why* the
// package that used to be half generated models and half a hand-written
// migrations.FS() wrapper is now entirely generated except for module.go: the
// wiring emits a `//go:embed` over MigrationsDir, and go:embed cannot reach
// outside the package it is written in, so MigrationsDir has to live inside
// store/ rather than beside it the way the fxkit doc once showed.
//
// No TypeScript, Dart or CLI client: example/tasks emits all three and this
// one has nothing to add on that subject.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Dir:     "store",
			Package: "store",

			// The two contributions this schema determines on its own: the
			// migration history and the resource mount. Both join fxkit's
			// groups by the exact type names fxkit/groups.go declares — get
			// one wrong and go build says so, in store/wiring_gen.go, rather
			// than fx failing to resolve a provider at boot.
			//
			// Name is set explicitly on both, and set to two different
			// strings, because noteschema declares into the *default*
			// registry (schema.Table, not schema.NewModule) — genuinely
			// unnamed, the schema.NewRegistry() case ADR-0059 exists for.
			// "notes" names the migration set, matching the tracking table
			// prefix the hand-written provideMigrations used: one history
			// covers both spaces and notes, and it is named for the module
			// whose foreign key decides the apply order, not for the package
			// it happens to be generated into. "store" names the resource
			// mount, matching the package these resources live in. Neither
			// is derivable — this is exactly the gap #171 named.
			WiringMigrations: codegen.WiringSet{
				Type:     "github.com/mind-vm/sqlb/example/fxapp/fxkit.MigrationSet",
				Group:    fxkit.GroupMigrations,
				Name:     "notes",
				EmbedDir: "migrations",
			},
			WiringOperations: codegen.WiringSet{
				Type:  "github.com/mind-vm/sqlb/example/fxapp/fxkit.OperationSet",
				Group: fxkit.GroupOperations,
				Name:  "store",
			},
		},

		// MinPostgres(18) makes UUIDv7 primary keys default to the built-in
		// uuidv7(), so the migration applies to a stock postgres:18 with no
		// extension installed. cmd/migrate passes the same 18, and the two
		// have to agree: one history with two spellings of the same generator
		// is a history that only replays on the machine it was written on.
		MinPostgres: 18,

		// Relative to the module root (example/fxapp), same as Dir — and
		// nested under it now, which WiringMigrations.EmbedDir depends on.
		MigrationsDir: "store/migrations",
	}
}
