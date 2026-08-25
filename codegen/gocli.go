package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// This file emits a Go command-line client, built on spf13/cobra, for the REST
// surface the schema exposes. ADR-0029.
//
// It is the same generator argument the TypeScript client makes, aimed at a
// different consumer. There, the vocabulary a column's capabilities define is
// carried across the wire as types, and an illegal request fails at `tsc`. A
// CLI has no compile step to fail at, so the vocabulary lands in `--help`
// instead: one flag per filterable column, its operators named in the usage
// string, and `--sort`/`--select`/`--expand` completing from the columns that
// opted in. What the type system refuses for the web client, the help text
// discloses for the CLI — which matters most for the caller this exists for,
// an agent, because reading `--help` is how it learns what the API accepts
// without a round trip and without a 400 to interpret.
//
// The transport is a field rather than a decision. Base URL, credentials,
// timeout and retry are not derivable from a schema, so the generated package
// carries a Client with a built-in HTTP implementation and a Transport field
// that replaces it — the seam `rest` makes by mounting onto a huma.API the
// application built, and the TypeScript client makes by taking a request
// function.
//
// Two packages, not one file. The client — Request, Transport, Client, Do, Run
// and the problem document — imports the standard library and nothing else, and
// the cobra command tree is a second package that imports it.
//
// This file used to argue for one file, on the grounds that both halves would
// need cobra. That was true of the split it was considering (invariant runtime
// versus per-table commands) and false of the one that matters: nothing about
// Client, Do or Request ever needed cobra, so a Go program that wanted the
// typed client — a sync job, a server-to-server caller, an admin tool with a
// command tree of its own — was taking a command-line framework to make one
// HTTP request (#97). It is the split the TypeScript emitter already makes, and
// for the same reason: a consumer that does not want the framework should not
// pay for it.
//
// The old argument's real point survives, and is answered rather than ignored:
// an import set that depends on which operations a schema exposes is a failure
// that surfaces at the consumer's build, because gofmt parses an unused import
// happily. So the CLI's imports are derived from the rendered body rather than
// written down — see usedImports.

// renderGoClient emits the transport-only client: the request encoder, the
// transport seam, the cursor walk and the typed problem document.
//
// Returns nil when the schema exposes nothing, for the reason renderGoCLI does.
func renderGoClient(opts Options) ([]byte, error) {
	resources, err := cliResources(opts.Registry, opts.cliName())
	if err != nil {
		return nil, err
	}
	if len(resources) == 0 {
		return nil, nil
	}
	// The runtime is emitted whole whatever the schema contains, so its import
	// set is invariant and can be written down.
	b := header(opts.clientPackage(), []string{
		"bytes", "context", "encoding/json", "errors", "fmt", "io",
		"net/http", "net/url", "os", "strconv", "strings", "time",
	})
	b.WriteString(clientRuntime)
	return gofmt(opts.clientFile(), b.Bytes())
}

// renderGoCLI emits the cobra command tree over the generated client.
//
// Returns nil when the schema exposes nothing, so a package with no REST
// surface does not acquire a dependency on cobra.
func renderGoCLI(opts Options) ([]byte, error) {
	resources, err := cliResources(opts.Registry, opts.cliName())
	if err != nil {
		return nil, err
	}
	if len(resources) == 0 {
		return nil, nil
	}
	importPath, err := opts.clientImportPath()
	if err != nil {
		return nil, err
	}

	// Rendered first, imports second. Which of these the tree touches depends
	// on the operations the schema exposes — a schema with no list command
	// mentions no url.Values — and an import written down for a body that does
	// not use it compiles here and fails at the consumer.
	var body bytes.Buffer
	body.WriteString(cliCobra)
	cliRoot(&body, opts, resources)
	for _, r := range resources {
		cliResourceSection(&body, r)
	}

	b := header(opts.cliPackage(), usedImports(body.String(), []string{
		"bytes", "context", "encoding/json", "errors", "fmt", "io",
		"net/http", "net/url", "os", "strconv", "strings", "time",
		"github.com/spf13/cobra", "github.com/spf13/pflag", importPath,
	}))
	b.Write(body.Bytes())

	return gofmt(opts.cliFile(), b.Bytes())
}

// usedImports keeps the candidates the body actually refers to, by the
// qualifier each one contributes.
//
// It exists so that splitting the emission cannot produce an unused import. The
// alternative — deriving the set from the schema — means re-deriving it
// whenever a command emits a new call, and getting that wrong is silent here
// and fatal at the consumer's build.
func usedImports(body string, candidates []string) []string {
	var out []string
	for _, path := range candidates {
		if strings.Contains(body, importQualifier(path)+".") {
			out = append(out, path)
		}
	}
	return out
}

// importQualifier is the name a package is referred to by, which for every
// import here is the last element of its path.
func importQualifier(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// cliResource is one exposed table, resolved once so the emitters below read
// as output rather than as lookups.
type cliResource struct {
	table    *schema.TableDef
	goPlural string // Tasks, for the generated constructor names
	command  string // tasks, as typed on the command line
	bin      string // taskctl, for the worked examples
	path     string
	ops      schema.Op

	filterable []*schema.FieldDesc
	sortable   []string
	selectable []string
	searchable []string
	relations  []string
	pk         string

	// wire is the schema's wire case. A flag is a local affordance and keeps
	// its kebab-cased spelling either way; what this changes is the query
	// parameter and the request-body key, which are wire format.
	wire schema.WireCase
}

// n spells one of this resource's column names the way the wire does.
func (r *cliResource) n(name string) string { return r.wire.WireName(name) }

func cliResources(reg *schema.Registry, bin string) ([]cliResource, error) {
	var out []cliResource
	for _, t := range reg.Tables() {
		rest := t.Rest()
		if rest == nil {
			continue
		}
		r := cliResource{
			table:     t,
			goPlural:  GoName(t.LocalName()),
			command:   cliName(t.LocalName()),
			bin:       bin,
			path:      rest.Path,
			ops:       rest.Ops,
			relations: expandableRelations(reg, t),
			wire:      reg.Wire(),
		}
		for _, f := range t.Fields() {
			d := f.Desc()
			// A hidden column has no spelling here at all: not as a filter, not
			// in the projection vocabulary, not as a flag on a write. This is
			// the guarantee ADR-0006 makes, and the CLI is one more place it
			// would be easy to leak from. A write-only column is narrower: it
			// still gets a write flag, from bodyFields, just not a place in
			// this read-side projection list.
			if d.Hidden || d.WriteOnly {
				continue
			}
			if d.PrimaryKey {
				r.pk = r.n(d.Name)
			}
			r.selectable = append(r.selectable, r.n(d.Name))
			if d.Filterable {
				r.filterable = append(r.filterable, d)
			}
			if d.Sortable {
				r.sortable = append(r.sortable, r.n(d.Name))
			}
			if d.Searchable {
				r.searchable = append(r.searchable, r.n(d.Name))
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// cliRoot emits New, which assembles the whole tree.
func cliRoot(b *bytes.Buffer, opts Options, resources []cliResource) {
	name := opts.cliName()
	env := cliEnvPrefix(name)

	fmt.Fprintf(b, "\n// New returns the root command.\n")
	fmt.Fprintf(b, "//\n// c carries what the schema cannot derive — the base URL, the credential, the\n")
	fmt.Fprintf(b, "// timeout — and is bound to the persistent flags below. Pass nil for a client\n")
	fmt.Fprintf(b, "// configured entirely from flags and the environment; pass one with Transport\n")
	fmt.Fprintf(b, "// set to route every request through your own code instead.\n")
	fmt.Fprintf(b, "//\n//\tfunc main() {\n")
	fmt.Fprintf(b, "//\t    if err := %s.New(nil).Execute(); err != nil {\n", opts.cliPackage())
	fmt.Fprintf(b, "//\t        os.Exit(1)\n//\t    }\n//\t}\n")
	fmt.Fprintln(b, "func New(c *client.Client) *cobra.Command {")
	fmt.Fprintln(b, "\tif c == nil {\n\t\tc = &client.Client{}\n\t}")
	fmt.Fprintln(b, "\troot := &cobra.Command{")
	fmt.Fprintf(b, "\t\tUse:   %q,\n", name)
	fmt.Fprintf(b, "\t\tShort: %q,\n", "Command-line client for the "+name+" API")
	fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(cliRootLong(name, env)))
	// A failed request has already been reported; printing the usage after it
	// buries the error under a page of flags, which is the one thing a caller
	// reading stderr does not need.
	fmt.Fprintln(b, "\t\tSilenceUsage: true,")
	fmt.Fprintln(b, "\t}")
	fmt.Fprintln(b, "\t// Column names are snake_case, and cobra flags are conventionally kebab.")
	fmt.Fprintln(b, "\t// Accepting both spellings means a caller that read a column name out of")
	fmt.Fprintln(b, "\t// sqlb.json can type it verbatim.")
	fmt.Fprintln(b, "\troot.SetGlobalNormalizationFunc(normalizeFlag)")
	fmt.Fprintln(b)
	// Every default is the field's own value where the caller set one, because
	// binding a flag writes its default into the variable — so a Client
	// configured in Go would otherwise be undone by registration.
	fmt.Fprintln(b, "\tflags := root.PersistentFlags()")
	fmt.Fprintf(b, "\tflags.StringVar(&c.BaseURL, \"base-url\", orString(c.BaseURL, envOr(%q, \"http://localhost:8080\")),\n", env+"_BASE_URL")
	fmt.Fprintf(b, "\t\t\"Root of the API, without a trailing slash. Defaults to $%s.\")\n", env+"_BASE_URL")
	fmt.Fprintf(b, "\tflags.StringVar(&c.Token, \"token\", orString(c.Token, os.Getenv(%q)),\n", env+"_TOKEN")
	fmt.Fprintf(b, "\t\t\"Bearer token, sent as Authorization. Defaults to $%s, which is where it\\n\"+\n", env+"_TOKEN")
	fmt.Fprintln(b, "\t\t\t\"belongs: a token on the command line is visible in the process list.\")")
	fmt.Fprintln(b, "\tflags.DurationVar(&c.Timeout, \"timeout\", orDuration(c.Timeout, 30*time.Second), \"Per-request timeout.\")")
	fmt.Fprintln(b, "\tflags.BoolVar(&c.Compact, \"compact\", c.Compact,")
	fmt.Fprintln(b, "\t\t\"Write each response as one line rather than indented.\")")
	fmt.Fprintln(b, "\tflags.BoolVarP(&c.Verbose, \"verbose\", \"v\", c.Verbose,")
	fmt.Fprintln(b, "\t\t\"Log each request's method and URL to stderr.\")")
	fmt.Fprintln(b)
	for _, r := range resources {
		fmt.Fprintf(b, "\troot.AddCommand(new%sCommand(c))\n", r.goPlural)
	}
	fmt.Fprintln(b, "\treturn root\n}")
}

func cliRootLong(name, env string) string {
	return fmt.Sprintf(`%s drives the API generated from this project's sqlb schema.

Every response is written to stdout as JSON, so output pipes into jq without a
flag. Failures go to stderr and set a non-zero exit code; a rejected request
prints what the server would have accepted instead, which is usually the whole
fix.

Configuration comes from $%s_BASE_URL and $%s_TOKEN, or from --base-url
and --token.

Filtering, sorting, searching and expansion are restricted to the columns that
declared the capability, so a "list" command's --help is an exact statement of
what that resource accepts.`, name, env, env)
}

// cliResourceSection emits one resource's command and its operations.
// singleton reports whether this resource is the caller's one row, in which
// case no command of it takes an id and every route is the collection path
// itself (#166).
func (r cliResource) singleton() bool { return r.ops.Has(schema.OpSingleton) }

// readsOne reports whether the resource serves a single row by either shape.
func (r cliResource) readsOne() bool { return r.ops.Has(schema.OpRead) || r.singleton() }

// itemRoute is what help text calls the single-row route.
func (r cliResource) itemRoute() string {
	if r.singleton() {
		return r.path
	}
	return r.path + "/{id}"
}

func cliResourceSection(b *bytes.Buffer, r cliResource) {
	fmt.Fprintf(b, "\n// %s\n", cliRule(r.path))

	short := r.table.Comment()
	if short == "" {
		short = "Work with " + r.command
	}
	fmt.Fprintf(b, "\n// new%sCommand groups the operations %s exposes.\n", r.goPlural, r.path)
	fmt.Fprintf(b, "func new%sCommand(c *client.Client) *cobra.Command {\n", r.goPlural)
	fmt.Fprintln(b, "\tcmd := &cobra.Command{")
	fmt.Fprintf(b, "\t\tUse:   %q,\n", r.command)
	fmt.Fprintf(b, "\t\tShort: %q,\n", strings.TrimSuffix(short, "."))
	fmt.Fprintln(b, "\t}")
	if r.ops.Has(schema.OpList) {
		fmt.Fprintf(b, "\tcmd.AddCommand(new%sListCommand(c))\n", r.goPlural)
	}
	if r.readsOne() {
		fmt.Fprintf(b, "\tcmd.AddCommand(new%sGetCommand(c))\n", r.goPlural)
	}
	if r.ops.Has(schema.OpCreate) {
		fmt.Fprintf(b, "\tcmd.AddCommand(new%sCreateCommand(c))\n", r.goPlural)
	}
	if r.canUpdate() {
		fmt.Fprintf(b, "\tcmd.AddCommand(new%sUpdateCommand(c))\n", r.goPlural)
	}
	if r.ops.Has(schema.OpDelete) {
		fmt.Fprintf(b, "\tcmd.AddCommand(new%sDeleteCommand(c))\n", r.goPlural)
	}
	cliActionAdds(b, r)
	fmt.Fprintln(b, "\treturn cmd\n}")

	if r.ops.Has(schema.OpList) {
		cliListCommand(b, r)
	}
	if r.readsOne() {
		cliGetCommand(b, r)
	}
	if r.ops.Has(schema.OpCreate) {
		cliWriteCommand(b, r, forCreate)
	}
	if r.canUpdate() {
		cliWriteCommand(b, r, forUpdate)
	}
	if r.ops.Has(schema.OpDelete) {
		cliDeleteCommand(b, r)
	}
	cliActionCommands(b, r)
}

// canUpdate reports whether a patch command would have anything to write. A
// resource whose every column is read-only, hidden or immutable exposes no
// update, exactly as the generated REST body does not.
func (r cliResource) canUpdate() bool {
	return r.ops.Has(schema.OpUpdate) && len(bodyFields(r.table, forUpdate)) > 0
}

func cliListCommand(b *bytes.Buffer, r cliResource) {
	fmt.Fprintf(b, "\n// new%sListCommand is GET %s.\n", r.goPlural, r.path)
	fmt.Fprintf(b, "func new%sListCommand(c *client.Client) *cobra.Command {\n", r.goPlural)

	fmt.Fprintln(b, "\tvar (")
	for _, d := range r.filterable {
		fmt.Fprintf(b, "\t\t%s []string\n", cliFilterVar(d))
	}
	if len(r.sortable) > 0 {
		fmt.Fprintln(b, "\t\tsort []string")
	}
	fmt.Fprintln(b, "\t\tselect_ []string")
	if len(r.relations) > 0 {
		fmt.Fprintln(b, "\t\texpand []string")
	}
	if len(r.searchable) > 0 {
		fmt.Fprintln(b, "\t\tsearch string")
	}
	fmt.Fprintln(b, "\t\tpage    int")
	fmt.Fprintln(b, "\t\tperPage int")
	fmt.Fprintln(b, "\t\tcount   bool")
	if r.pk != "" {
		fmt.Fprintln(b, "\t\tcursor  string")
		fmt.Fprintln(b, "\t\tall     bool")
	}
	fmt.Fprintln(b, "\t)")

	fmt.Fprintln(b, "\tcmd := &cobra.Command{")
	fmt.Fprintln(b, "\t\tUse:   \"list\",")
	fmt.Fprintf(b, "\t\tShort: %q,\n", "List "+r.command)
	fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(cliListLong(r)))
	fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString(cliListExample(r)))
	fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
	fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, _ []string) error {")
	fmt.Fprintln(b, "\t\t\tq := url.Values{}")
	for _, d := range r.filterable {
		// Repeating a parameter conjoins its conditions, so a repeated flag
		// becomes a repeated parameter rather than a joined value.
		fmt.Fprintf(b, "\t\t\tfor _, v := range %s {\n\t\t\t\tq.Add(%q, v)\n\t\t\t}\n", cliFilterVar(d), r.n(d.Name))
	}
	if len(r.searchable) > 0 {
		fmt.Fprintln(b, "\t\t\tif search != \"\" {\n\t\t\t\tq.Set(\"search\", search)\n\t\t\t}")
	}
	if len(r.sortable) > 0 {
		fmt.Fprintln(b, "\t\t\tif len(sort) > 0 {\n\t\t\t\tq.Set(\"sort\", strings.Join(sort, \",\"))\n\t\t\t}")
	}
	fmt.Fprintln(b, "\t\t\tif len(select_) > 0 {\n\t\t\t\tq.Set(\"select\", strings.Join(select_, \",\"))\n\t\t\t}")
	if len(r.relations) > 0 {
		fmt.Fprintln(b, "\t\t\tif len(expand) > 0 {\n\t\t\t\tq.Set(\"expand\", strings.Join(expand, \",\"))\n\t\t\t}")
	}
	// Only a flag the caller actually typed is sent. Sending the zero value
	// would override the resource's own default page size with 0, which the
	// server then has to reinterpret.
	fmt.Fprintln(b, "\t\t\tif cmd.Flags().Changed(\"page\") {\n\t\t\t\tq.Set(\"page\", strconv.Itoa(page))\n\t\t\t}")
	fmt.Fprintln(b, "\t\t\tif cmd.Flags().Changed(\"per-page\") {\n\t\t\t\tq.Set(\"per_page\", strconv.Itoa(perPage))\n\t\t\t}")
	fmt.Fprintln(b, "\t\t\tif count {\n\t\t\t\tq.Set(\"count\", \"exact\")\n\t\t\t}")
	if r.pk != "" {
		fmt.Fprintln(b, "\t\t\tif cursor != \"\" {\n\t\t\t\tq.Set(\"cursor\", cursor)\n\t\t\t}")
		// The server refuses the combination too. Refusing it here names both
		// flags rather than the parameters they became.
		fmt.Fprintln(b, "\t\t\tif all && (cmd.Flags().Changed(\"page\") || cursor != \"\") {")
		fmt.Fprintln(b, "\t\t\t\treturn errors.New(\"--all walks the result set with cursors, so it cannot be combined with --page or --cursor\")")
		fmt.Fprintln(b, "\t\t\t}")
	}
	fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodGet, Path: %q, Query: q}, %s)\n",
		r.path, cliAllArg(r))
	fmt.Fprintln(b, "\t\t},\n\t}")

	fmt.Fprintln(b, "\tflags := cmd.Flags()")
	for _, d := range r.filterable {
		fmt.Fprintf(b, "\tflags.StringArrayVar(&%s, %q, nil,\n\t\t%s)\n",
			cliFilterVar(d), cliFlagName(d.Name), goRawString(cliFilterUsage(d)))
		if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
			// Completion offers the values with an `eq.` prefix as well as
			// bare, because both are legal and the prefixed form is the one a
			// caller reaches for second.
			fmt.Fprintf(b, "\tregisterCompletion(cmd, %q, %s)\n",
				cliFlagName(d.Name), goSliceLiteral(cliEnumCompletions(d.EnumValues)))
		}
	}
	if len(r.sortable) > 0 {
		terms := make([]string, 0, len(r.sortable)*2)
		for _, name := range r.sortable {
			terms = append(terms, name, "-"+name)
		}
		fmt.Fprintf(b, "\tflags.StringSliceVar(&sort, \"sort\", nil,\n\t\t%s)\n",
			goRawString("Ordering, most significant first. Prefix a column with - for descending.\nColumns: "+strings.Join(r.sortable, ", ")+"."))
		fmt.Fprintf(b, "\tregisterCompletion(cmd, \"sort\", %s)\n", goSliceLiteral(terms))
	}
	fmt.Fprintf(b, "\tflags.StringSliceVar(&select_, \"select\", nil,\n\t\t%s)\n",
		goRawString("Columns to return; the primary key is always included. Omitted columns are\nabsent from the response rather than present and empty.\nColumns: "+strings.Join(r.selectable, ", ")+"."))
	fmt.Fprintf(b, "\tregisterCompletion(cmd, \"select\", %s)\n", goSliceLiteral(r.selectable))
	if len(r.relations) > 0 {
		fmt.Fprintf(b, "\tflags.StringSliceVar(&expand, \"expand\", nil,\n\t\t%s)\n",
			goRawString("Relations to embed in each row. Relations: "+strings.Join(r.relations, ", ")+"."))
		fmt.Fprintf(b, "\tregisterCompletion(cmd, \"expand\", %s)\n", goSliceLiteral(r.relations))
	}
	if len(r.searchable) > 0 {
		fmt.Fprintf(b, "\tflags.StringVar(&search, \"search\", \"\",\n\t\t%s)\n",
			goRawString("Case-insensitive substring match, fanned out across "+strings.Join(r.searchable, ", ")+"."))
	}
	fmt.Fprintln(b, "\tflags.IntVar(&page, \"page\", 1, \"Page number, 1-based.\")")
	fmt.Fprintf(b, "\tflags.IntVar(&perPage, \"per-page\", 0, %s)\n",
		goRawString("Rows per page. A value above the resource's ceiling is capped rather than\nrejected."))
	fmt.Fprintf(b, "\tflags.BoolVar(&count, \"count\", false, %s)\n",
		goRawString("Include the total number of matching rows. It costs a second query, which is\nwhy has_more is always present and this is not."))
	if r.pk != "" {
		fmt.Fprintf(b, "\tflags.StringVar(&cursor, \"cursor\", \"\", %s)\n",
			goRawString("Resume after the next_cursor of a previous response. Cursor paging costs the\nsame at any depth and cannot skip or repeat a row when the table is written to\nmid-walk, so prefer it to --page for anything that walks a whole result set. A\ncursor is only valid for the --sort it was issued under."))
		fmt.Fprintf(b, "\tflags.BoolVar(&all, \"all\", false, %s)\n",
			goRawString("Follow next_cursor until the result set is exhausted and write every row as one\nresponse. This is the loop a caller would otherwise write, and it pages by\ncursor, so a concurrent insert cannot make it read a row twice."))
	}
	fmt.Fprintln(b, "\treturn cmd\n}")
}

func cliListLong(r cliResource) string {
	var b strings.Builder
	if c := r.table.Comment(); c != "" {
		fmt.Fprintf(&b, "%s\n\n", c)
	}
	fmt.Fprintf(&b, "GET %s\n\n", r.path)
	b.WriteString("A filter flag takes operator.value, or a bare value for equality, and repeating\n")
	b.WriteString("one conjoins its conditions — a lower and an upper bound on the same column are\n")
	b.WriteString("two occurrences of its flag.\n\n")
	b.WriteString("Only the columns that declared a capability are reachable through it, so the\n")
	b.WriteString("flags below are the complete list: a column absent from them cannot be\n")
	b.WriteString("filtered, sorted or searched by any spelling.")
	return b.String()
}

// cliListExample is the worked example cobra prints under the flags.
//
// It is written out of the resource's own columns rather than left abstract,
// because the reader most likely to need it is one deciding whether a request
// is expressible at all, and a runnable line answers that faster than a
// grammar does.
func cliListExample(r cliResource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  # The first page, twenty rows at a time\n  %s\n", r.line("list --per-page 20"))
	if d := r.exampleFilter(); d != nil {
		fmt.Fprintf(&b, "\n  # Conditions on one column conjoin; conditions on two intersect\n  %s\n",
			r.line("list --"+cliFilterExample(d)))
	}
	if len(r.sortable) > 0 {
		fmt.Fprintf(&b, "\n  # Ordered, and projected down to the columns about to be read\n  %s\n",
			r.line(fmt.Sprintf("list --sort -%s --select %s",
				r.sortable[0], strings.Join(cliFirstN(r.selectable, 2), ","))))
	}
	if r.pk != "" {
		fmt.Fprintf(&b, "\n  # Every matching row, walked by cursor rather than by page number\n  %s\n",
			r.line("list --all"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// line writes one example invocation of this resource.
func (r cliResource) line(rest string) string {
	return r.bin + " " + r.command + " " + rest
}

// exampleFilter is the column a worked filter is written against.
//
// An enum first, because its example carries a value a reader can recognise
// rather than a placeholder; then anything that is neither the primary key nor
// a foreign key, since filtering a collection by a key is the request `get` and
// `?expand` already serve better.
func (r cliResource) exampleFilter() *schema.FieldDesc {
	var fallback *schema.FieldDesc
	for _, d := range r.filterable {
		switch {
		case d.Type == schema.TypeEnum && len(d.EnumValues) > 0:
			return d
		case d.PrimaryKey || d.Ref != nil:
			if fallback == nil {
				fallback = d
			}
		case fallback == nil || fallback.PrimaryKey || fallback.Ref != nil:
			fallback = d
		}
	}
	return fallback
}

// cliFilterExample writes one plausible condition for a column, with an
// operator its type actually accepts.
func cliFilterExample(d *schema.FieldDesc) string {
	flag := cliFlagName(d.Name)
	switch {
	case d.Type == schema.TypeEnum && len(d.EnumValues) > 0:
		return fmt.Sprintf("%s eq.%s", flag, d.EnumValues[0])
	case d.Type == schema.TypeBool:
		return flag + " eq.true"
	case d.Type == schema.TypeSmallInt, d.Type == schema.TypeInt, d.Type == schema.TypeBigInt,
		d.Type == schema.TypeReal, d.Type == schema.TypeFloat, d.Type == schema.TypeNumeric:
		return fmt.Sprintf("%s gte.10 --%s lt.100", flag, flag)
	case d.Type == schema.TypeTimestamp:
		return flag + " gte.2026-01-01T00:00:00Z"
	case d.Type == schema.TypeDate:
		return flag + " gte.2026-01-01"
	case d.Type == schema.TypeUUID:
		return flag + " eq.00000000-0000-0000-0000-000000000000"
	}
	return flag + " contains.something"
}

func cliFirstN(values []string, n int) []string {
	if len(values) < n {
		return values
	}
	return values[:n]
}

// cliAllArg is the paging argument to run: a resource with no primary key
// cannot issue a cursor, so there is no walk to offer.
func cliAllArg(r cliResource) string {
	if r.pk != "" {
		return "all"
	}
	return "false"
}

func cliGetCommand(b *bytes.Buffer, r cliResource) {
	fmt.Fprintf(b, "\n// new%sGetCommand is GET %s.\n", r.goPlural, r.itemRoute())
	fmt.Fprintf(b, "func new%sGetCommand(c *client.Client) *cobra.Command {\n", r.goPlural)
	if len(r.relations) > 0 {
		fmt.Fprintln(b, "\tvar expand []string")
	}
	fmt.Fprintln(b, "\tcmd := &cobra.Command{")
	if r.singleton() {
		fmt.Fprintln(b, "\t\tUse:   \"get\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Fetch your "+Singular(r.command))
		fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(fmt.Sprintf(
			"GET %s\n\nThere is no id: this resource holds one row per caller and the server settles\nwhich, so a request that authenticated has already said everything the route\nneeds. Answers 404 when you have no row yet.", r.path)))
	} else {
		fmt.Fprintln(b, "\t\tUse:   \"get <id>\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Fetch one "+Singular(r.command)+" by primary key")
		fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(fmt.Sprintf(
			"GET %s/{id}\n\nThe item endpoint declares no query parameters but expand, and rejects any\nother rather than answering a question that was not asked.", r.path)))
	}
	fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString(cliGetExample(r)))
	if r.singleton() {
		fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
	} else {
		fmt.Fprintln(b, "\t\tArgs:  cobra.ExactArgs(1),")
	}
	fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, args []string) error {")
	fmt.Fprintln(b, "\t\t\tq := url.Values{}")
	if len(r.relations) > 0 {
		fmt.Fprintln(b, "\t\t\tif len(expand) > 0 {\n\t\t\t\tq.Set(\"expand\", strings.Join(expand, \",\"))\n\t\t\t}")
	}
	if r.singleton() {
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodGet, Path: %q, Query: q}, false)\n", r.path)
	} else {
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodGet, Path: client.ItemPath(%q, args[0]), Query: q}, false)\n", r.path)
	}
	fmt.Fprintln(b, "\t\t},\n\t}")
	if len(r.relations) > 0 {
		fmt.Fprintf(b, "\tcmd.Flags().StringSliceVar(&expand, \"expand\", nil,\n\t\t%s)\n",
			goRawString("Relations to embed. Relations: "+strings.Join(r.relations, ", ")+"."))
		fmt.Fprintf(b, "\tregisterCompletion(cmd, \"expand\", %s)\n", goSliceLiteral(r.relations))
	}
	fmt.Fprintln(b, "\treturn cmd\n}")
}

func cliDeleteCommand(b *bytes.Buffer, r cliResource) {
	fmt.Fprintf(b, "\n// new%sDeleteCommand is DELETE %s.\n", r.goPlural, r.itemRoute())
	fmt.Fprintf(b, "func new%sDeleteCommand(c *client.Client) *cobra.Command {\n", r.goPlural)
	fmt.Fprintln(b, "\treturn &cobra.Command{")
	if r.singleton() {
		fmt.Fprintln(b, "\t\tUse:   \"delete\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Delete your "+Singular(r.command))
		fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(fmt.Sprintf(
			"DELETE %s\n\nThere is no id: this resource holds one row per caller. A successful delete\nwrites nothing, so there is nothing to print.", r.path)))
		fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString("  "+r.line("delete")))
		fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
		fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, args []string) error {")
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodDelete, Path: %q}, false)\n", r.path)
		fmt.Fprintln(b, "\t\t},\n\t}\n}")
		return
	}
	fmt.Fprintln(b, "\t\tUse:   \"delete <id>\",")
	fmt.Fprintf(b, "\t\tShort: %q,\n", "Delete one "+Singular(r.command)+" by primary key")
	fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(fmt.Sprintf(
		"DELETE %s/{id}\n\nA successful delete writes nothing: the response carries no body, so there is\nnothing to print.", r.path)))
	fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString("  "+r.line("delete "+cliIDPlaceholder(r))))
	fmt.Fprintln(b, "\t\tArgs:  cobra.ExactArgs(1),")
	fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, args []string) error {")
	fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodDelete, Path: client.ItemPath(%q, args[0])}, false)\n", r.path)
	fmt.Fprintln(b, "\t\t},\n\t}\n}")
}

// cliWriteCommand emits create or update, which differ in enough places to
// share a body: the path takes an id, every field is optional, and a nullable
// column needs a spelling for "set this back to null" that a flag alone cannot
// give.
func cliWriteCommand(b *bytes.Buffer, r cliResource, kind bodyKind) {
	fields := bodyFields(r.table, kind)
	// The declared inputs that are not columns take a flag like anything else
	// the body carries (#309): the CLI is a client, and what a client sends is
	// what the body declares. Only a create declares them, so `all` and
	// `fields` are the same list on a patch.
	var props []*schema.Field
	if kind == forCreate {
		props = createInput(r.table)
	}
	all := append(append([]*schema.Field(nil), fields...), props...)
	verb, method := "Create", "http.MethodPost"
	if kind == forUpdate {
		verb, method = "Update", "http.MethodPatch"
	}

	fmt.Fprintf(b, "\n// new%s%sCommand is %s %s%s.\n", r.goPlural, verb,
		strings.TrimPrefix(method, "http.Method"), r.path, cliItemSuffix(r, kind))
	fmt.Fprintf(b, "func new%s%sCommand(c *client.Client) *cobra.Command {\n", r.goPlural, verb)

	fmt.Fprintln(b, "\tvar (")
	for _, f := range all {
		fmt.Fprintf(b, "\t\t%s %s\n", cliValueVar(f.Desc()), cliFlagType(f.Desc()))
	}
	nullable := cliNullableNames(fields)
	if kind == forUpdate && len(nullable) > 0 {
		fmt.Fprintln(b, "\t\tsetNull []string")
	}
	fmt.Fprintln(b, "\t)")

	fmt.Fprintln(b, "\tcmd := &cobra.Command{")
	if kind == forCreate {
		fmt.Fprintln(b, "\t\tUse:   \"create\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Create one "+Singular(r.command))
		fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
	} else if r.singleton() {
		fmt.Fprintln(b, "\t\tUse:   \"update\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Update your "+Singular(r.command))
		fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
	} else {
		fmt.Fprintln(b, "\t\tUse:   \"update <id>\",")
		fmt.Fprintf(b, "\t\tShort: %q,\n", "Update one "+Singular(r.command)+" by primary key")
		fmt.Fprintln(b, "\t\tArgs:  cobra.ExactArgs(1),")
	}
	fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(cliWriteLong(r, kind)))
	fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString(cliWriteExample(r, kind, all)))
	if kind == forCreate || r.singleton() {
		fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, _ []string) error {")
	} else {
		fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, args []string) error {")
	}
	fmt.Fprintln(b, "\t\t\tbody := map[string]any{}")
	for _, f := range fields {
		cliBodyAssignment(b, f.Desc(), r.wire)
	}
	for _, f := range props {
		// Verbatim, not the schema's wire case: a declared property is not a
		// column, so its name is not something a case is a function of. It is
		// sent exactly as it was declared, which is what the Go body, the
		// TypeScript client and the Dart client all do with it too.
		cliBodyAssignment(b, f.Desc(), schema.Verbatim)
	}
	if kind == forUpdate && len(nullable) > 0 {
		// A flag can say "title is now empty" but not "title is now null", and
		// the two write different SQL. This is the command-line form of the
		// presence map the generated patch body keeps for the same reason.
		fmt.Fprintf(b, "\t\t\tif err := setNullFields(body, setNull, %s); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n",
			goSliceLiteral(nullable))
	}
	if kind == forUpdate {
		fmt.Fprintln(b, "\t\t\tif len(body) == 0 {")
		fmt.Fprintln(b, "\t\t\t\treturn errors.New(\"nothing to update: pass at least one field flag\")")
		fmt.Fprintln(b, "\t\t\t}")
		if r.singleton() {
			fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: %s, Path: %q, Body: body}, false)\n", method, r.path)
		} else {
			fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: %s, Path: client.ItemPath(%q, args[0]), Body: body}, false)\n", method, r.path)
		}
	} else {
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: %s, Path: %q, Body: body}, false)\n", method, r.path)
	}
	fmt.Fprintln(b, "\t\t},\n\t}")

	fmt.Fprintln(b, "\tflags := cmd.Flags()")
	for _, f := range all {
		d := f.Desc()
		fmt.Fprintf(b, "\tflags.%sVar(&%s, %q, %s,\n\t\t%s)\n",
			cliFlagKind(d), cliValueVar(d), cliFlagName(d.Name), cliFlagZero(d),
			goRawString(cliWriteUsage(d, kind)))
		if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
			fmt.Fprintf(b, "\tregisterCompletion(cmd, %q, %s)\n",
				cliFlagName(d.Name), goSliceLiteral(d.EnumValues))
		}
		// Required only on create, and only where the database has no answer of
		// its own: a defaulted column left out means the default applies, and a
		// nullable one left out means NULL.
		if kind == forCreate && !optionalOnCreate(d) {
			fmt.Fprintf(b, "\t_ = cmd.MarkFlagRequired(%q)\n", cliFlagName(d.Name))
		}
	}
	if kind == forUpdate && len(nullable) > 0 {
		fmt.Fprintf(b, "\tflags.StringArrayVar(&setNull, \"set-null\", nil,\n\t\t%s)\n",
			goRawString("Set a column to null, which no value flag can express. Repeatable.\nColumns: "+strings.Join(nullable, ", ")+"."))
		fmt.Fprintf(b, "\tregisterCompletion(cmd, \"set-null\", %s)\n", goSliceLiteral(nullable))
	}
	fmt.Fprintln(b, "\treturn cmd\n}")
}

func cliItemSuffix(r cliResource, kind bodyKind) string {
	if kind == forUpdate && !r.singleton() {
		return "/{id}"
	}
	return ""
}

func cliWriteLong(r cliResource, kind bodyKind) string {
	var b strings.Builder
	if kind == forCreate {
		fmt.Fprintf(&b, "POST %s\n\n", r.path)
		b.WriteString("Read-only columns have no flag: the database or a BeforeCreate hook owns them,\n")
		b.WriteString("so there is nothing for a caller to send. A column with a default is optional,\n")
		b.WriteString("and leaving it out means the database supplies the value rather than the zero\n")
		b.WriteString("value overwriting it.")
		// The declared inputs are the flags that are not columns, and nothing
		// else in this help says so — a caller reading a value back off a
		// subsequent `get` would otherwise expect to find it (#309).
		if props := createInput(r.table); len(props) > 0 {
			var flags []string
			for _, f := range props {
				flags = append(flags, "--"+cliFlagName(f.Desc().Name))
			}
			fmt.Fprintf(&b, "\n\n%s %s not a column: the server derives what it stores from\n%s, "+
				"so nothing sent there comes back on a later read.",
				strings.Join(flags, ", "), plural(len(flags), "is", "are"), plural(len(flags), "it", "them"))
		}
		return b.String()
	}
	fmt.Fprintf(&b, "PATCH %s\n\n", r.itemRoute())
	b.WriteString("Only the flags you pass are sent, so an update writes the columns it names and\n")
	b.WriteString("no others — which is also why a flag left out and a flag set to an empty value\n")
	b.WriteString("mean different things. Immutable columns have no flag here: they are settable\n")
	b.WriteString("once, at create.")
	return b.String()
}

// cliWriteExample writes one runnable invocation, filling every flag the
// command requires and nothing it does not.
func cliWriteExample(r cliResource, kind bodyKind, fields []*schema.Field) string {
	var args []string
	if kind == forUpdate {
		// One flag is enough to show the shape of a patch, and listing every
		// column would suggest that one has to name them all.
		if !r.singleton() {
			args = append(args, cliIDPlaceholder(r))
		}
		if d := cliExampleField(fields); d != nil {
			args = append(args, "--"+cliFlagName(d.Name)+" "+cliValueExample(d))
		}
	}
	for _, f := range fields {
		d := f.Desc()
		// On create, the required flags are the example: they are what the
		// command will refuse to run without.
		if kind == forUpdate || optionalOnCreate(d) {
			continue
		}
		args = append(args, "--"+cliFlagName(d.Name)+" "+cliValueExample(d))
	}
	verb := "create"
	if kind == forUpdate {
		verb = "update"
	}
	line := "  " + r.line(verb+" "+strings.Join(args, " "))
	if kind == forUpdate {
		nullable := cliNullableNames(fields)
		if len(nullable) > 0 {
			// The column's own spelling, which is what --set-null's usage lists
			// and what an error response would name. The kebab form works too.
			line += "\n\n  # Back to null, which no value flag can say\n  " +
				r.line("update "+cliIDPlaceholder(r)+" --set-null "+nullable[0])
		}
	}
	return line
}

// cliExampleField is the column a worked write is written against: an enum
// first, because its example carries a value a reader can recognise, then
// anything that is not a foreign key, whose value is a placeholder either way.
func cliExampleField(fields []*schema.Field) *schema.FieldDesc {
	var fallback *schema.FieldDesc
	for _, f := range fields {
		d := f.Desc()
		switch {
		case d.Type == schema.TypeEnum && len(d.EnumValues) > 0:
			return d
		case d.Ref != nil:
			if fallback == nil {
				fallback = d
			}
		case fallback == nil || fallback.Ref != nil:
			fallback = d
		}
	}
	return fallback
}

// cliValueExample is a plausible value for one column, in the spelling the
// server parses.
func cliValueExample(d *schema.FieldDesc) string {
	switch d.Type {
	case schema.TypeEnum:
		if len(d.EnumValues) > 0 {
			return d.EnumValues[0]
		}
	case schema.TypeBool:
		return "true"
	case schema.TypeSmallInt, schema.TypeInt, schema.TypeBigInt:
		return "1"
	case schema.TypeReal, schema.TypeFloat, schema.TypeNumeric:
		return "1.5"
	case schema.TypeTimestamp:
		return "2026-01-01T09:00:00Z"
	case schema.TypeDate:
		return "2026-01-01"
	case schema.TypeTime:
		return "09:00:00"
	case schema.TypeUUID:
		return "<" + d.Name + ">"
	case schema.TypeJSON:
		return `'{"key":"value"}'`
	case schema.TypeBytes:
		return "<base64>"
	}
	return "'<" + d.Name + ">'"
}

// cliGetExample shows the read, and the expansion if there is one, because a
// relation embedded in the same response is the request a caller would
// otherwise make twice.
func cliGetExample(r cliResource) string {
	id := " " + cliIDPlaceholder(r)
	if r.singleton() {
		id = ""
	}
	line := "  " + r.line("get"+id)
	if len(r.relations) > 0 {
		line += "\n\n  # With " + strings.Join(r.relations, " and ") + " embedded, in one request\n  " +
			r.line("get"+id+" --expand "+strings.Join(r.relations, ","))
	}
	return line
}

// cliIDPlaceholder stands in for a primary key in an example, since there is no
// value a generator could know.
func cliIDPlaceholder(r cliResource) string {
	if r.pk == "" {
		return "<id>"
	}
	return "<" + r.pk + ">"
}

// cliBodyAssignment emits the line that copies one flag into the request body,
// if the caller passed it.
//
// Presence is read from the flag rather than from the value, because a flag
// left out and a flag set to the zero value must send different requests: the
// first writes nothing, the second writes 0, "" or false.
func cliBodyAssignment(b *bytes.Buffer, d *schema.FieldDesc, wire schema.WireCase) {
	name := cliFlagName(d.Name)
	v := cliValueVar(d)
	fmt.Fprintf(b, "\t\t\tif cmd.Flags().Changed(%q) {\n", name)
	if d.Type == schema.TypeJSON {
		// A jsonb column takes a document, so the flag's text is the value
		// rather than a string containing it. Checking it here names the flag;
		// sending it as a JSON string instead would store the text and succeed.
		fmt.Fprintf(b, "\t\t\t\tif !json.Valid([]byte(%s)) {\n", v)
		fmt.Fprintf(b, "\t\t\t\t\treturn fmt.Errorf(\"--%s: not valid JSON: %%s\", %s)\n", name, v)
		fmt.Fprintf(b, "\t\t\t\t}\n")
		fmt.Fprintf(b, "\t\t\t\tbody[%q] = json.RawMessage(%s)\n", wire.WireName(d.Name), v)
	} else {
		fmt.Fprintf(b, "\t\t\t\tbody[%q] = %s\n", wire.WireName(d.Name), v)
	}
	fmt.Fprintln(b, "\t\t\t}")
}

// cliNullableNames is the columns a patch may set to null.
func cliNullableNames(fields []*schema.Field) []string {
	var out []string
	for _, f := range fields {
		if f.Desc().Nullable {
			out = append(out, f.Desc().Name)
		}
	}
	return out
}

// cliFlagType is the Go type of the variable a flag writes into.
//
// Times, uuids, enums and json arrive as text and are validated by the server
// against the column, which is the only place that knows the column's type.
// Parsing them here would put a second, weaker validator in front of the real
// one and produce two different messages for the same mistake.
func cliFlagType(d *schema.FieldDesc) string {
	switch d.Type {
	case schema.TypeSmallInt:
		return "int16"
	case schema.TypeInt:
		return "int32"
	case schema.TypeBigInt:
		return "int64"
	case schema.TypeReal:
		return "float32"
	case schema.TypeFloat, schema.TypeNumeric:
		return "float64"
	case schema.TypeBool:
		return "bool"
	}
	return "string"
}

// cliFlagKind is the pflag method that binds the variable.
func cliFlagKind(d *schema.FieldDesc) string {
	switch cliFlagType(d) {
	case "int32":
		return "Int32"
	case "int64":
		return "Int64"
	case "float64":
		return "Float64"
	case "bool":
		return "Bool"
	}
	return "String"
}

func cliFlagZero(d *schema.FieldDesc) string {
	switch cliFlagType(d) {
	case "int32", "int64":
		return "0"
	case "float64":
		return "0"
	case "bool":
		return "false"
	}
	return `""`
}

// cliWriteUsage documents one writable column: what it holds, what the wire
// format is where the flag is text standing in for something else, and what
// happens if it is left out.
func cliWriteUsage(d *schema.FieldDesc, kind bodyKind) string {
	var b strings.Builder
	if c := d.Comment; c != "" {
		b.WriteString(strings.TrimSuffix(c, ".") + ". ")
	}
	switch d.Type {
	case schema.TypeTimestamp:
		b.WriteString("RFC 3339 timestamp, e.g. 2026-07-28T09:00:00Z. ")
	case schema.TypeDate:
		b.WriteString("Date as YYYY-MM-DD. ")
	case schema.TypeTime:
		b.WriteString("Time of day as HH:MM:SS. ")
	case schema.TypeUUID:
		b.WriteString("UUID. ")
	case schema.TypeJSON:
		b.WriteString("A JSON document, sent as-is rather than as a string. ")
	case schema.TypeBytes:
		b.WriteString("Base64-encoded bytes. ")
	case schema.TypeEnum:
		if len(d.EnumValues) > 0 {
			fmt.Fprintf(&b, "One of: %s. ", strings.Join(d.EnumValues, ", "))
		}
	}
	if d.Ref != nil {
		fmt.Fprintf(&b, "References %s. ", cliRefTarget(d))
	}
	switch {
	case kind == forUpdate:
		// Every patch flag is optional by construction and the command's Long
		// says so, so there is nothing to add — except where the column has no
		// comment and no format note, which would otherwise leave the flag
		// documented by its own name.
		if b.Len() == 0 {
			fmt.Fprintf(&b, "Sets %s.", d.Name)
		}
	case d.Nullable:
		b.WriteString("Optional; left out, the column is null.")
	case d.DatabaseSupplied():
		b.WriteString("Optional; left out, the database supplies its default.")
	default:
		b.WriteString("Required.")
	}
	return strings.TrimSpace(b.String())
}

func cliRefTarget(d *schema.FieldDesc) string {
	if d.Ref.Table != nil {
		return d.Ref.Table.Name()
	}
	return d.Ref.Name
}

// cliFilterUsage documents the operator vocabulary one column accepts.
//
// The operator set is narrowed by column type, exactly as the server narrows
// it: a pattern match needs text, and a null test needs a nullable column, so
// offering either where it does not apply would document a request that
// parsing rejects.
func cliFilterUsage(d *schema.FieldDesc) string {
	var b strings.Builder
	if c := d.Comment; c != "" {
		b.WriteString(strings.TrimSuffix(c, ".") + ".\n")
	}
	if d.Array {
		fmt.Fprintf(&b, "Filter on %s, an array column: written operator.value, or a bare\n", d.Name)
		b.WriteString("comma-separated list for whole-array equality.\n")
	} else {
		fmt.Fprintf(&b, "Filter on %s, written operator.value, or a bare value for equality.\n", d.Name)
	}
	b.WriteString("Repeat the flag to conjoin conditions. Operators: ")
	b.WriteString(strings.Join(cliOperators(d), ", "))
	b.WriteString(".")
	if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
		fmt.Fprintf(&b, "\nValues: %s.", strings.Join(d.EnumValues, ", "))
	}
	return b.String()
}

// cliOperators mirrors the set rest documents for a column, and filter parses.
func cliOperators(d *schema.FieldDesc) []string {
	// An array column takes containment and whole-array equality, and none of
	// the ordering or pattern operators. Naming them here is the point of the
	// CLI: --help states what the resource accepts without a request having to
	// be sent to find out (ADR-0029).
	if d.Array {
		ops := []string{"eq", "ne", "has", "hasany", "hasall", "nhas", "nhasany", "nhasall"}
		if d.Nullable {
			ops = append(ops, "isnull", "notnull")
		}
		return ops
	}
	// A document column takes containment and nothing else. Without this branch
	// it fell through to the scalar list below and --help offered `gt` and
	// `like` on a jsonb column, which the server refuses — the same mistake the
	// array branch above exists to avoid.
	if d.Type == schema.TypeJSON {
		ops := []string{"hasdoc", "nhasdoc"}
		if d.Nullable {
			ops = append(ops, "isnull", "notnull")
		}
		return ops
	}
	ops := []string{"eq", "ne", "gt", "gte", "lt", "lte", "in", "nin", "between"}
	if d.Nullable {
		ops = append(ops, "isnull", "notnull")
	}
	// The pattern operators need a text column. Enum and uuid columns are Go
	// strings, so the server offers them there too; matching that here keeps
	// the help honest rather than conservative.
	if d.Type.GoType() == "string" {
		ops = append(ops, "like", "ilike", "contains", "startswith", "endswith")
	}
	return ops
}

func cliEnumCompletions(values []string) []string {
	out := make([]string, 0, len(values)*2)
	out = append(out, values...)
	for _, v := range values {
		out = append(out, "eq."+v)
	}
	return out
}

// cliFilterVar is the local a filter flag writes into.
func cliFilterVar(d *schema.FieldDesc) string { return "filter" + GoName(d.Name) }

// cliValueVar is the local a write flag writes into.
func cliValueVar(d *schema.FieldDesc) string { return "val" + GoName(d.Name) }

// cliFlagName is the column name as a flag: kebab-case, which is the cobra
// convention. The normalisation function on the root command accepts the
// snake_case spelling too, so a name copied out of the manifest also works.
// cliFlagName turns a column name into a flag word.
//
// It kebab-cases both spellings a schema can carry, so the flag is the same
// whichever WireCase is in force: created_at and createdAt both give
// --created-at. A flag is not a wire format — it is a local affordance that maps
// onto one — and holding it stable means switching WireCase does not rewrite
// every documented command line (ADR-0036's amendment).
func cliFlagName(column string) string {
	var b strings.Builder
	b.Grow(len(column) + 4)
	for i := 0; i < len(column); i++ {
		c := column[i]
		switch {
		case c == '_':
			b.WriteByte('-')
		case c >= 'A' && c <= 'Z':
			if b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// cliName is a table's local name as a command word.
func cliName(local string) string { return strings.ReplaceAll(local, "_", "-") }

// cliEnvPrefix is the environment-variable prefix for a binary name.
func cliEnvPrefix(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		return "API" + out
	}
	return out
}

// isGoIdent reports whether s may be used as a package clause.
func isGoIdent(s string) bool {
	if s == "" || isGoKeyword(s) {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// goRawString renders s as a Go string literal, using a backquoted one for
// anything multi-line so that a help paragraph reads as a paragraph in the
// generated source.
func goRawString(s string) string {
	if strings.Contains(s, "\n") && !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	return fmt.Sprintf("%q", s)
}

func goSliceLiteral(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = fmt.Sprintf("%q", v)
	}
	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// cliRule is the section divider between resources, so that a file with six
// tables in it can be skimmed.
func cliRule(label string) string {
	const width = 74
	rule := strings.Repeat("-", max(3, width-len(label)-1))
	return rule + " " + label
}
