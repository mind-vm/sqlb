// Stage 4 of four. See docs/refactoring-from-sqlc.md.

package withsqlc

import (
	"context"
	"errors"
	"net/http"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/blog"
	"github.com/mind-vm/sqlb/rest"

	// Imported for its side effects, as in stage 3.
	_ "github.com/mind-vm/sqlb/example/blog/blogschema"
)

// orgContextKey carries the tenant the way a real application's authentication
// middleware would: on the context, put there once per request.
type orgContextKey struct{}

// WithOrg scopes a context to one tenant. In a real program this is what the
// middleware that validated the token does, and nothing downstream takes an
// org as an argument again.
func WithOrg(ctx context.Context, orgID string) context.Context {
	return context.WithValue(ctx, orgContextKey{}, orgID)
}

// ErrNoOrg is what the hook returns for a context no middleware scoped. It
// fails the query rather than serving one, which is the only safe direction: a
// multi-tenant read whose tenant predicate went missing returns every tenant's
// rows.
var ErrNoOrg = errors.New("no tenant on the context")

// RegisterStage4Hooks installs the two predicates that were arguments and
// hand-written Where clauses in stages 1 through 3.
//
// This is the move stage 4 is really about, and it is worth more than the
// deleted handler below. One registration constrains *every* read of Post — the
// generated list, the generated read, the ones the expand machinery issues, and
// any query written by hand later — so scoping is no longer something each call
// site has to remember (ADR-0008). Stage 3's handler could have forgotten the
// org predicate and would have compiled, tested green against a single-tenant
// fixture, and leaked in production.
//
// example/blog/hooks.go registers the soft-delete half and says tenant scoping
// belongs on the same hook, left out there only because that example has no
// authentication to read a tenant from. This is that hook with the missing half
// supplied.
//
// It returns the registry it registered into, and the handle carries it
// (ADR-0047). With no ambient registry to write to, "the hook is installed"
// and "the handle runs it" become one statement instead of two that can drift.
func RegisterStage4Hooks() *sqlb.Registry {
	reg := sqlb.NewRegistry()
	sqlb.On[blog.Post](reg).BeforeQuery(func(ctx context.Context, q *sqlb.Builder[blog.Post]) error {
		org, ok := ctx.Value(orgContextKey{}).(string)
		if !ok || org == "" {
			return ErrNoOrg
		}
		q.Where(blog.PostCols.OrgID.Eq(org), blog.PostCols.DeletedAt.IsNull())
		return nil
	})
	return reg
}

// ServerStage4 is the whole of stage 4: there is no ListPosts function, because
// nothing here writes one.
//
// blog.Register is generated from example/blog/blogschema and mounts every
// resource the schema exposes. For posts that is list, read, create and update
// at /posts, with the filter grammar, the sort, the search, the pagination
// envelope and the OpenAPI entry all derived from the capabilities the columns
// declared. The list endpoint stage 3 hand-wrote is one of them.
//
// What is left to write is what is genuinely this application's: the hook above,
// and the soft delete below, which serves DELETE /posts/{id} as an update to
// deleted_at because the schema deliberately does not expose OpDelete.
//
// The honest cost, stated where someone deciding can see it: this is the step
// that takes the dependency. rest is an adapter onto huma, so a project that
// stops at stage 3 keeps sqlb's engine on pgx and nothing else, and a project
// that takes stage 4 accepts a web framework it did not choose. That is the
// trade ADR-0007 argues, and the reason each stage here is a stopping point
// rather than a step on the way to a mandatory destination.
func ServerStage4(exec sqlb.Executor) (http.Handler, error) {
	// posts declares SoftDelete, so rest refuses to mount the resource until a
	// hook filters the column (ADR-0030). The handle is what carries that hook,
	// so it is built here rather than taken.
	//
	// The double-registration guard this used to need is gone with the ambient
	// registry it guarded: each call registers into a registry of its own, so
	// calling twice produces two servers with one predicate each instead of one
	// predicate applied twice.
	db := sqlb.New(exec).WithHooks(RegisterStage4Hooks())

	srv := rest.NewServer(rest.Config{Title: "Blog", Version: "1.0.0"})
	if err := blog.Register(srv.API, db); err != nil {
		return nil, err
	}
	blog.RegisterPostSoftDelete(srv.API, db)
	return srv.Handler, nil
}
