package tasks2

import (
	"context"
	"net/http"
	"time"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/rest"
)

// CompleteTask is the verb behind the declared "complete" action, in its
// item form.
//
// CompleteTaskInput is generated (rest_gen.go, from Action.Body), so this
// file only writes the verb.
//
// The envelope — fetch the row, decode the body, run this inside the
// transaction, persist Writes, answer with the row — is the whole
// application-side cost of an item-form action.
//
// The CallerFrom check is the other half of what main.go wires: this is a
// POST, so RequireAuthForWrites already refused the request if it had no
// caller — nothing here should be reachable without one. It is asserted
// anyway, as the defense-in-depth a Do func gets almost for free: proof the
// context value actually crossed from the group's middleware, through the
// envelope, into application code, rather than a route that would 500 on a
// nil dereference if that wiring were ever wrong.
func CompleteTask(ctx context.Context, task *Task, in CompleteTaskInput) error {
	if _, ok := CallerFrom(ctx); !ok {
		return &rest.Problem{
			Title:  http.StatusText(http.StatusUnauthorized),
			Status: http.StatusUnauthorized,
			Detail: "no caller in context; RequireAuthForWrites should have refused this request already",
		}
	}
	if task.Status == TaskStatusDone {
		return &rest.Problem{
			Title:  http.StatusText(http.StatusConflict),
			Status: http.StatusConflict,
			Detail: "the task is already done",
		}
	}
	now := time.Now().UTC()
	task.Status = TaskStatusDone
	task.CompletedAt = &now
	_ = in // Note is accepted and ignored in this example.
	return nil
}

// ClearCompleted is the verb behind the declared "clear-completed" action.
//
// Unlike CompleteTask there is no row the envelope fetched — this is a
// collection action, whose signature is func(ctx, In) error with no
// Executor at all, so reaching the transaction the envelope opened is
// sqlb.TxFrom(ctx), the same seam a hook uses to reach one.
func ClearCompleted(ctx context.Context, in ClearCompletedTaskInput) error {
	tx, ok := sqlb.TxFrom(ctx)
	if !ok {
		return rest.ErrNoTransaction
	}
	q := sqlb.DeleteRows[Task]().Where(sqlb.F("status").Eq(TaskStatusDone))
	if in.ListID != nil {
		q = q.Where(sqlb.F("list_id").Eq(*in.ListID))
	}
	_, err := q.Exec(ctx, tx)
	return err
}
