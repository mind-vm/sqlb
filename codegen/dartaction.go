package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/mind-vm/sqlb/schema"
)

// A declared verb in the Dart client (ADR-0043).
//
// Same argument as the TypeScript side, with one difference that matters on a
// phone: Dart's named parameters mean an action's body class can grow an
// optional property without moving any call site, so the input class is emitted
// only when there is something to put in it.

// dartActionBase is the stem of every name one verb generates: CompleteTask.
func dartActionBase(base string, a schema.Action) string {
	return GoName(strings.ReplaceAll(a.Name, "-", "_")) + base
}

// dartActionInput is the request body class.
func dartActionInput(base string, a schema.Action) string {
	return dartActionBase(base, a) + "Input"
}

// dartActionResult is the response class of a verb that declares one.
func dartActionResult(base string, a schema.Action) string {
	return dartActionBase(base, a) + "Result"
}

// dartActionMethod is the transport function: completeTask, beside createTask.
func dartActionMethod(base string, a schema.Action) string {
	return lowerFirstRune(dartActionBase(base, a))
}

// dartActionBodies emits one body class per verb that declares properties.
func dartActionBodies(b *bytes.Buffer, t *schema.TableDef, base string) {
	if t.Rest() == nil {
		return
	}
	for _, a := range t.Actions() {
		if len(a.Body) == 0 {
			continue
		}
		name := dartActionInput(base, a)
		fmt.Fprintln(b)
		dartDoc(b, "", fmt.Sprintf("The request body for POST %s.", a.FullPath(t.Rest().Path)))
		fmt.Fprintf(b, "class %s {\n", name)
		dartDoc(b, "  ", "Builds a request body. A property with no default here is one the\naction declares as required.")

		var params []string
		for _, f := range a.Body {
			d := dartDeclared(f)
			required := ""
			if !optionalOnCreate(d) {
				required = "required "
			}
			params = append(params, required+"this."+dartMember(d.Name))
		}
		dartNamedCtor(b, name, params)

		for _, f := range a.Body {
			d := dartDeclared(f)
			fmt.Fprintln(b)
			dartDoc(b, "  ", dartColumnDoc(d, fmt.Sprintf("The %s property of the request body.", d.Name)))
			fmt.Fprintf(b, "  final %s %s;\n", dartBodyType(base, d, true), dartMember(d.Name))
		}

		fmt.Fprintln(b)
		dartDoc(b, "  ", "The JSON body. Absent properties are the ones left unset.")
		var entries []string
		for _, f := range a.Body {
			d := f.Desc()
			member := dartMember(d.Name)
			entry := fmt.Sprintf("%s: _wire(%s)", dartString(d.Name), member)
			if optionalOnCreate(d) {
				entry = fmt.Sprintf("if (%s != null) %s", member, entry)
			}
			entries = append(entries, entry)
		}
		dartMapBody(b, "Map<String, dynamic> toJson()", entries)
		fmt.Fprintln(b, "}")
	}
}

// dartDeclared is a declared property as the Dart emitters should see it: with
// its enum values dropped.
//
// The reason is the one tsDeclaredType gives. An enum *column* is emitted as a
// named Dart enum beside its resource, and dartElemType names that type; a
// declared property is not a column, so the name is one nothing declares and
// the emitted client does not analyse. The Go emitter has always typed such a
// property as a plain string with the value set in a tag.
//
// Dart gets the string and TypeScript gets an inline union, which is the same
// asymmetry the two clients already carry elsewhere: a union is a type there
// and would be a generated enum here, and a generated enum needs a name, a
// place and a collision rule that a one-use property does not earn. The value
// set is enforced by the server and documented in the OpenAPI schema either
// way.
func dartDeclared(f *schema.Field) *schema.FieldDesc {
	d := *f.Desc()
	d.EnumValues = nil
	return &d
}

// dartActionResults emits one response class per verb that declares a result.
//
// It extends Row, which is where the typed readers live, so a declared property
// is read the way a column is: decoded on access, and absent-versus-null is a
// distinction the reader keeps. That is more machinery than a plain data class,
// and it is machinery this file would otherwise have to repeat.
//
// An enum property is read as a plain String — see dartDeclared.
func dartActionResults(b *bytes.Buffer, t *schema.TableDef, base string) {
	if t.Rest() == nil {
		return
	}
	for _, a := range t.Actions() {
		if len(a.Returns) == 0 {
			continue
		}
		name := dartActionResult(base, a)
		fmt.Fprintln(b)
		dartDoc(b, "", fmt.Sprintf("The response body of POST %s.", a.FullPath(t.Rest().Path)))
		dartDoc(b, "", "")
		dartDoc(b, "", "The verb answers with this rather than with a row of "+t.Name()+".")
		fmt.Fprintf(b, "class %s extends Row {\n", name)
		dartDoc(b, "  ", "Wraps one decoded response object. Properties are read on access.")
		fmt.Fprintf(b, "  %s.fromJson(super.json) : super.fromJson();\n", name)
		for _, f := range a.Returns {
			d := dartDeclared(f)
			fmt.Fprintln(b)
			dartDoc(b, "  ", dartColumnDoc(d, fmt.Sprintf("The %s property of the response.", d.Name)))
			fmt.Fprintf(b, "  %s\n", dartGetter(base, dartMember(d.Name), d, schema.Verbatim))
		}
		fmt.Fprintln(b, "}")
	}
}

// dartActionMethods emits the transport function for each declared verb.
func dartActionMethods(b *bytes.Buffer, r dartResource) {
	for _, a := range r.table.Actions() {
		summary := a.Summary
		if summary == "" {
			summary = actionSummary(a, r.table.LocalName())
		}
		fmt.Fprintln(b)
		dartDoc(b, "", fmt.Sprintf("POST %s — %s.", a.FullPath(r.path),
			strings.ToLower(summary[:1])+summary[1:]))

		method := dartActionMethod(r.base, a)
		hasBody := len(a.Body) > 0

		// A declared result is the response, whichever form the verb takes.
		// Without one a collection verb answers 204, so there is nothing to
		// decode, and an item verb answers with the row the envelope left.
		returns := ""
		switch {
		case len(a.Returns) > 0:
			returns = dartActionResult(r.base, a)
		case !a.IsCollection():
			returns = r.row
		}
		if returns == "" {
			fmt.Fprintf(b, "Future<void> %s(\n", method)
		} else {
			fmt.Fprintf(b, "Future<%s> %s(\n", returns, method)
		}
		// The brace opening the named-parameter list goes on whichever
		// positional parameter turns out to be last, and a collection verb with
		// no body has none — so it opens on the transport itself.
		switch {
		case hasBody && !a.IsCollection():
			fmt.Fprintln(b, "  Transport request,")
			fmt.Fprintln(b, "  Object id,")
			fmt.Fprintf(b, "  %s body, {\n", dartActionInput(r.base, a))
		case hasBody:
			fmt.Fprintln(b, "  Transport request,")
			fmt.Fprintf(b, "  %s body, {\n", dartActionInput(r.base, a))
		case !a.IsCollection():
			fmt.Fprintln(b, "  Transport request,")
			fmt.Fprintln(b, "  Object id, {")
		default:
			fmt.Fprintln(b, "  Transport request, {")
		}
		fmt.Fprintln(b, "  Object? cancel,")
		fmt.Fprintln(b, "}) async {")

		if a.IsCollection() {
			fmt.Fprintf(b, "  const path = %s;\n", dartString(a.FullPath(r.path)))
		} else {
			// Through a local rather than concatenated inline: Dart's
			// prefer_interpolation_to_compose_strings reports the `+` form, and
			// `dart analyze --fatal-infos` is a gate this example runs.
			_, after, _ := strings.Cut(a.Path, "{id}")
			fmt.Fprintf(b, "  final item = _itemPath(%s, id);\n", dartString(r.path))
			if after == "" {
				fmt.Fprintln(b, "  final path = item;")
			} else {
				fmt.Fprintf(b, "  final path = '$item%s';\n", after)
			}
		}

		payload := "const <String, dynamic>{}"
		if hasBody {
			payload = "body.toJson()"
		}
		if returns == "" {
			fmt.Fprintf(b, "  await request(_post(path, %s, cancel));\n}\n", payload)
			continue
		}
		fmt.Fprintf(b, "  final json = await request(_post(path, %s, cancel));\n", payload)
		fmt.Fprintf(b, "  return _row(json, %s.fromJson);\n}\n", returns)
	}
}
