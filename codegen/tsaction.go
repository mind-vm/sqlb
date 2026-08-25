package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// A declared verb in the TypeScript client (ADR-0043).
//
// This is where most of the value of an action actually lands. A verb the
// server generates but the client does not is still four hand-written things —
// the fetch call, its argument type, the response type, and the knowledge that
// the route exists at all — and the adoption review measured the drift living
// in exactly those. So the emitters read the declaration and the verb arrives
// typed, next to the CRUD functions, spelled the same way.

// tsActionName is the client function: the verb, then the type. "complete" on
// tasks gives completeTask, beside createTask and updateTask.
func tsActionName(t *schema.TableDef, a schema.Action) string {
	verb := GoName(strings.ReplaceAll(a.Name, "-", "_"))
	return lowerFirstRune(verb + TypeName(t))
}

// tsActionProp is the verb's spelling on the mutations object: "mark-done"
// becomes markDone, beside create, update and delete. The type is not repeated
// because the object is already the resource's.
func tsActionProp(a schema.Action) string {
	return lowerFirstRune(GoName(strings.ReplaceAll(a.Name, "-", "_")))
}

// tsActionInput is the request body interface, named for the function that
// takes it.
func tsActionInput(t *schema.TableDef, a schema.Action) string {
	verb := GoName(strings.ReplaceAll(a.Name, "-", "_"))
	return verb + TypeName(t) + "Input"
}

// tsActionResult is the response interface of a verb that declares one.
func tsActionResult(t *schema.TableDef, a schema.Action) string {
	verb := GoName(strings.ReplaceAll(a.Name, "-", "_"))
	return verb + TypeName(t) + "Result"
}

// tsDeclaredType is the TypeScript type of a *declared* property — an action's
// body, an action's result, or the non-column half of a create body.
//
// It differs from tsType in one place, and the difference is a compile error
// rather than a nicety. An enum *column* is emitted as a named union beside its
// resource, and tsType names that type; a declared property is not a column, so
// there is no such type and the name is one nothing declares. The Go emitter
// has always known this — renderActionInput types an enum property as a plain
// string carrying Huma's enum tag, and says why — and the two client emitters
// did not, so a schema with an enum in a declared body emitted a client that
// did not compile.
//
// Inline rather than a generated union type per property: the value set is the
// property's own, one use site, and a union written where it is used needs no
// name and cannot collide with a column's.
func tsDeclaredType(typeName string, d *schema.FieldDesc) string {
	if d.Type != schema.TypeEnum || len(d.EnumValues) == 0 {
		return tsType(typeName, d)
	}
	parts := make([]string, 0, len(d.EnumValues))
	for _, v := range d.EnumValues {
		parts = append(parts, tsString(v))
	}
	out := strings.Join(parts, " | ")
	if d.Array {
		out = "(" + out + ")[]"
	}
	if d.Nullable {
		out += " | null"
	}
	return out
}

func lowerFirstRune(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	// A leading initialism lowers whole, or ID becomes iD — the same rule
	// unexportedGoName applies on the Go side.
	lead := 0
	for lead < len(r) && r[lead] >= 'A' && r[lead] <= 'Z' {
		lead++
	}
	if lead > 1 && lead < len(r) {
		lead--
	}
	if lead == 0 {
		lead = 1
	}
	for i := 0; i < lead && i < len(r); i++ {
		r[i] += 'a' - 'A'
	}
	return string(r)
}

// tsActionBodies emits one interface per verb that declares a body.
//
// A verb with no body gets no interface, unlike the Go side: there is no
// signature to keep stable, because a TypeScript function that grows an
// optional parameter is not a breaking change to its callers.
func tsActionBodies(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	for _, a := range t.Actions() {
		if len(a.Body) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n/** The request body for `POST %s`. */\n", a.FullPath(t.Rest().Path))
		fmt.Fprintf(b, "export interface %s {\n", tsActionInput(t, a))
		for _, f := range a.Body {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s%s: %s;\n", tsProp(d.Name), tsOptional(optionalOnCreate(d)), tsDeclaredType(typeName, d))
		}
		fmt.Fprintln(b, "}")
	}
}

// tsActionResults emits one interface per verb that declares a result.
//
// A verb that declares none gets none: its function returns the row or void,
// both of which are types the client already has (#312).
func tsActionResults(b *bytes.Buffer, t *schema.TableDef, typeName string) {
	for _, a := range t.Actions() {
		if len(a.Returns) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n/** The response body of `POST %s`. */\n", a.FullPath(t.Rest().Path))
		fmt.Fprintf(b, "export interface %s {\n", tsActionResult(t, a))
		for _, f := range a.Returns {
			d := f.Desc()
			tsDoc(b, "  ", d.Comment)
			fmt.Fprintf(b, "  %s%s: %s;\n", tsProp(d.Name), tsOptional(optionalOnCreate(d)), tsDeclaredType(typeName, d))
		}
		fmt.Fprintln(b, "}")
	}
}

// tsActionFunctions emits the transport function for each declared verb.
func tsActionFunctions(b *bytes.Buffer, r tsResource) {
	for _, a := range r.table.Actions() {
		path := a.FullPath(r.path)
		fn := tsActionName(r.table, a)

		var params []string
		params = append(params, "request: Transport")
		if !a.IsCollection() {
			params = append(params, "id: string | number")
		}
		if len(a.Body) > 0 {
			params = append(params, "body: "+tsActionInput(r.table, a))
		}
		params = append(params, "signal?: AbortSignal")

		// A declared result is the response, whichever form the verb takes.
		// Without one a collection verb answers 204, so there is nothing to
		// type, and an item verb answers with the row it left behind.
		result := "void"
		switch {
		case len(a.Returns) > 0:
			result = tsActionResult(r.table, a)
		case !a.IsCollection():
			result = r.typeName
		}

		summary := a.Summary
		if summary == "" {
			summary = actionSummary(a, r.table.LocalName())
		}
		fmt.Fprintf(b, "\n/** `POST %s` — %s. */\n", path, strings.ToLower(summary[:1])+summary[1:])
		fmt.Fprintf(b, "export function %s(%s): Promise<%s> {\n", fn, strings.Join(params, ", "), result)

		fmt.Fprintf(b, "  return request({ method: 'POST', path: %s, ", tsActionPath(r.path, a))
		if len(a.Body) > 0 {
			fmt.Fprint(b, "body, ")
		}
		fmt.Fprint(b, "signal });\n}\n")
	}
}

// tsActionPath renders the route expression. An item verb interpolates the id
// through the same encoder every other item path uses, so that a key needing
// escaping is escaped once and in one place.
func tsActionPath(resource string, a schema.Action) string {
	if a.IsCollection() {
		return tsString(a.FullPath(resource))
	}
	// "/tasks/{id}/complete" becomes itemPath('/tasks', id) + '/complete'.
	_, after, _ := strings.Cut(a.Path, "{id}")
	expr := fmt.Sprintf("itemPath(%s, id)", tsString(resource))
	if after != "" {
		expr += " + " + tsString(after)
	}
	return expr
}
