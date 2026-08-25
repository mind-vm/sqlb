package blog

import "github.com/mind-vm/sqlb"

// Hand-written extensions to the generated types.
//
// Generated code covers what the schema implies: a setter per writable column,
// typed by that column. Anything that encodes a domain decision belongs here,
// in a file the generator does not touch — which is the seam that keeps
// generated CRUD useful once the rules get specific.

// AddViewCount increments the counter in the database rather than reading it
// first, so concurrent increments do not lose updates.
//
// The generator cannot produce this: view_count is ReadOnly in the schema, so
// there is no SetViewCount, and the read-modify-write it would replace is a
// correctness decision rather than a mechanical one.
func (u *PostUpdate) AddViewCount(n int64) *PostUpdate {
	u.Stmt().SetExpr("view_count", sqlb.Raw{SQL: "view_count + ?", Args: []any{n}})
	return u
}
