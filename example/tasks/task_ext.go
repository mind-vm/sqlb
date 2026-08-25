package tasks

import "github.com/mind-vm/sqlb"

// Hand-written extensions to the generated types.
//
// Generated code covers what the schema implies: a setter per writable column,
// typed by that column. Anything that encodes a domain decision belongs here,
// in a file the generator does not touch — which is the seam that keeps
// generated CRUD useful once the rules get specific.

// AddCommentCount moves the counter by n in the database rather than reading it
// first, so two comments posted at the same moment both count.
//
// The generator cannot produce this. comment_count is ReadOnly in the schema,
// so there is no SetCommentCount, and the read-modify-write it replaces is a
// correctness decision rather than a mechanical one: SetCommentCount(old+1)
// compiles, passes review, and loses an increment the first time two requests
// interleave.
func (u *TaskUpdate) AddCommentCount(n int32) *TaskUpdate {
	u.Stmt().SetExpr("comment_count", sqlb.Raw{SQL: "comment_count + ?", Args: []any{n}})
	return u
}
