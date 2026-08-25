package app

import (
	"context"
	"net/http"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/rest"
)

// The verbs behind the declared actions (ADR-0043).
//
// This file is the whole application-side cost of POST /tasks/{id}/complete.
// What is not here is the envelope: the id is parsed, the row is fetched under
// the workspace predicate the BeforeQuery hook installs, a missing one is a
// 404, the body is decoded, this runs inside the transaction, the declared
// columns are written, and the row comes back — none of which is specific to
// completing a task, and all of which the six hand-written verb handlers this
// example would otherwise need would repeat.
//
// Compare deletes.go, which is the same shape written out by hand for soft
// delete. That one is three lines of domain logic in forty of envelope, and it
// stays hand-written because DELETE-means-update is a routing decision rather
// than a verb.

// completeTask marks a task done.
//
// The rule is the reason this is a verb and not a PATCH. status and
// completed_at have to move together — the check constraint forbids the state
// where only one of them did — and a task that is already done is a request to
// refuse rather than a no-op to accept. A client that had to say this by
// PATCHing status could get the first part wrong and could not express the
// second at all.
func completeTask(ctx context.Context, task *tasks.Task, in tasks.CompleteTaskInput) error {
	if task.Status == tasks.TaskStatusDone {
		// The escape hatch, and it needs no mechanism the package did not have:
		// an error carrying its own status is answered with it.
		return &rest.Problem{
			Title:  http.StatusText(http.StatusConflict),
			Status: http.StatusConflict,
			Detail: "the task is already done",
		}
	}

	now := time.Now().UTC()
	task.Status = tasks.TaskStatusDone
	task.CompletedAt = &now

	// Anything outside the declared write set is this function's own business,
	// and it has the transaction to do it in: the comment and the completion
	// commit together or neither does.
	if in.Note == nil || *in.Note == "" {
		return nil
	}
	tx, ok := sqlb.TxFrom(ctx)
	if !ok {
		// Only reachable with Options.DisableTransactions, where there is no
		// unit of work to join and writing the comment anyway would leave one
		// half of this durable without the other.
		return rest.ErrNoTransaction
	}
	_, err := sqlb.InsertRows(&tasks.Comment{
		TaskID:   task.ID,
		AuthorID: task.AuthorID,
		Body:     *in.Note,
	}).One(ctx, tx)
	return err
}
