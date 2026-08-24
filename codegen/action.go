package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// Emitting a declared action (ADR-0043).
//
// Three things come out of one declaration: the input type the body decodes
// into, a field on the Actions struct whose signature is the contract between
// the schema and the application, and the registration that mounts the
// envelope. The handler itself is not generated — rest.Action is one generic
// function, the same arrangement rest.Resource already has with CRUD.
//
// The Actions struct is where the compiler earns its keep. An action added to
// the schema grows a field, so the next build fails at the call site rather
// than at runtime with a route nobody wired. What the compiler cannot do is
// insist the field is non-nil, since Actions{} is a valid literal — that half
// is refused at mount (rest.missingDo).

// actionDef pairs an action with the table that declared it, which is what
// every name it generates is derived from.
type actionDef struct {
	table  *schema.TableDef
	action schema.Action
}

// actionsOf collects the declared verbs of the exposed tables, in table order
// and then declaration order.
func actionsOf(exposed []*schema.TableDef) []actionDef {
	var out []actionDef
	for _, t := range exposed {
		for _, a := range t.Actions() {
			out = append(out, actionDef{table: t, action: a})
		}
	}
	return out
}

// goName is the action's exported identifier: the verb, then the type it acts
// on. "complete" on tasks gives CompleteTask, which is both the Actions field
// and the stem of the input type.
//
// The verb is hyphenated in the URL and in the schema — mark-read — because
// that is what reads well in a path. GoName splits on underscores, so the
// spellings are reconciled here rather than by constraining the declaration to
// what a Go identifier happens to look like.
func (d actionDef) goName() string {
	verb := GoName(strings.ReplaceAll(d.action.Name, "-", "_"))
	return verb + TypeName(d.table)
}

// inputName is the generated request body type. It is emitted even when the
// action declares no properties, so that adding the first one later does not
// change the signature of the func the application already wrote.
func (d actionDef) inputName() string { return d.goName() + "Input" }

// fullPath is the route: the resource path with the action's own appended.
func (d actionDef) fullPath() string { return d.action.FullPath(d.table.Rest().Path) }

// summary is the operation's one-line description, defaulted here rather than
// in the declaration because writing it needs the singular of the table name.
func (d actionDef) summary() string {
	if s := d.action.Summary; s != "" {
		return s
	}
	return actionSummary(d.action, d.table.LocalName())
}

// actionSummary renders the operation's one-line description: "Complete a task"
// for a verb on one row, "Purge archived tasks" for one on the collection. The
// article follows the shape of the route rather than being fixed, because a
// collection verb that says "a task" is describing the wrong endpoint.
func actionSummary(a schema.Action, local string) string {
	words := strings.ReplaceAll(a.Name, "-", " ")
	words = strings.ToUpper(words[:1]) + words[1:]
	if a.IsCollection() {
		return words + " " + local
	}
	return words + " a " + Singular(local)
}

// renderActionInput writes one action's request body type.
//
// The rules are the create body's, minus the ones that are about columns.
// A nullable or defaulted property is a pointer so that absent is
// distinguishable from zero, and an enum is a plain string carrying Huma's
// enum tag rather than the generated enum type: a body property is not a
// column, so there may be no such type, and the tag is what enforces the value
// set at the boundary in either case.
func renderActionInput(b *bytes.Buffer, d actionDef) {
	name := d.inputName()

	fmt.Fprintf(b, "\n// %s is the request body for %s.\n", name, d.fullPath())
	if len(d.action.Body) == 0 {
		fmt.Fprintf(b, "//\n// The action declares no properties. The type is emitted anyway, so that\n")
		fmt.Fprintf(b, "// declaring the first one later does not change the signature of %s.\n", d.goName())
		fmt.Fprintf(b, "type %s struct{}\n", name)
		return
	}
	fmt.Fprintf(b, "//\n// A property with a default or one that may be null is a pointer, so that\n")
	fmt.Fprintf(b, "// leaving it out is distinguishable from sending its zero value.\n")
	fmt.Fprintf(b, "type %s struct {\n", name)
	for _, f := range d.action.Body {
		desc := f.Desc()
		fmt.Fprintf(b, "\t%s %s `json:\"%s%s\"%s`", GoName(desc.Name), actionBodyType(desc), desc.Name,
			omitEmpty(optionalOnCreate(desc)), enumTag(desc))
		if c := desc.Comment; c != "" {
			fmt.Fprintf(b, " // %s", c)
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintln(b, "}")
}

// actionBodyType is the Go type of one body property.
func actionBodyType(d *schema.FieldDesc) string {
	base := d.GoType()
	if strings.HasPrefix(base, "*") || strings.HasPrefix(base, "[]") {
		return base
	}
	if optionalOnCreate(d) {
		return "*" + base
	}
	return base
}

// actionBodyImports records the packages an action's body names.
//
// Overrides are deliberately not consulted. An override is keyed by table and
// column, and a body property is not a column — one that happened to share a
// name with an overridden column would silently acquire its type, which is a
// coincidence rather than a declaration.
func actionBodyImports(imports map[string]bool, defs []actionDef) {
	for _, d := range defs {
		for _, f := range d.action.Body {
			switch goType := f.Desc().GoType(); {
			case strings.Contains(goType, "time.Time"):
				imports["time"] = true
			case strings.Contains(goType, "json.RawMessage"):
				imports["encoding/json"] = true
			}
		}
	}
}

// renderActions writes the struct that carries the application's verbs.
//
// One field per declared action, each with the exact signature the envelope
// will call. An item action receives the fetched row and may mutate it; a
// collection action has no row, because it did not fetch one.
func renderActions(b *bytes.Buffer, defs []actionDef) {
	fmt.Fprintf(b, "\n// Actions carries the domain funcs the declared actions call.\n")
	fmt.Fprintf(b, "//\n// Each field is the verb of one action, and the envelope around it —\n")
	fmt.Fprintf(b, "// the id, the scoped fetch, the transaction, the write set and the\n")
	fmt.Fprintf(b, "// response — is generated. What the func does is the application's.\n")
	fmt.Fprintf(b, "//\n// A field left nil is refused when Register mounts the resource, not by\n")
	fmt.Fprintf(b, "// the request that would have called it.\n")
	fmt.Fprintln(b, "type Actions struct {")
	for i, d := range defs {
		if i > 0 {
			fmt.Fprintln(b)
		}
		fmt.Fprintf(b, "\t// %s runs POST %s.\n", d.goName(), d.fullPath())
		if desc := d.action.Description; desc != "" {
			fmt.Fprintf(b, "\t//\n\t// %s\n", desc)
		}
		if w := d.action.Writes; len(w) > 0 {
			fmt.Fprintf(b, "\t//\n\t// The envelope persists %s off this row afterwards, and nothing\n"+
				"\t// else — which bounds the envelope and not the func: the transaction\n"+
				"\t// is yours through sqlb.TxFrom, and statements issued there take\n"+
				"\t// their own locks, in an order this code owns.\n", quoteList(w))
		}
		if tt := d.action.Touches; len(tt) > 0 {
			fmt.Fprintf(b, "\t//\n\t// Declared reach beyond that row: %s. Nothing checks it; it is\n"+
				"\t// what the route tells `sqlb impact`, the OpenAPI document and the\n"+
				"\t// CLI's --help, so a change here belongs in the schema.\n", quoteList(tt))
		}
		if d.action.IsCollection() {
			fmt.Fprintf(b, "\t%s func(context.Context, %s) error\n", d.goName(), d.inputName())
			continue
		}
		fmt.Fprintf(b, "\t%s func(context.Context, *%s, %s) error\n",
			d.goName(), TypeName(d.table), d.inputName())
	}
	fmt.Fprintln(b, "}")
}

// renderActionCalls writes the registrations for one table's verbs.
func renderActionCalls(b *bytes.Buffer, optsVar string, defs []actionDef) {
	for _, d := range defs {
		typeName := TypeName(d.table)
		if d.action.IsCollection() {
			fmt.Fprintf(b, "\tif err := rest.CollectionAction[%s](api, db, %s, ", d.inputName(), optsVar)
		} else {
			fmt.Fprintf(b, "\tif err := rest.Action[%s, %s](api, db, %s, ", typeName, d.inputName(), optsVar)
		}
		fmt.Fprintf(b, "rest.ActionSpec{\n")
		fmt.Fprintf(b, "\t\tName:  %q,\n", d.action.Name)
		fmt.Fprintf(b, "\t\tPath:  %q,\n", d.fullPath())
		fmt.Fprintf(b, "\t\tField: %q,\n", d.goName())
		fmt.Fprintf(b, "\t\tSummary: %q,\n", d.summary())
		if s := d.action.Description; s != "" {
			fmt.Fprintf(b, "\t\tDescription: %q,\n", s)
		}
		if w := d.action.Writes; len(w) > 0 {
			fmt.Fprintf(b, "\t\tWrites: []string{%s},\n", quotedList(w))
		}
		if tt := d.action.Touches; len(tt) > 0 {
			fmt.Fprintf(b, "\t\tTouches: []string{%s},\n", quotedList(tt))
		}
		if len(d.action.Body) > 0 {
			fmt.Fprintf(b, "\t\tHasBody: true,\n")
		}
		fmt.Fprintf(b, "\t}, actions.%s); err != nil {\n\t\treturn err\n\t}\n", d.goName())
	}
}

// quotedList renders a name set as Go source: "status", "closed_at".
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return strings.Join(quoted, ", ")
}

// quoteList renders a column set for a doc comment: `status` and `closed_at`.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}
