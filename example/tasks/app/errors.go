package app

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// Errors from hooks travel a long way: a hook returns one, sqlb passes it back
// unwrapped, and the generated REST handler hands it to Huma. Huma writes an
// error's own status when it implements huma.StatusError and 500 otherwise, so
// translating here is what keeps an authorisation failure from arriving as an
// internal error.
//
// The translation lives in this package rather than in auth, because auth
// should not know that it is being consumed over HTTP. It is used by background
// jobs and by tests that never build a router.

// errForbidden is a 403: the caller is authenticated and not permitted.
func errForbidden(detail string) error { return huma.Error403Forbidden(detail) }

// errUnauthenticated is a 401, and should be unreachable through the router:
// the middleware rejects an unauthenticated request long before a hook sees it.
// It exists for every other caller — a job, a test, a future gRPC surface —
// where the hook is the only thing standing between a missing identity and an
// unscoped query.
func errUnauthenticated(err error) error {
	return huma.Error401Unauthorized("the request carries no authenticated identity", err)
}

// claimsOrError is what hooks call instead of auth.Require, so that the
// fail-closed path produces a 401 rather than a 500 when it is reached over
// HTTP.
func claimsOrError(ctx context.Context) (auth.Claims, error) {
	c, err := auth.Require(ctx)
	if err != nil {
		if errors.Is(err, auth.ErrNoClaims) {
			return auth.Claims{}, errUnauthenticated(err)
		}
		return auth.Claims{}, err
	}
	return c, nil
}

// workspaceOf is claimsOrError for the hooks that need only the tenant.
func workspaceOf(ctx context.Context) (string, error) {
	c, err := claimsOrError(ctx)
	if err != nil {
		return "", err
	}
	return c.Workspace, nil
}
