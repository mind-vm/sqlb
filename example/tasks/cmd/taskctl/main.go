// Command taskctl drives the task manager's API from a shell.
//
//	export TASKCTL_BASE_URL=http://localhost:8080
//	export TASKCTL_TOKEN="$(go run ./cmd/taskctl login \
//	    --email you@example.com --password '...' | jq -r .token)"
//
//	go run ./cmd/taskctl tasks list --status eq.todo --sort -created_at
//	go run ./cmd/taskctl tasks list --help
//
// Every command below this one is generated from taskschema, so a column that
// gains a capability gains its flag at the next `go generate ./...` and one
// that loses it loses the flag rather than starting to 400.
//
// This file is the part that is not generated, and it is nearly the whole of
// it: the transport, the credential and the exit code are decisions a schema
// cannot make.
//
// `login` is the other part. POST /auth/login mints the credential every
// generated command sends, so no generator can emit it — but it is added to the
// same root, against the same client, so it behaves like the commands beside
// it. See cli/login.go, and docs/cli/README.md, "Adding a command of your own".
package main

import (
	"os"

	"github.com/mind-vm/sqlb/example/tasks/cli"
	"github.com/mind-vm/sqlb/example/tasks/cli/client"
)

func main() {
	// One client for the whole tree. New binds the root's persistent flags to
	// this value, so a hand-written command holding the same pointer is
	// configured by --base-url, --token and --timeout exactly as a generated one
	// is. Constructing a second client inside the command is the mistake this
	// line exists to avoid; `cli.New(nil)` would make it unavoidable, since the
	// client it builds internally never comes back out.
	c := &client.Client{}
	root := cli.New(c)
	root.AddCommand(cli.NewLoginCommand(c))

	// The error is not printed here: cobra has already written it to stderr,
	// including the list of values the server said it would have accepted, and
	// printing it twice would bury that under a repetition.
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
