package codegen

import (
	"bytes"
	"fmt"
	"strings"
)

// docLines renders text as the body of a Go doc comment: every line of it
// prefixed, not just the first.
//
// A declared Description or Comment is prose written by whoever wrote the
// schema, and prose comes in paragraphs. Interpolating it into a `// %s`
// wrote the second paragraph as bare source, so the generated package did not
// compile and the diagnostic named a line in a file nobody had written (#326).
// Multi-paragraph is the ordinary shape for an operation description — OpenAPI
// renders the blank line as a paragraph break — so this is the first thing a
// hand-written route brings with it when it becomes a declared one.
//
// indent is what precedes each `//`, so a struct field's comment passes "\t"
// and a package-level one passes "". A blank line becomes a bare `//`, which is
// what makes the paragraph break survive into the rendered doc.
func docLines(b *bytes.Buffer, indent, text string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			fmt.Fprintf(b, "%s//\n", indent)
			continue
		}
		fmt.Fprintf(b, "%s// %s\n", indent, line)
	}
}

// oneLine flattens whitespace, for the places that have room for one line and
// no more: a YAML frontmatter value, where two lines are a different document
// than the one intended, and the trailing `// …` after a struct field, where a
// newline ends the comment and leaves the rest as source.
//
// Flattened rather than moved above the field, because the field comments this
// serves are the short ones a column carries, and a two-line column comment
// pushed above its field would reflow the struct around a rarity. The doc
// comments that are meant to be prose — an action's Description, a table's —
// go through docLines and keep their shape.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
