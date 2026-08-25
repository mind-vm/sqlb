// Stage 1 of four. See docs/refactoring-from-sqlc.md; refactor_test.go holds
// all four to the same answers.

package withsqlc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mind-vm/sqlb/example/withsqlc/sqlcgen"
)

// The bounds a list endpoint needs. Stage 1 writes them as constants because
// there is nowhere else to put them; by stage 3 they are DefaultPageSize and
// MaxPageSize on the schema's Expose, which is also where the generated
// OpenAPI document reads them from.
const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

// stage1Sort is the ordering baked into the ListPosts query text.
const stage1Sort = "-published_at"

// ErrSortUnavailable is what stage 1 returns when asked for an ordering its
// query does not have. It is not a validation failure — the request is
// perfectly reasonable — it is the shape of static SQL surfacing as a runtime
// refusal, and the only fix is another query.
var ErrSortUnavailable = errors.New("this sort needs a query of its own")

// ListPostsStage1 serves a filterable list of posts with sqlc alone.
//
// The generated function does the typed part well: ListPostsParams is checked
// against the real schema at build time, and a column that does not exist fails
// `sqlc generate` rather than a request. Everything around it is hand-written,
// and that is the part this stage exists to show.
//
// Three costs, none of which are sqlc doing something wrong:
//
//   - **Every optional filter is unpacked by hand** into the null-carrying type
//     its arm expects, and the query sends all three on every request whether or
//     not they mean anything.
//   - **The sort is not a parameter.** It is in the query text, so "sort by
//     view_count" is a second entry in query.sql, a second generated function,
//     and a branch here to choose between them. n sortable columns in two
//     directions is 2n queries.
//   - **This function is the security boundary**, and nothing marks it as one.
//     That `status` is filterable and `password_hash` is not is a fact about
//     which lines were written here, so reviewing the API surface means reading
//     the handler rather than reading the schema (ADR-0006).
//
// The ordering of the arms in query.sql and the ordering of the assignments
// below have to agree, and nothing checks that they do.
func ListPostsStage1(ctx context.Context, db sqlcgen.DBTX, orgID string, query url.Values) ([]sqlcgen.Post, error) {
	params := sqlcgen.ListPostsParams{OrgID: orgID, PageLimit: defaultPageLimit}

	if v := query.Get("status"); v != "" {
		params.Status = pgtype.Text{String: v, Valid: true}
	}
	if v := query.Get("min_views"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("min_views: %w", err)
		}
		params.MinViews = pgtype.Int8{Int64: n, Valid: true}
	}
	if v := query.Get("search"); v != "" {
		params.Search = pgtype.Text{String: v, Valid: true}
	}
	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("limit: %w", err)
		}
		// Bounded here because there is nowhere else. A missing clamp is how a
		// list endpoint becomes a way to ask for the whole table.
		if n < 1 || n > maxPageLimit {
			return nil, fmt.Errorf("limit: %d is outside 1..%d", n, maxPageLimit)
		}
		params.PageLimit = int32(n)
	}

	// Where the shape stops being merely verbose and starts refusing work.
	if sort := query.Get("sort"); sort != "" && sort != stage1Sort {
		return nil, fmt.Errorf("%w: ListPosts sorts by %s", ErrSortUnavailable, stage1Sort)
	}

	return sqlcgen.New(db).ListPosts(ctx, params)
}
