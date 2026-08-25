package codegen_test

import (
	"strings"
	"testing"

	"github.com/jryannel/sqlb/schema"
)

// A verb that answers with something that is not a row (#312), through every
// emitter that has to type the answer.
//
// Two verbs, because the two forms had different defaults to displace: an item
// action answered with the row, and a collection action answered 204.
func actionResultFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Filterable().Sortable(),
		schema.Int("attempts").Default(schema.Value("0")),
	).Expose(schema.REST{Path: "/lessons", Ops: schema.CRUD | schema.OpList}).
		AddAction(schema.Action{
			Name:    "submit-quiz",
			Body:    schema.Body(schema.JSON("answers")),
			Returns: schema.Result(schema.Int("score"), schema.Int("total"), schema.Text("grade").Nullable()),
			Writes:  []string{"attempts"},
		}).
		AddAction(schema.Action{
			Name:    "mark-all-read",
			Path:    "/mark-all-read",
			Returns: schema.Result(schema.Int("marked")),
		})
	return r
}

func TestActionResultReachesTheGoSurface(t *testing.T) {
	src := generateAll(t, actionResultFixture())["rest_gen.go"]

	for _, want := range []string{
		"type SubmitQuizLessonResult struct",
		"Score int32   `json:\"score\"`",
		"Grade *string `json:\"grade,omitempty\"`",
		// The signature is the contract: a verb that declares a result returns
		// one, and the application's func does not compile until it does.
		"SubmitQuizLesson func(context.Context, *Lesson, SubmitQuizLessonInput) (SubmitQuizLessonResult, error)",
		"MarkAllReadLesson func(context.Context, MarkAllReadLessonInput) (MarkAllReadLessonResult, error)",
		"rest.ActionReturning[Lesson, SubmitQuizLessonInput, SubmitQuizLessonResult]",
		"rest.CollectionActionReturning[MarkAllReadLessonInput, MarkAllReadLessonResult]",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rest_gen.go is missing %q:\n%s", want, src)
		}
	}
}

// A verb that declares none is untouched, which is what makes this additive:
// the envelope, the signature and the registration are the ones every existing
// schema already generates.
func TestWithoutAResultTheActionIsUnchanged(t *testing.T) {
	src := generateAll(t, actionFixture())["rest_gen.go"]
	for _, unwanted := range []string{"Result struct", "ActionReturning", "CollectionActionReturning"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("a schema declaring no result emitted %q:\n%s", unwanted, src)
		}
	}
}

func TestActionResultReachesTheClients(t *testing.T) {
	files := generateAll(t, actionResultFixture())

	ts := files["client.gen.ts"]
	for _, want := range []string{
		"export interface SubmitQuizLessonResult {",
		"score: number;",
		"grade?: string | null;",
		"): Promise<SubmitQuizLessonResult> {",
		"): Promise<MarkAllReadLessonResult> {",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("the TypeScript client is missing %q", want)
		}
	}

	dart := files["client.gen.dart"]
	for _, want := range []string{
		"class SubmitQuizLessonResult extends Row {",
		"int get score => _int('score');",
		"String? get grade => _strOrNull('grade');",
		"Future<SubmitQuizLessonResult> submitQuizLesson(",
		"return _row(json, SubmitQuizLessonResult.fromJson);",
		"Future<MarkAllReadLessonResult> markAllReadLesson(",
	} {
		if !strings.Contains(dart, want) {
			t.Errorf("the Dart client is missing %q", want)
		}
	}

	// The CLI prints whatever comes back, so what it has to get right is the
	// help: a caller told to expect the row would go looking for columns.
	cli := files["cli_gen.go"]
	cmd := cli[strings.LastIndex(cli, "func newLessonsSubmitQuizCommand"):]
	if !strings.Contains(cmd, "It answers with score, total, grade.") {
		t.Errorf("the CLI help does not say what the verb answers with:\n%s", cmd)
	}
	if strings.Contains(cmd, "answers with the row as it now stands") {
		t.Errorf("the CLI help still promises the row:\n%s", cmd)
	}
}

// The document an agent reads before calling the verb.
func TestActionResultReachesTheSkill(t *testing.T) {
	skill := skillOf(t, actionResultFixture())
	for _, want := range []string{
		"| Verb | Route | Answers | Writes | Also writes |",
		"`score`, `total`, `grade`",
		"`marked`",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill is missing %q:\n%s", want, skill)
		}
	}
}

// A declared property carrying enum values is a plain string with the value set
// enforced at the boundary — which is what the Go emitter has always done, and
// what the two client emitters did not: they named a union type that is emitted
// per enum *column*, so a schema with an enum in a declared body produced a
// TypeScript client that did not compile and a Dart client that did not
// analyse.
//
// It is asserted here rather than in the action tests because this is the
// change that found it: a result type would have inherited the same bug.
func TestADeclaredEnumPropertyNamesNoUndeclaredType(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("lessons",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title"),
	).Expose(schema.REST{Path: "/lessons", Ops: schema.OpRead}).
		AddAction(schema.Action{
			Name:    "grade",
			Body:    schema.Body(schema.Enum("mode", "practice", "graded")),
			Returns: schema.Result(schema.Enum("outcome", "pass", "fail")),
		})
	files := generateAll(t, r)

	ts := files["client.gen.ts"]
	for _, want := range []string{"mode: 'practice' | 'graded';", "outcome: 'pass' | 'fail';"} {
		if !strings.Contains(ts, want) {
			t.Errorf("the TypeScript client is missing %q", want)
		}
	}
	// The name tsType would have produced. Nothing declares it, so a client
	// carrying it does not compile.
	if strings.Contains(ts, "LessonMode") || strings.Contains(ts, "LessonOutcome") {
		t.Errorf("the TypeScript client names a type nothing declares:\n%s", ts)
	}

	dart := files["client.gen.dart"]
	if !strings.Contains(dart, "final String mode;") {
		t.Errorf("the Dart client does not type a declared enum property as a string")
	}
	if strings.Contains(dart, "LessonMode") || strings.Contains(dart, "LessonOutcome") {
		t.Errorf("the Dart client names a type nothing declares:\n%s", dart)
	}
}
