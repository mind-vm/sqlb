package codegen_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/schema"
)

// kanbanFixture is the collision as it was reported: a boards table and a
// board_columns table, whose singularised name is BoardColumn — which is also
// what the <Entity>Column convention calls Board's selectable-column union
// (#261).
func kanbanFixture() *schema.Registry {
	r := schema.NewRegistry()
	boards := r.Table("boards",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	r.Table("board_columns",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("board", boards).Filterable(),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})
	return r
}

// The generator refuses rather than writing a file tsc will not compile.
//
// It used to write it and report success, so the first sign of trouble was two
// "Duplicate identifier" errors naming neither the schema nor the two tables
// that produced them. Everything in the message below is here because the
// emitted file does not carry it: the identifier, what each table contributed,
// and that a table's TypeScript names come from its own name.
func TestTSCollisionIsRefused(t *testing.T) {
	_, err := codegen.Generate(codegen.Options{
		Registry: kanbanFixture(), Dir: t.TempDir(), Package: "gen", TSDir: "web/api",
	})
	if err == nil {
		t.Fatal("a schema that generates two BoardColumn declarations was accepted")
	}
	for _, want := range []string{
		"BoardColumn",
		"the row type of board_columns",
		"the selectable-column type of boards",
		"TS2300",
		"TableDef.TypeName pins a different one",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// The Dart client has the same collision from the same conventions: a class
// against an enum where TypeScript had an interface against a union. Fixed in
// the same commit because a guard on one client would leave a project that
// generates both with the same silent invalid output, one language over.
func TestDartCollisionIsRefused(t *testing.T) {
	_, err := codegen.Generate(codegen.Options{
		Registry: kanbanFixture(), Dir: t.TempDir(), Package: "gen", DartDir: "dart/api",
	})
	if err == nil {
		t.Fatal("a schema that generates two BoardColumn declarations was accepted")
	}
	for _, want := range []string{
		"BoardColumn",
		"the row type of board_columns",
		"the selectable-column type of boards",
		"one top-level namespace",
		"TableDef.TypeName pins a different one",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q:\n%v", want, err)
		}
	}
}

// And it refuses only that. A schema whose names do not collide generates, so
// the guard is not a rule against tables sharing a prefix: board_columns beside
// kanban_boards is the same pair of tables, renamed the way the message says.
func TestTSNamesThatDoNotCollideAreFine(t *testing.T) {
	r := schema.NewRegistry()
	boards := r.Table("kanban_boards",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	r.Table("board_columns",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("board", boards).Filterable(),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	files := generateTS(t, r)
	client := files["client.gen.ts"]
	for _, want := range []string{
		"export interface BoardColumn {",
		"export type KanbanBoardColumn =",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("the client is missing %q", want)
		}
	}
}

// #262: the collision is real, but renaming board_columns to fix it is a
// live-data migration for a naming problem that has nothing to do with the
// data model. TypeName resolves the same collision by pinning the generated
// name instead — the storage name, and every hand-written reference to it,
// stays untouched.
func kanbanFixtureWithTypeNameOverride() *schema.Registry {
	r := schema.NewRegistry()
	boards := r.Table("boards",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Text("name").Sortable(),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList})

	r.Table("board_columns",
		schema.UUIDv7("id").PrimaryKey(),
		schema.Ref("board", boards).Filterable(),
		schema.Text("title"),
	).Expose(schema.REST{Ops: schema.OpRead | schema.OpList}).
		TypeName("KanbanColumn")
	return r
}

func TestTypeNameOverrideResolvesTheCollisionInGo(t *testing.T) {
	files := generate(t, kanbanFixtureWithTypeNameOverride())
	models := files["models_gen.go"]
	if !contains(models, "type KanbanColumn struct {") {
		t.Errorf("the override did not rename the struct:\n%s", models)
	}
	if contains(models, "type BoardColumn struct {") {
		t.Errorf("the derived name was emitted alongside the override:\n%s", models)
	}
	// The storage name is untouched — only the generated identifier moved.
	if !contains(models, `func (KanbanColumn) TableName() string { return "board_columns" }`) {
		t.Errorf("TypeName should not change the table's storage name:\n%s", models)
	}
}

func TestTypeNameOverrideResolvesTheCollisionInTS(t *testing.T) {
	files := generateTS(t, kanbanFixtureWithTypeNameOverride())
	client := files["client.gen.ts"]
	if !contains(client, "export interface KanbanColumn {") {
		t.Errorf("the override did not rename the TS interface:\n%s", client)
	}
}

func TestTypeNameOverrideResolvesTheCollisionInDart(t *testing.T) {
	src := generateDart(t, kanbanFixtureWithTypeNameOverride())
	if !contains(src, "class KanbanColumn extends Row {") {
		t.Errorf("the override did not rename the Dart class:\n%s", src)
	}
}
