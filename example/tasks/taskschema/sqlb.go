package taskschema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/codegen"
)

// shadowDSNEnv names the scratch database `sqlb migrate` replays the history
// into. Anything is fine as long as it is not a database anyone cares about:
//
//	docker run --rm -d -p 5433:5432 -e POSTGRES_PASSWORD=x postgres:18-alpine
//	export SQLB_SHADOW_DSN='postgres://postgres:x@localhost:5433/postgres?sslmode=disable'
const shadowDSNEnv = "SQLB_SHADOW_DSN"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// example/tasks is a module of its own, so the module root is example/tasks and
// every path below is relative to it. That is why Dir is left empty: the
// generated Go lands beside go.mod, which is where the tasks package is.
//
// What this replaces is worth being precise about, because it is the entire
// case for #16. It was cmd/gen/main.go: a flag for -check, a flag for -dir with
// a default that was correct from the module root and wrong from the directory
// go generate actually runs in, two error branches, and this literal. The
// literal is the only part that said anything about this project. Everything
// else was the same thirty lines every sqlb project had to write, and get
// right, before the tool would run at all.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Package: "tasks",

			// The TypeScript client, emitted into the frontend that consumes it
			// rather than published as a package. A client generated against the
			// server it talks to cannot be a version behind it, which is the
			// property models_gen.go already has and a published SDK cannot.
			TSDir: "web/src/api",

			// The Dart client, into the Flutter app's package. Same argument as the
			// TypeScript one, and one more that is specific to a phone: a list on a
			// small screen loads as it is scrolled, which is cursor paging, and
			// cursor paging is the thing hand-written clients reimplement out of
			// has_more and an offset counter.
			DartDir: "mobile/lib/api",

			// The command-line client, for the same reason and for one more: the
			// caller most likely to drive this API is an agent, and `taskctl tasks
			// list --help` is a statement of what the resource accepts that costs
			// no round trip and no 400 to read. cmd/taskctl is the four-line main
			// that runs it.
			CLIDir:  "cli",
			CLIName: "taskctl",

			// The agent skill, into the directory an agent working in this
			// module reads from. It says what these resources actually accept,
			// which is the question a static document cannot answer: capabilities
			// are opt-in, so the answer is different in every project.
			//
			// Committed and covered by `sqlb check`, which is the only reason
			// writing instructions into a repository is safe — a skill that has
			// drifted from the schema is confidently wrong about the one thing it
			// exists to know. Note where it lands: example/tasks is its own
			// module, so this is example/tasks/.claude, scoped to this subtree
			// rather than claiming to describe the repository above it.
			SkillDir:           ".claude/skills",
			SkillSchemaPackage: "./taskschema",
		},

		MigrationsDir: "migrations",

		// The same 18 cmd/migrate passes, and for the same reason: it makes a
		// UUIDv7 primary key default to the built-in uuidv7() rather than the
		// pg_uuidv7 extension's spelling, so the DDL applies to a stock
		// Postgres. Passing it here and not there — or the reverse — would
		// leave one history with two spellings of the same generator.
		MinPostgres: 18,

		ShadowDB: shadowDB,
	}
}

// shadowDB opens the scratch database `sqlb migrate` replays the committed
// history into, so that the current side of the diff is what the migrations
// build rather than what anyone remembers writing.
//
// Note what this example does *not* do: replace cmd/migrate. Its second
// migration is three things the DSL cannot express — two triggers and a pair of
// composite foreign keys — written as migrate.Change values by hand, and no
// diff will ever produce them. What `sqlb migrate` adds here is the other
// direction: `sqlb migrate -check ./taskschema` answers whether the history
// still builds the declared schema, which is the question that goes stale every
// time someone edits schema.go.
//
// Running it against this history also demonstrates the case the command warns
// about rather than refuses. Those triggers come back from introspection as
// constructs the DSL cannot express, so `current` is an incomplete picture and
// the command says so before showing the diff.
func shadowDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv(shadowDSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf(
			"%s is not set, and replaying the migration history needs a scratch database. "+
				"Any empty one will do — see the comment on %s in taskschema/sqlb.go",
			shadowDSNEnv, shadowDSNEnv)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	// The destructive half, written out in the repository that owns the
	// database rather than hidden in the tool. sqlb will not empty a database
	// itself, on the grounds that it cannot know which ones are scratch — this
	// line is this project saying that this one is.
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("emptying the shadow database at %s: %w", shadowDSNEnv, err)
	}
	return pool, nil
}
