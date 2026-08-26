// Package cli is the generated command-line client for the tasks example,
// plus the one command the generator cannot emit.
//
// cli_gen.go is generated from the schema and not edited here. login.go sits
// beside it: POST /auth/login establishes the identity every other command
// sends, so nothing generic can emit a command for it — see login.go for the
// seam the generator leaves for exactly this case.
package cli
