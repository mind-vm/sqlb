package codegen

// This file is the half of the `sqlb` command that has to be compiled inside
// your module. The other half is cmd/sqlb, which cannot see your schema at all.
//
// The schema is Go, and a table is registered by the side effect of importing
// the package that declares it (ADR-0004). So no prebuilt binary can read a
// registry: the only program that can is one linked against the schema package.
// cmd/sqlb writes that program — three imports and a call to Main — compiles it
// inside the module, and throws it away. Everything below is what that program
// runs, and it lives here rather than being emitted as source because generated
// code cannot be tested and this can.
//
// The convention that joins the two halves is ProjectFunc: an exported,
// argument-free function in the schema package returning a Project. It is a
// convention rather than a config file because the alternative is a second
// declaration language that mirrors Options, drifts from it, and reports its
// mistakes at run time instead of at compile time (ADR-0032).

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"path/filepath"

	"github.com/jryannel/sqlb/schema"
)

// ProjectFunc is the name cmd/sqlb looks for in a schema package.
//
// Exported so that the command, the error message it prints when the function
// is missing, and the documentation are all reading the same string. A
// convention spelled out in three places is a convention that will eventually
// be spelled two ways.
const ProjectFunc = "SqlbProject"

// Project is everything `sqlb` needs to know about a repository that it cannot
// work out for itself.
//
// A schema package declares one by exporting a function of this name:
//
//	// taskschema/sqlb.go
//	func SqlbProject() codegen.Project {
//		return codegen.Project{
//			Options: codegen.Options{
//				Package: "tasks",
//				TSDir:   "web/src/api",
//				DartDir: "mobile/lib/api",
//				CLIDir:  "cli",
//				CLIName: "taskctl",
//			},
//		}
//	}
//
// # Paths are relative to the module root
//
// Every directory in Options is resolved against the directory holding go.mod,
// not against the schema package and not against wherever the command was
// invoked. That is the one rule that makes `sqlb generate ./taskschema` mean the
// same thing from a shell, from a //go:generate directive, and from CI — the
// three places the old hand-written generator had to be told `-dir ..` and got
// it wrong if any of them disagreed.
//
// # Why this wraps Options rather than being it
//
// Options is the emitters. Project is the repository, and the repository has
// more in it than emitter output — everything from MigrationsDir down exists
// for `sqlb migrate`, and landed after the type did without changing what any
// project's SqlbProject returns. That was the point of the wrapper.
type Project struct {
	// Options configures the emitters.
	//
	// Registry may be left nil, and usually is: Main fills it from
	// schema.DefaultRegistry(), which is what declaring a table populates. Set
	// it only if the project builds a registry of its own instead of using the
	// default one.
	Options Options

	// MigrationsDir is where `sqlb migrate` writes, relative to the module
	// root. Empty means the project does not generate migrations with sqlb, and
	// the command says so rather than picking a directory.
	MigrationsDir string

	// ContractFile is where `sqlb impact` records the REST contract snapshot it
	// diffs against, relative to the module root. Empty defaults to
	// "restcontract.json" beside the generated code (Options.Dir). It is a
	// committed artefact — the answer to "backward compatible relative to what?"
	// (ADR-0039) — so it belongs in the repository like the migration history.
	ContractFile string

	// FeaturesFile is where `sqlb docs` writes the feature checklist, relative
	// to the module root. Empty defaults to "FEATURES.md" beside the generated
	// code (Options.Dir). Unlike every other artefact, a rerun does not treat
	// this file as fully owned: it keeps whatever notes the file already
	// carries for an endpoint that still exists, so it is meant to be
	// committed and hand-edited, not regenerated and diffed against.
	FeaturesFile string

	// HandwrittenOps names the endpoints `sqlb docs` cannot see on its own —
	// mounted by hand with huma.Register alongside the resources codegen
	// generates, rather than declared in the schema. Left empty, FEATURES.md
	// covers only the generated slice of the REST surface; a project with a
	// hand-written register/login/invite flow lists those routes here once so
	// the checklist covers what it is actually meant to (#211) — exactly the
	// endpoints with no Describe() to fall back on, and so the ones a written
	// note is most needed for.
	HandwrittenOps []HandwrittenOp

	// EjectDir is where `sqlb eject` writes, relative to the module root.
	//
	// Empty defaults to "ejected" beside the generated code, which is the
	// answer for the case this verb exists for: a repository that wants the way
	// out committed and checked, rather than discovered on the day it is
	// needed. Set it to move the package; set EjectPackage if the directory's
	// base name is not the package name you want.
	EjectDir     string
	EjectPackage string

	// MigrationFormat names the runner's file layout: "goose" (the default),
	// "golang-migrate", or "plain". Resolved by migrate.ByName, so an unknown
	// name is refused with the list of the ones that exist.
	//
	// A string rather than a migrate.Format because a Format is an interface
	// with unexported methods on the other side of this package, and a project
	// that wants a custom one is writing its own generator anyway.
	MigrationFormat string

	// MinPostgres declares the oldest Postgres major version the generated DDL
	// must run on. Zero means unset, which is what every migration generated
	// before the option existed already assumes — see migrate.MinPostgres, and
	// pass it consistently or don't pass it at all.
	MinPostgres int

	// PostgresSchema is the Postgres schema the shadow replay reads back.
	// Empty means "public".
	PostgresSchema string

	// Module scopes the tables read back from the shadow database to one sqlb
	// module (ADR-0015). Empty means unscoped, which is right unless the
	// project's tables carry a module prefix — in which case leaving it empty
	// gives a `current` that disagrees with the declaration about every table
	// name, and a diff that proposes recreating all of them.
	Module string

	// ShadowDB opens a connection to an **empty** scratch database, which is
	// what migrate.Diff needs to be given a trustworthy `current`: the schema
	// the checked-in history builds, rather than whatever production drifted
	// into (ADR-0014).
	//
	// It is a function in your code, not a DSN in ours, for two reasons that
	// point the same way.
	//
	// The first is that the DSN is the project's. sqlb depends on pgx and could
	// dial one itself (ADR-0040), but it has no idea which database is safe to
	// replay into, and that question has exactly one right answerer.
	//
	// The second is that the database has to be *empty*, and shadow.Build will
	// not empty it: creating and dropping databases needs credentials the rest
	// of sqlb never asks for, and dropping the wrong one is unrecoverable. So
	// the destructive half stays with the caller who knows which database is
	// scratch — and this function is exactly where that caller lives. Doing it
	// here means the statement that wipes a database is written out, by name,
	// in a file in your repository, against a DSN you chose.
	//
	//	ShadowDB: func(ctx context.Context) (*pgxpool.Pool, error) {
	//		pool, err := pgxpool.New(ctx, os.Getenv("SQLB_SHADOW_DSN"))
	//		if err != nil {
	//			return nil, err
	//		}
	//		// Scratch, and this line is the assertion that it is.
	//		_, err = pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	//		return pool, err
	//	}
	//
	// The command closes what this returns. It is not called at all when the
	// migration directory is empty, because a baseline diffs against nothing
	// and there is no history to replay.
	ShadowDB func(context.Context) (*pgxpool.Pool, error)
}

// Main runs the driver program cmd/sqlb generates, and exits.
//
// The verb and its flags arrive in os.Args because one driver serves every
// verb; baking the verb into the emitted source instead would mean compiling
// once per thing CI does on a push.
func Main(p Project) {
	code := Run(p, os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

// Run is Main without the exit, which is what makes it testable.
//
// It returns a process exit code rather than an error because "the tree is
// stale" is not an error — it is the answer `check` exists to give, and it has
// to be distinguishable from a schema that would not compile.
func Run(p Project, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		line(stderr, "sqlb: driver expected a verb and got none")
		return 2
	}

	if err := p.Validate(); err != nil {
		line(stderr, err)
		return 1
	}

	opts := p.Options
	if opts.Registry == nil {
		opts.Registry = schema.DefaultRegistry()
	}
	// Empty means the module root. Options requires Dir because a caller
	// writing its own generator has no root to default to and a silent "." is
	// a file written somewhere surprising; a Project does have one, so the
	// common case — a repository generating into itself — costs no line.
	if opts.Dir == "" {
		opts.Dir = "."
	}

	verb, rest := args[0], args[1:]
	if verb != "migrate" && verb != "impact" && verb != "check" && verb != "eject" && len(rest) > 0 {
		say(stderr, "sqlb: %s takes no flags, got %q\n", verb, rest[0])
		return 2
	}

	switch verb {
	case "migrate":
		// The target of the diff is the declared schema — the same registry the
		// emitters read, so a migration and the models it implies cannot be
		// generated from two different pictures.
		return runMigrate(p, opts.Registry, rest, stdout, stderr)

	case "generate":
		// Asked before generating, because generating is what creates them. An
		// error here (a "//"-relative TSDir/DartDir with no go.mod above the
		// working directory) is the same one generate resolves the same way
		// immediately below, so it is left to report there rather than twice.
		stranded, _ := strandedClientDirs(opts)
		written, rewrote, err := generate(opts)
		if err != nil {
			line(stderr, err)
			return 1
		}
		for _, f := range written {
			line(stdout, f)
		}
		// Every file the generation owns is listed on stdout, because that is
		// the manifest a caller wants. The count on stderr is the smaller
		// number: what actually changed on disk. A file whose bytes already
		// match is left untouched so a language server has no event to react
		// to (#269), and saying "wrote" of a file nothing was written to would
		// be the wrong report of that.
		say(stderr, "sqlb: %d files, %d rewritten\n", len(written), rewrote)
		// A client whose whole directory tree had to be invented is almost
		// always a path that resolved somewhere other than intended, and it is
		// the one mistake here that leaves every build green (#290). Printed
		// after the count so the number and the paths above it are the report,
		// and this is the exception to it.
		for _, s := range stranded {
			say(stderr, "%s", s.warning(opts.Dir))
		}
		// The nudge that closes #204: `go mod tidy` run before generate had
		// no way to see an import generate is only now writing — outbox's
		// SSE handler pulling in huma's adapters/sse package is the case
		// that surfaced it. Fired on any content change rather than only a
		// new import; see generate's doc comment in codegen.go for why.
		if rewrote > 0 {
			say(stderr, "sqlb: generated output changed — dependencies may have changed too; run `go mod tidy` again\n")
		}
		return 0

	case "check":
		// Two questions, and only the second one needs a database: are the
		// generated files current, and — with -database — does the declaration
		// still describe the database it is meant to be the truth about
		// (issue #54).
		return runCheck(p, opts, rest, stdout, stderr)

	case "impact":
		// The REST contract the current schema generates, diffed against the
		// checked-in snapshot. It reads capabilities the migration diff ignores,
		// because the sharpest API breaks — un-exposing a column, dropping an
		// operation, a rename — produce no DDL (ADR-0039).
		return runImpact(p, opts, opts.Registry, rest, stdout, stderr)

	case "eject":
		// The way out: the schema as SQL and the resources as plain handlers,
		// depending on pgx and the standard library and nothing else. It reads
		// the same registry the emitters do, so the exit describes the schema as
		// it stands rather than as it was when someone last remembered to run
		// this.
		return runEject(p, opts, rest, stdout, stderr)

	case "docs":
		// The feature checklist: one section per endpoint, derived from the
		// schema, with a notes block a coding agent or a teammate fills in and
		// a rerun preserves. See docs.go.
		return runDocs(p, opts, opts.Registry, stdout, stderr)

	default:
		say(stderr, "sqlb: driver does not know the verb %q\n", verb)
		return 2
	}
}

// say and line are the driver's output, and they drop the write error on
// purpose.
//
// This is the one place in sqlb where an unchecked write is right. Everything
// else here is a library, where swallowing an error hides a failure from a
// caller who could act on it; this is a command, and a program whose stderr has
// gone away has no remaining channel to report that on. The alternative is ten
// error checks that can only be ignored, which is how a package trains its
// readers to skip them.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func line(w io.Writer, v any) {
	_, _ = fmt.Fprintln(w, v)
}

// Validate reports what is wrong with a Project before anything is compiled
// against it.
//
// Options.validate covers the emitters; this covers the one thing it cannot,
// which is that a path means something different here. Options is happy with an
// absolute Dir — a caller writing its own generator may well want one — and a
// Project is not, because a path that resolves against the module root cannot
// be absolute and still mean the same thing on another machine.
func (p Project) Validate() error {
	for _, f := range []struct{ name, path string }{
		{"Options.Dir", p.Options.Dir},
		{"MigrationsDir", p.MigrationsDir},
	} {
		if filepath.IsAbs(f.path) {
			return fmt.Errorf(
				"codegen: Project %s is %q, which is absolute; paths in a Project resolve "+
					"against the module root, so this would write outside the repository on "+
					"the machine that ran it and somewhere else on yours", f.name, f.path)
		}
	}
	return nil
}
