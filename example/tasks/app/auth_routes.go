package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// The hand-written half of the API.
//
// Registration and login are the two operations that cannot be generated, and
// the reason is the same for both: they are the endpoints that *establish* the
// identity everything else is scoped by. A generated handler runs inside the
// workspace boundary; these two decide what the boundary is.
//
// They use a handle with no hooks attached (see newAuthAPI), because a login
// must read a user before there is a workspace to scope the read to. That is a
// deliberate, named exception rather than a gap: it is one handle, created in
// one place, used by the two endpoints in this file.

// authAPI holds what the auth endpoints need.
type authAPI struct {
	// sys reads and writes outside the workspace boundary. Everything else in
	// the application uses the hooked handle.
	sys *sqlb.DB
	// hooks is the boundary, kept separately so that the same connection can be
	// used with it or without it — the two handles differ only in which
	// registry they resolve against.
	hooks  *sqlb.Registry
	signer *auth.Signer
}

// app returns the handle every ordinary read goes through.
func (a *authAPI) app() *sqlb.DB { return a.sys.WithHooks(a.hooks) }

// hooked puts the boundary back on a transaction handle. WithHooks copies the
// handle rather than replacing its executor, so the returned one is still
// inside tx — the hooks apply and the statements stay in the same unit of work.
func (a *authAPI) hooked(tx *sqlb.DB) *sqlb.DB { return tx.WithHooks(a.hooks) }

// tokenBody is what a successful register or login returns.
type tokenBody struct {
	Token     string    `json:"token" doc:"Bearer token; send as 'Authorization: Bearer <token>'"`
	ExpiresAt time.Time `json:"expires_at" doc:"When the token stops being accepted"`
	UserID    string    `json:"user_id"`
	Workspace string    `json:"workspace_id" doc:"The workspace this token is scoped to"`
	Role      string    `json:"role" doc:"The caller's role in that workspace"`
}

type registerInput struct {
	Body struct {
		Name     string `json:"name" minLength:"1" maxLength:"200" doc:"The person's display name"`
		Email    string `json:"email" format:"email"`
		Password string `json:"password" minLength:"12" maxLength:"200" doc:"At least 12 characters"`

		// Registering creates a workspace as well as an account: a user with no
		// workspace could not be issued a token, because a token is scoped to
		// one. Joining an existing workspace is an invitation from inside it —
		// POST /memberships — not something an anonymous request may do.
		Workspace string `json:"workspace" minLength:"1" maxLength:"200" doc:"Name of the workspace to create"`
	}
}

type registerOutput struct {
	Status int
	Body   tokenBody
}

type loginInput struct {
	Body struct {
		Email    string `json:"email" format:"email"`
		Password string `json:"password"`

		// Optional. A user who belongs to several workspaces gets a token for
		// one of them; omitting this picks the oldest membership, which is the
		// workspace they registered with.
		Workspace string `json:"workspace,omitempty" doc:"Workspace slug to sign in to; defaults to the oldest membership"`
	}
}

type loginOutput struct {
	Body tokenBody
}

type meOutput struct {
	Body struct {
		User        tasks.User        `json:"user"`
		Workspace   tasks.Workspace   `json:"workspace"`
		Role        string            `json:"role"`
		Memberships []tasks.Workspace `json:"workspaces" doc:"Every workspace this user may switch to"`
	}
}

// registerAuthRoutes mounts /auth/register, /auth/login and /auth/me.
func registerAuthRoutes(api huma.API, a *authAPI) {
	// public overrides the API-wide security requirement. Without it the
	// OpenAPI document would tell a generated client to send a bearer token to
	// the endpoint that issues bearer tokens.
	public := []map[string][]string{}

	huma.Register(api, huma.Operation{
		OperationID:   "register",
		Method:        http.MethodPost,
		Path:          "/auth/register",
		Summary:       "Create an account and its first workspace",
		Tags:          []string{"auth"},
		Security:      public,
		DefaultStatus: http.StatusCreated,
	}, a.register)

	huma.Register(api, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/auth/login",
		Summary:     "Exchange an email and password for a bearer token",
		Tags:        []string{"auth"},
		Security:    public,
	}, a.login)

	huma.Register(api, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/auth/me",
		Summary:     "The signed-in user, their workspace, and the workspaces they can switch to",
		Tags:        []string{"auth"},
	}, a.me)
}

// register creates the user, the workspace and the owning membership.
//
// All three in one transaction, because two of the three are useless alone: an
// account with no workspace cannot be issued a token, and a workspace with no
// owner cannot be administered. Under autocommit a failure between them leaves
// exactly those states behind.
func (a *authAPI) register(ctx context.Context, in *registerInput) (*registerOutput, error) {
	hash, err := auth.HashPassword(in.Body.Password)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity("the password could not be hashed", err)
	}

	var out registerOutput
	err = a.sys.WithTx(ctx, func(ctx context.Context, tx *sqlb.DB) error {
		// OnConflictDoNothing rather than "does this email exist?" first: the
		// check-then-insert version is a race, and the unique index is the only
		// thing that actually decides. A conflict returns no rows, which is how
		// the taken case is detected without reading the error's driver-specific
		// SQLSTATE.
		users, err := sqlb.InsertRows(&tasks.User{
			Email:        strings.ToLower(strings.TrimSpace(in.Body.Email)),
			Name:         in.Body.Name,
			PasswordHash: hash,
		}).OnConflictDoNothing("email").Exec(ctx, tx)
		if err != nil {
			return fmt.Errorf("creating the user: %w", err)
		}
		if len(users) == 0 {
			return huma.Error409Conflict("that email address is already registered")
		}
		user := users[0]

		workspaces, err := sqlb.InsertRows(&tasks.Workspace{
			Name: in.Body.Workspace,
			Slug: slugify(in.Body.Workspace),
		}).OnConflictDoNothing("slug").Exec(ctx, tx)
		if err != nil {
			return fmt.Errorf("creating the workspace: %w", err)
		}
		if len(workspaces) == 0 {
			return huma.Error409Conflict("a workspace with that name already exists")
		}
		workspace := workspaces[0]

		// From here on the transaction acts *as* the new owner rather than
		// around the rules. The membership insert goes through the same
		// BeforeCreate hook every other membership does — which stamps the
		// workspace and checks the role — and it passes because the claims say
		// owner, not because this code is exempt.
		//
		// Bypassing the hook with a raw INSERT would work today and would be
		// the first place a future rule is forgotten.
		ctx = auth.WithClaims(ctx, auth.Claims{
			Subject:   user.ID,
			Email:     user.Email,
			Workspace: workspace.ID,
			Role:      auth.RoleOwner,
		})
		if _, err := sqlb.InsertRows(&tasks.Membership{
			UserID: user.ID,
			Role:   tasks.MembershipRoleOwner,
		}).Exec(ctx, a.hooked(tx)); err != nil {
			return fmt.Errorf("creating the membership: %w", err)
		}

		out.Status = http.StatusCreated
		out.Body, err = a.issue(user, workspace.ID, auth.RoleOwner)
		return err
	})
	if err != nil {
		return nil, asHTTP(err)
	}
	return &out, nil
}

// login verifies the password and issues a token for one workspace.
func (a *authAPI) login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	email := strings.ToLower(strings.TrimSpace(in.Body.Email))

	user, err := sqlb.Query[tasks.User]().
		Where(tasks.UserCols.Email.Eq(email)).
		One(ctx, a.sys)

	switch {
	case errors.Is(err, sqlb.ErrNotFound):
		// Hash the supplied password against a throwaway hash anyway.
		//
		// Returning early here would make "unknown account" measurably faster
		// than "wrong password", and a few hundred milliseconds is not a subtle
		// difference — it turns the login endpoint into an account enumerator
		// that works over the public internet.
		_ = auth.CheckPassword(decoyHash(), in.Body.Password)
		return nil, errBadCredentials()
	case err != nil:
		return nil, fmt.Errorf("looking up the user: %w", err)
	}

	if err := auth.CheckPassword(user.PasswordHash, in.Body.Password); err != nil {
		return nil, errBadCredentials()
	}

	membership, workspaceID, err := a.membershipFor(ctx, user.ID, in.Body.Workspace)
	if err != nil {
		return nil, err
	}

	body, err := a.issue(user, workspaceID, string(membership.Role))
	if err != nil {
		return nil, err
	}
	return &loginOutput{Body: body}, nil
}

// me answers from the token plus two scoped reads.
func (a *authAPI) me(ctx context.Context, _ *struct{}) (*meOutput, error) {
	claims, err := claimsOrError(ctx)
	if err != nil {
		return nil, err
	}

	// These go through the hooked handle, so they are subject to exactly the
	// same scoping as any other read — including the user hook, which is what
	// makes "the user in the token" and "a user this token may see" the same
	// question.
	user, err := sqlb.Query[tasks.User]().
		Where(tasks.UserCols.ID.Eq(claims.Subject)).
		One(ctx, a.app())
	if err != nil {
		return nil, asHTTP(err)
	}
	workspace, err := sqlb.Query[tasks.Workspace]().One(ctx, a.app())
	if err != nil {
		return nil, asHTTP(err)
	}

	// The switch list is deliberately *not* scoped: its whole purpose is to name
	// the workspaces outside the current one, so it uses the system handle and a
	// predicate written here instead.
	memberships, err := sqlb.Query[tasks.Membership]().
		Where(tasks.MembershipCols.UserID.Eq(claims.Subject)).
		All(ctx, a.sys)
	if err != nil {
		return nil, fmt.Errorf("listing memberships: %w", err)
	}
	ids := make([]any, 0, len(memberships))
	for _, m := range memberships {
		ids = append(ids, m.WorkspaceID)
	}
	var switchable []tasks.Workspace
	if len(ids) > 0 {
		switchable, err = sqlb.Query[tasks.Workspace]().
			Where(sqlb.F("id").OneOf(ids...)).
			OrderBy(tasks.WorkspaceCols.Name.Asc()).
			All(ctx, a.sys)
		if err != nil {
			return nil, fmt.Errorf("listing workspaces: %w", err)
		}
	}

	var out meOutput
	out.Body.User = user
	out.Body.Workspace = workspace
	out.Body.Role = claims.Role
	out.Body.Memberships = switchable
	return &out, nil
}

// membershipFor resolves which workspace a login is for.
func (a *authAPI) membershipFor(ctx context.Context, userID, slug string) (tasks.Membership, string, error) {
	var zero tasks.Membership

	if slug != "" {
		workspace, err := sqlb.Query[tasks.Workspace]().
			Where(tasks.WorkspaceCols.Slug.Eq(slugify(slug))).
			One(ctx, a.sys)
		if errors.Is(err, sqlb.ErrNotFound) {
			// Not "no such workspace": that would let anyone with an account
			// enumerate which workspaces exist. Same answer as not being a
			// member of one that does.
			return zero, "", errNoSuchMembership()
		}
		if err != nil {
			return zero, "", fmt.Errorf("looking up the workspace: %w", err)
		}
		m, err := sqlb.Query[tasks.Membership]().
			Where(tasks.MembershipCols.UserID.Eq(userID),
				tasks.MembershipCols.WorkspaceID.Eq(workspace.ID)).
			One(ctx, a.sys)
		if errors.Is(err, sqlb.ErrNotFound) {
			return zero, "", errNoSuchMembership()
		}
		if err != nil {
			return zero, "", fmt.Errorf("looking up the membership: %w", err)
		}
		return m, workspace.ID, nil
	}

	m, err := sqlb.Query[tasks.Membership]().
		Where(tasks.MembershipCols.UserID.Eq(userID)).
		OrderBy(tasks.MembershipCols.CreatedAt.Asc()).
		Limit(1).
		One(ctx, a.sys)
	if errors.Is(err, sqlb.ErrNotFound) {
		// Reachable only if a membership was removed after the account was
		// made. The account is real and can do nothing, which is worth saying
		// plainly rather than answering "bad credentials".
		return zero, "", huma.Error403Forbidden("this account belongs to no workspace")
	}
	if err != nil {
		return zero, "", fmt.Errorf("looking up the membership: %w", err)
	}
	return m, m.WorkspaceID, nil
}

func (a *authAPI) issue(user tasks.User, workspaceID, role string) (tokenBody, error) {
	token, err := a.signer.Sign(auth.Claims{
		Subject:   user.ID,
		Email:     user.Email,
		Workspace: workspaceID,
		Role:      role,
	})
	if err != nil {
		return tokenBody{}, fmt.Errorf("signing the token: %w", err)
	}
	return tokenBody{
		Token:     token,
		ExpiresAt: time.Now().Add(a.signer.TTL()),
		UserID:    user.ID,
		Workspace: workspaceID,
		Role:      role,
	}, nil
}

// errBadCredentials is one answer for two causes, on purpose: "no such account"
// and "wrong password" told apart is an account enumerator.
func errBadCredentials() error {
	return huma.Error401Unauthorized("the email address or password is not correct")
}

func errNoSuchMembership() error {
	return huma.Error403Forbidden("this account is not a member of that workspace")
}

// asHTTP passes a huma.StatusError through and leaves anything else alone, so
// that a 409 raised inside a transaction is not flattened into a 500 by the
// wrapping that carried it out.
func asHTTP(err error) error {
	var status huma.StatusError
	if errors.As(err, &status) {
		return status
	}
	if errors.Is(err, sqlb.ErrNotFound) {
		return huma.Error404NotFound("not found")
	}
	return err
}

// decoyHash is a real hash of a value nobody knows, computed once. Checking a
// password against it costs what checking against a real one costs, which is
// the entire point.
var decoyHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword("decoy: this exists only to burn the same CPU as a real check")
	if err != nil {
		// Unreachable: HashPassword fails only on an empty password or a broken
		// crypto/rand, and the second means far worse things are wrong.
		panic(fmt.Sprintf("app: hashing the decoy password: %v", err))
	}
	return h
})

// slugify reduces a workspace name to a URL-safe identifier. Runs of anything
// that is not a letter or digit collapse to a single "-".
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	if b.Len() == 0 {
		return "workspace"
	}
	return b.String()
}
