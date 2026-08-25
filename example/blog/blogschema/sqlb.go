package blogschema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import "github.com/mind-vm/sqlb/codegen"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// It replaces a cmd/gen/main.go that was thirty lines of flag parsing around
// this literal, and — more to the point — it removes the -dir argument that
// main had to be given. Paths here resolve against the module root, which for
// this example is the repository root, so "example/blog" means the same thing
// whether the command is run from a shell, from the //go:generate directive in
// schema.go, or from CI.
//
// The blog example emits Go only. It is the short path from a schema to a
// server; example/tasks is the one that adds the TypeScript, Dart and CLI
// clients.
//
// It also declares an EjectDir, which no real project has to: `sqlb eject`
// defaults to "ejected" beside the generated code and this is exactly that
// path. It is written out because this example is where the exit is kept
// current — `mise run eject-check` diffs the committed package against what the
// schema would produce now, and pgtest/eject_test.go serves it beside the
// generated resources and compares the answers (ADR-0042).
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Dir:     "example/blog",
			Package: "blog",
		},
		EjectDir: "example/blog/ejected",
	}
}
