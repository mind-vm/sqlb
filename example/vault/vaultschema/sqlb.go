package vaultschema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import "github.com/mind-vm/sqlb/codegen"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// example/vault is a module of its own, so the module root is example/vault
// and Dir is left empty: the generated Go lands beside go.mod, the way
// example/tasks's taskschema.SqlbProject already argues.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Package: "vault",
		},
	}
}
