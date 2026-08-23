package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// init is the other verb that needs no schema package, and for the opposite
// reason introspect does: there is nothing yet to import because nothing has
// been written yet.
//
// It writes a small, real project rather than an empty one — a single Task
// table, exposed CRUD — on the grounds that an empty schema package proves
// nothing runs and answers no question a first-time user actually has. The
// three files it writes (go.mod, <pkg>schema/schema.go, <pkg>schema/sqlb.go)
// are exactly what every other command here already needs to exist; init's
// whole job is writing the boilerplate this file's own package doc says a
// project has to have before `sqlb generate` has anything to read.
//
// cmd/server and the migration runner are scaffolded too, so that the last
// step really is `go run ./cmd/server` — but init does not run `go mod tidy`,
// `go generate` or `sqlb migrate` itself. Each needs something init cannot
// promise: `go mod tidy` needs the network to resolve a module this command
// just started depending on, `go generate` needs that resolution to have
// already happened, and the migration needs the generated code `go generate`
// produces to exist so a driver program can import it. Printing the three
// commands in order costs one paragraph; running them here would mean this
// command's own success depending on the network and on a Go toolchain
// finding what it needs, and failing three different, confusing ways instead
// of one.
const initUsage = `sqlb init -module <path> [dir] writes a new project: a schema with one
table, a server that mounts it, and a migration runner — everything else
here operates on a project this leaves you to grow.

    -module <path>   the new project's module path, e.g. github.com/you/app (required)

<dir> defaults to ".". It must not already contain a go.mod.

What it writes:

    go.mod
    doc.go                  placeholder for the package go generate writes into
    <pkg>schema/schema.go   the schema: one Task table, exposed as CRUD + list
    <pkg>schema/sqlb.go     SqlbProject, so ` + "`sqlb generate`" + ` and ` + "`sqlb migrate`" + ` know where output goes
    migrations/             empty; the first migration is a command away, not a file here
    cmd/server/main.go      rest.NewServer, migrations applied from disk at startup
    predicate_test.go       two database-free tests of a hook, with sqlbtest
    sqlb.md                 next steps, a command cheat-sheet, and the REST query grammar

<pkg> is the last path segment of -module, lower-cased to a Go identifier.

What it does not do: run go mod tidy, go generate, or sqlb migrate. Each needs
something this command cannot promise — network, or output an earlier step
produces — so they are the next commands you run, and init prints them.
`

func initCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	module := fs.String("module", "", "the new project's module path (required)")
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, initUsage) }
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	if strings.TrimSpace(*module) == "" {
		return errors.New("init needs -module, for example: sqlb init -module github.com/you/app")
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("init takes at most one directory argument, got %d", fs.NArg())
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	pkg := identFromModule(*module)
	if pkg == "" {
		return fmt.Errorf("init: %q has no path segment usable as a Go package name; "+
			"pass a module ending in one, e.g. github.com/you/app", *module)
	}

	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return fmt.Errorf("init: %s already exists; init writes a new project and refuses to "+
			"overwrite one", filepath.Join(dir, "go.mod"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("init: checking for an existing go.mod: %w", err)
	}

	data := initData{
		Module:    *module,
		Pkg:       pkg,
		SchemaPkg: pkg + "schema",
		EnvPrefix: strings.ToUpper(pkg),
	}

	files := []struct {
		path string
		tmpl string
	}{
		{"go.mod", initGoMod},
		// doc.go exists so the root package is non-empty before `go generate`
		// fills it in. Without it `go mod tidy` cannot resolve cmd/server's
		// import of it — the directory codegen.Options.Package targets has no
		// .go files yet, and that reads as "no such local package" rather than
		// "not generated yet", which fails resolution before generate ever
		// gets to run.
		{"doc.go", initDocGo},
		{filepath.Join(data.SchemaPkg, "schema.go"), initSchemaGo},
		{filepath.Join(data.SchemaPkg, "sqlb.go"), initSqlbGo},
		{filepath.Join("cmd", "server", "main.go"), initMainGo},
		// The scaffold is what an adopter copies from, so what it does not
		// contain is effectively not available. It contained no test, and
		// sqlbtest was therefore undiscoverable from the front door: a
		// consumer wrote an entire tenant boundary and its whole suite against
		// a real Postgres without learning the package existed (#287). This is
		// the test they would have wanted first.
		{"predicate_test.go", initPredicateTest},
		{"sqlb.md", initSqlbMd},
	}
	for _, f := range files {
		full := filepath.Join(dir, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		if err := renderFile(full, f.tmpl, data); err != nil {
			return fmt.Errorf("init: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		return fmt.Errorf("init: %w", err)
	}

	_, _ = fmt.Fprintf(stdout, `wrote a new project to %s:

    go.mod
    doc.go                 (placeholder; go generate fills this package in)
    %s/schema.go
    %s/sqlb.go
    migrations/            (empty)
    cmd/server/main.go
    predicate_test.go       two tests of a hook that need no database, so the
                            suite runs before Postgres does
    sqlb.md                 the next steps below, plus a command and capability
                            cheat-sheet, so they outlive this shell

Next:

    cd %s
    go mod tidy
    go generate ./...
    # if that printed "run go mod tidy again", do — a schema feature can pull
    # in a dependency go generate only now writes an import for
    go run github.com/jryannel/sqlb/cmd/sqlb migrate -name initial_schema ./%s
    export %s_DATABASE_URL='postgres://user:pass@localhost:5432/%s?sslmode=disable'
    go run ./cmd/server
`, dir, data.SchemaPkg, data.SchemaPkg, dir, data.SchemaPkg, data.EnvPrefix, data.Pkg)
	return nil
}

type initData struct {
	Module    string
	Pkg       string
	SchemaPkg string
	EnvPrefix string
}

// identFromModule takes the last path segment of a module path and lowers it
// to a valid, lower-case Go identifier — the same shape codegen already
// requires of a package name, so a project init writes never has one
// generate refuses.
func identFromModule(module string) string {
	seg := module
	if i := strings.LastIndexByte(module, '/'); i >= 0 {
		seg = module[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(seg) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || out[0] < 'a' || out[0] > 'z' {
		return ""
	}
	return out
}

func renderFile(path, tmpl string, data initData) error {
	content := strings.NewReplacer(
		"{{.Module}}", data.Module,
		"{{.Pkg}}", data.Pkg,
		"{{.SchemaPkg}}", data.SchemaPkg,
		"{{.EnvPrefix}}", data.EnvPrefix,
	).Replace(tmpl)
	return os.WriteFile(path, []byte(content), 0o644)
}

const initGoMod = `module {{.Module}}

go 1.25
`

const initDocGo = `// Package {{.Pkg}} holds what ` + "`sqlb generate`" + ` writes from {{.SchemaPkg}} — models,
// the typed column facade, and the REST resources. Run it:
//
//	go generate ./...
package {{.Pkg}}
`

const initSchemaGo = `// Package {{.SchemaPkg}} is {{.Pkg}}'s schema — the single source of truth
// ` + "`sqlb generate`" + ` and ` + "`sqlb migrate`" + ` read.
//
// Add tables, columns and capabilities as the project needs them. Task below
// is not special beyond being what a first run needs something to serve —
// see [github.com/jryannel/sqlb/schema]'s doc comment for the vocabulary.
package {{.SchemaPkg}}

import "github.com/jryannel/sqlb/schema"

// Task is a unit of work.
var Task = schema.Table("tasks",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("title").Searchable().Sortable(),
	schema.Bool("done").Default(schema.Value(false)).Filterable().Sortable(),
	schema.Timestamps(),
).
	Describe("A unit of work.").
	Expose(schema.REST{
		Path:            "/tasks",
		Ops:             schema.CRUD | schema.OpList,
		DefaultPageSize: 25,
		MaxPageSize:     100,
	})
`

// The go:generate line is split across two string literals so that this
// file's own `go generate ./...` at the repository root does not find it.
// go generate's directive scanner is a plain line-based text scan — it does
// not parse Go syntax, so it cannot tell that a "//go:generate" line inside
// a raw string literal here is a template being written out, not a real
// directive. Before this split, `go generate ./...` found the line below,
// inside its own source, and tried to run it from cmd/sqlb — which is
// package main and cannot be imported as a schema package (issues #200,
// #205). Splitting the literal keeps "//go:generate" off of any single
// physical line in this file while leaving the rendered value — what
// actually lands in a new project's sqlb.go, where it must work as a real
// directive — byte-identical to before.
const initSqlbGo = "package {{.SchemaPkg}}\n\n" +
	"//" + "go:generate go run github.com/jryannel/sqlb/cmd/sqlb generate .\n\n" +
	`import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/codegen"
)

// shadowDSNEnv names the scratch database ` + "`sqlb migrate`" + ` replays the migration
// history into, once there is history to replay. The first migration diffs
// against nothing and needs no database at all.
const shadowDSNEnv = "{{.EnvPrefix}}_SHADOW_DSN"

// SqlbProject tells ` + "`sqlb generate`" + ` and ` + "`sqlb migrate`" + ` what this project emits and
// where.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Package: "{{.Pkg}}",
		},
		MigrationsDir: "migrations",
		MinPostgres:   18,
		ShadowDB:      shadowDB,
	}
}

func shadowDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv(shadowDSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf("%s is not set, and replaying the migration history needs a scratch database", shadowDSNEnv)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, ` + "`DROP SCHEMA public CASCADE; CREATE SCHEMA public`" + `); err != nil {
		pool.Close()
		return nil, fmt.Errorf("emptying the shadow database at %s: %w", shadowDSNEnv, err)
	}
	return pool, nil
}
`

const initMainGo = `// Command server runs {{.Pkg}}.
//
//	export {{.EnvPrefix}}_DATABASE_URL='postgres://user:pass@localhost:5432/{{.Pkg}}?sslmode=disable'
//	go run ./cmd/server
//
// Then http://localhost:8080/docs for the API.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/rest"

	"{{.Module}}"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	dsn := os.Getenv("{{.EnvPrefix}}_DATABASE_URL")
	if dsn == "" {
		log.Error("exiting", "error", "{{.EnvPrefix}}_DATABASE_URL is not set")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := rest.Serve(ctx, rest.ServeConfig{
		DSN:     dsn,
		Server:  rest.Config{Title: "{{.Pkg}}", Version: "0.0.0"},
		Log:     log,
		Migrate: migrate,
	}, mount)
	if err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// mount is the seam rest.Serve leaves to the application. This scaffold
// declares one table and no actions, mutations or queries, so it is one
// call; a schema that grows any of those grows this func, not rest.Serve.
func mount(srv *rest.Server, db *sqlb.DB) error {
	return {{.Pkg}}.Register(srv.API, db)
}

// migrate applies migrations/*.sql with goose, reading the directory from
// disk rather than embedding it — the simplest thing that works for
// ` + "`go run ./cmd/server`" + ` from the module root, and enough until this ships as a
// binary that runs somewhere the source tree will not be. See
// github.com/jryannel/sqlb's example/tasks2/migrations for the embedded form.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, "migrations")
}
`

// initSqlbMd is the durable form of the "Next:" block init prints to stdout,
// plus enough of a command and vocabulary cheat-sheet that a second developer
// — or the first one, in six months — does not have to reconstruct it from
// terminal scrollback or from reading rest/params.go and schema/field.go
// directly. It restates rather than links where sqlb.dev is not guaranteed to
// match this binary's version; it links where the answer is long and lives in
// the module this project already imports.
const initSqlbMd = `# {{.Pkg}}

What ` + "`sqlb init`" + ` scaffolded, and how to keep working in it.

## Next steps

    cd .
    go mod tidy
    go generate ./...
    # if that printed "run go mod tidy again", do — a schema feature can pull
    # in a dependency go generate only now writes an import for
    go run github.com/jryannel/sqlb/cmd/sqlb migrate -name initial_schema ./{{.SchemaPkg}}
    export {{.EnvPrefix}}_DATABASE_URL='postgres://user:pass@localhost:5432/{{.Pkg}}?sslmode=disable'
    go run ./cmd/server

Then http://localhost:8080/docs for the generated API.

## Commands

Run from the module root; ` + "`<pkg>`" + ` below means ` + "`./{{.SchemaPkg}}`" + `.

    sqlb generate <pkg>          write every artefact the schema declares
    sqlb check <pkg>             report stale artefacts, write nothing; also
                                 runs Lint and prints its (advisory) diagnostics
    sqlb migrate -name <n> <pkg> write the migration that closes the gap
                                 between the migration history and the schema
    sqlb impact <pkg>            report how a schema edit changes the REST
                                 contract, against a checked-in baseline
    sqlb eject <pkg>             write the exit: the schema as SQL and the
                                 resources as plain handlers, no sqlb import
    sqlb docs <pkg>              write the feature checklist: one section per
                                 endpoint, with a notes block a rerun preserves
    sqlb survey <src> <dst>      the adoption probe — report which tables of an
                                 existing (undeclared) database sqlb could
                                 describe, and why not
    sqlb introspect -dsn <dsn>   read a database and report, or write, the
                                 schema DSL declaration for it

` + "`sqlb help`" + ` prints the full flag list for each. generate, impact, eject and docs
need no database; check needs one only when given ` + "`-database`" + `; migrate replays the
committed migration history into a scratch Postgres (the schema's ` + "`ShadowDB`" + `),
except for the first migration, which diffs against nothing.

## Testing

Two kinds, and the split is worth keeping.

**Predicates, with no database.** ` + "`predicate_test.go`" + ` is scaffolded and passing.
` + "`github.com/jryannel/sqlb/sqlbtest`" + ` is a scripted Executor: it parses no SQL and
evaluates no predicate, and its value is in what it records — the statements
your code produced and the values it bound. That answers the questions a round
trip cannot answer at all:

    exec := sqlbtest.New(sqlbtest.Reply{Cols: []string{"id"}})
    db := sqlb.New(exec).WithHooks(hooks())
    // ... run the code under test, then:
    exec.LastStatement()   // did the hook's predicate reach the SQL?
    exec.LastArgs()        // did the id come from the token, not the body?
    exec.Statements()      // a refused read must have issued none at all

A refused read is the sharp one: after a round trip, "the query ran and matched
nothing" and "the query never ran" both look like zero rows, and they are the
difference between a boundary and a filter that happened to be empty today.

**Rows, against a real Postgres.** Whether a query returns the right rows needs a
database, and nothing above can stand in for it. Keep both in one
` + "`go test ./...`" + ` by skipping when there is no DSN:

    func testDB(t *testing.T) *pgxpool.Pool {
        t.Helper()
        dsn := os.Getenv("{{.EnvPrefix}}_TEST_DATABASE_URL")
        if dsn == "" || testing.Short() {
            t.Skip("set {{.EnvPrefix}}_TEST_DATABASE_URL for the round-trip tests")
        }
        // ... open, migrate, t.Cleanup(close)
    }

Both conditions, not just the env var: a developer who has exported the DSN
otherwise has no way to get the fast lane back, and ` + "`go test -short ./...`" + ` is what
the inner loop wants.

A ` + "`compose.yaml`" + ` with one long-lived Postgres is usually the right shape for that
DSN — the same server ` + "`sqlb migrate`" + ` already needs for its shadow replay. A
container per package is the alternative, and it is materially slower: one
reported port measured 17.3s cold that way against 4.0s cold and 0.4s warm with
a shared server, on the same machine for a comparable number of tests.

## Skills

If you work on this project with a coding agent, sqlb ships skills for it.

The static ones cover the DSL vocabulary, where the builder ends and
hand-written SQL begins, and whether a codebase should adopt sqlb at all:

    npx skills add jryannel/sqlb

That is your invocation, not part of the build; nothing in sqlb depends on Node,
and a skill is a directory with a ` + "`SKILL.md`" + ` in it, so a checkout and
` + "`cp -r skills/sqlb-* .claude/skills/`" + ` is the same thing.

One more is generated from *this* schema — which columns each resource will
actually accept, which is the answer no static document can carry, because
capabilities are opt-in and therefore per-project. It is opt-in because it
writes into a directory sqlb does not own. Set it in ` + "`{{.SchemaPkg}}/sqlb.go`" + `:

    Options: codegen.Options{
        Package:  "{{.Pkg}}",
        SkillDir: ".claude/skills",
    }

` + "`sqlb check`" + ` fails when that file has drifted from the schema, which is the only
reason writing instructions into a repository is safe: a skill that disagrees
with the schema is worse than no skill, since it is confidently wrong about the
one thing it exists to know.

## Schema capability vocabulary

A column has no behavior beyond storage until a capability turns it on —
nothing is filterable, sortable or searchable by default
(github.com/jryannel/sqlb/schema's doc comment is the full reference; this is
the at-a-glance version):

    .Filterable()    reachable by ` + "`?column=op.value`" + ` on the list endpoint
    .Sortable()      reachable by ` + "`?sort=column`" + ` / ` + "`?sort=-column`" + `
    .Searchable()    included in ` + "`?search=`" + `'s substring fan-out
    .Expandable()    reachable by ` + "`?expand=relation`" + ` (on a Ref column)
    .Hidden()        never read back through the generated surface
    .ReadOnly()      accepted on read, refused on create/update
    .Scoped()        every generated query, and the write ops, are confined by
                     this column's registered hook — see rest/doc.go's "reads
                     are hooked" section
    .Unique()        a unique constraint, and — combined with .Filterable() —
                     a single-row lookup by that column
    .Nullable()      the Go type becomes a pointer / sql null-wrapper
    .Default(v)      omitted from create leaves the database default in place

## REST query grammar

Generated list endpoints share one grammar (rest/params.go documents each
parameter per-resource, scoped to that resource's actual capable columns —
this is the shape, not a specific resource's parameter list):

    ?column=value                 shorthand for eq
    ?column=op.value              gt, gte, lt, lte, ne, in, nin, between,
                                  like, ilike, contains, startswith, endswith
                                  (pattern ops need a text column)
    ?column=eq.a&column=eq.b      repeated params conjoin
    ?or=(a.eq.1,b.eq.2)           disjunction group; ?and=, ?not= too
    ?filter=<url-encoded JSON>    arbitrary and/or nesting the grammar above
                                  cannot spell — see the ` + "`filter`" + ` param's own
                                  description in the generated OpenAPI doc
    ?expand=relation              only relations declared .Expandable()
    ?sort=col,-col2                most significant first; ` + "`-`" + ` for descending
    ?select=col,col2               omitted columns are absent, not null
    ?search=text                   across every .Searchable() column
    ?page=N&per_page=N             or ?limit=&?offset=
    ?cursor=<token>                keyset paging; prefer this for a full walk
    ?count=exact                   include the total row count (costs a query)

The exact operators and enums a given endpoint accepts are in its entry at
http://localhost:8080/docs once the server is running — that document is
generated from this same schema, so it never drifts from what the columns
above actually declare.
`
