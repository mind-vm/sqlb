package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// A declared description is prose, and prose has paragraphs.
//
// Every such string reaches a doc comment in at least one emitter, and a
// newline in one used to be written into the comment verbatim: the second
// paragraph landed as bare source, so `sqlb generate` refused its own output
// with a parse error naming a line in a file nobody had written (#326). It is
// the first thing a hand-written huma.Operation brings with it when it becomes
// a declared action, which is exactly the moment it fired.
//
// The guard is TestGeneratedGoCompiles, which this fixture is a case of; what
// is asserted here is the rendering, since a file that compiles could still
// have flattened the paragraphs into one run-on line.

// proseFixture puts a multi-paragraph string everywhere one is accepted.
func proseFixture() *schema.Registry {
	const twoParagraphs = "Grades the answers on the server and records the attempt.\n\n" +
		"Which option is correct is never sent to a client, so grading cannot\nhappen anywhere else."

	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
		// A column comment reaches a trailing `// …` after the struct field,
		// where there is only one line to be had.
		schema.Bool("published").Default(schema.Value(false)).
			Comment("Whether the lesson is visible.\n\nDrafts are visible to their author only."),
	).Describe("stores one lesson.\n\nA lesson is the unit a learner completes.").
		Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddAction(schema.Action{
			Name:        "submit-quiz",
			Summary:     "Submit quiz answers",
			Description: twoParagraphs,
			Body:        schema.Body(schema.JSON("answers").Comment("The answers.\n\nOne entry per question.")),
			Writes:      []string{"published"},
		}).
		AddQuery(schema.Query{
			Name:        "recent",
			Description: twoParagraphs,
			Params:      schema.Body(schema.Timestamp("since").Comment("Lower bound.\n\nExclusive.")),
		})
	return r
}

func TestDeclaredProseKeepsItsParagraphsInTheDocComment(t *testing.T) {
	src := generate(t, proseFixture())["rest_gen.go"]

	// The paragraph break survives as a bare `//`, indented with the field it
	// documents. Rendering it as `// ` with a trailing space would be a
	// gofmt-unstable file, and dropping it would join two paragraphs.
	for _, want := range []string{
		"\t// Grades the answers on the server and records the attempt.\n\t//\n\t// Which option is correct",
		"\t// happen anywhere else.\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the declared description did not keep its shape; want %q in:\n%s", want, src)
		}
	}
	// And none of it reached the file as source. That is the failure itself:
	// a continuation line emitted with no `//` is a sentence the compiler
	// tries to parse. Checked per line over the part before the comment
	// marker, so a trailing `// …` after a struct field still counts as a
	// comment. The lines carrying an escaped \n are the ActionSpec's own
	// Description string literal, which is a value and not a comment.
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, `\n`) {
			continue
		}
		code := line
		if i := strings.Index(code, "//"); i >= 0 {
			code = code[:i]
		}
		for _, prose := range []string{"happen anywhere else", "One entry per question", "Exclusive"} {
			if strings.Contains(code, prose) {
				t.Errorf("a declared string reached the file as source rather than as a comment: %q", line)
			}
		}
	}
}

// The trailing comment after a struct field is the one place with a single
// line to give: a newline there ends the comment and leaves the remainder as
// source. It is flattened rather than dropped, so the sentence is still there.
func TestAColumnCommentIsFlattenedOntoItsField(t *testing.T) {
	src := generate(t, proseFixture())["models_gen.go"]

	want := "// Whether the lesson is visible. Drafts are visible to their author only."
	if !strings.Contains(src, want) {
		t.Errorf("a multi-line column comment should flatten onto its field; want %q in:\n%s", want, src)
	}
}
