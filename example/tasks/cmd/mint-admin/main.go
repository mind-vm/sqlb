// Command mint-admin prints a platform-admin bearer token, signed with the
// same secret the running server verifies against.
//
//	export TASKS_JWT_SECRET="$(head -c 32 /dev/urandom | base64)"   # same value the server uses
//	go run ./cmd/mint-admin -subject ops-1 -workspace w1 -role admin
//
// It touches no database and needs none: minting is a pure function of the
// secret and the claims. That is deliberate — the only public ways to get a
// token are /auth/register and /auth/login, and PlatformAdmin has neither,
// because a field a caller could ask for on a self-serve endpoint is not an
// access boundary. Whoever runs this command with the server's own secret is
// already trusted with more than a bearer token communicates; this exists so
// that trust does not also require hand-assembling a JWT.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// ttl is short deliberately: a platform-admin token is minted for one
// operator session, not carried around the way a workspace token is.
const ttl = time.Hour

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mint-admin:", err)
		os.Exit(1)
	}
}

func run() error {
	subject := flag.String("subject", "", "user id to mint the token for (required)")
	workspace := flag.String("workspace", "", "workspace id the token names (required by Signer.Sign; unused by admin routes' own hooks)")
	role := flag.String("role", "admin", "workspace role to carry alongside PlatformAdmin")
	email := flag.String("email", "", "optional, for a token that is also readable as an ordinary one")
	flag.Parse()

	if *subject == "" || *workspace == "" {
		return errors.New("-subject and -workspace are required")
	}

	secret := []byte(os.Getenv("TASKS_JWT_SECRET"))
	if len(secret) < 32 {
		return errors.New("TASKS_JWT_SECRET is unset or shorter than 32 bytes; " +
			"set it to the same value the target server was started with")
	}

	s, err := auth.NewSigner(secret, "tasks", ttl)
	if err != nil {
		return err
	}

	token, err := s.Sign(auth.Claims{
		Subject:       *subject,
		Email:         *email,
		Workspace:     *workspace,
		Role:          *role,
		PlatformAdmin: true,
	})
	if err != nil {
		return err
	}

	fmt.Println(token)
	return nil
}
