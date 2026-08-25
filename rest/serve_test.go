package rest_test

import (
	"context"
	"testing"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// Serve's guard clauses need no database, so they are the part of it this
// package can test without one — the pool-open, migrate and listen paths are
// exercised live in example/tasks2 against real Postgres instead.

func TestServeRefusesAnEmptyDSN(t *testing.T) {
	err := rest.Serve(context.Background(), rest.ServeConfig{}, func(*rest.Server, *sqlb.DB) error {
		t.Fatal("mount was called despite an empty DSN")
		return nil
	})
	if err == nil {
		t.Fatal("want an error for an empty DSN")
	}
}

func TestServeRefusesANilMount(t *testing.T) {
	err := rest.Serve(context.Background(), rest.ServeConfig{DSN: "postgres://example/db"}, nil)
	if err == nil {
		t.Fatal("want an error for a nil mount func")
	}
}
