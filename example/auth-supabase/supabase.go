// Package authsupabase verifies Supabase Auth access tokens — the JWTs a
// Supabase project signs and hands to a client after login, presented back to
// this application as a bearer credential — and maps them to the
// application's own principal type.
//
// It implements sqlb.Verifier[T] (see auth.go in the sqlb module), so it plugs
// into sqlb.Middleware[T] exactly like this repository's other worked auth
// examples: example/auth-workos's AuthKit adapter, example/tasks/auth's
// hand-rolled HS256 JWT, and example/fxapp/access's shared-secret bearer keys.
//
// # Two signing regimes, one Verifier
//
// A Supabase project signs its tokens either with asymmetric keys published at
// the project's JWKS endpoint — [New] — or with the legacy shared secret from
// the project's settings — [NewWithSecret]. Which one a project uses is a
// setting rather than a property of the application, and moving between them
// should not be a code change beyond the constructor, so both build the same
// Verifier and Verify behaves identically.
//
// # The anon key is a valid signature
//
// A project's publishable anon key and its service_role key are, in the legacy
// regime, JWTs signed with the same secret that signs a user's access token.
// The anon key is *published* — it ships in every browser bundle — so a
// verifier that checks only the signature accepts it as a logged-in caller,
// and the service_role key would arrive with even more authority than the user
// it impersonates. What separates them is the "role" claim, and Verify refuses
// any token whose role is not "authenticated" for that reason. The issuer
// check would catch today's anon keys too, since they name "supabase" rather
// than the project, but that is a fact about how one vintage of key happens to
// be minted; the role claim is what Supabase documents as the caller's role.
//
// An anonymous sign-in is a different thing and is *not* refused: it is a real
// user row with role "authenticated" and is_anonymous true, which reaches the
// mapper as [Claims.IsAnonymous] — whether such a caller may do anything is
// the application's rule to write, not this package's to make.
//
// # Why a separate module
//
// sqlb core is dependency-locked to pgx only; this package needs a JWT/JWKS
// library, which sqlb's own go.mod may not carry. It lives under example/ with
// its own go.mod for exactly that reason — see docs/architecture.md's "A
// Verifier composes with the principal seam" decision.
//
// # Why Verify never returns sqlb.TransientError
//
// The same reason example/auth-workos gives: golang-jwt wraps whatever the
// Keyfunc callback returns as jwt.ErrTokenUnverifiable, and keyfunc's own
// error does not distinguish "the JWKS was fetched but does not contain this
// key" from "the JWKS could not be fetched at all". String-matching an error
// message to guess would be worse than not guessing, so every rejection is a
// plain error (401 via sqlb.Middleware). The one place an unreachable project
// surfaces cleanly is New, which fetches the JWKS synchronously and fails as
// an ordinary startup error. NewWithSecret makes no network call at all.
package authsupabase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// authenticatedRole is the "role" claim a signed-in user's token carries.
// Anything else — "anon", "service_role", or a custom role a hook set — is
// refused, for the reason the package comment gives.
const authenticatedRole = "authenticated"

// Claims is what Verify extracts from a validated Supabase access token. The
// registered claims — iss, exp, iat — are checked during verification and not
// carried forward: a caller that already trusts a Claims value has no use for
// re-inspecting them.
type Claims struct {
	// Subject is the Supabase user id, a UUID — "sub". It is the value that
	// joins to a row of auth.users, which is what a schema declaring
	// schema.ExternalRef("user", "auth.users.id").Enforced() points at.
	Subject string
	// Email and Phone are the identifiers the user signed in with, either of
	// which may be empty depending on the provider.
	Email string
	Phone string
	// SessionID is the session this token was issued for — "session_id".
	// Revoking a session invalidates its refresh token rather than this
	// access token, so an application that needs immediate revocation checks
	// this against its own store; one that does not, ignores it.
	SessionID string
	// AAL is the assurance level reached — "aal1" for a password or OAuth
	// sign-in, "aal2" once a second factor has been verified. An application
	// gating a sensitive route on MFA reads this.
	AAL string
	// IsAnonymous marks a token minted by an anonymous sign-in: a real user
	// row with no identity attached to it yet.
	IsAnonymous bool
	// AppMetadata is what the platform and its providers set — "provider",
	// "providers", and anything a project's own hooks add. It is not writable
	// by the user, which is what makes it the place a role or a tenant id
	// belongs.
	AppMetadata map[string]any
	// UserMetadata is what the user themself can write, through the client
	// SDK's updateUser. Read it for a display name; never for a permission.
	UserMetadata map[string]any
}

// Verifier implements sqlb.Verifier[T]: it verifies a Supabase access token
// and maps the result to T via the mapper supplied at construction.
//
// Exactly one of jwks and secret is set, by whichever constructor built it.
type Verifier[T any] struct {
	jwks    keyfunc.Keyfunc
	secret  []byte
	issuer  string
	methods []string
	mapper  func(Claims) T
}

// New returns a Verifier that checks tokens against the project's JWKS
// endpoint — projectURL + "/auth/v1/.well-known/jwks.json" — which is the
// regime a project using asymmetric signing keys is in.
//
// projectURL is the project's base URL, "https://<ref>.supabase.co", or the
// base URL of a self-hosted deployment. The issuer every token is checked
// against is derived from it, so a token minted by another project fails here
// rather than reaching the mapper.
//
// ctx should be long-lived — an application's own root context, not a
// per-request one — because keyfunc ties its background key-set refresh to it:
// cancelling ctx after New returns stops that refresh, not just the initial
// fetch. Rotating a signing key is a routine operation on a Supabase project,
// and a verifier whose refresh has stopped rejects every token minted after
// the next rotation.
func New[T any](ctx context.Context, projectURL string, mapper func(Claims) T) (*Verifier[T], error) {
	base, err := baseURL(projectURL)
	if err != nil {
		return nil, err
	}
	if mapper == nil {
		return nil, errors.New("authsupabase: mapper is nil")
	}
	jwksURL := base + "/auth/v1/.well-known/jwks.json"
	// false so New's initial JWKS fetch surfaces an error instead of being
	// silently swallowed — keyfunc.NewDefaultCtx defaults this to true.
	noErrorReturnFirst := false
	jwks, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{jwksURL}, keyfunc.Override{
		NoErrorReturnFirstHTTPReq: &noErrorReturnFirst,
	})
	if err != nil {
		return nil, fmt.Errorf("authsupabase: fetching JWKS from %s: %w", jwksURL, err)
	}
	return NewWithKeyfunc(jwks, base, mapper), nil
}

// NewWithKeyfunc builds a Verifier from an already-constructed
// keyfunc.Keyfunc, skipping New's network fetch. It is for tests backed by an
// in-memory key set (keyfunc.NewJWKSetJSON) and for a caller needing a key set
// New cannot express — a pinned one, or one backed by several URLs.
//
// projectURL is still required, because the issuer check does not come from
// the key set. A Verifier built with an empty one rejects every token rather
// than silently accepting a token minted by any project whose key happens to
// be in the set.
func NewWithKeyfunc[T any](jwks keyfunc.Keyfunc, projectURL string, mapper func(Claims) T) *Verifier[T] {
	base, err := baseURL(projectURL)
	if err != nil {
		base = ""
	}
	return &Verifier[T]{
		jwks:   jwks,
		issuer: issuerOf(base),
		// Supabase's asymmetric keys are ECDSA P-256 by default and RSA by
		// choice. Pinning the accepted algorithms before the signature is
		// used is what closes the confusion attack where a token signed with
		// the *public* key's bytes as an HMAC secret is presented as HS256 —
		// see example/tasks/auth/jwt.go for why this is one of the three
		// mistakes that turn a JWT verifier into a bypass.
		methods: []string{"ES256", "RS256"},
		mapper:  mapper,
	}
}

// NewWithSecret returns a Verifier for a project still signing with the legacy
// shared secret — the "JWT secret" in the project's API settings — which is
// HS256 and needs no network call at all.
//
// The secret verifies tokens and could also mint them. Nothing here mints
// anything, but the value is as sensitive as a database password and belongs
// in the same place: a project that has moved to asymmetric keys should use
// [New] instead and stop shipping this value to its servers.
func NewWithSecret[T any](projectURL, secret string, mapper func(Claims) T) (*Verifier[T], error) {
	base, err := baseURL(projectURL)
	if err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, errors.New("authsupabase: the JWT secret is empty")
	}
	if mapper == nil {
		return nil, errors.New("authsupabase: mapper is nil")
	}
	return &Verifier[T]{
		secret:  []byte(secret),
		issuer:  issuerOf(base),
		methods: []string{"HS256"},
		mapper:  mapper,
	}, nil
}

// Verify checks cred as a Supabase access token: signature, issuer and expiry
// first, then the two claims that decide whether this is a signed-in user at
// all — a "role" of "authenticated", and a non-empty "sub".
func (v *Verifier[T]) Verify(_ context.Context, cred string) (T, error) {
	var zero T

	// A Verifier built by NewWithKeyfunc cannot fail at construction, so a bad
	// configuration is caught here instead — rejecting every token rather than
	// letting the issuer check degrade to "" == "" or the mapper panic on the
	// first valid one.
	if v.issuer == "" {
		return zero, errors.New("authsupabase: the project URL is empty, so no issuer can be checked")
	}
	if v.mapper == nil {
		return zero, errors.New("authsupabase: mapper is nil")
	}

	// v.jwks is an interface, so the method value has to be taken behind the
	// nil check rather than in front of it.
	keyfn := jwt.Keyfunc(func(*jwt.Token) (any, error) { return v.secret, nil })
	if v.jwks != nil {
		keyfn = v.jwks.Keyfunc
	}

	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(cred, claims, keyfn,
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods(v.methods),
	); err != nil {
		return zero, fmt.Errorf("authsupabase: %w", err)
	}

	if role := stringClaim(claims, "role"); role != authenticatedRole {
		return zero, fmt.Errorf("authsupabase: token role is %q, not %q; a project's anon and "+
			"service_role keys are signed by the same project and are not a signed-in user",
			role, authenticatedRole)
	}
	subject := stringClaim(claims, "sub")
	if subject == "" {
		return zero, errors.New("authsupabase: token carries no subject, so it identifies no user")
	}

	isAnonymous, _ := claims["is_anonymous"].(bool)
	return v.mapper(Claims{
		Subject:      subject,
		Email:        stringClaim(claims, "email"),
		Phone:        stringClaim(claims, "phone"),
		SessionID:    stringClaim(claims, "session_id"),
		AAL:          stringClaim(claims, "aal"),
		IsAnonymous:  isAnonymous,
		AppMetadata:  mapClaim(claims, "app_metadata"),
		UserMetadata: mapClaim(claims, "user_metadata"),
	}), nil
}

// baseURL normalises a project URL to its origin, without a trailing slash.
//
// A caller who pastes the issuer or the JWKS URL out of a dashboard rather
// than the project's base URL is the mistake worth absorbing: both end in a
// path this package appends its own to, and the resulting 404 at startup names
// a URL nobody wrote.
func baseURL(projectURL string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(projectURL), "/")
	if base == "" {
		return "", errors.New("authsupabase: projectURL is empty; it is the project's base URL, " +
			"\"https://<ref>.supabase.co\"")
	}
	for _, suffix := range []string{"/auth/v1/.well-known/jwks.json", "/auth/v1"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return "", fmt.Errorf("authsupabase: projectURL %q has no scheme; it is the project's "+
			"base URL, \"https://<ref>.supabase.co\"", projectURL)
	}
	return base, nil
}

// issuerOf is the "iss" claim a project's tokens carry, which is its auth
// service's base path rather than the project's.
func issuerOf(base string) string {
	if base == "" {
		return ""
	}
	return base + "/auth/v1"
}

func stringClaim(claims jwt.MapClaims, key string) string {
	s, _ := claims[key].(string)
	return s
}

func mapClaim(claims jwt.MapClaims, key string) map[string]any {
	m, _ := claims[key].(map[string]any)
	return m
}
