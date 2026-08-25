package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/schema"
	"github.com/mind-vm/sqlb/shadow"
)

// introspect is the one verb that needs no schema package, and therefore no
// driver.
//
// Every other command here compiles a program that imports the project's schema
// package, because a table is registered by the side effect of that import
// (ADR-0004) and a prebuilt binary cannot read a registry nothing has filled.
// This one runs the other way round: it reads a database and produces the
// declaration, so there is nothing to link and it executes in this process.
//
// That asymmetry is why it was missing, and why its absence cost something. The
// first step of every adoption — look at the database you are trying to declare
// — was the only step with no shell, so diagnosing a refusal meant writing a
// throwaway Go program, standing up a scratch container, running it, reading two
// lines, and deleting the program so it did not get committed (issue #112).
func introspectCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("introspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dsn        = fs.String("dsn", "", "the database to read (required)")
		migrations = fs.String("migrations", "", "replay this migration directory into -dsn and read back what it built, instead of reading -dsn as it stands")
		module     = fs.String("module", "", "read the tables of one module, whose tables are named <module>_<table>")
		only       = fs.String("only", "", "comma-separated tables to read, and no others")
		exclude    = fs.String("exclude", "", "comma-separated tables to leave out")
		out        = fs.String("out", "", "write the schema declaration as Go source to this file instead of reporting")
		pkgName    = fs.String("package", "", "package name for -out (default: the file's directory name)")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr, introspectUsage)
	}
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	if strings.TrimSpace(*dsn) == "" {
		return fmt.Errorf("introspect needs a database, for example: " +
			"sqlb introspect -dsn postgres://user@localhost/app")
	}
	if fs.NArg() > 0 {
		// The other verbs take a package as their last argument, so this is the
		// mistake the habit produces. Said plainly, because the reason is not
		// guessable: this verb genuinely has nothing to import.
		return fmt.Errorf(
			"introspect takes no package argument (got %q) — it reads a database rather than a "+
				"declaration, so there is no schema package to link", fs.Arg(0))
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	opts := introspect.Options{
		Module:  *module,
		Only:    splitFlag(*only),
		Exclude: splitFlag(*exclude),
	}

	var (
		reg *schema.Registry
		rep *introspect.Report
	)
	if strings.TrimSpace(*migrations) != "" {
		// The stronger source, and the one the adoption log kept reaching for.
		// A live database tells you what it looks like; a replayed history tells
		// you what the checked-in migrations *build*, which is a different and
		// better claim when the question is why a drift gate failed (ADR-0014).
		reg, rep, _, err = shadow.Build(ctx, pool, shadow.Options{
			Dir:     *migrations,
			Module:  *module,
			Only:    opts.Only,
			Exclude: opts.Exclude,
		})
		if err != nil {
			return fmt.Errorf("replaying %s: %w", *migrations, err)
		}
	} else {
		reg, rep, err = introspect.Registry(ctx, pool, opts)
		if err != nil {
			return fmt.Errorf("reading the database: %w", err)
		}
	}

	if strings.TrimSpace(*out) != "" {
		return writeSchema(reg, rep, *out, *pkgName, stdout, stderr)
	}

	_, _ = fmt.Fprintf(stdout, "%d table(s) read\n\n", len(reg.Tables()))
	_, _ = fmt.Fprintln(stdout, rep)

	// Non-zero on a skip, so this is usable in a script and in a gate. A skip is
	// the answer to "why did the drift gate refuse this module", and a command
	// that exits 0 while printing it makes the caller parse prose to find out.
	if !rep.Empty() {
		return exitCode(1)
	}
	return nil
}

// writeSchema renders the registry as Go source, which is the other half of the
// adoption: sixty-nine tables become sixty-nine declarations to review rather
// than to write.
func writeSchema(reg *schema.Registry, rep *introspect.Report, path, pkgName string, stdout, stderr io.Writer) error {
	if pkgName == "" {
		pkgName = packageFromPath(path)
	}
	src, err := codegen.RenderSchema(reg, codegen.SchemaOptions{Package: pkgName})
	if err != nil {
		return fmt.Errorf("rendering the schema: %w", err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s (package %s, %d tables)\n", path, pkgName, len(reg.Tables()))

	// On stderr and after the success line, deliberately. The file is written
	// either way — a partial declaration is what an adoption starts from — but
	// what was left out has to arrive somewhere the caller cannot mistake for
	// part of the file.
	if !rep.Empty() {
		_, _ = fmt.Fprintf(stderr,
			"\nthe declaration does not describe the database completely:\n%s\n", rep)
		return exitCode(1)
	}
	if len(rep.Extensions) > 0 {
		_, _ = fmt.Fprintln(stderr, rep)
	}
	return nil
}

func splitFlag(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// packageFromPath takes the package name from the directory the file is written
// into, which is the Go convention and is right often enough to be the default.
func packageFromPath(path string) string {
	dir := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir = path[:i]
	} else {
		return "schema"
	}
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[i+1:]
	}
	if dir == "" || dir == "." {
		return "schema"
	}
	return dir
}

const introspectUsage = `sqlb introspect reads a database and reports what the schema DSL can declare.

Usage:

    sqlb introspect -dsn <dsn> [flags]

Flags:

    -dsn <dsn>            the database to read (required)
    -migrations <dir>     replay this migration directory into -dsn and read back
                          what it built, rather than reading -dsn as it stands.
                          The stronger source: it says what the checked-in history
                          *builds*, not what someone's hotfix left behind
    -module <name>        read one module, whose tables are named <module>_<table>
    -only a,b             read these tables and no others
    -exclude a,b          leave these tables out
    -out <file>           write the declaration as Go source instead of reporting
    -package <name>       package name for -out (default: the directory's name)

Unlike every other sqlb command this takes no package argument: it reads a
database rather than a declaration, so there is no schema package to link.

It exits non-zero when the database holds something the DSL cannot declare, so
"why did the drift gate refuse this module" is a command rather than a program
you have to write.
`
