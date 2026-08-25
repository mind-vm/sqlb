package codegen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// The value set that could not be declared at all: dotted values are the
// ordinary spelling of an event kind, and title-casing one whole produced
// `NotificationTypeTask.assigned`, which does not parse (issue #138).
func notifications() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("notifications",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("type",
			"member.welcome",
			"task.assigned",
			"task.completed",
			"project.update",
			"invitation",
			"system",
			"document.shared",
		).Filterable(),
	).Expose(schema.REST{Ops: schema.OpList})
	return r
}

func TestEnumConst(t *testing.T) {
	for _, c := range []struct{ value, want string }{
		{"draft", "Draft"},
		{"task.assigned", "TaskAssigned"},
		{"member.welcome", "MemberWelcome"},
		{"image/png", "ImagePng"},
		{"content-type", "ContentType"},
		{"in progress", "InProgress"},
		{"already_snake", "AlreadySnake"},
		// The initialism table is GoName's and reaches here unchanged, which
		// is the point of deriving this from GoName rather than beside it.
		{"api.key", "APIKey"},
		// A leading digit needs no escape: the type name is always in front.
		{"2fa.enabled", "2faEnabled"},
		// A value with no word in it would otherwise name the type itself.
		{"", "Empty"},
		{"...", "Empty"},
	} {
		if got := codegen.EnumConst(c.value); got != c.want {
			t.Errorf("EnumConst(%q) = %q, want %q", c.value, got, c.want)
		}
	}
}

// Generate parses everything it writes, so a name that is not an identifier
// fails here even without an assertion naming it. The assertions say which
// name, so a regression reads as a naming change rather than a parse error.
func TestModelsEnumConstsAreIdentifiers(t *testing.T) {
	out := generate(t, notifications())
	models := out["models_gen.go"]
	for _, want := range []string{
		`NotificationTypeMemberWelcome NotificationType = "member.welcome"`,
		`NotificationTypeTaskAssigned NotificationType = "task.assigned"`,
		`NotificationTypeTaskCompleted NotificationType = "task.completed"`,
		`NotificationTypeProjectUpdate NotificationType = "project.update"`,
		`NotificationTypeInvitation NotificationType = "invitation"`,
		`NotificationTypeSystem NotificationType = "system"`,
		`NotificationTypeDocumentShared NotificationType = "document.shared"`,
	} {
		if !contains(models, want) {
			t.Errorf("models_gen.go missing %s", want)
		}
	}
}

// The TypeScript union is the artefact the port actually wanted, and it needs
// no identifier — so it was already correct, and the point of asserting it
// here is that the two emitters now agree about which value sets exist.
func TestClientsCarryDottedEnumValues(t *testing.T) {
	dir := t.TempDir()
	files, err := codegen.Generate(codegen.Options{
		Registry: notifications(), Dir: dir, Package: "gen",
		TSDir: "web/api", DartDir: "mobile/lib/api",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = string(b)
	}

	ts := out["client.gen.ts"]
	if !contains(ts, "export type NotificationType =") {
		t.Errorf("TypeScript client declares no NotificationType")
	}
	for _, want := range []string{"'member.welcome'", "'task.assigned'", "'document.shared'"} {
		if !contains(ts, want) {
			t.Errorf("TypeScript union missing %s", want)
		}
	}
	// Dart names its members, and derived them the same broken way.
	for _, want := range []string{
		"memberWelcome('member.welcome')",
		"taskAssigned('task.assigned')",
		"documentShared('document.shared')",
	} {
		if !contains(out["client.gen.dart"], want) {
			t.Errorf("Dart client missing %s", want)
		}
	}
}

// Two values that spell one name is refused with both named. Emitting it would
// be a duplicate-const compile error in the consumer's package, with nothing in
// it saying which two values collided.
func TestEnumConstCollisionRefused(t *testing.T) {
	r := schema.NewRegistry()
	r.Table("notifications",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Enum("type", "task.assigned", "task_assigned"),
	).Expose(schema.REST{Ops: schema.OpList})

	_, err := codegen.Generate(codegen.Options{Registry: r, Dir: t.TempDir(), Package: "gen"})
	if err == nil {
		t.Fatal("expected a collision error, generated cleanly")
	}
	for _, want := range []string{"task.assigned", "task_assigned", "TaskAssigned", "type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}
