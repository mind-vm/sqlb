package schema

import (
	"fmt"
	"strings"
)

// An action is a domain verb the table exposes: POST /tasks/{id}/complete.
//
// What is declared here is the *envelope* — the route, the request body, and
// the columns the verb is allowed to leave changed. What runs inside it is a
// plain Go func, bound at registration rather than here. That split is the
// whole of ADR-0043: domain logic is where a DSL's expressiveness runs out
// first, so this one does not try, and the seam is the one BeforeCreate
// already uses.
//
// The func cannot live in this struct even if it wanted to. A registry is a
// value five emitters read and sqlb.json serialises, and it is linked into the
// sqlb command — a func is neither serialisable nor readable by a generator,
// and putting the application's domain code here would make the driver depend
// on the application.

// Action is a domain verb exposed on a table.
//
// Declare one with [TableDef.AddAction] — renamed from Action after
// [TableDef.AddQuery] settled the family: an ADR fixes the architecture a
// name sits in, not the name itself, and the declaration methods reading
// alike matters more than this one keeping the spelling ADR-0043 happened to
// pick before it had a sibling to agree with. ADR-0057 also tried
// TableDef.AddMutation, a same-shaped second name for this type's item form,
// and retired it the same day it shipped: an item-form Action is what a
// row-scoped write is, in this schema.
//
//	Task.AddAction(schema.Action{
//	    Name:   "complete",
//	    Body:   schema.Body(schema.Text("note").Nullable()),
//	    Writes: []string{"status", "closed_at"},
//	})
//
// which serves POST /tasks/{id}/complete and asks the application, at
// registration, for a func(context.Context, *Task, CompleteTaskInput) error.
type Action struct {
	// Name is the verb. It appears in the URL, in the operation ID, and in the
	// generated identifiers — "complete" gives POST /tasks/{id}/complete and an
	// Actions.CompleteTask field.
	//
	// It may not be the verb of an operation the table already exposes. An
	// action named "create" beside OpCreate does not override it: both spell one
	// operation id and one client function, and Validate refuses the pair rather
	// than leaving the duplicate to be discovered at mount.
	Name string

	// Path is the sub-path under the collection. It defaults to
	// "/{id}/"+Name, which is the item form.
	//
	// A path that does not contain "{id}" is a *collection* action: there is no
	// row to fetch, so the verb receives only the body and answers 204. Note
	// what that costs — with no generated fetch there is no BeforeQuery for a
	// declared scope to hang off, so a collection action inherits none of
	// ADR-0030's closure and is in the same position as a sqlb.Query in
	// application code.
	Path string

	// Body is the request body, declared in the field vocabulary. Build it with
	// [Body].
	//
	// It is declared rather than reflected from an application type for two
	// reasons. The value of an action is that the verb reaches the TypeScript,
	// Dart, CLI and OpenAPI emitters, and those read this declaration — a body
	// sqlb cannot see produces a client method typed `unknown`, which is the
	// drift this feature exists to remove. And reflecting an application struct
	// would invert the dependency, since models are generated *from* the schema.
	//
	// Leaving it empty is normal: most verbs carry nothing. The generated input
	// type is still emitted, empty, so that adding the first property later
	// does not change the shape of the func the application wrote.
	Body []*Field

	// Writes names the columns the envelope persists after the verb returns,
	// and it is enforced rather than documented: exactly these columns are
	// written, from the row the verb mutated.
	//
	// A verb that has to touch anything else has the transaction and can issue
	// the statement itself — see Touches, which is where the route says so. It
	// is worth being precise about the scope of this field, because three
	// surfaces print it and none of them can widen it: Writes is what the
	// *envelope* persists, not a bound on the transaction.
	//
	// What it does buy is the row lock. A declared write set is exactly the
	// case where a read-modify-write can be lost, so the envelope's fetch takes
	// SELECT … FOR UPDATE on this row — and on this row only.
	//
	// It must be empty on a collection action, which has no row.
	Writes []string

	// Touches names the tables the verb writes through the transaction, beyond
	// the row the envelope persists. It is documentation with no enforcement
	// behind it, and that is the whole design: the alternative was a route that
	// prints a two-column write set while opening eleven tables' worth of
	// transaction, with nothing in the generated surfaces to say so (#149).
	//
	//	Order.AddAction(schema.Action{
	//	    Name:    "place",
	//	    Writes:  []string{"status", "placed_at"},
	//	    Touches: []string{"order_lines", "inventory_reservations", "payments"},
	//	})
	//
	// `sqlb impact`, the OpenAPI description and the CLI's --help carry it
	// beside Writes, which is what makes a wide route say it is wide. The
	// caller ADR-0029 has in mind — one with no compile step, "such as an
	// agent" — reads a declared write set of two columns and concludes the verb
	// is confined to one row; that inference is the one the surface invites,
	// and this is how a route declines it.
	//
	// Nothing checks the claim. A test asserting it against the statements the
	// verb actually issued is the application's to write, and can be; a checker
	// here would have to trace application code the schema package cannot see.
	// So a stale Touches is possible, and it is still strictly better than the
	// silence it replaces — the failure mode is an over-broad claim rather than
	// a confident understatement.
	//
	// Unlike Writes it is legal on a collection action, which does all of its
	// work through the transaction and has no row of its own at all. Naming
	// this table is legal too, and means what it says: the envelope writes one
	// row of it, and a verb that writes others has no other way to declare them.
	Touches []string

	// Returns declares the verb's response body, in the field vocabulary. Build
	// it with [Result].
	//
	//	Lesson.AddAction(schema.Action{
	//	    Name: "submit-quiz",
	//	    Body: schema.Body(schema.JSON("answers")),
	//	    Returns: schema.Result(
	//	        schema.Int("score"),
	//	        schema.Int("total"),
	//	    ),
	//	})
	//
	// Leaving it empty is the default and is what almost every verb wants: an
	// item action answers with the row it acted on, and a collection action
	// answers 204. This is for the verb whose *answer* is the point and is not
	// a row of this table — grading a quiz returns a score, and a score is not
	// a Lesson (#312).
	//
	// Declaring one replaces the response rather than adding to it. An item
	// action that returns a score does not also return the lesson, because one
	// operation has one response body; a verb that wants both declares both
	// halves here, or the client re-reads the row it already knows the id of.
	//
	// The status is 200 either way. A verb that created something and wants to
	// say so with a 201 is describing OpCreate, which is its own operation with
	// its own body and its own response — see [REST.CreateInput] for the case
	// where that create needs an input the row has no column for.
	//
	// It is declared rather than reflected from the func's return type for the
	// reason Body is: the value of an action is that it reaches the TypeScript,
	// Dart, CLI and OpenAPI emitters, and a result those cannot see produces a
	// client method typed `unknown`.
	Returns []*Field

	// Summary is the one-line description in the OpenAPI document.
	//
	// Left empty it is filled in downstream, as "Complete a task", rather than
	// here: writing that sentence needs the singular of the table name, and a
	// singulariser is a thing codegen has and this package deliberately does
	// not — a wrong guess in a Go type name is cosmetic, and one baked into a
	// declaration is not.
	Summary string

	// Description documents the operation at length.
	Description string
}

// AddAction declares a domain verb on the table and returns the table, so
// that declarations chain the way Expose and AddIndex already do.
//
// The table must also be exposed: an action is a route on the resource, and a
// table with no resource has nowhere to put one.
func (t *TableDef) AddAction(a Action) *TableDef {
	if a.Path == "" {
		a.Path = "/{id}/" + a.Name
	}
	t.actions = append(t.actions, a)
	return t
}

// Actions returns the table's declared verbs, in declaration order.
func (t *TableDef) Actions() []Action { return t.actions }

// IsCollection reports whether the action addresses the collection rather than
// one row — which is to say, whether its path names no id.
func (a Action) IsCollection() bool { return !strings.Contains(a.Path, "{id}") }

// FullPath is the action's route: the resource path with the action's own path
// appended.
func (a Action) FullPath(resource string) string { return resource + a.Path }

// validateActions checks one table's verbs.
//
// Every rule here closes something that is otherwise silent at runtime: a verb
// whose Writes names a column that does not exist writes nothing and answers
// 200, and a body field claiming Filterable looks like it did something.
func (r *Registry) validateActions(t *TableDef, report func(string, string, string, ...any)) {
	if len(t.actions) == 0 {
		return
	}
	if t.rest == nil {
		report(t.name, "", "declares %d action(s) but is not exposed; an action is a route on the resource, so add Expose", len(t.actions))
		return
	}

	seen := make(map[string]bool, len(t.actions))
	paths := make(map[string]string, len(t.actions))
	for _, a := range t.actions {
		switch {
		case a.Name == "":
			report(t.name, "", "action has no Name")
			continue
		case !isActionName(a.Name):
			report(t.name, "", "action name %q must be a lower-case identifier, optionally hyphenated: complete, archive, mark-read", a.Name)
		}
		if seen[a.Name] {
			report(t.name, "", "action %q declared twice", a.Name)
		}
		seen[a.Name] = true

		if op, verb, dup := collidesWithOp(t.rest.Ops, a.Name); dup {
			report(t.name, "", "action %q collides with %s, which the resource already generates as its "+
				"%q operation: the two share an operation id in the OpenAPI document, which Huma refuses "+
				"at mount, and a function name in every generated client, which then does not compile. "+
				"Name the verb for the transition it performs — complete, archive, mark-read — or drop "+
				"%s from Expose, which leaves the action as the resource's only %s route",
				a.Name, op, verb, op, verb)
		}

		if !strings.HasPrefix(a.Path, "/") {
			report(t.name, "", "action %q has path %q, which must start with %q", a.Name, a.Path, "/")
		}
		if prev, dup := paths[a.Path]; dup {
			report(t.name, "", "action %q uses the same path as %q, so routing would depend on declaration order", a.Name, prev)
		}
		paths[a.Path] = a.Name

		// The item form addresses a row, so it needs something to address it
		// by. Reported here rather than at mount because the declaration is
		// where the mistake is.
		if !a.IsCollection() && t.PrimaryKey() == nil {
			if cols := t.CompositeKey(); len(cols) > 0 {
				report(t.name, "", "action %q addresses a row by id but the table's key is composite (%s), "+
					"and one column is what an id is", a.Name, strings.Join(cols, ", "))
			} else {
				report(t.name, "", "action %q addresses a row by id but the table declares no primary key", a.Name)
			}
		}

		r.validateBody(t, fmt.Sprintf("action %q", a.Name), a.Body, report)
		r.validateBody(t, fmt.Sprintf("action %q: result", a.Name), a.Returns, report)
		r.validateActionWrites(t, a, report)
		r.validateActionTouches(t, a, report)
	}
}

// collidesWithOp reports whether name is the verb an exposed operation is
// already generated under, and if so names both: the operation as Expose spells
// it, and the verb every surface spells it with.
//
// The verbs are not [Op.String]'s. That one names the operation — read — and
// this one names the word the generated code carries: OpRead is get%s in the
// TypeScript and Dart clients, `get` on the command line and get-%s in the
// OpenAPI document, and OpSingleton is the same word without the id. The
// mapping has to be here rather than in codegen because a duplicate is not a
// codegen problem: a declaration this package accepts produces a server that
// panics at mount on the duplicate operation id, and four clients that fail to
// compile on the duplicate declaration. The refusal belongs where the mistake
// is.
//
// The comparison is literal, not normalised. An action name is already
// constrained to lower-case and hyphens by [isActionName], and no operation
// verb carries a hyphen — so `cre-ate` is creAte%s in a client and cre-ate-%s
// in the document, which collides with nothing, and stripping hyphens before
// comparing would refuse it for a collision it does not have.
func collidesWithOp(ops Op, name string) (op, verb string, found bool) {
	for _, e := range []struct {
		op       Op
		declared string // how Expose spells it
		verb     string // how the generated surfaces spell it
	}{
		{OpCreate, "OpCreate", "create"},
		{OpRead, "OpRead", "get"},
		{OpUpdate, "OpUpdate", "update"},
		{OpDelete, "OpDelete", "delete"},
		{OpList, "OpList", "list"},
		{OpSingleton, "OpSingleton", "get"},
	} {
		if ops.Has(e.op) && name == e.verb {
			return e.declared, e.verb, true
		}
	}
	return "", "", false
}

// validateActionTouches checks the declared blast radius is a set of table
// names, which is as far as checking can go: the tables are the application's
// to write and may live in another module or another schema entirely, so a name
// this registry does not know is a legitimate declaration rather than a typo.
//
// What it does refuse is a claim that says nothing: an empty name, or the same
// table twice. The table's own name is allowed and is not redundant — the
// envelope writes one row of it, and a verb that writes *other* rows of the
// same table has no other way to say so.
func (r *Registry) validateActionTouches(t *TableDef, a Action, report func(string, string, string, ...any)) {
	seen := make(map[string]bool, len(a.Touches))
	for _, name := range a.Touches {
		switch {
		case name == "":
			report(t.name, "", "action %q: Touches has an empty table name", a.Name)
			continue
		case seen[name]:
			report(t.name, "", "action %q: Touches names %q twice", a.Name, name)
			continue
		}
		seen[name] = true
	}
}

// validateActionWrites checks that the declared write set is writable.
func (r *Registry) validateActionWrites(t *TableDef, a Action, report func(string, string, string, ...any)) {
	if a.IsCollection() && len(a.Writes) > 0 {
		report(t.name, "", "action %q is a collection action and has no row to write; drop Writes, or give the path an {id}", a.Name)
		return
	}
	seen := make(map[string]bool, len(a.Writes))
	for _, name := range a.Writes {
		if seen[name] {
			report(t.name, name, "action %q: Writes names the column twice", a.Name)
		}
		seen[name] = true

		f := t.Field(name)
		switch {
		case f == nil:
			report(t.name, name, "action %q: Writes names no column of this table", a.Name)
		case f.Desc().Computed():
			report(t.name, name, "action %q: Writes names a computed column, which has no storage to write to", a.Name)
		case f.Desc().PrimaryKey:
			report(t.name, name, "action %q: Writes names the primary key; a verb that re-keys a row is a delete and an insert", a.Name)
		}
	}
}

// isActionName reports whether s is a legal verb: lower-case, starting with a
// letter, and hyphen-separated. It is a URL segment and a Go identifier at
// once, so it has to survive both.
func isActionName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
