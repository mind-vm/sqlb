package blog

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb"
)

// postID addresses one post. The generated handlers have their own copy of this
// shape; it is unexported there, and duplicating three lines is cheaper than
// exporting a type to share them.
type postID struct {
	ID string `path:"id" doc:"Primary key of the post"`
}

// RegisterPostSoftDelete serves DELETE /posts/{id} as an update to deleted_at.
//
// Post leaves OpDelete out of its Expose, because the generated delete is a
// real DELETE and this table's deletes are meant to be soft. No hook can bridge
// that: BeforeDelete receives a *Delete, so it can abort the statement or amend
// its predicate, but not turn it into an UPDATE. The endpoint is written here
// instead — which is the same seam post_ext.go uses, for the same reason.
//
// The BeforeQuery registration in hooks.go is the other half. Without it this
// endpoint would hide nothing, and the row would come straight back on the next
// list.
func RegisterPostSoftDelete(api huma.API, db sqlb.Executor) {
	huma.Register(api, huma.Operation{
		OperationID: "delete-post",
		Method:      http.MethodDelete,
		Path:        "/posts/{id}",
		Summary:     "Soft-delete a post",
		Description: "Stamps deleted_at. The row stays in the table and drops out of every read.",
		Tags:        []string{"posts"},
		// 204, and no body: the row still exists, but nothing the API will
		// show again.
		DefaultStatus:                http.StatusNoContent,
		RejectUnknownQueryParameters: true,
	}, func(ctx context.Context, in *postID) (*struct{}, error) {
		// The deleted_at predicate makes a second delete a 404 rather than a
		// silent re-stamp, and matches what a reader of this API can see: a
		// post already soft-deleted is, to every other endpoint, gone.
		rows, err := sqlb.UpdateRows[Post]().
			Where(sqlb.F("id").Eq(in.ID), sqlb.F("deleted_at").IsNull()).
			SetExpr("deleted_at", sqlb.Raw{SQL: "now()"}).
			Exec(ctx, db)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, huma.Error404NotFound("no post matched")
		}
		return nil, nil
	})
}
