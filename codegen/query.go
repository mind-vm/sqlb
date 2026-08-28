package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// Emitting a declared query (ADR-0057).
//
// A query has no envelope to generate — no fetch, no lock, no obligation
// check, see rest.Query's doc comment — so what comes out of one declaration
// is narrower than an action's: the parameter type the request decodes into,
// a field on the Queries struct, and the registration. The result type is
// the one thing schema.Query deliberately does not declare (ADR-0057's own
// argument: the func's actual return type is the contract, not a second
// field vocabulary for outputs), and codegen has to emit something concrete
// regardless — see resultType.

// queryDef pairs a query with the table that declared it.
type queryDef struct {
	table *schema.TableDef
	query schema.Query
}

// queriesOf collects the declared queries of the exposed tables, in table
// order and then declaration order.
func queriesOf(exposed []*schema.TableDef) []queryDef {
	var out []queryDef
	for _, t := range exposed {
		for _, q := range t.Queries() {
			out = append(out, queryDef{table: t, query: q})
		}
	}
	return out
}

// goName is the query's exported identifier — "overdue" on tasks gives
// OverdueTasks, the same convention actionDef.goName uses.
func (d queryDef) goName() string {
	verb := GoName(strings.ReplaceAll(d.query.Name, "-", "_"))
	return verb + TypeName(d.table)
}

// paramsName is the generated request parameter type, emitted even when the
// query declares none — see actionDef.inputName.
func (d queryDef) paramsName() string { return d.goName() + "Params" }

// fullPath is the route: the resource path with the query's own appended.
func (d queryDef) fullPath() string { return d.query.FullPath(d.table.Rest().Path) }

// resultName is the generated row type of a query that declared one.
func (d queryDef) resultName() string { return d.goName() + "Result" }

// returns is the declared result shape, empty when the query answers with rows
// of its own table.
func (d queryDef) returns() []*schema.Field { return d.query.Returns }

// resultType is what Do returns: a slice of the table's own model, or a slice
// of the declared result where the query says its answer is not rows of this
// table.
//
// The default was the whole of what ADR-0057 settled, and it left the rest
// open with a trigger: revisit if the fixed []T turns out to be the wrong
// default often enough that a result type is worth declaring. It did. A
// metering table's actual read is a chart — per-bucket sums — and a bucketed
// sum is a row of no declared table, so every application with one hand-wrote
// that endpoint outside the generated surface no matter how much of the rest
// was generated (#240).
func (d queryDef) resultType() string {
	if len(d.returns()) > 0 {
		return "[]" + d.resultName()
	}
	return "[]" + TypeName(d.table)
}

// summary defaults to "Overdue tasks" — capitalised verb, then the table
// name. A query has no collection/item distinction to pick an article by,
// unlike actionSummary.
func (d queryDef) summary() string {
	if s := d.query.Summary; s != "" {
		return s
	}
	words := strings.ReplaceAll(d.query.Name, "-", " ")
	words = strings.ToUpper(words[:1]) + words[1:]
	return words + " " + d.table.LocalName()
}

// queryParamType is the Go type of one declared parameter.
//
// Never a pointer, which is where this differs from actionBodyType and is not
// a stylistic choice: huma panics at Register with "pointers are not supported
// for form/header/path/query parameters", so a query whose declaration carried
// one optional parameter brought the server down at mount rather than serving
// a route. The body rule — a pointer distinguishes omitted from zero — was
// copied here from a position where it is right, and a query string is not
// that position. `required` below is what carries the distinction instead: a
// parameter huma does not require and the caller did not send arrives as the
// zero value, and the func reads that as "not asked for", which is the same
// thing an absent query parameter has always meant.
func queryParamType(d *schema.FieldDesc) string {
	return strings.TrimPrefix(d.GoType(), "*")
}

// queryParamTags renders one parameter's struct tags.
//
// required is stated rather than left to huma's default, which treats every
// query parameter as optional. A declared parameter that is neither nullable
// nor defaulted is one the read cannot answer without, so a request omitting
// it should be a 422 naming the parameter rather than a call with a zero
// value the func has no way to tell from a deliberate one.
func queryParamTags(d *schema.FieldDesc) string {
	tags := fmt.Sprintf("query:%q", d.Name)
	if !optionalOnCreate(d) {
		tags += ` required:"true"`
	}
	return tags + valueTags(d)
}

// renderQueryParams writes one query's request parameter type.
//
// query, not json: a query's parameters arrive on the query string, which
// rest.Query binds In onto directly as huma's input struct rather than
// wrapping it in a decoded body — see rest.Query's doc comment for why there
// is no wrapper type to hide the tag choice behind.
func renderQueryParams(b *bytes.Buffer, d queryDef) {
	name := d.paramsName()

	fmt.Fprintf(b, "\n// %s is the query parameters for %s.\n", name, d.fullPath())
	if len(d.query.Params) == 0 {
		fmt.Fprintf(b, "//\n// The query declares no parameters. The type is emitted anyway, so that\n")
		fmt.Fprintf(b, "// declaring the first one later does not change the signature of %s.\n", d.goName())
		fmt.Fprintf(b, "type %s struct{}\n", name)
		return
	}
	fmt.Fprintf(b, "//\n// No property is a pointer: huma refuses one on a query parameter. A\n")
	fmt.Fprintf(b, "// parameter that may be omitted arrives as its zero value, and one that\n")
	fmt.Fprintf(b, "// may not is tagged required, so omitting it is a 422 rather than a call\n")
	fmt.Fprintf(b, "// the func cannot distinguish from a deliberate zero.\n")
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range d.query.Params {
		desc := f.Desc()
		fmt.Fprintf(b, "\t%s %s `%s`", GoName(desc.Name), queryParamType(desc), queryParamTags(desc))
		if c := desc.Comment; c != "" {
			fmt.Fprintf(b, " // %s", oneLine(c))
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintln(b, "}")
}

// renderQueryResult writes one query's declared row type.
//
// It is renderActionResult for a read, and deliberately the same shape: the
// two declare their result in one vocabulary, so a reader who has met one
// recognises the other.
func renderQueryResult(b *bytes.Buffer, d queryDef) {
	props := d.returns()
	if len(props) == 0 {
		return
	}
	name := d.resultName()
	fmt.Fprintf(b, "\n// %s is one row of the answer to %s.\n", name, d.fullPath())
	fmt.Fprintf(b, "//\n// The query answers with these rather than with rows of %s: what it reads\n", TypeName(d.table))
	fmt.Fprintf(b, "// is grouped or aggregated, and the result is a row of no declared table.\n")
	fmt.Fprintf(b, "// sqlb.Collect[%s] is how the func reads one out of the database, and the\n", name)
	fmt.Fprintf(b, "// query hooks run for it as they do for any other read.\n")
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range props {
		desc := f.Desc()
		// A db tag as well as json: this type is the destination of a
		// sqlb.Collect, which matches result columns to fields by db tag, and
		// without one the func cannot scan into what it was handed.
		fmt.Fprintf(b, "\t%s %s `db:%q json:\"%s\"%s`", GoName(desc.Name), actionBodyType(desc), desc.Name,
			desc.Name, valueTags(desc))
		if c := desc.Comment; c != "" {
			fmt.Fprintf(b, " // %s", oneLine(c))
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintln(b, "}")
}

// queryParamImports records the packages a query's parameters name. Mirrors
// actionBodyImports.
func queryParamImports(imports map[string]bool, defs []queryDef) {
	for _, d := range defs {
		for _, f := range append(append([]*schema.Field{}, d.query.Params...), d.query.Returns...) {
			switch goType := f.Desc().GoType(); {
			case strings.Contains(goType, "time.Time"):
				imports["time"] = true
			case strings.Contains(goType, "json.RawMessage"):
				imports["encoding/json"] = true
			}
		}
	}
}

// renderQueries writes the struct that carries the application's declared
// reads — Actions' and Mutations' sibling, one field per declared query.
func renderQueries(b *bytes.Buffer, defs []queryDef) {
	fmt.Fprintf(b, "\n// Queries carries the domain funcs the declared queries call.\n")
	fmt.Fprintf(b, "//\n// Each field is one declared read. What is generated is the route and the\n")
	fmt.Fprintf(b, "// parameter binding; there is no fetch, no lock and no obligation check —\n")
	fmt.Fprintf(b, "// a query confines what it reads the way sqlb.Query in application code\n")
	fmt.Fprintf(b, "// already does, by running against the Executor it is handed.\n")
	fmt.Fprintf(b, "//\n// A field left nil is refused when Register mounts the resource, not by\n")
	fmt.Fprintf(b, "// the request that would have called it.\n")
	fmt.Fprintln(b, "type Queries struct {")
	for i, d := range defs {
		if i > 0 {
			fmt.Fprintln(b)
		}
		fmt.Fprintf(b, "\t// %s runs GET %s.\n", d.goName(), d.fullPath())
		if desc := d.query.Description; desc != "" {
			fmt.Fprintf(b, "\t//\n")
			docLines(b, "\t", desc)
		}
		if reads := d.query.Reads; len(reads) > 0 {
			names := make([]string, len(reads))
			for i, t := range reads {
				names[i] = t.Name()
			}
			fmt.Fprintf(b, "\t//\n\t// Also reads: %s. Declared, not enforced — see the schema.\n", quoteList(names))
		}
		fmt.Fprintf(b, "\t%s func(context.Context, sqlb.Executor, %s) (%s, error)\n",
			d.goName(), d.paramsName(), d.resultType())
	}
	fmt.Fprintln(b, "}")
}

// renderQueryCalls writes the registrations for one table's queries.
func renderQueryCalls(b *bytes.Buffer, optsVar string, defs []queryDef) {
	for _, d := range defs {
		fmt.Fprintf(b, "\tif err := rest.Query[%s, %s](api, db, %s, rest.QuerySpec{\n",
			d.paramsName(), d.resultType(), optsVar)
		fmt.Fprintf(b, "\t\tName:  %q,\n", d.query.Name)
		fmt.Fprintf(b, "\t\tPath:  %q,\n", d.fullPath())
		fmt.Fprintf(b, "\t\tField: %q,\n", d.goName())
		fmt.Fprintf(b, "\t\tSummary: %q,\n", d.summary())
		if s := d.query.Description; s != "" {
			fmt.Fprintf(b, "\t\tDescription: %q,\n", s)
		}
		if reads := d.query.Reads; len(reads) > 0 {
			names := make([]string, len(reads))
			for i, t := range reads {
				names[i] = t.Name()
			}
			fmt.Fprintf(b, "\t\tReads: []string{%s},\n", quotedList(names))
		}
		if len(d.query.Params) > 0 {
			fmt.Fprintf(b, "\t\tHasParams: true,\n")
		}
		fmt.Fprintf(b, "\t}, queries.%s); err != nil {\n\t\treturn err\n\t}\n", d.goName())
	}
}
