// Package access decides which space a request speaks for.
//
// A space presents a shared secret as a bearer token, and the middleware turns
// it into a verified principal on the request context — through sqlb's
// principal seam, so the hooks that read it back never name this package's
// mechanism. That is the whole of it, and it is deliberately the smallest
// thing that is still a boundary rather than a convention: the alternative an
// example is tempted by — a plain X-Space header — lets any caller name any
// tenant, which would make every hook in this module list decorative.
//
// It is *not* an authentication system. There are no users, no sessions, no
// revocation, and a leaked key is a leaked tenant until the configuration
// changes. example/tasks is where authentication lives: registration, login,
// password hashing, and an HS256 verifier with the three checks that make one
// safe. If you are copying from this repository, copy that one for the
// identity half and this one for the wiring.
package access

import (
	"go.uber.org/fx"

	"github.com/mind-vm/sqlb/example/fxapp/fxkit"
)

var Module = fx.Module("access",
	fx.Provide(NewConfig),
	fxkit.ProvideMiddleware(Config.Middleware),
)
