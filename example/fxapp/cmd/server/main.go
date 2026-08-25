// Command server runs the notes API.
//
//	docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18
//	export FXAPP_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
//	export FXAPP_SPACE_KEYS="acme=$(head -c 24 /dev/urandom | base64)"
//	go run ./cmd/server
//
// Then http://localhost:8080/docs. The migrations apply at startup and the
// configured spaces are created, so an empty database is enough.
//
// The body of main is one call, which is the claim this example is making: the
// startup sequence — connect, migrate, provision, register hooks, mount
// resources, listen, and unwind all of it on SIGINT — is a graph the container
// derives from what each module declares, not a list this file has to keep in
// the right order.
package main

import "github.com/mind-vm/sqlb/example/fxapp"

func main() { fxapp.Run() }
