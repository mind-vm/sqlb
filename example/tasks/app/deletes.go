package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
)

// Soft deletes, served as DELETE.
//
// schema.SoftDelete adds a deleted_at column and stops. Nothing in sqlb writes
// it, and nothing filters it back out — so the two halves are here and in
// hooks.go, and the tables that declare it do not expose the generated DELETE.
// See the note in taskschema/schema.go.
//
// The route is one generic function used three times rather than three
// handlers, for the same reason the read scoping is: the operation does not
// differ per model, and three copies is three chances for one of them to forget
// the deleted_at IS NULL predicate.

type deleteInput struct {
	ID string `path:"id" format:"uuid"`
}

func registerSoftDeleteRoutes(api huma.API, db *sqlb.DB) {
	softDelete[tasks.Task](api, db, "delete-task", "/tasks/{id}", "task")
	softDelete[tasks.List](api, db, "delete-list", "/lists/{id}", "list")
	softDelete[tasks.Comment](api, db, "delete-comment", "/comments/{id}", "comment")
}

func softDelete[T any](api huma.API, db *sqlb.DB, opID, path, resource string) {
	huma.Register(api, huma.Operation{
		OperationID: opID,
		Method:      http.MethodDelete,
		Path:        path,
		Summary:     "Delete a " + resource,
		Description: "A soft delete: the row is marked rather than removed, and disappears " +
			"from every read. Not a generated handler — the generated DELETE would " +
			"remove the row outright.",
		Tags:          []string{resource + "s"},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *deleteInput) (*struct{}, error) {
		// No workspace predicate here, and that is the point: the BeforeUpdate
		// hook adds it. A row in another workspace therefore matches nothing and
		// answers 404 — the same as an id that never existed, which is the
		// answer it should get.
		//
		// The deleted_at IS NULL half is this statement's own, so that deleting
		// something twice is a 404 rather than a silent second stamp that moves
		// the deletion time.
		rows, err := sqlb.UpdateRows[T]().
			Set("deleted_at", time.Now().UTC()).
			Where(sqlb.F("id").Eq(in.ID), sqlb.F("deleted_at").IsNull()).
			Exec(ctx, db)
		if err != nil {
			return nil, asHTTP(fmt.Errorf("deleting the %s: %w", resource, err))
		}
		if len(rows) == 0 {
			return nil, huma.Error404NotFound("no " + resource + " matched")
		}
		return &struct{}{}, nil
	})
}
