// Stage 2 of four. See docs/refactoring-from-sqlc.md.

package withsqlc

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/withsqlc/sqlcgen"
)

// The capabilities of a struct sqlb did not generate, restated at startup
// because they cannot be read off a type that never declared them.
//
// This is the honest cost of adopting sqlb this way, and it is the cost stage 3
// removes rather than one it hides: the schema declaration in
// example/blog/blogschema says all of this once, and everything downstream —
// models, the typed columns, the REST filter grammar, the generated clients —
// reads it from there.
//
// In init rather than in the function below, because Describe mutates a cached
// model without locking and panics if a statement has already been built
// against it. Startup is the only safe moment.
func init() {
	sqlb.Describe[sqlcgen.Post]().
		PrimaryKey("id").
		Filterable("status", "author_id", "view_count", "org_id").
		Sortable("published_at", "view_count").
		Searchable("title", "body").
		ReadOnly("view_count")
}

// stage2Sortable is the allow-list a client's ?sort is checked against.
//
// It says the same thing the Sortable call above says, and nothing makes the
// two agree. That duplication is the whole argument for stage 3: one of these
// is the capability the REST layer enforces and the other is a map in a
// handler, and a column added to one and not the other fails in whichever
// direction nobody tested.
var stage2Sortable = map[string]bool{
	"published_at": true,
	"view_count":   true,
}

// ListPostsStage2 does stage 1's job with the query builder, over the structs
// sqlc generated. Nothing in sqlcgen changes, and nothing in it knows sqlb
// exists.
//
// What this buys, in order of how much it matters:
//
//   - **Only what was asked reaches Postgres.** A predicate exists because a
//     branch added it, so the three-armed NULL check is gone rather than
//     optimised away.
//   - **The sort is a value.** `?sort=-view_count` is an Order, not a second
//     entry in query.sql, so the 2n queries collapse back to one.
//   - **One transaction still carries both sides.** This function takes an
//     sqlb.Executor and the generated one takes a DBTX; a pgx.Tx is both, so the
//     dashboard query in query.sql and this list can run inside one unit of work
//     (ADR-0040, and adopt_test.go asserts it at compile time).
//
// What it does not buy, and what stage 3 is for: the request-to-predicate
// translation below is still hand-written, and so is the allow-list above.
func ListPostsStage2(ctx context.Context, db sqlb.Executor, orgID string, query url.Values) ([]sqlcgen.Post, error) {
	// A query is a value, so the tenant scope is just the first predicate and
	// the optional ones append to it.
	q := sqlb.Query[sqlcgen.Post]().
		Where(sqlb.F("org_id").Eq(orgID), sqlb.F("deleted_at").IsNull())

	if v := query.Get("status"); v != "" {
		q = q.Where(sqlb.F("status").Eq(v))
	}
	if v := query.Get("min_views"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("min_views: %w", err)
		}
		q = q.Where(sqlb.F("view_count").Gte(n))
	}
	if v := query.Get("search"); v != "" {
		q = q.Where(sqlb.F("title").Contains(v))
	}

	order, err := stage2Order(query.Get("sort"))
	if err != nil {
		return nil, err
	}
	q = q.OrderBy(order)

	limit := defaultPageLimit
	if v := query.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("limit: %w", err)
		}
		if n < 1 || n > maxPageLimit {
			return nil, fmt.Errorf("limit: %d is outside 1..%d", n, maxPageLimit)
		}
		limit = n
	}

	return q.Limit(limit).All(ctx, db)
}

// stage2Order turns `-published_at` into an Order, refusing a column that is
// not on the allow-list.
//
// Twelve lines that stage 3 deletes: filter.Parse does this against the
// declared capabilities, for every column at once, and its rejection names the
// columns that would have worked (ADR-0011). This one just says no.
func stage2Order(sort string) (sqlb.Order, error) {
	if sort == "" {
		sort = stage1Sort
	}
	column, desc := strings.TrimPrefix(sort, "-"), strings.HasPrefix(sort, "-")
	if !stage2Sortable[column] {
		return sqlb.Order{}, fmt.Errorf("sort: %q is not sortable", column)
	}
	if desc {
		return sqlb.F(column).Desc(), nil
	}
	return sqlb.F(column).Asc(), nil
}
