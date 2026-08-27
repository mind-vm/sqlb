# example/auth-supabase — a sqlb.Verifier[T] adapter for Supabase Auth

A `sqlb.Verifier[T]` (see `auth.go` in the sqlb module) that verifies the
access tokens a Supabase project's Auth service signs, and maps a verified
token to the application's own principal type. It plugs into
`sqlb.Middleware[T]` exactly like this repository's other worked auth
examples: `example/auth-workos`'s AuthKit adapter, `example/tasks/auth`'s
hand-rolled HS256 JWT, and `example/fxapp/access`'s shared-secret bearer keys.

[docs/supabase.md](../../docs/supabase.md) is the whole integration — which
connection each component needs, what to point the shadow database at, and
how sqlb and Supabase's own PostgREST divide the work. This module is its
authentication half.

## Why its own module

sqlb core is dependency-locked to pgx only; this package needs a JWT/JWKS
library ([`golang-jwt/jwt/v5`](https://github.com/golang-jwt/jwt),
[`MicahParks/keyfunc/v3`](https://github.com/MicahParks/keyfunc)), which
sqlb's own `go.mod` may not carry. It lives under `example/` with its own
`go.mod` for exactly that reason — see [docs/architecture.md's "A Verifier
composes with the principal
seam"](../../docs/architecture.md#a-verifier-composes-with-the-principal-seam).

## Using it

```go
type Principal struct {
	UserID string // the auth.users row this caller is
	Email  string
	Tenant string
}

func mapClaims(c authsupabase.Claims) Principal {
	tenant, _ := c.AppMetadata["tenant_id"].(string)
	return Principal{UserID: c.Subject, Email: c.Email, Tenant: tenant}
}

// A project signing with asymmetric keys: verified against the JWKS the
// project publishes. ctx should be the application's root context — keyfunc
// ties its background key rotation to it.
verifier, err := authsupabase.New(ctx, "https://<ref>.supabase.co", mapClaims)
if err != nil {
	log.Fatal(err) // the JWKS is fetched here, at startup
}

// A project still on the legacy shared secret, which needs no network call:
//	verifier, err := authsupabase.NewWithSecret(projectURL, jwtSecret, mapClaims)

mux.Handle("/", sqlb.Middleware[Principal](verifier, sqlb.BearerToken)(handler))
```

A verified request carries its `Principal` through
`sqlb.PrincipalFrom[Principal](ctx)`, same as every other adapter behind this
seam — and that is what a `BeforeQuery` hook reads to scope every query to the
caller, which is where authorization lives in a sqlb application rather than in
RLS policies.

## What it refuses, and why that matters here

A project's **anon key is a JWT signed by the same project**, and it is
published in every browser bundle. A verifier that checked only the signature
would accept it as a signed-in caller — and would accept the `service_role`
key with even more authority. `Verify` refuses any token whose `role` claim is
not `authenticated`, and refuses one carrying no `sub`.

An *anonymous sign-in* is a different thing and is allowed through: it is a
real `auth.users` row, and it reaches the mapper as `Claims.IsAnonymous` so
the application can decide what such a caller may do.

The rest is the usual list: the signature, the project's own issuer, a
required expiry, and a pinned algorithm — `ES256`/`RS256` against the key set,
`HS256` against the secret — so a token signed with a public key's bytes as an
HMAC secret is refused rather than trusted.

## Testing

```bash
mise run test-auth-supabase
```

Database-free and network-free, like `test-auth-workos`: tokens are minted in
the test against a freshly generated key, and the asymmetric half verifies
against an in-memory JWKS rather than a live project.
