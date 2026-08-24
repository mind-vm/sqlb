package codegen

// The README the exit ships with.
//
// It exists because an eject that quietly served fewer requests than the
// resource it replaced would be worse than no eject at all. Everything sqlb did
// that this code does not is listed here by name, in the same file as the code
// that does not do it, so nobody has to discover the gap from a client bug
// report.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

func ejectReadme(opts EjectOptions, tables []*schema.TableDef) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# `%s` — ejected from a sqlb schema\n\n", opts.pkg())
	b.WriteString(`This package was written by ` + "`sqlb eject`" + `. It imports
[pgx](https://github.com/jackc/pgx) and the standard library, and nothing else —
no sqlb, no huma, no router. Deleting sqlb from ` + "`go.mod`" + ` after taking
this is a supported end state, which is the entire point of the command.

Nothing here is precious. Edit it, delete the parts you do not serve, or keep
running ` + "`sqlb eject -check`" + ` in CI for as long as you want the exit kept
current — and drop that gate on the day you stop.

## What is here

| File | What it is |
|---|---|
| ` + "`schema.sql`" + ` | The whole schema as DDL. The same statements ` + "`sqlb migrate`" + ` would write for a first migration. |
| ` + "`models.go`" + ` | The row structs, with the ` + "`sqlb`" + ` tags removed. |
| ` + "`store.go`" + ` | One function per statement. The SQL is written out. |
| ` + "`support.go`" + ` | Query-string parsing, WHERE assembly, JSON writing. The only file that is the same in every project. |
| ` + "`handlers.go`" + ` | ` + "`net/http`" + ` handlers, one per exposed operation. |

`)

	b.WriteString("## The endpoints\n\n")
	exposed := exposedTables(tables)
	if len(exposed) == 0 {
		b.WriteString("The schema exposed no REST surface, so there are no handlers. " +
			"What came out is the schema and the statements, which is the whole exit for a project " +
			"that used sqlb as a query builder.\n\n")
	} else {
		b.WriteString("| Method | Path | Handler |\n|---|---|---|\n")
		for _, t := range exposed {
			r := t.Rest()
			name := TypeName(t)
			if r.Ops.Has(schema.OpList) {
				fmt.Fprintf(&b, "| GET | `%s` | `List%s` |\n", r.Path, name)
			}
			if r.Ops.Has(schema.OpSingleton) {
				fmt.Fprintf(&b, "| GET | `%s` | `Get%s` |\n", r.Path, name)
			}
			if r.Ops.Has(schema.OpRead) {
				fmt.Fprintf(&b, "| GET | `%s/{id}` | `Get%s` |\n", r.Path, name)
			}
			if r.Ops.Has(schema.OpCreate) {
				fmt.Fprintf(&b, "| POST | `%s` | `Insert%s` |\n", r.Path, name)
			}
			// A singleton's writes take the collection path too: it has one row
			// per caller, so there is no segment to name it with.
			item := r.Path + "/{id}"
			if r.Ops.Has(schema.OpSingleton) {
				item = r.Path
			}
			if r.Ops.Has(schema.OpUpdate) {
				fmt.Fprintf(&b, "| PATCH | `%s` | `Update%s` |\n", item, name)
			}
			if r.Ops.Has(schema.OpDelete) {
				fmt.Fprintf(&b, "| DELETE | `%s` | `Delete%s` |\n", item, name)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## What came out whole\n\n" + `
- **CRUD and list**, at the same paths, with the same status codes and the same
  JSON envelope — ` + "`items`, `page`, `per_page`, `has_more`" + `, and ` + "`total`" + ` when
  ` + "`?count=exact`" + ` was asked for.
- **The filter operators that are one SQL fragment each**: ` + "`eq`, `ne`, `lt`," + `
  ` + "`lte`, `gt`, `gte`, `in`, `nin`, `isnull`, `notnull`, `between`, `like`," + `
  ` + "`ilike`, `contains`, `startswith`, `endswith`" + ` — and the bare
  ` + "`?column=value`" + ` shorthand for equality.
- **` + "`?sort`" + `**, ` + "**`?search`**" + ` and ` + "**`?page`/`?per_page`**" + `, with the ceilings the
  schema declared.
- **Capabilities as refusals.** A column that never declared ` + "`Filterable`" + ` cannot
  be filtered here either, and the rejection lists the ones that can be. That is
  a security property, not a convenience: a column left out of the grammar
  cannot be probed through it. Hidden columns are absent from the column table
  entirely.
- **The error shape.** RFC 9457 problem documents, with the ` + "`allowed`" + ` list on
  each detail, so a client's error handling does not change.
- **The constraint mapping.** A duplicate unique value is still a ` + "`409`" + `, and a
  foreign-key, check or not-null violation still a ` + "`422`" + `, classified off
  SQLSTATE class 23 exactly as before — so a retry loop keyed on ` + "`409`" + ` keeps
  working. The ` + "`detail`" + ` text is generic where the API named the resource; the
  status, which is what clients branch on, is identical.
- **The request budgets.** ` + "`MaxFilters`" + `, ` + "`MaxSortTerms`" + ` and ` + "`MaxOffset`" + ` come from
  the schema; the list cap (100 values in one ` + "`in`/`nin`" + `) and the value-length cap
  (256 bytes) are constants at the top of ` + "`support.go`" + `, edit them there. The
  offset budget matters more here than it did behind the API: ` + "`?cursor`" + ` did not
  come out, so a client reading deep has no cheaper spelling to be sent to.
  ` + "`?search`" + ` escapes ` + "`%`" + ` and ` + "`_`" + ` in the term, so a search for a
  literal percent sign still matches literally.
- **The default ordering.** ` + "`DefaultSort`" + ` comes out as a resolved
  ` + "`[]Order`" + ` on the resource's ` + "`Limits`" + `, applied when a request names no
  ` + "`?sort`" + `. It is not a budget and it is here for a different reason: a list
  is well-formed in any order, so an exit that dropped it would answer every
  unsorted request with a different collection and nothing would say so.
- **The obligation.** A table that declared ` + "`Scoped`" + ` or ` + "`SoftDelete`" + ` refuses to
  register without a ` + "`Confine`" + ` hook, and a scoped table with a create endpoint
  refuses without an ` + "`Assign`" + ` hook. Startup errors, exactly as before.

## What did not come out, by name

`)

	b.WriteString(`- **Keyset pagination (` + "`?cursor`" + `).** Offset paging is here; the cursor is
  not. ` + "`?cursor`" + ` is refused with a message saying so rather than ignored.
- **Sparse projections (` + "`?select`" + `).** Every read returns the full row.
- **Relation expansion (` + "`?expand`" + `).** One statement that joined a target and
  built a JSON object for it was the engine, not the surface. Fetch the related
  row from its own endpoint.
- **The JSON filter tree (` + "`?filter=`" + `).** Arbitrary and/or/not nesting is gone;
  the query-parameter operators are not. Negation survives only where an
  operator spells it — ` + "`ne`, `nin`, `notnull`" + ` — so a filter that leaned on a
  ` + "`not`" + ` group has to be restated as one of those or moved into the handler.
- **Array and document operators** (` + "`has`, `hasany`, `hasall`, `hasdoc`" + `, and
  their negations ` + "`nhas`, `nhasany`, `nhasall`, `nhasdoc`" + `). The columns are
  still there and still returned; the containment operators are not.
- **The OpenAPI document**, and with it the generated TypeScript, Dart and CLI
  clients. They were emitted from the schema, and the schema is what you are
  leaving. The wire format they speak is unchanged, so a committed client keeps
  working — it just has no generator behind it any more.
- **Hooks other than the two seams above.** ` + "`BeforeCreate`, `AfterUpdate`" + ` and the
  rest were registrations on a runtime that is no longer here; the handler is a
  function, so the code that ran in a hook goes in it.
- **Transactions across handlers.** Each handler runs one statement. ` + "`DB`" + ` is an
  interface a ` + "`pgx.Tx`" + ` satisfies, so wrapping is yours to arrange.
- **Type overrides.** The models use the default type mapping; a column that had
  a ` + "`Types`" + ` override in the generator has its default Go type here. Enums are
  plain strings, and the CHECK constraint in ` + "`schema.sql`" + ` is what still
  enforces the value set.

`)

	if notes := ejectSchemaNotes(tables); len(notes) > 0 {
		b.WriteString("## Notes for this schema\n\n")
		for _, n := range notes {
			b.WriteString("- " + n + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(`## Wiring it up

` + "```go" + `
mux := http.NewServeMux()
pool, err := pgxpool.New(ctx, dsn)
if err != nil {
	log.Fatal(err)
}
if err := ` + opts.pkg() + `.Register(mux, pool, ` + opts.pkg() + `.Options{
` + ejectOptionsExample(opts.pkg(), exposed) + `}); err != nil {
	// A resource whose schema declared Scoped or SoftDelete fails here rather
	// than serving unconfined rows.
	log.Fatal(err)
}
log.Fatal(http.ListenAndServe(":8080", mux))
` + "```" + `
`)
	return b.String()
}

// ejectOptionsExample writes the Options literal this schema actually needs,
// with the required hooks present and stubbed.
//
// A worked example that would not run is worse than none: the blog's posts
// resource declares a soft delete, so an Options{} literal fails at Register,
// and a reader copying the snippet would meet that failure before anything
// else.
func ejectOptionsExample(pkg string, exposed []*schema.TableDef) string {
	var b strings.Builder
	for _, t := range exposed {
		o := obligationsOf(t)
		if !o.any() {
			continue
		}
		fmt.Fprintf(&b, "\t%s: %s.%sHooks{\n", resourceName(t), pkg, resourceName(t))
		fmt.Fprintf(&b, "\t\t// Required: %s.\n", o.because())
		fmt.Fprintf(&b, "\t\tConfine: func(r *http.Request) ([]%s.Condition, error) {\n", pkg)
		fmt.Fprintf(&b, "\t\t\treturn []%s.Condition{", pkg)
		if o.scope != "" {
			fmt.Fprintf(&b, "\n\t\t\t\t{Column: %q, Op: %s.OpEq, Value: tenantFrom(r)},", o.scope, pkg)
		}
		if o.soft != "" {
			fmt.Fprintf(&b, "\n\t\t\t\t{Column: %q, Op: %s.OpIsNull},", o.soft, pkg)
		}
		fmt.Fprintf(&b, "\n\t\t\t}, nil\n\t\t},\n")
		if o.scope != "" && t.Rest().Ops.Has(schema.OpCreate) {
			fmt.Fprintf(&b, "\t\t// Required: %s is read-only, so no request body carries it.\n", o.scope)
			fmt.Fprintf(&b, "\t\tAssign: func(r *http.Request) (map[string]any, error) {\n")
			fmt.Fprintf(&b, "\t\t\treturn map[string]any{%q: tenantFrom(r)}, nil\n\t\t},\n", o.scope)
		}
		fmt.Fprintf(&b, "\t},\n")
	}
	if b.Len() == 0 {
		return "\t// No resource here declared Scoped or SoftDelete, so every hook may stay nil.\n"
	}
	return b.String()
}

// ejectSchemaNotes are the things true of this particular schema that a reader
// of the exit needs to know — the gaps that are not general, but theirs.
func ejectSchemaNotes(tables []*schema.TableDef) []string {
	var notes []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			notes = append(notes, s)
		}
	}

	for _, t := range tables {
		for _, f := range t.Fields() {
			d := f.Desc()
			switch {
			case d.Computed() && len(d.Needs) > 0:
				add(fmt.Sprintf(
					"`%s.%s` is a computed column whose expression takes a per-request bind (`%s`). "+
						"Nothing here supplies one, so it is absent from the projection and its field "+
						"stays at its zero value — the expression is in `models.go` as a comment, and "+
						"wiring it back means adding the bind to the statement in `store.go`.",
					t.Name(), d.Name, strings.Join(d.Needs, ", ")))
			case d.Computed():
				add(fmt.Sprintf(
					"`%s.%s` is a computed column: it is an expression in the SELECT list of "+
						"`store.go`, not a column of the table, and `schema.sql` does not create it.",
					t.Name(), d.Name))
			}
			if d.Type == schema.TypeVector {
				add(fmt.Sprintf(
					"`%s.%s` is a pgvector column. sqlb's `Vector` type was its codec and the "+
						"similarity search was `Near`; here the column is read as text and nothing "+
						"searches it.", t.Name(), d.Name))
			}
			if d.Array && d.Filterable {
				add(fmt.Sprintf(
					"`%s.%s` is a filterable array column. The containment operators "+
						"(`has`, `hasany`, `hasall` and their negations `nhas`, `nhasany`, "+
						"`nhasall`) did not come out, so the column is returned "+
						"and sortable-by-nothing rather than filterable.", t.Name(), d.Name))
			}
			if d.Type == schema.TypeJSON && d.Filterable {
				add(fmt.Sprintf(
					"`%s.%s` is a filterable jsonb column. `hasdoc` containment and its negation "+
						"`nhasdoc` did not come out; the column is still returned whole.",
					t.Name(), d.Name))
			}
		}
		for _, f := range t.Fields() {
			if d := f.Desc(); d.Ref != nil && d.Expandable {
				add(fmt.Sprintf(
					"`%s` expanded `%s` through `?expand`. The foreign key is still returned; "+
						"the joined row is not.", t.Name(), d.Ref.Name))
			}
		}
	}
	sort.Strings(notes)
	return notes
}
