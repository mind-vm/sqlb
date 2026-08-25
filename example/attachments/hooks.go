package attachments

import (
	"context"
	"errors"
	"fmt"

	"github.com/mind-vm/sqlb"
)

// RegisterHooks ties the object's lifetime to the row's, so that deleting an
// attachment through *any* path removes its bytes.
//
// A hook rather than a wrapper around the delete endpoint, and that is the
// whole reason this is worth showing: the generated `DELETE /attachments/{id}`
// goes through it, and so does a cascade, a bulk cleanup written in Go, and
// the admin script somebody runs at two in the morning. Wiring the endpoint
// would leave the feed of deletions silent for exactly the callers most likely
// to matter — the identical argument for why tenant scoping lives in
// BeforeQuery rather than in each handler.
//
// [sqlb.Hooks.AfterDeleteRows] rather than AfterDelete: the count hook knows
// how many rows went, and this needs to know *which*, because the object key
// is on the row. That costs the delete a `RETURNING`, which is the price of
// being able to clean up after it.
func RegisterHooks(reg *sqlb.Registry, s *Store) {
	sqlb.On[Attachment](reg).AfterDeleteRows(func(ctx context.Context, rows []Attachment) error {
		// The rows are captured now and used later: AfterCommit runs after the
		// transaction that removed them has ended, and there is nothing left
		// to read them from by then.
		keys := make([]string, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, row.Key)
		}

		return sqlb.AfterCommit(ctx, func(ctx context.Context) error {
			// After the commit, never inside it. Object storage is not in the
			// transaction, so a delete issued inside one becomes permanent the
			// moment it is sent — and if the transaction then rolls back, the
			// row is still there, pointing at bytes that are gone. A row that
			// renders as a broken image is worse than an object nobody
			// references, because only one of the two can be found again.
			//
			// The failure this leaves behind is therefore an orphaned object,
			// which is what Store.Sweep exists to collect.
			var failed []error
			for _, key := range keys {
				if err := s.removeObject(ctx, key); err != nil {
					failed = append(failed, err)
				}
			}
			if len(failed) > 0 {
				// Reported, not swallowed: the row is already gone, so this
				// cannot be undone, and an application that logs nothing here
				// will not find out that its bucket is growing. The error
				// joins under sqlb.ErrAfterCommit, so a caller can tell "the
				// delete did not happen" from "the delete happened and the
				// cleanup did not".
				return fmt.Errorf("attachments: the rows were deleted and %d object(s) were not: %w",
					len(failed), errors.Join(failed...))
			}
			return nil
		})
	})
}
