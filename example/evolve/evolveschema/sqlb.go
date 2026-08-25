package evolveschema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import "github.com/mind-vm/sqlb/codegen"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// Go only, and no EjectDir: this example is about what happens to a schema over
// time, and example/blog is where the exit is kept current.
//
// The generated code matters here for one reason beyond completeness. A schema
// edit changes two things that drift independently — the code sqlb writes, and
// the API that code serves — and the repository gates them separately.
// `generate-check` catches the first, `impact-check` the second, and revision 4
// of the history below is the case where they disagree: a rename is a clean
// migration and a broken client at once.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Dir:     "example/evolve",
			Package: "evolve",
		},
	}
}
