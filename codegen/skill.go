package codegen

// The agent skill: what this project's schema declares, written where an agent
// working in the repository will find it.
//
// Every other emitter here produces something a program consumes — Go, Dart,
// TypeScript, JSON. This one produces instructions, and that difference is the
// whole of why it is written the way it is (ADR-0049).
//
// # Why this is generated rather than written
//
// The thing an agent gets wrong about sqlb is not the API, which it can read.
// It is which capabilities *this project* declared: capabilities are opt-in
// (ADR-0006), so a filter on a column that never said Filterable is a 400 at
// runtime and a wasted round trip. That answer is per-project by construction,
// so a static document cannot carry it and a generated one cannot be wrong for
// long — `sqlb check` fails when this file has drifted, which is the only reason
// it is safe to write instructions into a repository at all. A skill that has
// drifted from the schema is worse than no skill, because it is confidently
// wrong about the one thing it exists to know.
//
// # Structure, not prose
//
// This emitter carries names, types, capability flags and paths. It deliberately
// does **not** carry Comment strings, and that is a trust boundary rather than a
// style choice.
//
// The schema is first-party Go, so most of what is in a registry is text the
// project's own authors wrote. `sqlb introspect` breaks that: it reads a
// column's comment off a live database and calls Field.Comment, so an adopted
// database's comments become schema comments — and an adopted database is not
// first-party source. Every other emitter passes those through to DDL and to the
// OpenAPI document safely, because DDL and an OpenAPI document are read as
// *data*. A skill is read as *instructions*, so the same string arriving here
// would be an instruction written by whoever last commented that column.
//
// Hidden columns are absent for the reason the manifest gives for omitting them:
// a name is itself information. Reading the declaration is how someone learns
// what a table holds; this file says what the wire accepts.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// The directory the skill lands in is also the name in its frontmatter, and it
// comes from Options.skillName — see [Options.SkillName] for the default and for
// why a repository with more than one registry may need to set it.

// renderSkill writes the project-specific agent skill.
func renderSkill(opts Options) ([]byte, error) {
	m := opts.Registry.BuildManifest()
	dropForeignFromManifest(m, opts)
	if err := applyOverridesToManifest(m, opts); err != nil {
		return nil, err
	}

	var exposed, internal []schema.TableManifest
	for _, t := range m.Tables {
		if t.REST != nil {
			exposed = append(exposed, t)
			continue
		}
		internal = append(internal, t)
	}

	var b strings.Builder
	skillFrontmatter(&b, opts.skillName(), m)
	skillHeading(&b, m, exposed, internal)
	skillCommands(&b, opts)
	skillWireSurface(&b, exposed)
	skillInternalTables(&b, internal)
	skillObligations(&b, m)
	skillLimits(&b, m)
	skillSiblings(&b, opts)
	return []byte(b.String()), nil
}

// staticSkills are the skills this repository ships that are true in every
// project, named here so the generated one can point at them.
//
// The list is checked against skills/ by TestTheGeneratedSkillNamesEverySibling
// rather than trusted, which is the same bargain the rest of this file makes:
// a generated file that is confidently wrong about something is worse than one
// that says nothing.
var staticSkills = []struct{ name, answers string }{
	{
		"sqlb-authoring",
		"the DSL vocabulary this file's lists are written in — which column constructors and " +
			"capability flags exist, what each enforces, and which combinations are refused",
	},
	{
		"sqlb-queries",
		"where the builder ends and `Raw` or hand-written SQL begins, plus the failure modes " +
			"that compile, pass their tests and are wrong at runtime",
	},
	{
		"sqlb-adoption",
		"whether an existing codebase should adopt sqlb at all — a census, a ratio and a pilot",
	},
}

// skillSiblings points at the static skills.
//
// This is the whole of #291, and the argument is placement rather than content.
// The documents existed, were good, and did not reach the agent doing the work:
// a consumer finished an entire port — making mistakes `sqlb-authoring` covers
// by name — without learning it existed. This file is the only sqlb artefact
// guaranteed to be in a consumer's repository and in front of an agent from the
// first turn, and it named none of them.
//
// It belongs directly after "What this file does not say", because that section
// is where a reader has just been told the four things this file will not
// answer — and three of them are what the siblings are for.
func skillSiblings(b *strings.Builder, opts Options) {
	b.WriteString("\n## Where the rest of it is\n\n")
	b.WriteString("This file is generated from one project's schema. The vocabulary it is " +
		"written in is not per-project, and sqlb ships it as skills of its own:\n\n")
	for _, s := range staticSkills {
		fmt.Fprintf(b, "- **`%s`** — %s\n", s.name, s.answers)
	}
	b.WriteString("\nLoad `sqlb-authoring` for \"does `Filterable` exist, and what does `Scoped` " +
		"enforce\"; this file for \"can I filter *this* column\". They do not overlap: " +
		"capabilities are opt-in, so what the vocabulary offers and what this schema turned on " +
		"are different questions.\n\n")
	b.WriteString("Generating this file does not install them: this is the one emitter that " +
		"writes into a directory sqlb does not own, so it writes exactly one file and nothing " +
		"beside it. ")
	fmt.Fprintf(b, "They go in `%s/`, next to this one:\n\n", opts.SkillDir)
	b.WriteString("```bash\n" +
		"npx skills add mind-vm/sqlb\n" +
		"```\n\n")
	fmt.Fprintf(b, "That is your invocation and not part of sqlb's build — nothing in the library "+
		"depends on Node. A skill is a directory with a `SKILL.md` in it, so if you would rather "+
		"not, a checkout and `cp -r skills/sqlb-* %s/` is the same thing.\n", opts.SkillDir)
}

// skillFrontmatter writes the YAML header. The description is the trigger rather
// than documentation: an agent decides whether to load this file from that one
// sentence, and "use this when working with sqlb" does not fire on "add a due
// date to invoices", which is the sentence that actually arrives. So it names
// the project's real tables.
func skillFrontmatter(b *strings.Builder, name string, m *schema.Manifest) {
	b.WriteString("---\n")
	fmt.Fprintf(b, "name: %s\n", name)

	subjects := make([]string, 0, len(m.Tables))
	for _, t := range m.Tables {
		subjects = append(subjects, t.Name)
	}

	fmt.Fprintf(b, "description: %s\n", oneLine(fmt.Sprintf(
		"Use when editing this project's sqlb schema or writing a query, filter, sort or REST "+
			"request against its tables: %s. Says which columns each resource actually accepts — "+
			"capabilities are opt-in, so anything not listed here is rejected at runtime — "+
			"and the commands to run after a schema edit. Generated from the schema declaration.",
		skillSubjects(subjects))))
	b.WriteString("---\n\n")
}

func skillHeading(b *strings.Builder, m *schema.Manifest, exposed, internal []schema.TableManifest) {
	b.WriteString("# This project's sqlb schema\n\n")

	if m.Module != "" {
		fmt.Fprintf(b, "Module `%s`. ", m.Module)
	}
	fmt.Fprintf(b, "%s declared, %s on the wire",
		count(len(m.Tables), "table"), plainCount(len(exposed)))
	if len(internal) > 0 {
		fmt.Fprintf(b, ", %s not exposed", plainCount(len(internal)))
	}
	b.WriteString(".\n\n")

	b.WriteString("**Generated by `sqlb generate` — do not edit.** `sqlb check` fails when this " +
		"file has drifted from the schema, so an edit here is reverted by the next generate " +
		"rather than kept. Change the schema declaration instead.\n\n")
}

// skillCommands writes what to run after a schema edit — the step most often
// missed, because nothing about editing a Go file suggests that four other
// artefacts now disagree with it.
func skillCommands(b *strings.Builder, opts Options) {
	b.WriteString("## After editing the schema\n\n")

	pkg := opts.SkillSchemaPackage
	if pkg == "" {
		b.WriteString("Regenerate every artefact, then write the migration that closes the gap:\n\n")
		b.WriteString("```bash\ngo generate ./...\n```\n\n")
		b.WriteString("If the schema package carries no `//go:generate` directive, name it " +
			"explicitly — `sqlb generate ./yourschema`.\n\n")
	} else {
		b.WriteString("Regenerate every artefact, then write the migration that closes the gap:\n\n")
		fmt.Fprintf(b, "```bash\nsqlb generate %s\n```\n\n", pkg)
		fmt.Fprintf(b, "```bash\nsqlb migrate -name describes_what_changed %s\n```\n\n", pkg)
	}

	b.WriteString("Generating needs no database. Writing a migration replays the committed " +
		"history into a scratch Postgres, because the trustworthy answer to \"what does the " +
		"schema look like now\" is what the migrations build rather than what a live database " +
		"drifted into.\n\n")
	b.WriteString("A schema edit is also an API edit. `sqlb impact` reports what it did to the " +
		"REST contract against the checked-in baseline, and the sharpest breaks — un-exposing a " +
		"column, dropping an operation, a rename — produce no DDL at all, so the migration diff " +
		"will not mention them.\n\n")
}

// skillWireSurface is the payload: per resource, exactly what a request may ask
// for. Anything absent is a rejection rather than an oversight, which is the
// sentence this whole file exists to make available before the 400 rather than
// after it.
func skillWireSurface(b *strings.Builder, exposed []schema.TableManifest) {
	b.WriteString("## The wire surface\n\n")

	if len(exposed) == 0 {
		b.WriteString("No table declares a REST surface, so this schema generates models and a " +
			"typed column facade and no endpoints. Queries go through the builder; there is " +
			"nothing here for a request to reach.\n\n")
		return
	}

	b.WriteString("| Resource | Path | Operations | Page size |\n|---|---|---|---|\n")
	for _, t := range exposed {
		r := t.REST
		fmt.Fprintf(b, "| `%s` | `%s` | %s | %s |\n",
			t.Name, r.Path, joinCode(r.Operations, " "), pageSize(r))
	}
	b.WriteString("\n")

	// Said once, for every resource below that does not say otherwise. Primary
	// key order is not an ordering anybody chose, and a list is well-formed in
	// any order — so without this a reader has no way to tell "this collection
	// has a meaning for silence" from "this collection has none" (#165).
	b.WriteString("A list request that names no `?sort=` gets rows in primary-key order unless the " +
		"resource below states its own ordering. Name an ordering whenever one matters.\n\n")

	for _, t := range exposed {
		skillResource(b, t)
	}
}

func skillResource(b *strings.Builder, t schema.TableManifest) {
	r := t.REST
	fmt.Fprintf(b, "### `%s`\n\n", t.Name)

	// A singleton is the one resource whose shape an agent cannot infer from the
	// table above, and guessing it wrong costs a round trip in both directions:
	// asking for `/x/{id}` gets a 404 and asking for `?filter` gets a 400. Said
	// here rather than only in the operations column, because this is the
	// section a reader is in when composing the request (#166).
	if isSingleton(r) {
		fmt.Fprintf(b, "`GET %s` — the caller's own row, as a bare object rather than a page. "+
			"There is no `{id}`: the resource holds one row per caller and the server settles "+
			"which, so an authenticated request has already said everything the route needs. "+
			"404 means the caller has no row yet. It takes no filter, sort, page or `?select`.\n\n",
			r.Path)
		skillSingletonWrites(b, r)
		skillEnums(b, t)
		return
	}

	if t.PrimaryKey != "" {
		// Wire, like everything else under an exposed resource: the item path is
		// `{id}` whatever the key is called, so the only place this name is
		// actually written is a filter or a response field.
		fmt.Fprintf(b, "Addressed by `%s`. ", wireOfColumn(t, t.PrimaryKey))
	}
	fmt.Fprintf(b, "`%s`\n\n", r.Path)

	rows := [][2]string{
		{"Filterable", joinCode(r.Filterable, ", ")},
		{"Sortable", joinCode(r.Sortable, ", ")},
		{"Searchable", joinCode(r.Searchable, ", ")},
		{"Expandable", joinCode(r.Expandable, ", ")},
	}
	b.WriteString("| Capability | Columns |\n|---|---|\n")
	for _, row := range rows {
		value := row[1]
		if value == "" {
			// Named rather than dropped: "nothing is sortable here" is a fact a
			// caller needs, and an absent row reads as an incomplete document.
			value = "*none*"
		}
		fmt.Fprintf(b, "| %s | %s |\n", row[0], value)
	}
	b.WriteString("\n")

	// What an unsorted list returns, when the resource decided. The fallback is
	// stated once for the document rather than once per resource — see
	// skillWireSurface — because it is the same sentence every time and this
	// section is read per resource.
	//
	// It is worth stating at all because it is invisible from a response: rows
	// in some order look well-formed in any order, so a caller that does not
	// know the resource has a meaning for "no ?sort" cannot tell it got the
	// wrong product (#165).
	if len(r.DefaultSort) > 0 {
		fmt.Fprintf(b, "Ordered by %s unless `?sort=` says otherwise — this is what the collection *is*, "+
			"so prefer it to restating an ordering.\n\n", joinCode(r.DefaultSort, ", "))
	}

	// The declared ceilings, and only those. A budget the schema left to the
	// runtime is one this document has no number for, and inventing the
	// package default here would state as a contract what the next release is
	// free to move.
	if r.MaxFilters > 0 {
		fmt.Fprintf(b, "At most %d filters per request.\n\n", r.MaxFilters)
	}
	if r.MaxSortTerms > 0 {
		fmt.Fprintf(b, "At most %d sort terms per request.\n\n", r.MaxSortTerms)
	}
	if r.MaxOffset > 0 {
		// The one budget a caller meets mid-task rather than on a malformed
		// request: paging until the rows run out walks into it, and the answer
		// is a different parameter rather than a smaller number.
		fmt.Fprintf(b, "Offset paging stops at %d rows. Use `?cursor=` to read deeper — "+
			"it costs the same at any depth.\n\n", r.MaxOffset)
	}

	// A property that is not a column is invisible from everywhere else in this
	// document: it is absent from the column table, from the filter vocabulary
	// and from every response. So a caller assembling a POST out of the columns
	// assembles a body the server refuses, and this is the only line that says
	// otherwise (#309).
	if len(r.CreateInput) > 0 {
		fmt.Fprintf(b, "**`POST %s` carries more than the columns.** %s "+
			"Not a column, not stored as sent, and absent from every response — the server "+
			"derives what it stores from it. A create body assembled from the column table "+
			"alone is refused.\n\n", r.Path, skillCreateInput(r.CreateInput))
	}

	skillEnums(b, t)

	if len(r.Actions) > 0 {
		b.WriteString("**Declared actions.** Domain verbs this resource owns. " +
			"Reaching the same outcome by PATCHing a column is the mistake these exist to " +
			"prevent — the verb owns the transition.\n\n")
		b.WriteString("| Verb | Route | Answers | Writes | Also writes |\n|---|---|---|---|---|\n")
		for _, a := range r.Actions {
			fmt.Fprintf(b, "| `%s` | `%s %s` | %s | %s | %s |\n",
				a.Name, a.Method, a.Path, skillActionAnswer(a),
				orNone(joinCode(a.Writes, ", ")), orNone(joinCode(a.Touches, ", ")))
		}
		b.WriteString("\n*Answers* is what comes back. A verb that declares a result answers with " +
			"exactly those properties and not with the row — reading a column off that " +
			"response is the mistake the column is missing from. *Writes* is the columns " +
			"the envelope persists on the addressed row. *Also writes* is the tables the " +
			"verb declares it reaches through its transaction — declared, not enforced, " +
			"and *none* there means no claim was made rather than that none are " +
			"written.\n\n")
	}

	if len(t.CollectedBy) > 0 {
		b.WriteString("**Collects.** Rows of another table this one gathers, and the name " +
			"`?expand` knows them by. Most relations are collections, capped and paged with " +
			"`has_more`; a relation backed by a unique foreign key is one-to-one instead and " +
			"an expansion returns the single row or `null`, never the capped envelope.\n\n")
		b.WriteString("| Relation | Table | Via | Expandable | Returns |\n|---|---|---|---|---|\n")
		for _, inv := range t.CollectedBy {
			returns := fmt.Sprintf("capped collection, %d max", inv.Limit)
			if inv.OneToOne {
				returns = "one row, or absent"
			}
			fmt.Fprintf(b, "| `%s` | `%s` | `%s` | %s | %s |\n",
				inv.Name, inv.Table, inv.Column, yesNo(inv.Expandable), returns)
		}
		b.WriteString("\n")
	}
}

// skillActionAnswer is what a verb's response carries: a declared result, the
// row, or nothing at all.
//
// It is in the table rather than in prose because it is the one fact about a
// verb that changes what a caller does *next* — an agent that expects the row
// back and gets a score will go looking for the columns in it.
func skillActionAnswer(a schema.ActionManifest) string {
	if len(a.Returns) > 0 {
		names := make([]string, 0, len(a.Returns))
		for _, p := range a.Returns {
			names = append(names, p.Name)
		}
		return joinCode(names, ", ")
	}
	if strings.Contains(a.Path, "{id}") {
		return "the row"
	}
	return "204, no body"
}

// skillCreateInput names the declared non-column properties of a create body,
// with the fact a caller needs about each: its type, and whether the request
// may leave it out.
func skillCreateInput(props []schema.BodyProperty) string {
	parts := make([]string, 0, len(props))
	for _, p := range props {
		required := ", required"
		if p.Nullable {
			required = ", optional"
		}
		values := ""
		if len(p.Enum) > 0 {
			values = fmt.Sprintf(", one of %s", joinCode(p.Enum, " "))
		}
		parts = append(parts, fmt.Sprintf("`%s` (%s%s%s)", p.Name, p.Type, values, required))
	}
	return "It also takes " + joinWords(parts) + "."
}

// joinWords renders a list the way a sentence does: commas, and an "and"
// before the last.
func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// skillEnums names the values a constrained column accepts.
//
// This is the one thing the per-column table carried that belongs here rather
// than with it. Measured against twelve real application schemas, that table was
// 44–49% of the document and mostly described what a *response* holds — types,
// nullability, which columns are read-only — none of which a request has to get
// right. An enum is the exception: `?status=eq.active` against a column whose
// values are todo/in_progress/blocked/done is a rejection, and the valid list is
// not guessable from the column's name. So the values stay and the table goes.
//
// One line per constrained column, and nothing when a resource has none.
func skillEnums(b *strings.Builder, t schema.TableManifest) {
	var lines []string
	for _, c := range t.Columns {
		if len(c.Enum) == 0 {
			continue
		}
		// The wire spelling, because the sentence this line exists to prevent is
		// `?status=eq.active` against a column whose values are something else —
		// and naming the column its database name there would be the same bug
		// the capability tables had (#143).
		lines = append(lines, fmt.Sprintf("`%s` is one of %s", wireOf(c), joinCode(c.Enum, " ")))
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "Values: %s.\n\n", strings.Join(lines, "; "))
}

func skillInternalTables(b *strings.Builder, internal []schema.TableManifest) {
	if len(internal) == 0 {
		return
	}
	b.WriteString("## Declared, not exposed\n\n")
	b.WriteString("These have models and typed columns but no endpoint. Reaching one means " +
		"writing a query, not calling an API — and exposing it is a schema edit with a REST " +
		"contract change attached.\n\n")
	for _, t := range internal {
		fmt.Fprintf(b, "- `%s`", t.Name)
		if t.PrimaryKey != "" {
			fmt.Fprintf(b, " — addressed by `%s`", t.PrimaryKey)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

// skillObligations names the tables whose declaration is a promise the
// application has to keep. A resource that declares its rows confined does not
// mount without the hook that confines them, so this is the list of places where
// a missing registration is a refusal to start rather than a silent leak.
func skillObligations(b *strings.Builder, m *schema.Manifest) {
	type obligation struct {
		table  string
		column string
		kind   string
	}
	var obs []obligation
	for _, t := range m.Tables {
		for _, c := range t.Columns {
			if c.Scoped {
				obs = append(obs, obligation{t.Name, c.Name, "scoped"})
			}
			if c.SoftDelete {
				obs = append(obs, obligation{t.Name, c.Name, "soft-delete"})
			}
		}
	}
	if len(obs) == 0 {
		return
	}

	b.WriteString("## Obligations this schema carries\n\n")
	b.WriteString("A declaration that rows are confined is an obligation, not a comment. " +
		"These tables refuse to register without the hook that enforces them, so a missing " +
		"registration fails at mount rather than leaking rows at runtime.\n\n")
	b.WriteString("| Table | Column | Declares |\n|---|---|---|\n")
	for _, o := range obs {
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", o.table, o.column, o.kind)
	}
	b.WriteString("\n")
}

// skillLimits is the honesty section. A generated document that reads as
// complete is worse than one that reads as partial, because the reader stops
// looking at exactly the wrong moment.
func skillLimits(b *strings.Builder, m *schema.Manifest) {
	b.WriteString("## What this file does not say\n\n")
	b.WriteString("- **Anything absent is a rejection, not an oversight.** Capabilities are " +
		"opt-in. A column missing from `Filterable` above cannot be filtered on, and the error " +
		"names what would have been accepted — so read the list rather than guessing and " +
		"retrying.\n")
	b.WriteString("- **No descriptions or comments are carried here,** deliberately. " +
		"A column comment can arrive from an introspected database rather than from this " +
		"project's authors, and this file is read as instructions. Read the schema declaration " +
		"for what a column means.\n")
	b.WriteString("- **This is what a request may ask for, not what a table holds.** There is no " +
		"column listing here on purpose — the types, the nullability and which columns are " +
		"read-only are all in the generated models, and repeating them made this file twice as " +
		"long without making a request more likely to be accepted. The models and the " +
		"declaration are where a column's shape lives.\n")
	b.WriteString("- **Nothing here says how to write a query.** Where the builder ends and " +
		"`Raw` or hand-written SQL begins is a separate question, and the failure modes that " +
		"matter there compile and pass their tests.\n")
	skillWireSpelling(b, m)
}

// skillWireSpelling is the sentence that tells an agent not to go looking for a
// mapping layer, and it has to be two sentences because the schema decides which
// one is true.
//
// Under the default the names above are the column names and the JSON field
// names at once, which is what makes "there is no mapping" the whole story.
// Under a declared WireCase they are still one spelling — ADR-0036 is unchanged —
// but that spelling is a *function* of the column name rather than the column
// name, and a file that says otherwise sends an agent to write `?org_id=eq.…`
// against an endpoint that accepts `orgId` (#143). The lists above are already
// the wire's, so what is left to say is what the column behind one is called.
func skillWireSpelling(b *strings.Builder, m *schema.Manifest) {
	if m.WireCase == "" {
		b.WriteString("- **One column has one wire spelling,** derived from its name. There is no " +
			"mapping layer and no per-field override in either direction, so the column names " +
			"above are also the JSON field names.\n")
		return
	}
	fmt.Fprintf(b, "- **One column has one wire spelling,** and this schema declares "+
		"`WireCase(%s)`: %s. Every name above is already spelled that way, and so are the "+
		"JSON field names and the filter parameters — there is no mapping layer and no "+
		"per-field override, so a request never sends a column's database spelling. The "+
		"declaration and the migrations use the database spelling; `sqlb.json` carries both "+
		"per column.\n", wireCaseName(m.WireCase), wireCaseExample(m.WireCase))
}

// wireCaseName spells a WireCase the way the declaration does, so the sentence
// above names something a reader can grep for.
func wireCaseName(c string) string {
	if c == string(schema.Camel) {
		return "schema.Camel"
	}
	// Unreachable for the cases that exist today, and a literal rather than a
	// panic: a WireCase added later should degrade to naming itself rather than
	// break every project's generate on the day it lands.
	return fmt.Sprintf("%q", c)
}

// wireCaseExample shows the transformation on a column every schema has.
func wireCaseExample(c string) string {
	if c == string(schema.Camel) {
		return "`created_at` is `createdAt` on the wire"
	}
	return fmt.Sprintf("column names are spelled %q on the wire", c)
}

// Helpers. Each keeps a formatting decision in one place so that two sections
// cannot spell the same thing two ways — the generated file is compared byte for
// byte by `sqlb check`, so consistency here is correctness rather than taste.

// wireOf is a column's spelling on the wire. The manifest carries Wire only
// where it differs, so an absent one means the two names are the same.
func wireOf(c schema.ColumnManifest) string {
	if c.Wire != "" {
		return c.Wire
	}
	return c.Name
}

// wireOfColumn is wireOf reached by name, for the one place that holds a column
// name rather than the column: TableManifest.PrimaryKey.
//
// A name with no matching column falls back to itself rather than to empty. The
// manifest omits hidden columns, and a hidden primary key is a schema this
// emitter should still describe as best it can rather than one it renders a
// blank for.
// isSingleton reports whether a resource is the caller's one row, which is
// carried in the manifest as an operation rather than as a flag: it is one, and
// the operation list is what a reader of the document already consults.
func isSingleton(r *schema.RESTManifest) bool {
	return slices.Contains(r.Operations, "singleton")
}

// skillSingletonWrites states the write routes a singleton exposes, which are
// the ones an agent would otherwise address by id and get a 404 from.
func skillSingletonWrites(b *strings.Builder, r *schema.RESTManifest) {
	var lines []string
	for _, op := range []struct{ name, line string }{
		{"create", fmt.Sprintf("`POST %s` creates it.", r.Path)},
		{"update", fmt.Sprintf("`PATCH %s` writes the columns the body names.", r.Path)},
		{"delete", fmt.Sprintf("`DELETE %s` removes it.", r.Path)},
	} {
		if slices.Contains(r.Operations, op.name) {
			lines = append(lines, op.line)
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "%s No id on any of them, for the same reason.\n\n", strings.Join(lines, " "))
}

func wireOfColumn(t schema.TableManifest, column string) string {
	for _, c := range t.Columns {
		if c.Name == column {
			return wireOf(c)
		}
	}
	return column
}

// pageSize renders the declared ceilings, and says nothing about the ones a
// schema left to the runtime. A zero in the manifest means undeclared, and
// printing it as "0" would read as a resource that returns no rows.
func pageSize(r *schema.RESTManifest) string {
	switch {
	case r.DefaultPageSize > 0 && r.MaxPageSize > 0:
		return fmt.Sprintf("%d, max %d", r.DefaultPageSize, r.MaxPageSize)
	case r.MaxPageSize > 0:
		return fmt.Sprintf("max %d", r.MaxPageSize)
	case r.DefaultPageSize > 0:
		return fmt.Sprintf("%d", r.DefaultPageSize)
	}
	return "*default*"
}

func joinCode(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, "`"+s+"`")
	}
	return strings.Join(quoted, sep)
}

func orNone(s string) string {
	if s == "" {
		return "*none*"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func count(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func plainCount(n int) string { return fmt.Sprintf("%d", n) }

// skillSubjects renders the table-name list the description triggers on, bounded
// by characters as well as by count.
//
// Both bounds are load-bearing and the second was missing until a guard found it.
// The agent tooling truncates a description past a documented ceiling and simply
// does not see the tail, so a budget overrun is silent — and counting names is not
// enough, because twelve names of a module-qualified length overrun it on their
// own. maxChars leaves the surrounding sentence room inside the ceiling.
func skillSubjects(names []string) string {
	const maxNames = 12
	const maxChars = 480

	var kept []string
	used := 0
	for _, n := range names {
		if len(kept) >= maxNames || used+len(n)+2 > maxChars {
			break
		}
		kept = append(kept, n)
		used += len(n) + 2
	}
	switch {
	case len(kept) == 0:
		return "none"
	case len(kept) == len(names):
		return andList(kept)
	}
	// "a, b and 28 more" would read as though 28 more were a table. The comma
	// form is the one that does not — and it is a separate branch because
	// andList's final "and" would otherwise double up with this one.
	return fmt.Sprintf("%s, and %d more", strings.Join(kept, ", "), len(names)-len(kept))
}

// andList renders a human list: "a, b and c".
func andList(items []string) string {
	switch len(items) {
	case 0:
		return "none"
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
