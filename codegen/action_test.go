package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/schema"
)

// actionFixture is a table with the three shapes an action comes in: an item
// verb with a body and a write set, an item verb with neither, and a
// collection verb.
//
// It is one fixture rather than three because the interesting failures are
// between them — Register takes the Actions parameter if *any* table declares
// a verb, the options variable is shared by the resource and all of its
// actions, and the input types have to coexist in one file.
func actionFixture() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("tasks",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("title").Sortable(),
		schema.Enum("status", "open", "done", "archived").Default(schema.Value("open")).Filterable(),
		schema.Timestamp("closed_at").Nullable(),
	).Expose(schema.REST{Ops: schema.CRUD | schema.OpList}).
		AddAction(schema.Action{
			Name: "complete",
			Body: schema.Body(
				schema.Text("note").Nullable(),
				schema.Timestamp("completed_at"),
			),
			Writes: []string{"status", "closed_at"},
			// The verb also writes a comment row through the transaction,
			// which the write set above cannot say and used to leave unsaid.
			Touches: []string{"comments"},
		}).
		AddAction(schema.Action{
			Name:   "archive",
			Writes: []string{"status"},
		}).
		AddAction(schema.Action{
			Name:    "purge-archived",
			Path:    "/purge-archived",
			Touches: []string{"tasks", "comments"},
		})
	return r
}

func TestActionsEmitTheirInputTypes(t *testing.T) {
	files := generate(t, actionFixture())
	src := files["rest_gen.go"]

	// Squashed, because gofmt aligns struct fields and the column widths are
	// not what this is about.
	got := squash(src)
	for _, want := range []string{
		// The verb and the type it acts on, in that order.
		"type CompleteTaskInput struct {",
		// Nullable, so a pointer; and the JSON name is the declared one.
		"Note *string `json:\"note,omitempty\"`",
		// Required, so not a pointer.
		"CompletedAt time.Time `json:\"completed_at\"`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rest_gen.go does not contain %q\n%s", want, src)
		}
	}
}

// An action that declares no body still gets a type. The signature of the func
// the application writes is then the same one it will have after the first
// property is added, which is the difference between "add a field" and "change
// every call site".
func TestABodylessActionStillEmitsAnInputType(t *testing.T) {
	files := generate(t, actionFixture())
	src := files["rest_gen.go"]

	if !strings.Contains(src, "type ArchiveTaskInput struct{}") {
		t.Errorf("rest_gen.go has no empty input type for the bodyless action:\n%s", src)
	}
	// And the spec says so, which is what stops Huma requiring a request body
	// on a verb that carries nothing.
	if strings.Contains(specOf(t, src, "archive"), "HasBody") {
		t.Error("the bodyless action declares HasBody")
	}
	if !strings.Contains(specOf(t, src, "complete"), "HasBody: true") {
		t.Error("the action with a body does not declare HasBody")
	}
}

// The Actions struct is the compiler's half of ADR-0043: the field names and
// signatures are what make an action added to the schema a build failure at
// the call site rather than a route nobody wired.
func TestActionsStructNamesEveryVerbWithItsSignature(t *testing.T) {
	files := generate(t, actionFixture())
	src := files["rest_gen.go"]

	for _, want := range []string{
		"type Actions struct {",
		"CompleteTask func(context.Context, *Task, CompleteTaskInput) error",
		"ArchiveTask func(context.Context, *Task, ArchiveTaskInput) error",
		// A collection action fetched no row, so it is handed none.
		"PurgeArchivedTask func(context.Context, PurgeArchivedTaskInput) error",
		// And Register asks for the struct.
		"func Register(api huma.API, db sqlb.Executor, actions Actions) error {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rest_gen.go does not contain %q\n%s", want, src)
		}
	}
}

// The declared reach reaches the runtime spec and the doc comment above the
// func the application writes.
//
// Both matter and for different readers: the spec is what puts the sentence in
// the OpenAPI document, and the comment is what the author of the verb sees at
// the moment they are deciding whether to reach for sqlb.TxFrom (#149).
func TestTheDeclaredReachReachesTheGeneratedCode(t *testing.T) {
	src := generate(t, actionFixture())["rest_gen.go"]

	if !strings.Contains(specOf(t, src, "complete"), `Touches: []string{"comments"}`) {
		t.Errorf("the action's spec does not carry its reach:\n%s", specOf(t, src, "complete"))
	}
	// A verb with no declared reach emits no field, so a schema that never uses
	// this generates what it generated before.
	if strings.Contains(specOf(t, src, "archive"), "Touches") {
		t.Error("a verb with no declared reach emitted a Touches field")
	}

	for _, want := range []string{
		"// Declared reach beyond that row: `comments`.",
		// And the write set now says what it bounds, which is the envelope and
		// not the transaction the func is handed.
		"the transaction",
		"is yours through sqlb.TxFrom",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the Actions doc comment is missing %q:\n%s", want, src)
		}
	}
}

// A schema with no verbs must generate exactly what it generated before this
// feature existed. The parameter is additive; the absence of it is the promise.
func TestRegisterKeepsItsSignatureWhenNothingDeclaresAnAction(t *testing.T) {
	files := generate(t, restFixture())
	src := files["rest_gen.go"]

	if !strings.Contains(src, "func Register(api huma.API, db sqlb.Executor) error {") {
		t.Errorf("Register grew a parameter for a schema with no actions:\n%s", src)
	}
	if strings.Contains(src, "type Actions struct") {
		t.Error("an Actions struct was emitted for a schema with no actions")
	}
}

// The resource and its verbs must not be able to disagree about the path, the
// tag or the transaction policy, so they read one options value.
func TestAResourceWithActionsSharesOneOptionsValue(t *testing.T) {
	files := generate(t, actionFixture())
	src := files["rest_gen.go"]

	for _, want := range []string{
		"tasksOptions := rest.Options{",
		"rest.Resource[Task, TaskCreate, TaskPatch](api, db, tasksOptions)",
		"rest.Action[Task, CompleteTaskInput](api, db, tasksOptions,",
		"rest.CollectionAction[PurgeArchivedTaskInput](api, db, tasksOptions,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rest_gen.go does not contain %q\n%s", want, src)
		}
	}
}

// Writes reaches the generated spec verbatim. It is the one part of an action
// the envelope enforces rather than documents, so a dropped column here is a
// route that answers 200 having written nothing.
func TestTheWriteSetReachesTheSpec(t *testing.T) {
	files := generate(t, actionFixture())
	spec := specOf(t, files["rest_gen.go"], "complete")

	if !strings.Contains(spec, `Writes: []string{"status", "closed_at"}`) {
		t.Errorf("the write set is not in the spec:\n%s", spec)
	}
	if !strings.Contains(spec, `Path: "/tasks/{id}/complete"`) {
		t.Errorf("the route is not in the spec:\n%s", spec)
	}
	// The field name is carried so that a nil func can be reported as the
	// thing the author has to go and set.
	if !strings.Contains(spec, `Field: "CompleteTask"`) {
		t.Errorf("the Actions field name is not in the spec:\n%s", spec)
	}
}

// specOf returns the rest.ActionSpec literal for one verb, squashed: gofmt
// aligns the keys of a struct literal, and how wide the column is depends on
// which other fields the action happened to declare.
func specOf(t *testing.T, src, name string) string {
	t.Helper()
	src = squash(src)
	marker := `Name: "` + name + `"`
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("no spec for action %q in:\n%s", name, src)
	}
	rest := src[i:]
	end := strings.Index(rest, "}, actions.")
	if end < 0 {
		t.Fatalf("spec for action %q is not terminated:\n%s", name, rest)
	}
	return rest[:end]
}

// The whole argument for declaring the body rather than reflecting an
// application type is that the verb reaches the client emitters. These are the
// three places that has to be true.

func TestActionsReachTheTypeScriptClient(t *testing.T) {
	src := generateTS(t, actionFixture())["client.gen.ts"]

	for _, want := range []string{
		"export interface CompleteTaskInput {",
		// Nullable, so optional and null-able — the same rule a create body's
		// property follows.
		"note?: string | null;",
		"export function completeTask(request: Transport, id: string | number, body: CompleteTaskInput, signal?: AbortSignal): Promise<Task> {",
		// No body declared, so no body parameter.
		"export function archiveTask(request: Transport, id: string | number, signal?: AbortSignal): Promise<Task> {",
		// A collection verb takes no id and resolves to nothing: it is a 204.
		"export function purgeArchivedTask(request: Transport, signal?: AbortSignal): Promise<void> {",
		// The id goes through the same encoder every other item path uses.
		"itemPath('/tasks', id) + '/complete'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the TypeScript client does not contain %q\n%s", want, src)
		}
	}
}

func TestActionsReachTheDartClient(t *testing.T) {
	src := generateDart(t, actionFixture())

	for _, want := range []string{
		"class CompleteTaskInput {",
		"const CompleteTaskInput({this.note, required this.completedAt});",
		"Future<Task> completeTask(",
		"Future<void> purgeArchivedTask(",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the Dart client does not contain %q\n%s", want, src)
		}
	}
}

func TestActionsReachTheCLI(t *testing.T) {
	src := generateCLI(t, actionFixture())

	for _, want := range []string{
		"cmd.AddCommand(newTasksCompleteCommand(c))",
		`Use:   "complete <id>",`,
		// A collection verb takes no positional argument.
		`Use:   "purge-archived",`,
		// A required body property is a required flag: refusing here costs a
		// round trip less than relaying the server's 422.
		`_ = cmd.MarkFlagRequired("completed-at")`,
		// The write set is in the help, because "what does this touch" is the
		// question an operator has at the prompt. It is stated as what comes
		// back on the row rather than as the blast radius, which it is not.
		"The response row carries status, closed_at, and no other column the server changed on it.",
		// And the declared reach beside it, which is the half the write set
		// understates. --help is the surface ADR-0029 argues about hardest: a
		// caller with no compile step reads this instead of making a request.
		"Beyond that row the route writes: comments.",
		"The schema declares that set; nothing enforces it.",
		// A collection verb has no row at all, so the reach is the only thing
		// it can state.
		"Beyond that row the route writes: tasks, comments.",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the CLI does not contain %q\n%s", want, src)
		}
	}
	// An undeclared reach is not a bound. `archive` names no Touches, and its
	// help says so rather than leaving the write set to be read as the whole
	// of it — which is the misreading the issue reported.
	archiveHelp := src[strings.Index(src, "newTasksArchiveCommand(c *client.Client)"):]
	if end := strings.Index(archiveHelp, "return cmd"); end > 0 {
		archiveHelp = archiveHelp[:end]
	}
	if !strings.Contains(archiveHelp, "the absence of a claim rather") {
		t.Errorf("a verb with no declared reach should say so:\n%s", archiveHelp)
	}
	// A verb with no body sends none, rather than posting `{}` at an operation
	// that does not declare one.
	archive := src[strings.Index(src, "newTasksArchiveCommand(c *client.Client)"):]
	if i := strings.Index(archive, "Body: body"); i >= 0 && i < strings.Index(archive, "return cmd") {
		t.Errorf("the bodyless verb sends a body:\n%s", archive[:600])
	}
}
