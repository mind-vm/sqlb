package codegen

// `sqlb eject` writes the exit.
//
// The strongest objection to sqlb is not a missing feature. It is that sqlc and
// chi are cheap to reverse because they own almost nothing, while sqlb owns the
// schema, the migrations, the wire format, the client and the CLI — and that
// concentration is the risk an adopter is actually weighing (issue #19, and
// §12.6 of the adoption review). This verb answers it the only way a pre-1.0
// library with no consumers credibly can: the exit is generated, and it is
// tested in CI.
//
// What comes out is a package that imports pgx and the standard library and
// nothing else — no sqlb, no huma, no router. The DDL as SQL, the row structs
// as plain structs, the statements as SQL text you can read, and the endpoints
// as net/http handlers. It compiles on its own, and deleting sqlb from go.mod
// afterwards is a supported end state rather than a hypothetical one.
//
// # What it does not carry, and why that is stated rather than hidden
//
// An eject that silently served fewer requests than the resource it replaced
// would be worse than no eject at all, so the emitted README lists every gap by
// name and the handlers carry the same list in their doc comments. The line is
// drawn at the difference between *the surface* and *the engine*:
//
//   - keyset cursors, `?select`, `?expand`, and the JSON filter tree are the
//     engine. Reproducing them would mean emitting a copy of sqlb, which is not
//     an exit — it is a fork with a different import path.
//   - CRUD, list, the filter operators that are one SQL fragment each, `?sort`,
//     `?search`, `?page`/`?per_page` and `?count=exact` are the surface, and
//     they come out whole, with the same wire format and the same envelope.
//
// # The obligation comes with it
//
// A table that declared `Scoped` or `SoftDelete` will not mount a REST resource
// until a hook confines it ([ADR-0030]). An ejected handler with no hook would
// serve every tenant's rows, which is the failure that decision closed — so the
// emitted `Register` refuses, at startup, a resource whose obligations are nil,
// naming the column that asked. The seam is a plain function field: the exit
// keeps the property, and drops the machinery.
//
// [ADR-0030]: https://github.com/mind-vm/sqlb/blob/main/docs/architecture.md#declared-scope-is-required

import (
	"bytes"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// EjectOptions configures an eject run.
type EjectOptions struct {
	// Registry supplies the tables. Required.
	Registry *schema.Registry
	// Dir is the output directory. Required. Everything lands directly in it:
	// the emitted package is one package.
	Dir string
	// Package is the package clause. Empty takes the directory's base name.
	Package string
	// MinPostgres declares the oldest Postgres major version the emitted DDL
	// must run on, exactly as it does for a migration — so the SQL in the exit
	// is the SQL the project was already applying.
	MinPostgres int
}

func (o EjectOptions) pkg() string {
	if o.Package != "" {
		return o.Package
	}
	return filepath.Base(o.Dir)
}

func (o EjectOptions) validate() error {
	switch {
	case o.Registry == nil:
		return fmt.Errorf("codegen: EjectOptions.Registry is required")
	case o.Dir == "":
		return fmt.Errorf("codegen: EjectOptions.Dir is required")
	}
	if !isGoIdent(o.pkg()) {
		return fmt.Errorf(
			"codegen: EjectOptions.Dir %q gives the package name %q, which is not a Go identifier; set EjectOptions.Package",
			o.Dir, o.pkg())
	}
	return nil
}

// Eject writes the ejected package and returns the paths it wrote.
func Eject(opts EjectOptions) ([]string, error) {
	files, err := renderEject(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, name := range sortedKeys(files) {
		path := filepath.Join(opts.Dir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

// EjectCheck reports which ejected files are missing or out of date.
//
// The exit is only worth having if it still works, and a committed one rots the
// same way generated code does — someone adds a column, the ejected handlers
// keep serving the old shape, and nobody finds out until the day they are
// needed. So this is `check` for the way out, and it belongs in CI beside it.
func EjectCheck(opts EjectOptions) ([]string, error) {
	files, err := renderEject(opts)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, name := range sortedKeys(files) {
		path := filepath.Join(opts.Dir, name)
		existing, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			stale = append(stale, path+" (missing)")
		case err != nil:
			return nil, err
		case !bytes.Equal(existing, files[name]):
			stale = append(stale, path+" (out of date)")
		}
	}
	return stale, nil
}

// renderEject produces the ejected files in memory.
func renderEject(opts EjectOptions) (map[string][]byte, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := opts.Registry.Validate(); err != nil {
		return nil, fmt.Errorf("codegen: schema does not validate, refusing to eject:\n%w", err)
	}
	tables := ownTablesFor(opts.Registry, opts.Package)
	if len(tables) == 0 {
		return nil, fmt.Errorf("codegen: registry has no tables (is the schema package imported for its side effects?)")
	}

	ddl, err := ejectDDL(opts)
	if err != nil {
		return nil, err
	}

	models, err := ejectModels(opts, tables)
	if err != nil {
		return nil, err
	}
	store, err := ejectStore(opts, tables)
	if err != nil {
		return nil, err
	}

	files := map[string][]byte{
		"schema.sql": ddl,
		"models.go":  models,
		"store.go":   store,
		"support.go": []byte(ejectSupportSource(opts.pkg())),
		"README.md":  []byte(ejectReadme(opts, tables)),
	}

	// Handlers exist only for what the schema exposed. A registry with no REST
	// surface ejects its schema and its statements and stops there, which is
	// the whole exit for a project that used sqlb as a query builder.
	if exposed := exposedTables(tables); len(exposed) > 0 {
		handlers, err := ejectHandlers(opts, exposed)
		if err != nil {
			return nil, err
		}
		files["handlers.go"] = handlers
	}
	return files, nil
}

func exposedTables(tables []*schema.TableDef) []*schema.TableDef {
	var out []*schema.TableDef
	for _, t := range tables {
		if t.Rest() != nil {
			out = append(out, t)
		}
	}
	return out
}

// ejectDDL renders the whole schema as SQL, by diffing it against nothing.
//
// It is the same path `sqlb migrate` takes for a first migration, so the file
// is not a second rendering of the schema that could disagree with the one the
// project has been applying.
func ejectDDL(opts EjectOptions) ([]byte, error) {
	var diffOpts []migrate.Option
	if opts.MinPostgres > 0 {
		diffOpts = append(diffOpts, migrate.MinPostgres(opts.MinPostgres))
	}
	changes, err := migrate.Diff(nil, opts.Registry, diffOpts...)
	if err != nil {
		return nil, fmt.Errorf("codegen: rendering the schema as SQL: %w", err)
	}

	var b bytes.Buffer
	b.WriteString("-- Ejected from a sqlb schema by `sqlb eject`. This file is yours now.\n")
	b.WriteString("--\n")
	b.WriteString("-- It is the same DDL `sqlb migrate` would have written for a first\n")
	b.WriteString("-- migration, so applying it produces the database the schema declared.\n")
	b.WriteString("-- Computed columns are absent by construction: they were expressions,\n")
	b.WriteString("-- never storage, and store.go carries them in the SELECT list instead.\n\n")
	for _, c := range changes {
		if strings.TrimSpace(c.Up) == "" {
			continue
		}
		if c.Comment != "" {
			fmt.Fprintf(&b, "-- %s\n", c.Comment)
		}
		b.WriteString(strings.TrimSpace(c.Up))
		b.WriteString("\n\n")
	}
	return b.Bytes(), nil
}

// ejectModels emits the row structs, without a single sqlb tag.
//
// The `db` tags stay, because they are the mapping between a column and a
// field and every hand-written scan in store.go is written against them; the
// `json` tags stay, because they are the wire format the clients already speak.
// The `sqlb` tags go, because there is nothing left to read them.
func ejectModels(opts EjectOptions, tables []*schema.TableDef) ([]byte, error) {
	b := new(bytes.Buffer)
	fmt.Fprintln(b, `
// The rows. These are the structs the generated models were, with the sqlb tags
// removed: nothing reads them any more. Relations are gone with them — ?expand
// was one statement built by the engine, and the exit does not carry it.`)

	for _, t := range tables {
		typeName := TypeName(t)
		fmt.Fprintln(b)
		if c := t.Comment(); c != "" {
			docLines(b, "", typeName+" "+lowerFirst(c))
		} else {
			fmt.Fprintf(b, "// %s is a row of %s.\n", typeName, t.Name())
		}
		fmt.Fprintf(b, "type %s struct {\n", typeName)
		for _, f := range t.Fields() {
			d := f.Desc()
			fmt.Fprintf(b, "\t%s %s `db:%q %s`", GoName(d.Name), ejectGoType(d), d.Name,
				jsonTag(d, opts.Registry.Wire()))
			switch {
			case d.Computed():
				fmt.Fprint(b, " // computed: ", d.Expr)
			case d.Comment != "":
				fmt.Fprint(b, " // ", d.Comment)
			}
			fmt.Fprintln(b)
		}
		fmt.Fprintln(b, "}")
	}
	return ejectFile("models.go", opts.pkg(), "The row structs, with the sqlb tags removed.", b)
}

// ejectGoType is the Go type a column takes in the ejected models.
//
// Deliberately the default mapping, with enums as plain strings: a type
// override reached the generated models through a package this one does not
// import, and an enum's constants were vocabulary rather than a constraint the
// database enforces — the CHECK in schema.sql still does that. The README says
// both, so a project that used either knows what to reapply.
func ejectGoType(d *schema.FieldDesc) string {
	if d.Type == schema.TypeVector {
		// A vector is pgvector's type and sqlb.Vector was its codec. Ejected,
		// it is the wire form pgx will hand back for an unknown type.
		if d.Nullable {
			return "*string"
		}
		return "string"
	}
	return d.GoType()
}

// ejectFile assembles one emitted Go file: the body, and then a header whose
// import block names the packages the body turned out to use.
//
// The order is what makes it correct. An import set decided before the body is
// written is a set of predictions, and the condition that puts a package in one
// of these files is never the condition that is convenient to state up front —
// `fmt` reaches handlers.go only through the obligation refusals, `errors` only
// through the by-id reads, `encoding/json` only through the request bodies, and
// `pgx` reaches store.go only through a table that has a primary key to look a
// row up by. Each of those was restated at the top of its emitter and kept in
// step by hand, until it was not: a schema whose exposed tables declared
// neither Scoped nor SoftDelete emitted a `fmt` that nothing used, which is an
// exit that does not compile. Asking the finished text closes that off for
// every package at once, including the next one a handler grows a use of.
func ejectFile(name, pkg, desc string, body *bytes.Buffer) ([]byte, error) {
	// Only models.go carries the real "Package pkg is ..." line (pkgDoc),
	// because ejectHeader is called once per file and go/doc concatenates
	// every file's package comment into `go doc`'s output: three identical
	// copies read as a stutter, and one file is enough for the package to
	// have a comment at all. models.go rather than handlers.go or store.go
	// because it exists even for a schema that exposes nothing over REST.
	b := ejectHeader(pkg, desc, name == "models.go", ejectImports(body.String()))
	b.Write(body.Bytes())
	return gofmt(name, b.Bytes())
}

// ejectImportable maps an import path to the identifier a use of it begins
// with, for every package the emitters can reach for.
//
// Written out rather than taken from the last element of the path, because two
// of these do not agree with it: encoding/json is json, and pgx/v5 is pgx. A
// package missing from this map is simply never imported, which is why adding
// one is part of using it.
var ejectImportable = map[string]string{
	"context":                 "context",
	"encoding/json":           "json",
	"errors":                  "errors",
	"fmt":                     "fmt",
	"net/http":                "http",
	"strings":                 "strings",
	"time":                    "time",
	"github.com/jackc/pgx/v5": "pgx",
}

// ejectImports are the packages an emitted body names, sorted.
//
// Over tokens rather than over the text, because a package named in a comment
// is prose and not a use — and these files are more comment than code, with a
// schema's own comments copied into them verbatim. A column commented "the API
// used to carry a time.Time here" would import time by a substring search, and
// put the unused import back by another route; TestEjectedGoCompiles keeps a
// case for exactly that. go/scanner is lexical, so it needs no import block to
// run on the fragment it is about to write one for, and it drops the comments
// before this sees them.
func ejectImports(src string) []string {
	used := map[string]bool{}
	var sc scanner.Scanner
	fset := token.NewFileSet()
	sc.Init(fset.AddFile("", fset.Base(), len(src)), []byte(src), nil, 0)
	prev := ""
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.PERIOD && prev != "" {
			used[prev] = true
		}
		if tok == token.IDENT {
			prev = lit
		} else {
			prev = ""
		}
	}

	out := make([]string, 0, len(ejectImportable))
	for path, qualifier := range ejectImportable {
		if used[qualifier] {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// ejectHeader is the file banner. It says the file was generated *and* that
// editing it is the point, which is the opposite of what every other emitted
// file in this project says.
//
// desc is the file-specific line under the banner. The banner and desc sit a
// blank comment line above `package`, which is deliberate and costs the
// package its doc comment: go/doc only credits a comment immediately adjacent
// to the package clause, with nothing in between, so a banner meant to read as
// a note rather than documentation must not also be read as the latter.
//
// pkgDoc adds that adjacent comment — "// Package pkg is ..." — for the one
// caller that should carry it. Every caller could, since each already gets
// its own ejectHeader call, but `go doc` concatenates every file's package
// comment into one page, and three identical copies would just be a stutter.
func ejectHeader(pkg, desc string, pkgDoc bool, imports []string) *bytes.Buffer {
	var b bytes.Buffer
	fmt.Fprintln(&b, "// Ejected from a sqlb schema by `sqlb eject`. This file is yours now:")
	fmt.Fprintln(&b, "// edit it, delete parts of it, or keep regenerating it — `sqlb eject -check`")
	fmt.Fprintln(&b, "// reports drift for as long as you want it to and is meant to be dropped")
	fmt.Fprintln(&b, "// from CI on the day you stop.")
	fmt.Fprintln(&b, "//")
	fmt.Fprintf(&b, "// %s\n", desc)
	fmt.Fprintln(&b)
	if pkgDoc {
		fmt.Fprintf(&b, "// Package %s is the exit `sqlb eject` wrote: pgx and the standard\n", pkg)
		fmt.Fprintln(&b, "// library, and nothing else.")
	}
	fmt.Fprintf(&b, "package %s\n", pkg)
	if len(imports) > 0 {
		fmt.Fprintln(&b)
		if len(imports) == 1 {
			fmt.Fprintf(&b, "import %q\n", imports[0])
		} else {
			fmt.Fprintln(&b, "import (")
			var std, other []string
			for _, path := range imports {
				if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
					other = append(other, path)
					continue
				}
				std = append(std, path)
			}
			for _, path := range std {
				fmt.Fprintf(&b, "\t%q\n", path)
			}
			if len(std) > 0 && len(other) > 0 {
				fmt.Fprintln(&b)
			}
			for _, path := range other {
				fmt.Fprintf(&b, "\t%q\n", path)
			}
			fmt.Fprintln(&b, ")")
		}
	}
	return &b
}

// runEject is the driver's `eject` verb: write the exit, or report that the
// committed one has drifted.
//
// `-check` is the gate half, and it is deliberately not part of `sqlb check`.
// Generated code is stale when it disagrees with the schema and there is one
// right answer; an ejected package is *meant* to be edited, and the day someone
// takes the exit is the day this check should be deleted rather than fixed. Two
// verbs keep that distinction visible in CI.
func runEject(p Project, opts Options, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sqlb eject", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "report whether the committed exit is stale; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		say(stderr, "sqlb: eject takes no positional arguments, got %q\n", fs.Arg(0))
		return 2
	}

	dir := p.EjectDir
	if dir == "" {
		// Beside the generated code, which is where a reader of the repository
		// will look for it.
		dir = filepath.Join(opts.Dir, "ejected")
	}
	ejectOpts := EjectOptions{
		Registry:    opts.Registry,
		Dir:         dir,
		Package:     p.EjectPackage,
		MinPostgres: p.MinPostgres,
	}

	if *check {
		stale, err := EjectCheck(ejectOpts)
		if err != nil {
			line(stderr, err)
			return 1
		}
		if len(stale) > 0 {
			line(stderr, "sqlb: the ejected package is out of date; run: sqlb eject")
			for _, f := range stale {
				line(stderr, "  "+f)
			}
			return 1
		}
		line(stderr, "sqlb: the ejected package is current")
		return 0
	}

	written, err := Eject(ejectOpts)
	if err != nil {
		line(stderr, err)
		return 1
	}
	for _, f := range written {
		line(stdout, f)
	}
	say(stderr, "sqlb: wrote %d files; %s/README.md says what came out and what did not\n",
		len(written), dir)
	return 0
}
