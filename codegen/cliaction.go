package codegen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/jryannel/sqlb/schema"
)

// A declared verb on the command line (ADR-0043).
//
// `taskctl tasks complete <id> --note shipped`, next to `tasks update` and
// spelled the same way. The CLI is the surface where a missing verb is least
// forgiving: there is no escape hatch short of curl, so a generated tool that
// serves CRUD and nothing else is one an operator abandons the first time they
// have to complete a task.
//
// The body properties become flags, on the create command's rules — a required
// property is a required flag, since the command can refuse before the round
// trip rather than relaying a 422.

// cliActionCommand is the constructor name for one verb's subcommand.
func cliActionCommand(r cliResource, a schema.Action) string {
	return "new" + r.goPlural + GoName(strings.ReplaceAll(a.Name, "-", "_")) + "Command"
}

// cliActionCommands emits every declared verb of one resource.
func cliActionCommands(b *bytes.Buffer, r cliResource) {
	for _, a := range r.table.Actions() {
		cliActionCommand1(b, r, a)
	}
}

// cliActionAdds emits the AddCommand lines that hang the verbs off the
// resource's command group.
func cliActionAdds(b *bytes.Buffer, r cliResource) {
	for _, a := range r.table.Actions() {
		fmt.Fprintf(b, "\tcmd.AddCommand(%s(c))\n", cliActionCommand(r, a))
	}
}

func cliActionCommand1(b *bytes.Buffer, r cliResource, a schema.Action) {
	path := a.FullPath(r.path)
	fields := a.Body

	fmt.Fprintf(b, "\n// %s is POST %s.\n", cliActionCommand(r, a), path)
	fmt.Fprintf(b, "func %s(c *client.Client) *cobra.Command {\n", cliActionCommand(r, a))

	if len(fields) > 0 {
		fmt.Fprintln(b, "\tvar (")
		for _, f := range fields {
			fmt.Fprintf(b, "\t\t%s %s\n", cliValueVar(f.Desc()), cliFlagType(f.Desc()))
		}
		fmt.Fprintln(b, "\t)")
	}

	use := a.Name
	if !a.IsCollection() {
		use += " <id>"
	}
	fmt.Fprintln(b, "\tcmd := &cobra.Command{")
	fmt.Fprintf(b, "\t\tUse:   %q,\n", use)
	fmt.Fprintf(b, "\t\tShort: %q,\n", actionSummary(a, r.table.LocalName()))
	fmt.Fprintf(b, "\t\tLong:  %s,\n", goRawString(cliActionLong(r, a)))
	fmt.Fprintf(b, "\t\tExample: %s,\n", goRawString(cliActionExample(r, a)))
	if a.IsCollection() {
		fmt.Fprintln(b, "\t\tArgs:  cobra.NoArgs,")
		fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, _ []string) error {")
	} else {
		fmt.Fprintln(b, "\t\tArgs:  cobra.ExactArgs(1),")
		fmt.Fprintln(b, "\t\tRunE: func(cmd *cobra.Command, args []string) error {")
	}

	// A verb that declares no body sends none. The operation does not read one
	// — that is what ActionSpec.HasBody decides — and posting `{}` at it would
	// be a request shape the document does not describe.
	body := ""
	if len(fields) > 0 {
		fmt.Fprintln(b, "\t\t\tbody := map[string]any{}")
		for _, f := range fields {
			cliBodyAssignment(b, f.Desc(), r.wire)
		}
		body = ", Body: body"
	}

	// A collection verb answers 204, so there is nothing to print; an item verb
	// answers with the row, the same as create and update do.
	switch {
	case a.IsCollection():
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodPost, Path: %q%s}, false)\n", path, body)
	default:
		_, after, _ := strings.Cut(a.Path, "{id}")
		fmt.Fprintf(b, "\t\t\treturn runRequest(c, cmd, client.Request{Method: http.MethodPost, Path: client.ItemPath(%q, args[0]) + %q%s}, false)\n",
			r.path, after, body)
	}
	fmt.Fprintln(b, "\t\t},\n\t}")

	if len(fields) > 0 {
		fmt.Fprintln(b, "\tflags := cmd.Flags()")
		for _, f := range fields {
			d := f.Desc()
			fmt.Fprintf(b, "\tflags.%sVar(&%s, %q, %s,\n\t\t%s)\n",
				cliFlagKind(d), cliValueVar(d), cliFlagName(d.Name), cliFlagZero(d),
				goRawString(cliActionUsage(d)))
			if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
				fmt.Fprintf(b, "\tregisterCompletion(cmd, %q, %s)\n",
					cliFlagName(d.Name), goSliceLiteral(d.EnumValues))
			}
			// A property the action declares as required is a required flag:
			// refusing here costs a round trip less than relaying the 422.
			if !optionalOnCreate(d) {
				fmt.Fprintf(b, "\t_ = cmd.MarkFlagRequired(%q)\n", cliFlagName(d.Name))
			}
		}
	}
	fmt.Fprintln(b, "\treturn cmd\n}")
}

func cliActionLong(r cliResource, a schema.Action) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s\n\n", a.FullPath(r.path))
	if a.Description != "" {
		b.WriteString(a.Description + "\n\n")
	}
	// What the caller gets back, which is the one thing about a verb that
	// --help has to be right about: a declared result replaces the default
	// answer, so a reader told "the row as it now stands" would be looking for
	// columns in an object that has none of them (#312).
	answer := ""
	if len(a.Returns) > 0 {
		answer = "It answers with " + strings.Join(returnNames(a), ", ") + "."
	}
	if a.IsCollection() {
		b.WriteString("A verb on the collection: it addresses no single row")
		if answer == "" {
			b.WriteString(", and a successful call\nwrites nothing to print.")
		} else {
			b.WriteString(".\n" + answer)
		}
		writeCLIReach(&b, a)
		return b.String()
	}
	b.WriteString("A verb on one row. The server fetches it and runs the transition.\n")
	if answer == "" {
		b.WriteString("It answers with the row as it now stands.")
		if len(a.Writes) > 0 {
			fmt.Fprintf(&b, "\n\nThe response row carries %s, and no other column the server changed on it.",
				strings.Join(a.Writes, ", "))
		}
	} else {
		b.WriteString(answer + " The row is written and not returned")
		if len(a.Writes) > 0 {
			fmt.Fprintf(&b, ";\nwhat is written to it is %s", strings.Join(a.Writes, ", "))
		}
		b.WriteString(".")
	}
	writeCLIReach(&b, a)
	return b.String()
}

// returnNames is the declared result's properties, for a sentence.
func returnNames(a schema.Action) []string {
	out := make([]string, 0, len(a.Returns))
	for _, f := range a.Returns {
		out = append(out, f.Desc().Name)
	}
	return out
}

// writeCLIReach states what the verb writes beyond the row it answers with.
//
// This is the surface ADR-0029's argument is sharpest about: --help is what a
// caller with no compile step reads instead of a request, and a caller reading
// a write set of two columns concludes the verb is confined to one row. The
// sentence goes in whether or not the schema declared a reach, because the
// absence of a declaration is not the absence of a reach — an undeclared verb
// still holds the transaction.
func writeCLIReach(b *strings.Builder, a schema.Action) {
	if len(a.Touches) > 0 {
		fmt.Fprintf(b, "\n\nBeyond that row the route writes: %s.\nThe schema declares that set; nothing enforces it.",
			strings.Join(a.Touches, ", "))
		return
	}
	b.WriteString("\n\nThe route declares nothing written beyond that row. A verb holds the\ntransaction and may write more, so this is the absence of a claim rather\nthan a checked bound.")
}

// cliActionExample writes one runnable invocation, filling every required flag.
func cliActionExample(r cliResource, a schema.Action) string {
	var args []string
	if !a.IsCollection() {
		args = append(args, cliIDPlaceholder(r))
	}
	for _, f := range a.Body {
		d := f.Desc()
		if optionalOnCreate(d) {
			continue
		}
		args = append(args, "--"+cliFlagName(d.Name)+" "+cliValueExample(d))
	}
	return "  " + r.line(strings.TrimSpace(a.Name+" "+strings.Join(args, " ")))
}

// cliActionUsage is one body property's flag help.
func cliActionUsage(d *schema.FieldDesc) string {
	usage := d.Comment
	if usage == "" {
		usage = "The " + d.Name + " property of the request body."
	}
	if d.Type == schema.TypeEnum && len(d.EnumValues) > 0 {
		usage += " One of: " + strings.Join(d.EnumValues, ", ") + "."
	}
	if optionalOnCreate(d) {
		usage += " Optional."
	}
	return usage
}
