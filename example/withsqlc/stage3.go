// Stage 3 of four. See docs/refactoring-from-sqlc.md.

package withsqlc

import (
	"context"
	"net/url"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	"github.com/mind-vm/sqlb/filter"

	// Imported for its side effects: declaring a table registers it, and this
	// is the declaration everything below reads from.
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
)

// ListPostsStage3 is stage 2's job again, with the model's capabilities coming
// from the schema declaration rather than from a Describe call and a map.
//
// The type changed — blog.Post rather than sqlcgen.Post — and that is the whole
// move. blog.Post is generated from example/blog/blogschema, which already
// states which columns are filterable, sortable and searchable, so:
//
//   - **The allow-list is gone.** filter.Parse checks every parameter against
//     the declared capabilities, so `?sort=body` is refused because body did not
//     declare Sortable, not because a map in this file happens to omit it. The
//     rejection names the columns that would have worked (ADR-0011), which
//     stage2Order could not do without a second list.
//   - **The hand-written parameter unpacking is gone**, and with it the three
//     `if v := query.Get(...)` blocks. One grammar covers every column the
//     schema opened, including the operators — `?view_count=gte.100` needed a
//     branch of its own in both earlier stages.
//   - **A misspelled column stops compiling.** blog.PostCols.OrgID is a
//     Col[string]; sqlb.F("org_id") was a string that had to be right. That is
//     the typed column facade (ADR-0009), and it is generated from the same
//     declaration.
//
// Still hand-written, and the reason stage 4 exists: this function, its route,
// its pagination envelope, its OpenAPI entry, and the tenant scope that has been
// an argument since stage 1.
//
// Note the query string changes here. Stages 1 and 2 read an ad-hoc spelling
// this handler invented (`?status=published&min_views=100&limit=20`); from here
// it is the documented filter grammar (`?status=eq.published&view_count=gte.100
// &per_page=20`). The rows are the same and refactor_test.go asserts that, but
// the wire format is not, so this is the release where a client changes.
func ListPostsStage3(ctx context.Context, db sqlb.Executor, orgID string, query url.Values) ([]blog.Post, error) {
	parsed, err := filter.Parse(query, filter.Options{
		Model:           sqlb.ModelOf[blog.Post](),
		DefaultPageSize: defaultPageLimit,
		MaxPageSize:     maxPageLimit,
	})
	if err != nil {
		return nil, err
	}

	// The tenant scope and the soft-delete predicate are still applied by hand,
	// which means every other query against posts has to remember to. Stage 4
	// moves both into a hook, where forgetting is not an option the code offers.
	q := sqlb.Query[blog.Post]().Where(
		blog.PostCols.OrgID.Eq(orgID),
		blog.PostCols.DeletedAt.IsNull(),
	)

	return filter.Apply(q, parsed).All(ctx, db)
}
