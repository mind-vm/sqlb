// Package vaultschema is the schema definition for the vault example: the
// single source of truth an author, or an agent, edits.
package vaultschema

import "github.com/mind-vm/sqlb/schema"

// Secret is a row whose entire payload only Go may write.
//
// owner_kind and owner_id are a polymorphic owner, deliberately declared as
// two plain columns rather than a Ref: the owners — users, teams, whatever
// else ends up allowed to hold a secret — live in modules this one does not
// import, the way docs/special-cases.md's census counts nine lines of this
// shape and finds no Ref that fits it. The cost is real and is not hidden
// here: nothing stops an owner_id naming a row that does not exist, or ever
// existed. A caller that needs that guarantee enforces it in the module that
// owns the referent, the same way ExternalRef's callers already have to.
//
// ciphertext, nonce and key_version are Hidden: never selectable, never
// filterable, and absent from the generated typed-column facade, which is
// the property that makes a table safe to expose at all. What that costs is
// the write side — see store.go. This is exactly the same declaration
// example/blog makes for authors.password_hash, generalised from one column
// to the whole payload: there, the table still declares OpCreate because one
// visible column (email) is worth writing through it; here, every payload
// column is hidden, so a generated create body would have nothing left to
// accept, and OpCreate is left off rather than kept as a decoration nothing
// can reach.
var Secret = schema.Table("secrets",
	schema.UUIDv7("id").PrimaryKey(),
	schema.Text("owner_kind").Filterable(),
	schema.UUID("owner_id").Filterable(),
	schema.Bytes("ciphertext").Hidden(),
	schema.Bytes("nonce").Hidden(),
	schema.Int("key_version").Hidden(),
	schema.Timestamps(),
).
	Index("owner_kind", "owner_id").
	Describe("A secret whose payload only Go may write; see store.go.").
	Expose(schema.REST{
		Path: "/secrets",
		// No OpCreate, no OpUpdate: a Hidden column can never reach either body,
		// so a table whose whole payload is Hidden has nothing left for a
		// generated write to accept. Declaring them anyway would mount an
		// endpoint that always no-ops on the one thing a caller would be there
		// for, which is worse than not mounting it.
		Ops: schema.OpRead | schema.OpList,
	})
