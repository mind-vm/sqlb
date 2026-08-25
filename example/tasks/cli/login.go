// login.go is hand-written, and sits beside cli_gen.go on purpose.
//
// POST /auth/login is not CRUD — it establishes the identity every other
// command sends — so nothing generic can emit a command for it, and the CLI
// generator does not try (docs/architecture.md, "The Go CLI is generated").
// What the generator does do is leave the seam wide enough that the missing
// command is fifteen lines rather than a second program: Client.Run takes a
// context and an io.Writer rather than a *cobra.Command, so a caller outside
// the generated tree gets the same conventions the generated commands get —
// JSON to stdout, --compact honoured, a Problem rendered to stderr with its
// allow-list intact, and a non-zero exit.
//
// The subtlety worth copying is which *client.Client this holds. It is the one
// main passes to New, not one of its own, so --base-url, --token and --timeout
// on the root reach this command too. A hand-written command that constructs
// its own client compiles, runs, and quietly ignores every persistent flag.

package cli

import (
	"net/http"

	"github.com/spf13/cobra"

	"github.com/mind-vm/sqlb/example/tasks/cli/client"
)

// NewLoginCommand returns `taskctl login`, issuing POST /auth/login through c.
//
// Exported because the command it belongs to is assembled in main:
//
//	c := &client.Client{}
//	root := cli.New(c)
//	root.AddCommand(cli.NewLoginCommand(c))
func NewLoginCommand(c *client.Client) *cobra.Command {
	var email, password, workspace string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Exchange an email and password for a bearer token",
		Args:  cobra.NoArgs,
		Long: `POST /auth/login

Not generated: a schema describes tables, and this endpoint mints the credential
the other commands send. It is written by hand against the same client, so it
shares the root's --base-url, --token and --timeout, and prints the token
document the way every other command prints its response.

The token is written to stdout rather than stored, so the shell decides where it
goes:

  export TASKCTL_TOKEN="$(taskctl login --email you@example.com --password ... | jq -r .token)"`,
		Example: "  taskctl login --email you@example.com --password 'correct horse battery staple'",
		RunE: func(cmd *cobra.Command, _ []string) error {
			body := map[string]any{"email": email, "password": password}
			// Absent rather than empty: the server reads a missing workspace as
			// "the oldest membership", and an empty string as a slug that
			// matches nothing.
			if workspace != "" {
				body["workspace"] = workspace
			}
			// cmd.Context() and cmd.OutOrStdout() are what make this behave like
			// a generated command: the first is what --timeout and a Ctrl-C
			// cancel travel on, the second is what a test can capture.
			return c.Run(cmd.Context(), cmd.OutOrStdout(), client.Request{
				Method: http.MethodPost,
				Path:   "/auth/login",
				Body:   body,
			}, false)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&email, "email", "", "The account's email address. Required.")
	_ = cmd.MarkFlagRequired("email")
	flags.StringVar(&password, "password", "",
		"The account's password. Required. Prefer a shell that keeps this out of\n"+
			"history: a password on the command line is visible in the process list.")
	_ = cmd.MarkFlagRequired("password")
	flags.StringVar(&workspace, "workspace", "",
		"Workspace slug to sign in to. Defaults to the oldest membership.")

	return cmd
}
