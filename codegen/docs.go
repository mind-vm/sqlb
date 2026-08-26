// `sqlb docs` is the checklist counterpart of the OpenAPI document: a single
// Markdown file, one section per exposed endpoint, meant to be committed and
// hand-edited rather than regenerated and thrown away.
//
// It works like every other verb — the driver has already linked the schema
// package in, so opts.Registry holds every exposed table, action and query —
// but it does two things none of the other emitters do.
//
// First, a rerun does not overwrite what a human or an agent wrote. Every
// endpoint's notes live in an HTML comment pair keyed by the resource and the
// operation — "posts list", "posts action: publish" — not by method and path,
// so renaming a resource's route carries its notes with it instead of
// archiving them under a new key for no reason a rename should matter to a
// checklist. A rerun reads the old file, keeps whatever it finds under a key
// that still exists, and files anything whose key disappeared under "Archived
// notes" instead of discarding it.
//
// Second, above every notes block sits what the schema already knows and the
// note therefore does not have to restate: which columns are filterable,
// sortable or expandable on a list, which are required or optional on a
// create, a declared verb's body and its write set. An agent sweeping many
// endpoints in one pass spends its writing time on the one thing only it can
// supply — Source, Request, Response, Invariants, Errors, the four the notes
// template asks for — rather than re-deriving mechanical facts from source
// every time — #203's problem, one review level down.
//
// A schema declaration cannot see a hand-written route mounted alongside the
// generated ones with a bare huma.Register call, and that is exactly the
// class of endpoint a checklist is most needed for — no Describe() to fall
// back on. Project.HandwrittenOps is how a project hands those over; see
// #211.

package codegen

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// HandwrittenOp describes one endpoint `sqlb docs` cannot see on its own:
// mounted by hand with huma.Register alongside the resources codegen
// generates, rather than declared in the schema. Filling this in — once, in
// SqlbProject() — is what closes #211: a project with a hand-written
// register/login/invite flow lists it in FEATURES.md next to the generated
// CRUD, instead of the checklist silently covering only the mechanical half
// of its REST surface.
type HandwrittenOp struct {
	Method      string
	Path        string
	Summary     string
	Description string
}

// featuresPath resolves where `sqlb docs` writes, defaulting to a file beside
// the generated code so a project that never sets FeaturesFile still works.
func featuresPath(p Project, opts Options) string {
	if p.FeaturesFile != "" {
		return p.FeaturesFile
	}
	return filepath.Join(opts.Dir, "FEATURES.md")
}

// runDocs writes the feature checklist, merging in whatever notes the file
// already carries.
func runDocs(p Project, opts Options, reg *schema.Registry, stdout, stderr io.Writer) int {
	path := featuresPath(p, opts)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		line(stderr, err)
		return 1
	}

	out, archived := renderFeatures(reg, p.HandwrittenOps, existing)
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			line(stderr, fmt.Errorf("could not create %s: %w", dir, err))
			return 1
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		line(stderr, fmt.Errorf("could not write %s: %w", path, err))
		return 1
	}

	line(stdout, path)
	say(stderr, "sqlb: wrote %s\n", path)
	if archived > 0 {
		say(stderr, "sqlb: %d note(s) archived — their endpoint is no longer in the schema\n", archived)
	}
	return 0
}

// notesPlaceholder is what a freshly rendered endpoint carries until someone
// — a coding agent, a teammate — replaces it. The four labelled lines are
// fixed on purpose: a diff between two versions of an Invariants: line is
// meaningful, a diff between two paragraphs of prose is not. Source exists
// for the same reason a port needs one — a stable pointer back to what this
// endpoint replaced, written once and read by whatever compares feature sets
// later, rather than re-matched by guessing from names.
const notesPlaceholder = `**Source:** _where this endpoint was ported from, if it was — e.g. dcoach/scheduling/views.py::BookingViewSet.create — blank if newly designed_
**Request:** _what the caller sends, beyond the shape above_
**Response:** _what the caller gets back, beyond the shape above_
**Invariants:** _business rules and side effects the schema cannot state_
**Errors:** _domain-specific failure modes beyond the generated 4xx/5xx_`

// endpoint is one row a resource exposes, one declared action or query, or
// one hand-written route — enough to render a heading, the facts the schema
// already knows, and a notes block.
type endpoint struct {
	table       string // "" for a hand-written op, which has no resource
	method      string
	path        string
	kind        string // "list", "create", "action: publish", "hand-written", ...
	description string
	facts       []string
	key         string
}

// renderFeatures projects a registry into the checklist, carrying forward any
// notes the existing file has for a key that still exists and archiving the
// rest. It returns the archived count so the caller can report it.
func renderFeatures(reg *schema.Registry, ops []HandwrittenOp, existing []byte) ([]byte, int) {
	notes, order := parseNotes(existing)
	used := map[string]bool{}

	var b bytes.Buffer
	b.WriteString("# API Features\n\n")
	b.WriteString("Generated by `sqlb docs` from the schema. The endpoint list, the facts above\n")
	b.WriteString("each notes block, and every declared description are derived and rewritten on\n")
	b.WriteString("every run. The notes themselves survive a rerun — keyed by resource and\n")
	b.WriteString("operation rather than by path, so renaming a resource's route does not lose\n")
	b.WriteString("them — and are structured on purpose: Source ties an endpoint back to what it\n")
	b.WriteString("replaced, Request/Response/Invariants/Errors are what only a human or an agent\n")
	b.WriteString("can supply. This is where the file earns its keep as a feature checklist\n")
	b.WriteString("against a PRD.\n")

	if len(ops) > 0 {
		b.WriteString("\n## Hand-written endpoints\n\n")
		b.WriteString("Mounted by hand alongside the resources below. Schema has no declaration to\n")
		b.WriteString("describe these from, which is exactly why they need a note most.\n")
		for _, o := range ops {
			ep := endpoint{method: o.Method, path: o.Path, kind: "hand-written", description: o.Description}
			if o.Summary != "" {
				ep.kind = o.Summary
			}
			ep.key = ep.method + " " + ep.path
			used[ep.key] = true
			writeEndpoint(&b, ep, notes[ep.key])
		}
	}

	for _, t := range reg.Tables() {
		rest := t.Rest()
		if rest == nil {
			continue
		}
		path := rest.Path
		if path == "" {
			path = "/" + t.LocalName()
		}

		fmt.Fprintf(&b, "\n## %s — `%s`\n", t.LocalName(), path)
		if c := t.Comment(); c != "" {
			fmt.Fprintf(&b, "\n%s\n", c)
		}

		for _, ep := range crudEndpoints(t, path) {
			used[ep.key] = true
			writeEndpoint(&b, ep, notes[ep.key])
		}
		for _, a := range t.Actions() {
			ep := endpoint{
				table: t.LocalName(), method: http.MethodPost, path: a.FullPath(path),
				kind: "action: " + a.Name, description: a.Description, facts: actionFacts(a),
			}
			ep.key = ep.table + " " + ep.kind
			used[ep.key] = true
			writeEndpoint(&b, ep, notes[ep.key])
		}
		for _, q := range t.Queries() {
			ep := endpoint{
				table: t.LocalName(), method: http.MethodGet, path: q.FullPath(path),
				kind: "query: " + q.Name, description: q.Description, facts: queryFacts(q),
			}
			ep.key = ep.table + " " + ep.kind
			used[ep.key] = true
			writeEndpoint(&b, ep, notes[ep.key])
		}
	}

	archived := 0
	var stale []string
	for _, k := range order {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		b.WriteString("\n## Archived notes\n\n")
		b.WriteString("These keys no longer exist in the schema — the resource, verb or hand-written\n")
		b.WriteString("route they named is gone, not merely moved (a rename keeps its key; see the\n")
		b.WriteString("file's own header). Move anything still useful above, then delete this section.\n")
		for _, k := range stale {
			archived++
			fmt.Fprintf(&b, "\n### `%s`\n\n%s\n", k, notesBlock(k, notes[k]))
		}
	}

	return b.Bytes(), archived
}

// crudEndpoints lists the routes one resource's Ops mask actually mounts,
// mirroring rest.Resource's own dispatch (rest/rest.go): OpSingleton replaces
// the item path with the bare collection path for OpUpdate and OpDelete, and
// is mutually exclusive with OpList/OpRead.
func crudEndpoints(t *schema.TableDef, path string) []endpoint {
	rest := t.Rest()
	table := t.LocalName()
	var out []endpoint
	add := func(method, p, kind string) {
		ep := endpoint{table: table, method: method, path: p, kind: kind, facts: facts(t, rest, kind)}
		ep.key = table + " " + kind
		out = append(out, ep)
	}
	item := path + "/{id}"

	if rest.Ops.Has(schema.OpSingleton) {
		add(http.MethodGet, path, "singleton")
		if rest.Ops.Has(schema.OpUpdate) {
			add(http.MethodPatch, path, "update")
		}
		if rest.Ops.Has(schema.OpDelete) {
			add(http.MethodDelete, path, "delete")
		}
		return out
	}

	if rest.Ops.Has(schema.OpList) {
		add(http.MethodGet, path, "list")
	}
	if rest.Ops.Has(schema.OpCreate) {
		add(http.MethodPost, path, "create")
	}
	if rest.Ops.Has(schema.OpRead) {
		add(http.MethodGet, item, "read")
	}
	if rest.Ops.Has(schema.OpUpdate) {
		add(http.MethodPatch, item, "update")
	}
	if rest.Ops.Has(schema.OpDelete) {
		add(http.MethodDelete, item, "delete")
	}
	return out
}

// facts renders the mechanical properties one CRUD kind actually has. A
// coding agent sweeping many endpoints already gets this for free, so its
// notes are left to say what the schema cannot: business rules and "why".
func facts(t *schema.TableDef, rest *schema.REST, kind string) []string {
	switch kind {
	case "list":
		return listFacts(t, rest)
	case "read", "singleton":
		return expandFacts(t)
	case "create":
		return createFacts(t)
	case "update":
		return updateFacts(t)
	default:
		return nil
	}
}

func listFacts(t *schema.TableDef, rest *schema.REST) []string {
	var out []string
	if names := columnsWhere(t, func(d *schema.FieldDesc) bool { return d.Filterable }); len(names) > 0 {
		out = append(out, "Filterable: "+strings.Join(names, ", "))
	}
	if names := columnsWhere(t, func(d *schema.FieldDesc) bool { return d.Sortable }); len(names) > 0 {
		out = append(out, "Sortable: "+strings.Join(names, ", "))
	}
	if names := columnsWhere(t, func(d *schema.FieldDesc) bool { return d.Searchable }); len(names) > 0 {
		out = append(out, "Searchable: "+strings.Join(names, ", "))
	}
	if names := expandNames(t); len(names) > 0 {
		out = append(out, "Expandable: "+strings.Join(names, ", "))
	}
	if p := pageFact(rest); p != "" {
		out = append(out, p)
	}
	return out
}

func expandFacts(t *schema.TableDef) []string {
	if names := expandNames(t); len(names) > 0 {
		return []string{"Expandable: " + strings.Join(names, ", ")}
	}
	return nil
}

// createFacts splits the create body the way it is actually enforced: a
// column with no default and no NULL to fall back on is required, everything
// else settable is optional. Read-only, hidden and the primary key never
// reach the body at all (see codegen's own bodyFields).
//
// The declared inputs that are not columns are split the same way and listed
// beside them, because the body is what this row of the document is about and a
// property missing from it is the one a caller will leave out (#309). They are
// marked, since "not a column" is the fact about them a reader needs.
func createFacts(t *schema.TableDef) []string {
	var required, optional []string
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.ReadOnly || d.Hidden || d.PrimaryKey {
			continue
		}
		if d.Nullable || d.DatabaseSupplied() {
			optional = append(optional, d.Name)
		} else {
			required = append(required, d.Name)
		}
	}
	for _, f := range createInput(t) {
		d := f.Desc()
		name := d.Name + " (input, not a column)"
		if optionalOnCreate(d) {
			optional = append(optional, name)
		} else {
			required = append(required, name)
		}
	}
	var out []string
	if len(required) > 0 {
		out = append(out, "Required: "+strings.Join(required, ", "))
	}
	if len(optional) > 0 {
		out = append(out, "Optional: "+strings.Join(optional, ", "))
	}
	return out
}

// updateFacts lists what a PATCH may actually name: settable at create minus
// Immutable, which leaves the body after create the way it left it in.
func updateFacts(t *schema.TableDef) []string {
	var names []string
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.ReadOnly || d.Hidden || d.PrimaryKey || d.Immutable {
			continue
		}
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		return nil
	}
	return []string{"Writable: " + strings.Join(names, ", ")}
}

func columnsWhere(t *schema.TableDef, pred func(*schema.FieldDesc) bool) []string {
	var out []string
	for _, f := range t.Fields() {
		if d := f.Desc(); pred(d) {
			out = append(out, d.Name)
		}
	}
	return out
}

func expandNames(t *schema.TableDef) []string {
	var out []string
	for _, f := range t.Fields() {
		d := f.Desc()
		if d.Expandable && d.Ref != nil {
			out = append(out, d.Ref.Name)
		}
	}
	return out
}

func pageFact(rest *schema.REST) string {
	switch {
	case rest.DefaultPageSize != 0 && rest.MaxPageSize != 0:
		return fmt.Sprintf("Page size: default %d, max %d", rest.DefaultPageSize, rest.MaxPageSize)
	case rest.MaxPageSize != 0:
		return fmt.Sprintf("Page size: max %d", rest.MaxPageSize)
	case rest.DefaultPageSize != 0:
		return fmt.Sprintf("Page size: default %d", rest.DefaultPageSize)
	default:
		return ""
	}
}

// actionFacts and queryFacts mirror what restcompat.captureActions already
// captures for the compatibility contract — Writes and Touches are exactly
// the blast-radius facts a reviewer wants next to a wide route — rendered as
// prose instead of JSON since this file is for reading, not diffing by tool.
func actionFacts(a schema.Action) []string {
	var out []string
	if len(a.Body) > 0 {
		out = append(out, "Body: "+fieldSummary(a.Body))
	}
	if len(a.Returns) > 0 {
		out = append(out, "Returns: "+fieldSummary(a.Returns))
	}
	if len(a.Writes) > 0 {
		out = append(out, "Writes: "+strings.Join(a.Writes, ", "))
	}
	if len(a.Touches) > 0 {
		out = append(out, "Touches: "+strings.Join(a.Touches, ", "))
	}
	return out
}

func queryFacts(q schema.Query) []string {
	var out []string
	if len(q.Params) > 0 {
		out = append(out, "Params: "+fieldSummary(q.Params))
	}
	if len(q.Reads) > 0 {
		names := make([]string, len(q.Reads))
		for i, t := range q.Reads {
			names[i] = t.LocalName()
		}
		out = append(out, "Reads: "+strings.Join(names, ", "))
	}
	return out
}

func fieldSummary(fs []*schema.Field) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		d := f.Desc()
		parts[i] = fmt.Sprintf("%s (%s)", d.Name, d.Type)
	}
	return strings.Join(parts, ", ")
}

func writeEndpoint(b *bytes.Buffer, ep endpoint, existingNote string) {
	fmt.Fprintf(b, "\n### `%s %s` — %s\n", ep.method, ep.path, ep.kind)
	if ep.description != "" {
		fmt.Fprintf(b, "\n%s\n", ep.description)
	}
	for _, f := range ep.facts {
		fmt.Fprintf(b, "\n- %s", f)
	}
	if len(ep.facts) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("\n" + notesBlock(ep.key, existingNote))
}

func notesBlock(key, note string) string {
	if note == "" {
		note = notesPlaceholder
	}
	return fmt.Sprintf("<!-- sqlb:notes %s -->\n%s\n<!-- /sqlb:notes -->\n", key, note)
}

// notesRE matches one notes block, capturing the key and the body between the
// markers. (?s) makes "." match a newline, so a multi-paragraph note round-trips.
var notesRE = regexp.MustCompile(`(?s)<!-- sqlb:notes (.+?) -->\n(.*?)\n<!-- /sqlb:notes -->`)

// parseNotes reads an existing FEATURES.md and returns its notes by key, plus
// the keys in the order they appeared — which is what lets a rerun's archive
// section list stale entries in a stable order instead of reshuffling them on
// every regeneration.
func parseNotes(existing []byte) (map[string]string, []string) {
	notes := map[string]string{}
	var order []string
	for _, m := range notesRE.FindAllSubmatch(existing, -1) {
		key := string(m[1])
		if _, ok := notes[key]; !ok {
			order = append(order, key)
		}
		notes[key] = string(m[2])
	}
	return notes, order
}
