package schema

import "strings"

// A query is a domain read the table exposes: GET /tasks/overdue.
//
// What is declared here is the *envelope* — the route and the request
// parameters — the same split ADR-0043 made for a row-scoped write. What
// runs inside it is a plain Go func, bound at registration rather than here.
//
// Two things a query deliberately does not declare, both for reasons
// ADR-0043 already argued through for Action. The result shape is not here:
// codegen reads it off the func's actual return type the way the compiler
// does, rather than from a second field vocabulary for outputs that would
// only duplicate the column one. And there is no precondition or filter
// language — Params is what the func receives, not a query the framework
// builds; the func is free to call sqlb.Query[T] itself, with whatever
// hooks the Executor it was handed carries.

// Query is a declared read exposed on a table.
//
// Declare one with [TableDef.AddQuery] — not [TableDef.Query], on purpose:
// sqlb.Query[T] is the read builder every hook and Do func already calls, and
// a method plainly named Query on the table you declare things about would
// read as one more way to run that, not as a declaration. AddQuery reads as
// what it is.
//
//	Task.AddQuery(schema.Query{
//	    Name:   "overdue",
//	    Params: schema.Body(schema.Timestamp("as_of")),
//	})
//
// which serves GET /tasks/overdue and asks the application, at registration,
// for a func(context.Context, sqlb.Executor, OverdueParams) ([]Task, error) —
// or any other result type; see rest.Query.
type Query struct {
	// Name names the read. It appears in the URL and in the operation ID:
	// "overdue" gives GET /tasks/overdue.
	Name string

	// Path is the sub-path under the collection. It defaults to "/"+Name.
	//
	// A query addresses no single row — there is no id to fetch by, so no
	// BeforeQuery obligation is generated for it. Confining the statements it
	// issues is the func's own job, the position sqlb.Query in application
	// code is already in (ADR-0030).
	Path string

	// Params is the request's query parameters, declared in the field
	// vocabulary. Build it with [Body], the same helper an action's request
	// body uses — the vocabulary is identical, only the wire position differs.
	Params []*Field

	// Reads names tables this query reads, besides the one it is declared on
	// — that one is implicit, the same way an update only ever names the
	// columns it writes beyond the primary key.
	//
	// It is typed, unlike [Action.Touches], because the difference that lets
	// Touches stay a string is absent here: a verb's transaction can reach a
	// table in another module's registry that this schema package does not
	// import, but a query's Do func cannot read a table it has not imported —
	// there is nothing for a string to name that a *TableDef could not.
	//
	// Nothing here enforces that Do actually reads what it lists — the same
	// discipline as Touches — and it exists for one reason: a generated
	// client cache that knows a query reads "tasks" can invalidate and
	// refetch it whenever a change-feed event for "tasks" arrives
	// ([rest.Event]), without the server computing or pushing a new result.
	// That is the whole of what "opt-in reactive queries" means here — coarse,
	// table-scoped invalidation on top of the outbox change feed that already
	// exists, not per-query dependency tracking. A query that declares no
	// Reads beyond its own table is simply never invalidated by anything else
	// changing; a client still gets its data by calling it again.
	//
	// That cache does not exist yet, and this is written in the present tense
	// because it describes the reason for the field rather than the state of
	// the emitters. As of today a declared query reaches the Go mount and the
	// docs checklist and no client emitter at all — no TypeScript method, no
	// Dart method, no CLI subcommand — so nothing turns Reads into an
	// invalidation. It is validated, carried into rest.QuerySpec, rendered
	// into the OpenAPI description as a sentence and recorded in the contract
	// snapshot, and then read by nothing. Declaring it costs nothing and buys
	// nothing yet; declare it anyway if it is true, since the emitter that
	// consumes it will want it (#316).
	Reads []*TableDef

	// Summary and Description document the operation.
	Summary     string
	Description string
}

// AddQuery declares a domain read on the table and returns the table, so
// that declarations chain the way Action, Expose and AddIndex already do.
//
// The table must also be exposed: a query is a route on the resource, and a
// table with no resource has nowhere to put one.
func (t *TableDef) AddQuery(q Query) *TableDef {
	if q.Path == "" {
		q.Path = "/" + q.Name
	}
	t.queries = append(t.queries, q)
	return t
}

// Queries returns the table's declared reads, in declaration order.
func (t *TableDef) Queries() []Query { return t.queries }

// FullPath is the query's route: the resource path with the query's own path
// appended.
func (q Query) FullPath(resource string) string { return resource + q.Path }

// validateQueries checks one table's declared reads.
func (r *Registry) validateQueries(t *TableDef, report func(string, string, string, ...any)) {
	if len(t.queries) == 0 {
		return
	}
	if t.rest == nil {
		report(t.name, "", "declares %d quer(y/ies) but is not exposed; a query is a route on the resource, so add Expose", len(t.queries))
		return
	}

	seen := make(map[string]bool, len(t.queries))
	paths := make(map[string]string, len(t.queries))
	for _, q := range t.queries {
		switch {
		case q.Name == "":
			report(t.name, "", "query has no Name")
			continue
		case !isActionName(q.Name):
			report(t.name, "", "query name %q must be a lower-case identifier, optionally hyphenated: overdue, by-assignee", q.Name)
		}
		if seen[q.Name] {
			report(t.name, "", "query %q declared twice", q.Name)
		}
		seen[q.Name] = true

		if op, verb, dup := collidesWithOp(t.rest.Ops, q.Name); dup {
			report(t.name, "", "query %q collides with %s, which the resource already generates as its "+
				"%q operation: the two share an operation id in the OpenAPI document, which Huma refuses "+
				"at mount, and a function name in every generated client, which then does not compile. "+
				"Name the read for what it returns — overdue, by-assignee — or drop %s from Expose",
				q.Name, op, verb, op)
		}
		for _, a := range t.actions {
			if a.Name == q.Name {
				report(t.name, "", "query %q has the same name as an action on this table; "+
					"the two share an operation id and a client method name", q.Name)
			}
		}

		if !strings.HasPrefix(q.Path, "/") {
			report(t.name, "", "query %q has path %q, which must start with %q", q.Name, q.Path, "/")
		}
		if prev, dup := paths[q.Path]; dup {
			report(t.name, "", "query %q uses the same path as %q, so routing would depend on declaration order", q.Name, prev)
		}
		paths[q.Path] = q.Name

		r.validateQueryParams(t, q, report)
		r.validateQueryReads(t, q, report)
	}
}

// validateQueryReads checks the declared invalidation set: no nil entry, no
// table named twice, and no naming of t itself — that one is implicit, and
// naming it too would just be Reads restating what Query already says.
func (r *Registry) validateQueryReads(t *TableDef, q Query, report func(string, string, string, ...any)) {
	seen := make(map[*TableDef]bool, len(q.Reads))
	for _, table := range q.Reads {
		switch {
		case table == nil:
			report(t.name, "", "query %q: Reads has a nil table", q.Name)
			continue
		case table == t:
			report(t.name, "", "query %q: Reads names %q, the table the query is declared on; "+
				"that one is implicit and does not need to be listed", q.Name, table.name)
			continue
		case seen[table]:
			report(t.name, "", "query %q: Reads names %q twice", q.Name, table.name)
			continue
		}
		seen[table] = true
	}
}

// validateQueryParams refuses the claims a query parameter cannot make — the
// same check an action's request body gets, since Params uses the same field
// vocabulary.
func (r *Registry) validateQueryParams(t *TableDef, q Query, report func(string, string, string, ...any)) {
	seen := make(map[string]bool, len(q.Params))
	for _, f := range q.Params {
		d := f.Desc()
		if !isIdent(d.Name) {
			report(t.name, d.Name, "query %q: parameter name is not a valid identifier", q.Name)
		}
		if seen[d.Name] {
			report(t.name, d.Name, "query %q: parameter declared twice", q.Name)
		}
		seen[d.Name] = true

		for _, c := range []struct {
			claimed bool
			what    string
		}{
			{d.PrimaryKey, "PrimaryKey"},
			{d.Unique, "Unique"},
			{d.Filterable, "Filterable"},
			{d.Sortable, "Sortable"},
			{d.Searchable, "Searchable"},
			{d.ReadOnly, "ReadOnly"},
			{d.Immutable, "Immutable"},
			{d.Hidden, "Hidden"},
			{d.WriteOnly, "WriteOnly"},
			{d.Scoped, "Scoped"},
			{d.Ref != nil, "Ref"},
			{d.Computed(), "Computed"},
		} {
			if c.claimed {
				report(t.name, d.Name, "query %q: parameter claims %s, which describes a column rather than a request parameter", q.Name, c.what)
			}
		}
	}
}
