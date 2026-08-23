package attachmentschema

//go:generate go run github.com/jryannel/sqlb/cmd/sqlb generate .

import "github.com/jryannel/sqlb/codegen"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// example/attachments is a module of its own, so the module root is
// example/attachments and Dir is left empty: the generated Go lands beside
// go.mod, the way example/vault's vaultschema.SqlbProject already argues.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Package: "attachments",
		},
	}
}
