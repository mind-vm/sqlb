package codegen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// A declared read in the TypeScript client (the read half of ADR-0043).
//
// A declared action has reached all four toolchains since it existed; a
// declared query reached the Go mount and the docs checklist and stopped
// there, so the one thing a declared route is for — that the verb arrives on
// every generated surface instead of being hand-maintained per client — held
// for writes and not for reads (#316).
//
// The last of the four things emitted here is the one that was the point.
// schema.Query.Reads exists so that a client cache knowing a read touches
// "tasks" can refetch it when a change-feed event for "tasks" arrives, and
// until there was a transport function there was no cache key to put in
// changeKeysByTable — so Reads was validated, carried into rest.QuerySpec,
// rendered into the OpenAPI description and read by nothing. A query's keys
// now sit under every served table in its reach.

// tsQueryName is the client function: the verb, then the table's plural.
// "overdue" on tasks gives overdueTasks, beside listTasks.
//
// Plural where tsActionName is singular, because the two answer differently:
// an action addresses one row and a query answers with a set, so completeTask
// and overdueTasks each read as what they return.
func tsQueryName(t *schema.TableDef, q schema.Query) string {
	return lowerFirstRune(tsQueryBase(t, q))
}

// tsQueryBase is the exported half of every name this file derives.
func tsQueryBase(t *schema.TableDef, q schema.Query) string {
	return GoName(strings.ReplaceAll(q.Name, "-", "_")) + GoName(t.LocalName())
}

// tsQueryParamsName is the request parameter interface, named for the function
// that takes it.
func tsQueryParamsName(t *schema.TableDef, q schema.Query) string {
	return tsQueryBase(t, q) + "Params"
}

// tsQueryResultName is the row interface of a read that declared its result.
func tsQueryResultName(t *schema.TableDef, q schema.Query) string {
	return tsQueryBase(t, q) + "Result"
}

// tsQueryProp is the read's spelling on the resource's queries object:
// "by-assignee" becomes byAssignee, beside list, infinite and detail.
func tsQueryProp(q schema.Query) string {
	return lowerFirstRune(GoName(strings.ReplaceAll(q.Name, "-", "_")))
}

// tsQueryReadFactories are the property names the generated read factory
// already occupies, which a declared query may therefore not take.
//
// schema.validateQueries refuses a query named for an operation the resource
// generates — "list" against OpList, "get" against OpRead — because those
// collide in the operation id and in every client's function name. These three
// collide only here: they are how this file spells the *shapes* of a generated
// read rather than its route, so nothing upstream has a reason to know them.
var tsQueryReadFactories = []string{"infinite", "detail", "single"}

// tsQueryPropCollision reports a declared read whose spelling on the queries
// object is one the read factory already emits.
//
// A generator that emitted both would produce an object literal with a
// duplicate key: the second wins silently under tsc's default settings, so the
// failure is a query that returns a page of rows and no error anywhere.
func tsQueryPropCollision(resources []tsResource) error {
	for _, r := range resources {
		for _, q := range r.table.Queries() {
			prop := tsQueryProp(q)
			for _, taken := range tsQueryReadFactories {
				if prop != taken {
					continue
				}
				return fmt.Errorf(
					"codegen: table %s declares the query %q, which the TypeScript read factory already "+
						"spells %q — %sQueries(request).%s is the resource's own %s read.\n"+
						"  Name the read for what it answers with (overdue, by-assignee) rather than for the shape it takes",
					r.table.Name(), q.Name, prop, r.ident, prop, prop)
			}
		}
	}
	return nil
}

// tsQueryParamTypes emits one interface per declared read.
//
// Emitted even for a read that declares no parameters, unlike an action's
// body: the function takes `params` in the middle of its argument list rather
// than at the end, so there is a signature here to keep stable in a way a
// trailing optional argument does not have.
func tsQueryParamTypes(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	for _, q := range t.Queries() {
		fmt.Fprintf(b, "\n/** The query parameters for `GET %s`. */\n", q.FullPath(t.Rest().Path))
		// A type alias rather than an interface, for the reason %sWhere is
		// one: encodeQueryParams takes a Record<string, unknown>, TypeScript
		// gives an object type alias an implicit index signature and an
		// interface none, so an interface here does not compile at the one
		// call site that matters.
		if len(q.Params) == 0 {
			fmt.Fprintf(b, "export type %s = Record<string, never>;\n", tsQueryParamsName(t, q))
			continue
		}
		fmt.Fprintf(b, "export type %s = {\n", tsQueryParamsName(t, q))
		for _, f := range q.Params {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s%s: %s;\n", tsProp(d.Name), tsOptional(optionalOnCreate(d)), tsDeclaredType(typeName, d))
		}
		fmt.Fprintln(b, "};")
	}
}

// tsQueryResults emits one interface per read that declared its result shape.
//
// A read that declares none answers with rows of its own table, which is a
// type the client already has.
func tsQueryResults(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	for _, q := range t.Queries() {
		if len(q.Returns) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n/** One row of the answer to `GET %s`. */\n", q.FullPath(t.Rest().Path))
		fmt.Fprintf(b, "export interface %s {\n", tsQueryResultName(t, q))
		for _, f := range q.Returns {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s%s: %s;\n", tsProp(d.Name), tsOptional(optionalOnCreate(d)), tsDeclaredType(typeName, d))
		}
		fmt.Fprintln(b, "}")
	}
}

// tsQueryRowType is what one call answers with: rows of the table, or rows of
// the shape the read declared.
func tsQueryRowType(t *schema.TableDef, q schema.Query) string {
	if len(q.Returns) > 0 {
		return tsQueryResultName(t, q)
	}
	return TypeName(t)
}

// tsQueryParamsArg is the params argument as it appears in a signature: the
// type, plus a default where every declared parameter may be omitted, so that
// the call is `overdueTasks(request)`.
//
// The transport function and the TanStack factory both take this argument and
// must agree about it — one defaulting where the other does not is a wrapper
// whose signature cannot satisfy the function it wraps.
func tsQueryParamsArg(t *schema.TableDef, q schema.Query) string {
	name := tsQueryParamsName(t, q)
	for _, f := range q.Params {
		// One parameter the read cannot answer without is enough: there is no
		// default that stands for it, so the caller has to pass the object.
		if !optionalOnCreate(f.Desc()) {
			return name
		}
	}
	return name + " = {}"
}

// tsQueryFunctions emits the transport function for each declared read.
func tsQueryFunctions(b *bytes.Buffer, r tsResource) {
	for _, q := range r.table.Queries() {
		path := q.FullPath(r.path)
		summary := q.Summary
		if summary == "" {
			summary = queryDef{table: r.table, query: q}.summary()
		}
		fmt.Fprintf(b, "\n/** `GET %s` — %s. */\n", path, strings.ToLower(summary[:1])+summary[1:])

		fmt.Fprintf(b, "export function %s(request: Transport, params: %s, signal?: AbortSignal): Promise<%s[]> {\n",
			tsQueryName(r.table, q), tsQueryParamsArg(r.table, q), tsQueryRowType(r.table, q))
		// encodeQueryParams rather than encodeListQuery: a declared read's
		// parameters are its own — no operator grammar, no paging, no
		// projection — so each property is one query parameter under the name
		// the schema gave it.
		fmt.Fprintf(b, "  return request({ method: 'GET', path: %s, query: encodeQueryParams(params), signal });\n}\n",
			tsString(path))
	}
}

// tsQueryKeys emits the key factory's declared-read entries.
//
// One generic pair rather than one entry per read, because the change-feed
// subscriber next door needs to name a read it was not generated beside: a
// query declared on tasks that also Reads projects puts its key in the
// projects entry, and a per-read factory entry would have to be reached
// through the declaring resource's ident anyway.
func tsQueryKeys(b *bytes.Buffer, r tsResource) {
	if len(r.table.Queries()) == 0 {
		return
	}
	name := tsString(r.table.Name())
	fmt.Fprintf(b, "  queries: () => [%s, 'query'] as const,\n", name)
	// params defaults to {}, so query('overdue') is the prefix that matches
	// every parameterisation of that read — the property TanStack's partial
	// key matching gives an object, and the same one detail(key) already
	// relies on.
	fmt.Fprintf(b, "  query: (name: string, params: unknown = {}) => [%s, 'query', name, params] as const,\n", name)
}

// tsQueryReach is the declared reads whose result a change to one table
// invalidates, keyed by table name.
//
// A read's reach is the table it is declared on — implicit, the way an
// update's primary key is — plus every table its Reads names. This is the
// whole of what Reads was for.
type tsQueryReach map[string][]tsReachedQuery

// tsReachedQuery is one declared read, as the table it reaches has to spell it.
type tsReachedQuery struct {
	ident   string // the declaring resource's key factory prefix: task, for taskKeys
	table   string // the declaring table's name
	name    string // the read's declared name
	ownedBy bool   // declared on the table whose entry this is
}

// tsQueryReaches builds the reach map for the resources being emitted.
func tsQueryReaches(resources []tsResource) tsQueryReach {
	reach := tsQueryReach{}
	for _, r := range resources {
		for _, q := range r.table.Queries() {
			reach[r.table.Name()] = append(reach[r.table.Name()], tsReachedQuery{
				ident: r.ident, table: r.table.Name(), name: q.Name, ownedBy: true,
			})
			for _, read := range q.Reads {
				if read.Name() == r.table.Name() {
					continue
				}
				reach[read.Name()] = append(reach[read.Name()], tsReachedQuery{
					ident: r.ident, table: r.table.Name(), name: q.Name,
				})
			}
		}
	}
	return reach
}

// key is this read's cache key expression, as the table it reaches has to spell
// it: under the declaring resource's factory, never under the reached table's.
//
// Written once because the two callers below would otherwise each carry a copy
// of the key's *shape*, and a shape that changed in one of them would emit a
// client whose change feed invalidates a key nothing registers — which
// typechecks, since both are strings.
func (q tsReachedQuery) key() string {
	return fmt.Sprintf("%sKeys.query('%s')", q.ident, q.name)
}

// keysFor renders the key expressions a change to table invalidates, for the
// branch that names one row.
//
// A read declared on the table itself is left out of the keyless branch by the
// caller and included here, because that branch already invalidates the
// table's whole namespace through all() — and a query key lives under it.
func (reach tsQueryReach) keysFor(table string) []string {
	var out []string
	for _, q := range reach[table] {
		out = append(out, q.key())
	}
	return out
}

// foreignKeysFor is the subset a keyless change also invalidates: the reads
// declared on *other* tables, whose keys sit outside this table's namespace
// and so are not covered by its all().
func (reach tsQueryReach) foreignKeysFor(table string) []string {
	var out []string
	for _, q := range reach[table] {
		if q.ownedBy {
			continue
		}
		out = append(out, q.key())
	}
	return out
}

// unservedReads names the tables a declared read lists in Reads that this
// client does not serve, with the reads that named them.
//
// A change to one of them invalidates nothing, because the subscriber drops
// every event for a table absent from keysByTable — there is no key factory to
// index and no TableName to narrow to. That is a real gap in what Reads
// promises, and the emitter states it in the file rather than leaving it to be
// discovered by a stale chart.
func unservedReads(resources []tsResource) []string {
	served := map[string]bool{}
	for _, r := range resources {
		served[r.table.Name()] = true
	}
	byTable := map[string][]string{}
	for _, r := range resources {
		for _, q := range r.table.Queries() {
			for _, read := range q.Reads {
				if served[read.Name()] {
					continue
				}
				byTable[read.Name()] = append(byTable[read.Name()], r.table.Name()+"."+q.Name)
			}
		}
	}
	var out []string
	for table, reads := range byTable {
		out = append(out, fmt.Sprintf("%s (read by %s)", table, strings.Join(reads, ", ")))
	}
	sort.Strings(out)
	return out
}

// tsQueryOptions emits the TanStack read factory's declared-read entries.
func tsQueryOptions(b *bytes.Buffer, r tsResource) {
	for _, q := range r.table.Queries() {
		fmt.Fprintf(b, "    %s: (params: %s) =>\n", tsQueryProp(q), tsQueryParamsArg(r.table, q))
		fmt.Fprint(b, "      queryOptions({\n")
		fmt.Fprintf(b, "        queryKey: %sKeys.query('%s', params),\n", r.ident, q.Name)
		fmt.Fprintf(b, "        queryFn: ({ signal }) => %s(request, params, signal),\n", tsQueryName(r.table, q))
		fmt.Fprint(b, "      }),\n")
	}
}
