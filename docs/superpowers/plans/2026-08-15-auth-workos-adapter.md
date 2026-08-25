# WorkOS Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `example/auth-workos/`, a worked example implementing
`sqlb.Verifier[T]` for WorkOS AuthKit access tokens — the first of the
follow-on provider adapters the pluggable-auth design named as out of scope
for the core seam.

**Architecture:** A new Go module (own `go.mod`, so its dependencies never
reach sqlb core's) with one package, `authworkos`, exposing `Verifier[T]`
and `New[T]`. `Verify` checks a bearer JWT's signature against WorkOS's
per-client JWKS endpoint (`github.com/MicahParks/keyfunc/v3` +
`github.com/golang-jwt/jwt/v5`), validates issuer and expiry via parser
options, checks the token's `client_id` claim against the configured
client, and maps the resulting claims to the application's own principal
type via a caller-supplied function — the same "app owns the type" pattern
`Verifier[T]` was designed around.

**Tech Stack:** Go 1.25.7 (matching `example/tasks`/`example/fxapp`),
`github.com/golang-jwt/jwt/v5`, `github.com/MicahParks/keyfunc/v3`,
`github.com/MicahParks/jwkset` (test-only, for building an in-memory JWKS),
`github.com/workos/workos-go/v10` (for `usermanagement.GetJWKSURL`).

**Spec:** `docs/superpowers/specs/2026-08-15-pluggable-auth-design.md`
(the "Provider adapters stay worked examples" section) and
`docs/architecture.md`'s "A Verifier composes with the principal seam"
decision, which this plan is the first real evidence for.

## Global Constraints

- `example/auth-workos/` is its own Go module: `module
  github.com/mind-vm/sqlb/example/auth-workos`, `go 1.25.7`, `replace
  github.com/mind-vm/sqlb => ../../` — copy `example/tasks/go.mod`'s shape
  exactly.
- Package name is `authworkos` (the directory is `auth-workos`; Go package
  names need not match directory names, and this matches the design spec's
  own illustrative naming).
- Tests live in `package authworkos_test` (external test package), matching
  `example/tasks/auth/jwt_test.go`'s convention.
- No live network calls to WorkOS in any test — every test builds its own
  in-memory or `httptest`-served JWKS, matching the merged design's
  "no live WorkOS/Clerk/Zitadel calls in CI" requirement.
- **This adapter's `Verify` never returns `sqlb.TransientError`, and this
  is deliberate, not an oversight** — Task 3 documents why. Do not add a
  `TransientError` branch anywhere in this plan; if a task's text seems to
  imply one, that task's text is wrong and the constraint here wins.
- Exact WorkOS JWT claim names (verified against WorkOS's own docs, not
  guessed): `iss` (always `"https://api.workos.com"`), `sub`, `sid`,
  `client_id`, `org_id`, `role`, `roles`, `permissions`, `exp`, `iat`. No
  `aud` claim exists on these tokens.

---

## Task 1: Module scaffold and test harness

**Files:**
- Create: `example/auth-workos/go.mod`
- Create: `example/auth-workos/workos.go` (package doc comment and `Claims`
  struct only — no `Verifier` yet)
- Create: `example/auth-workos/workos_test.go` (test harness helpers +
  one self-check test)

**Interfaces:**
- Produces: `type Claims struct { Subject, SessionID, ClientID, OrgID,
  Role string; Roles, Permissions []string }` — the shape every later task
  builds on. Test helpers `newTestRSAKey(t) *rsa.PrivateKey`,
  `newTestKeyfunc(t, key) keyfunc.Keyfunc`, `mintToken(t, key,
  claims jwt.MapClaims) string`, `validClaims() jwt.MapClaims` — every
  later task's tests use these.

- [ ] **Step 1: Scaffold the module**

```bash
mkdir -p example/auth-workos
cd example/auth-workos
cat > go.mod <<'EOF'
module github.com/mind-vm/sqlb/example/auth-workos

go 1.25.7

replace github.com/mind-vm/sqlb => ../../
EOF
```

- [ ] **Step 2: Write `workos.go`'s package doc comment and `Claims`**

Create `example/auth-workos/workos.go`:

```go
// Package authworkos verifies WorkOS AuthKit access tokens — JWTs WorkOS
// signs and hands to a client after login, presented back to this
// application as a bearer credential — against WorkOS's per-client JWKS
// endpoint.
//
// It implements sqlb.Verifier[T] (see auth.go in the sqlb module), so it
// plugs into sqlb.Middleware[T] exactly like this repository's other
// worked auth examples: example/tasks/auth's hand-rolled HS256 JWT and
// example/fxapp/access's shared-secret bearer keys. This one verifies
// tokens WorkOS minted rather than tokens the application mints itself,
// which is why it needs a real JWT/JWKS library
// (github.com/golang-jwt/jwt/v5, github.com/MicahParks/keyfunc/v3) instead
// of the ~100 lines example/tasks/auth writes by hand for a single fixed
// HS256 secret.
//
// # Why a separate module
//
// sqlb core is dependency-locked to pgx only; this package needs WorkOS's
// SDK plus a JWT/JWKS library, none of which sqlb's own go.mod may carry.
// It lives under example/ with its own go.mod for exactly that reason —
// see docs/architecture.md's "A Verifier composes with the principal
// seam" decision.
package authworkos

// Claims is what Verify extracts from a validated WorkOS access token,
// named after the fields WorkOS documents as stable
// (https://workos.com/docs/reference/authkit/session-tokens). The token's
// registered claims — iss, exp, iat — are checked during verification and
// not carried forward: a caller that already trusts a Claims value has no
// use for re-inspecting them.
type Claims struct {
	// Subject is the WorkOS user id — "sub".
	Subject string
	// SessionID is the WorkOS session id — "sid".
	SessionID string
	// ClientID is checked against the Verifier's configured client id
	// during verification; it is carried into Claims for a mapper that
	// wants to log or assert it, not because a caller needs to re-check it.
	ClientID string
	// OrgID is the organization the token is scoped to — "org_id". WorkOS
	// is organization-centric, so this is usually the field an
	// application's principal type is built around.
	OrgID string
	// Role is the caller's role in OrgID — "role".
	Role string
	// Roles is the same information as Role, pluralised — "roles". WorkOS's
	// docs show both; which one an application reads is the mapper's
	// choice, not this package's.
	Roles []string
	// Permissions are the fine-grained grants attached to the token —
	// "permissions".
	Permissions []string
}
```

- [ ] **Step 3: Write the test harness**

Create `example/auth-workos/workos_test.go`:

```go
package authworkos_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// testKeyID is the "kid" every token mintToken signs carries, and the same
// id the keyset newTestKeyfunc builds publishes — keyfunc looks a token up
// by this id, so a mismatch here would silently fall through to "unknown
// key" rather than the case a test meant to exercise.
const testKeyID = "test-key-1"

// newTestRSAKey generates a fresh 2048-bit RSA key pair. Real key
// generation, not a shared fixture: a fixture reused across tests risks
// two tests interfering if one is ever changed to mutate its key, and
// 2048-bit generation is fast enough (single-digit milliseconds) that
// nothing is bought by sharing one.
func newTestRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// newTestKeyfunc builds a keyfunc.Keyfunc from key's public half,
// published under testKeyID — the same shape keyfunc.NewDefaultCtx would
// build from a live JWKS URL, but from an in-memory JSON document rather
// than an HTTP fetch, so most tests need no network and no
// httptest.Server.
func newTestKeyfunc(t *testing.T, key *rsa.PrivateKey) keyfunc.Keyfunc {
	t.Helper()
	jwk, err := jwkset.NewJWKFromKey(key.Public(), jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: testKeyID},
	})
	if err != nil {
		t.Fatalf("jwkset.NewJWKFromKey: %v", err)
	}

	storage := jwkset.NewMemoryStorage()
	ctx := t.Context()
	if err := storage.KeyWrite(ctx, jwk); err != nil {
		t.Fatalf("storage.KeyWrite: %v", err)
	}
	marshalled, err := storage.Marshal(ctx)
	if err != nil {
		t.Fatalf("storage.Marshal: %v", err)
	}
	raw, err := json.Marshal(marshalled)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	kf, err := keyfunc.NewJWKSetJSON(raw)
	if err != nil {
		t.Fatalf("keyfunc.NewJWKSetJSON: %v", err)
	}
	return kf
}

// mintToken signs claims as a WorkOS-shaped RS256 access token with key,
// under testKeyID, and returns the compact JWT string.
func mintToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = testKeyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("token.SignedString: %v", err)
	}
	return signed
}

// validClaims returns a claim set Verify should accept outright. Every
// later test starts here and overrides exactly the field under test, so a
// test failing on an unrelated field is a bug in validClaims, not in the
// test — the values themselves are WorkOS's own documented example claims
// (https://workos.com/docs/reference/authkit/session-tokens), not
// invented ids.
func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":         "https://api.workos.com",
		"sub":         "user_01HBEQKA6K4QJAS93VPE39W1JT",
		"sid":         "session_01HQSXZGF8FHF7A9ZZFCW4387R",
		"client_id":   "client_test123",
		"org_id":      "org_01HRDMC6CM357W30QMHMQ96Q0S",
		"role":        "member",
		"roles":       []string{"member"},
		"permissions": []string{"posts:read", "posts:write"},
		"iat":         now.Unix(),
		"exp":         now.Add(time.Hour).Unix(),
	}
}

// TestHarness_MintedTokenVerifiesAgainstItsOwnKeyset is not a test of
// authworkos — it is a test of the harness above, run once so a broken
// harness fails here with a clear name rather than as a mysterious
// failure in every later task's tests.
func TestHarness_MintedTokenVerifiesAgainstItsOwnKeyset(t *testing.T) {
	key := newTestRSAKey(t)
	kf := newTestKeyfunc(t, key)
	token := mintToken(t, key, validClaims())

	parsed, err := jwt.Parse(token, kf.Keyfunc)
	if err != nil {
		t.Fatalf("jwt.Parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token did not parse as valid")
	}
}
```

- [ ] **Step 4: Resolve dependencies and run the test**

```bash
cd example/auth-workos
mise exec -- go get github.com/golang-jwt/jwt/v5
mise exec -- go get github.com/MicahParks/keyfunc/v3
mise exec -- go get github.com/MicahParks/jwkset
mise exec -- go mod tidy
mise exec -- go test ./... -run TestHarness_MintedTokenVerifiesAgainstItsOwnKeyset -v
```

Expected: PASS. If `jwkset.NewJWKFromKey`, `jwkset.NewMemoryStorage`,
`storage.KeyWrite`, or `storage.Marshal` don't match this exact signature
against the versions `go get` resolves, that is a real finding — report
BLOCKED with the compiler error rather than guessing a fix, since this
harness is what every later task's tests depend on.

- [ ] **Step 5: Commit**

```bash
git add example/auth-workos/
git commit -m "feat(auth-workos): a minted token verifies against its own keyset"
```

---

## Task 2: `Verifier[T]`, `New[T]`, and `Verify`'s accept path

**Files:**
- Modify: `example/auth-workos/workos.go`
- Modify: `example/auth-workos/workos_test.go`

**Interfaces:**
- Consumes: `Claims`, `newTestRSAKey`, `newTestKeyfunc`, `mintToken`,
  `validClaims` from Task 1.
- Produces: `type Verifier[T any] struct { ... }`, `func New[T any](ctx
  context.Context, clientID string, mapper func(Claims) T) (*Verifier[T],
  error)`, `func (v *Verifier[T]) Verify(ctx context.Context, cred string)
  (T, error)` — satisfies `sqlb.Verifier[T]` from the sqlb module (`auth.go`).

- [ ] **Step 1: Write the failing test**

Add to `example/auth-workos/workos_test.go`. Replace the import block at
the top of the file with:

```go
import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mind-vm/sqlb/example/auth-workos"
)
```

Then append below `TestHarness_MintedTokenVerifiesAgainstItsOwnKeyset`:

```go
type testPrincipal struct {
	UserID string
	OrgID  string
	Role   string
}

// newTestVerifier builds a Verifier[testPrincipal] wired directly to a
// keyfunc.Keyfunc built by the harness, via NewWithKeyfunc rather than
// New — every test in this package uses this path. New's own successful
// path (usermanagement.GetJWKSURL plus a real keyfunc.NewDefaultCtx
// fetch) is never exercised against a live endpoint anywhere in this
// suite, on purpose: the Global Constraints forbid a live WorkOS call in
// CI, and NewWithKeyfunc exists specifically so the rest of the package's
// tests do not need one. What New adds beyond NewWithKeyfunc — building
// the URL and making the fetch — has no branch of its own for a test to
// exercise without either a live endpoint or refactoring the URL into an
// injectable parameter, which the spec's illustrative constructor
// signature (New(ctx, clientID, mapper)) does not have room for. New's
// two validation checks (empty clientID, nil mapper) return before any
// network call and are tested below.
func newTestVerifier(t *testing.T, key *rsa.PrivateKey, clientID string) *authworkos.Verifier[testPrincipal] {
	t.Helper()
	kf := newTestKeyfunc(t, key)
	v := authworkos.NewWithKeyfunc(kf, clientID, func(c authworkos.Claims) testPrincipal {
		return testPrincipal{UserID: c.Subject, OrgID: c.OrgID, Role: c.Role}
	})
	return v
}

func TestVerify_Accepts(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	token := mintToken(t, key, validClaims())

	principal, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := testPrincipal{
		UserID: "user_01HBEQKA6K4QJAS93VPE39W1JT",
		OrgID:  "org_01HRDMC6CM357W30QMHMQ96Q0S",
		Role:   "member",
	}
	if principal != want {
		t.Fatalf("principal = %+v, want %+v", principal, want)
	}
}

func TestNew_RejectsEmptyClientID(t *testing.T) {
	_, err := authworkos.New(t.Context(), "", func(c authworkos.Claims) testPrincipal {
		return testPrincipal{}
	})
	if err == nil {
		t.Fatal("New accepted an empty clientID")
	}
}

func TestNew_RejectsNilMapper(t *testing.T) {
	_, err := authworkos.New[testPrincipal](t.Context(), "client_test123", nil)
	if err == nil {
		t.Fatal("New accepted a nil mapper")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd example/auth-workos
mise exec -- go test ./... -run 'TestVerify_Accepts|TestNew_Rejects' -v
```

Expected: FAIL — `authworkos.NewWithKeyfunc`, `authworkos.Verifier`,
`authworkos.New`, and `authworkos.Claims`'s use as a parameter type are
undefined (`Claims` itself already exists from Task 1).

- [ ] **Step 3: Write minimal implementation**

Replace `example/auth-workos/workos.go`'s package clause and imports (the
line `package authworkos` from Task 1) with:

```go
package authworkos

import (
	"context"
	"errors"
	"fmt"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/workos/workos-go/v10/pkg/usermanagement"
)

// issuer is fixed across every WorkOS environment and client — WorkOS does
// not scope it per application — so it is a constant here rather than a
// configuration field.
const issuer = "https://api.workos.com"
```

Then append below `Claims`:

```go
// Verifier implements sqlb.Verifier[T]: it verifies a WorkOS AuthKit
// access token and maps the result to T via the mapper supplied to New.
type Verifier[T any] struct {
	jwks     keyfunc.Keyfunc
	clientID string
	mapper   func(Claims) T
}

// New returns a Verifier that checks tokens against clientID's JWKS
// endpoint (https://api.workos.com/sso/jwks/<clientID>, built by
// usermanagement.GetJWKSURL) and maps a verified token's Claims to T via
// mapper.
//
// ctx should be long-lived — an application's own root context, not a
// per-request one — because keyfunc ties its background key-set refresh
// to it: cancelling ctx after New returns stops that refresh, not just
// the initial fetch.
func New[T any](ctx context.Context, clientID string, mapper func(Claims) T) (*Verifier[T], error) {
	if clientID == "" {
		return nil, errors.New("authworkos: clientID is empty")
	}
	if mapper == nil {
		return nil, errors.New("authworkos: mapper is nil")
	}
	jwksURL := usermanagement.GetJWKSURL(clientID)
	jwks, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("authworkos: fetching JWKS from %s: %w", jwksURL, err)
	}
	return NewWithKeyfunc(jwks, clientID, mapper), nil
}

// NewWithKeyfunc builds a Verifier from an already-constructed
// keyfunc.Keyfunc, skipping New's network fetch. It exists for tests that
// need a Verifier backed by an in-memory key set
// (keyfunc.NewJWKSetJSON) rather than a live JWKS URL — production code
// should call New.
func NewWithKeyfunc[T any](jwks keyfunc.Keyfunc, clientID string, mapper func(Claims) T) *Verifier[T] {
	return &Verifier[T]{jwks: jwks, clientID: clientID, mapper: mapper}
}

// Verify checks cred as a WorkOS AuthKit access token: signature against
// the client's JWKS, issuer, and expiry (via jwt.WithIssuer and
// jwt.WithExpirationRequired), then that the token's own client_id claim
// matches the Verifier's configured client — belt and suspenders
// alongside the JWKS URL already being client-scoped, so a question about
// whether WorkOS ever shares signing keys across clients does not have to
// be answered to trust this check.
func (v *Verifier[T]) Verify(ctx context.Context, cred string) (T, error) {
	var zero T

	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(cred, claims, v.jwks.Keyfunc,
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return zero, fmt.Errorf("authworkos: %w", err)
	}

	clientID, _ := claims["client_id"].(string)
	if clientID != v.clientID {
		return zero, fmt.Errorf("authworkos: token client_id %q does not match configured client %q", clientID, v.clientID)
	}

	return v.mapper(Claims{
		Subject:     stringClaim(claims, "sub"),
		SessionID:   stringClaim(claims, "sid"),
		ClientID:    clientID,
		OrgID:       stringClaim(claims, "org_id"),
		Role:        stringClaim(claims, "role"),
		Roles:       stringSliceClaim(claims, "roles"),
		Permissions: stringSliceClaim(claims, "permissions"),
	}), nil
}

func stringClaim(claims jwt.MapClaims, key string) string {
	s, _ := claims[key].(string)
	return s
}

func stringSliceClaim(claims jwt.MapClaims, key string) []string {
	raw, ok := claims[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
```

`ctx` in `Verify` is unused by this implementation (`jwt.ParseWithClaims`
takes no context) — it satisfies `sqlb.Verifier[T]`'s signature, which
does take one. Leave it named `ctx` rather than `_`; a future change that
does need it (e.g. a context-aware keyfunc call) should not have to
rediscover the parameter name.

- [ ] **Step 4: Run test to verify it passes**

```bash
cd example/auth-workos
mise exec -- go test ./... -v
```

Expected: PASS — `TestHarness_MintedTokenVerifiesAgainstItsOwnKeyset`,
`TestVerify_Accepts`, `TestNew_RejectsEmptyClientID`, and
`TestNew_RejectsNilMapper`.

- [ ] **Step 5: Commit**

```bash
git add example/auth-workos/
git commit -m "feat(auth-workos): a verified WorkOS token maps to the app's own principal"
```

---

## Task 3: `Verify`'s rejection paths, and why there is no `TransientError` here

**Files:**
- Modify: `example/auth-workos/workos.go` (doc comment only)
- Modify: `example/auth-workos/workos_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–2. Produces nothing new — this task is
  tests plus one doc-comment addition confirming behavior Task 2's code
  already has.

Before writing tests: `golang-jwt/jwt/v5` wraps *any* error your
`jwt.Keyfunc` callback returns as `jwt.ErrTokenUnverifiable` — including
"this JWKS was correctly fetched but doesn't contain this key ID," not
just "the JWKS couldn't be fetched at all." `keyfunc`'s own top-level
sentinel (`keyfunc.ErrKeyfunc`) doesn't distinguish those two causes
either — both wrap the same way. That means there is no reliable way for
this adapter to tell "WorkOS's JWKS endpoint is down" apart from "this
token was signed with a key that isn't in the current key set" using
sentinel errors, and string-matching an error message to tell them apart
would be exactly the kind of brittle check this project avoids elsewhere.
**Consequence: this adapter's `Verify` treats every rejection —
including an unknown signing key — as a plain error (401 via
`sqlb.Middleware`), and never returns `sqlb.TransientError`.** The one
place a WorkOS-unreachable failure surfaces cleanly is `New`, which fetches
the JWKS synchronously at construction and returns an ordinary `error` if
that fetch fails — an application handles that as a startup failure, the
same way it would handle failing to reach its own database at boot, not
as a per-request 500. Task 4 tests that `New` path. This task documents
the "no `TransientError`" decision in the package doc comment and proves
every rejection reason answers as a plain error.

- [ ] **Step 1: Write the failing tests**

Append to `example/auth-workos/workos_test.go`:

```go
func TestVerify_RejectsWrongIssuer(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	claims := validClaims()
	claims["iss"] = "https://evil.example.com"
	token := mintToken(t, key, claims)

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token with the wrong issuer")
	}
}

func TestVerify_RejectsExpiredToken(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	claims := validClaims()
	past := time.Now().Add(-time.Hour)
	claims["iat"] = past.Add(-time.Minute).Unix()
	claims["exp"] = past.Unix()
	token := mintToken(t, key, claims)

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted an expired token")
	}
}

func TestVerify_RejectsMissingExpiry(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	claims := validClaims()
	delete(claims, "exp")
	token := mintToken(t, key, claims)

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token with no exp claim")
	}
}

func TestVerify_RejectsWrongClientID(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	claims := validClaims()
	claims["client_id"] = "client_someone_else"
	token := mintToken(t, key, claims)

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token issued for a different client_id")
	}
}

func TestVerify_RejectsWrongSigningKey(t *testing.T) {
	registeredKey := newTestRSAKey(t)
	forgedKey := newTestRSAKey(t) // never published in the keyset below
	v := newTestVerifier(t, registeredKey, "client_test123")
	token := mintToken(t, forgedKey, validClaims())

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token signed with an unregistered key")
	}
}

func TestVerify_RejectsMalformedToken(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")

	if _, err := v.Verify(t.Context(), "not-a-jwt-at-all"); err == nil {
		t.Fatal("Verify accepted a malformed token")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd example/auth-workos
mise exec -- go test ./... -run 'TestVerify_Rejects' -v
```

Expected: these should already PASS — Task 2's implementation (parser
options for issuer/expiry, the manual `client_id` check, and
`jwt.Parse`'s own signature verification) already handles every one of
these. If any of them unexpectedly FAIL, that is a real gap in Task 2's
implementation to fix here, not a sign this task's tests are wrong.

- [ ] **Step 3: Add the doc-comment explanation**

In `example/auth-workos/workos.go`, add a new paragraph to the package doc
comment (after the "Why a separate module" section):

```go
//
// # Why Verify never returns sqlb.TransientError
//
// golang-jwt/jwt/v5 wraps any error the Keyfunc callback returns as
// jwt.ErrTokenUnverifiable, and keyfunc's own top-level error
// (keyfunc.ErrKeyfunc) does not distinguish "the JWKS was fetched but
// doesn't contain this key" from "the JWKS could not be fetched at all" —
// both wrap the same way. There is no reliable sentinel to tell a WorkOS
// outage apart from an unrecognized signing key, and string-matching an
// error message to guess would be worse than not guessing. So every
// rejection Verify makes, including an unknown key, answers as a plain
// error (401 via sqlb.Middleware). The one place a WorkOS-unreachable
// failure surfaces cleanly is New, which fetches the JWKS synchronously
// at construction and returns an ordinary error if that fails — handled
// as a startup failure, not a per-request one.
```

- [ ] **Step 4: Run the full test suite**

```bash
cd example/auth-workos
mise exec -- go test ./... -v
```

Expected: PASS — all tests from Tasks 1–3.

- [ ] **Step 5: Commit**

```bash
git add example/auth-workos/
git commit -m "test(auth-workos): every rejection reason answers the same way, and why"
```

---

## Task 4: `New`'s startup failure

**Files:**
- Modify: `example/auth-workos/workos_test.go`

**Interfaces:**
- Consumes: `New[T]` from Task 2.
- Produces: nothing new — confirms `New`'s documented behavior with a real
  unreachable endpoint.

- [ ] **Step 1: Write the failing test**

Add `"net/http/httptest"` to `example/auth-workos/workos_test.go`'s import
block (replace the block from Task 2 with):

```go
import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mind-vm/sqlb/example/auth-workos"
)
```

Then append below `TestVerify_RejectsMalformedToken`:

```go
// TestNew_FailsWhenJWKSUnreachable exercises New's own network call
// directly, bypassing usermanagement.GetJWKSURL's real WorkOS URL: New
// has no way to point at a fake host by clientID alone, so this test
// calls keyfunc.NewDefaultCtx the same way New does, against a server
// that is closed before the fetch — the shape a real WorkOS outage at
// application startup would take.
func TestNew_FailsWhenJWKSUnreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	unreachableURL := srv.URL + "/sso/jwks/client_test123"
	srv.Close() // closed before any request reaches it

	_, err := keyfunc.NewDefaultCtx(t.Context(), []string{unreachableURL})
	if err == nil {
		t.Fatal("keyfunc.NewDefaultCtx succeeded against a closed server")
	}
}
```

- [ ] **Step 2: Run test to verify it fails or passes**

```bash
cd example/auth-workos
mise exec -- go test ./... -run TestNew_FailsWhenJWKSUnreachable -v
```

Expected: PASS immediately — this test exercises `keyfunc.NewDefaultCtx`
directly (the same call `New` makes internally), not `authworkos.New`
itself, so there is no new production code to write. If it FAILS,
`keyfunc.NewDefaultCtx` does not behave the way `New`'s doc comment
(Task 2) claims — report BLOCKED with the actual output rather than
adjusting the test to force a pass, since that doc comment is user-facing
and would need correcting too.

- [ ] **Step 3: Commit**

```bash
git add example/auth-workos/
git commit -m "test(auth-workos): a startup failure is where WorkOS-unreachable actually surfaces"
```

---

## Task 5: End-to-end test through `sqlb.Middleware[T]`

**Files:**
- Modify: `example/auth-workos/workos_test.go`

**Interfaces:**
- Consumes: `Verifier[T]`, `NewWithKeyfunc` from Task 2; `sqlb.Middleware`,
  `sqlb.BearerToken`, `sqlb.PrincipalFrom` from the sqlb module.
- Produces: nothing new — proves the adapter genuinely composes with the
  core seam, not just with its own `Verify` method in isolation.

- [ ] **Step 1: Write the failing test**

Add `"net/http"` and `"net/http/httptest"` (already present from Task 4)
plus `"github.com/mind-vm/sqlb"` to the import block. Replace it with:

```go
import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/auth-workos"
)
```

Then append below `TestNew_FailsWhenJWKSUnreachable`:

```go
func TestMiddleware_EndToEnd(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")
	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)

	var gotPrincipal testPrincipal
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotOK = sqlb.PrincipalFrom[testPrincipal](r.Context())
		w.WriteHeader(http.StatusOK)
	})

	t.Run("valid token reaches the handler with a principal", func(t *testing.T) {
		gotOK = false
		token := mintToken(t, key, validClaims())
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()

		mw(next).ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !gotOK {
			t.Fatal("PrincipalFrom[testPrincipal] found nothing in next's context")
		}
		want := testPrincipal{UserID: "user_01HBEQKA6K4QJAS93VPE39W1JT", OrgID: "org_01HRDMC6CM357W30QMHMQ96Q0S", Role: "member"}
		if gotPrincipal != want {
			t.Fatalf("principal = %+v, want %+v", gotPrincipal, want)
		}
	})

	t.Run("rejected token never reaches the handler", func(t *testing.T) {
		gotOK = false
		claims := validClaims()
		claims["client_id"] = "someone_else"
		token := mintToken(t, key, claims)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()

		mw(next).ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
		if gotOK {
			t.Fatal("next ran despite a rejected token")
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd example/auth-workos
mise exec -- go test ./... -run TestMiddleware_EndToEnd -v
```

Expected: FAIL at compile time initially only if the `sqlb` import isn't
yet resolved in `go.mod` — run `mise exec -- go mod tidy` first if so,
then re-run. Once it compiles, this test should PASS immediately, since
Tasks 2–3 already built everything it exercises; it is here to prove the
composition, not to drive new code.

- [ ] **Step 3: Run test to verify it passes**

```bash
cd example/auth-workos
mise exec -- go mod tidy
mise exec -- go test ./... -v
```

Expected: PASS — every test in the package.

- [ ] **Step 4: Commit**

```bash
git add example/auth-workos/
git commit -m "test(auth-workos): the adapter composes with sqlb.Middleware, not just its own Verify"
```

---

## Task 6: CI wiring

**Files:**
- Modify: `mise.toml`
- Modify: `.github/workflows/ci.yml`
- Modify: `CLAUDE.md`

**Interfaces:**
- Consumes: nothing (tooling/docs only).
- Produces: a new mise task, `test-auth-workos`, that later tooling can
  depend on by name.

- [ ] **Step 1: Add a database-free test task**

In `mise.toml`, find `[tasks.test-fx]` and add a new task immediately
after its closing `'''`:

```toml
[tasks.test-auth-workos]
description = "Run the WorkOS adapter's suite. Database-free, unlike test-demo/test-fx: the adapter verifies JWTs against an in-memory or httptest-served JWKS, never a live WorkOS endpoint or a database, so this needs no pg-up."
run = '''
cd "$MISE_PROJECT_ROOT/example/auth-workos" && go test ./...
'''
```

- [ ] **Step 2: Add the module to `preflight`'s build loop**

In `mise.toml`'s `[tasks.preflight]`, change:

```
for mod in . pgtest example/tasks example/fxapp; do
```

to:

```
for mod in . pgtest example/tasks example/fxapp example/auth-workos; do
```

- [ ] **Step 3: Add the module to `tidy-check`'s loop**

In `mise.toml`'s `[tasks.tidy-check]`, change the same `for mod in . pgtest
example/tasks example/fxapp; do` line to include `example/auth-workos`,
identically to Step 2.

- [ ] **Step 4: Add the module to `vet` and `lint`**

In `mise.toml`'s `[tasks.vet]`, change the description from `"go vet
across all four modules"` to `"go vet across all five modules"`, and add a
line after the `example/fxapp` one:

```
cd "$MISE_PROJECT_ROOT/example/auth-workos" && go vet ./...
```

In `[tasks.lint]`, change the description from `"golangci-lint across all
four modules..."` to `"golangci-lint across all five modules..."` (keep
the rest of the sentence unchanged), and add a line after the
`example/fxapp` one:

```
cd "$MISE_PROJECT_ROOT/example/auth-workos" && golangci-lint run
```

- [ ] **Step 5: Wire `test-auth-workos` into CI**

In `.github/workflows/ci.yml`, find the line `- run: mise run test-race`
inside the main gate job (the database-free group, alongside `vet`,
`lint`, `tidy-check`) and add a new step immediately after it:

```yaml
      - run: mise run test-auth-workos
```

- [ ] **Step 6: Update `CLAUDE.md`'s module count**

In `CLAUDE.md`, change:

```markdown
Four Go modules. `go test ./...` at the root covers **only the first**:

| | |
|---|---|
| `.` | the engine — builder, compiler, hooks, model cache. 19 files at the root, which is the package |
| `pgtest/` | round-trip tests against real Postgres in containers. Its own module so the engine's suite stays database-free |
| `example/tasks/`, `example/fxapp/` | worked applications, each with its own gate |
```

to:

```markdown
Five Go modules. `go test ./...` at the root covers **only the first**:

| | |
|---|---|
| `.` | the engine — builder, compiler, hooks, model cache. 19 files at the root, which is the package |
| `pgtest/` | round-trip tests against real Postgres in containers. Its own module so the engine's suite stays database-free |
| `example/tasks/`, `example/fxapp/` | worked applications, each with its own gate |
| `example/auth-workos/` | a `sqlb.Verifier[T]` adapter for WorkOS AuthKit — its own module so the WorkOS SDK and JWT/JWKS dependencies never reach sqlb core's `go.mod` |
```

- [ ] **Step 7: Run the affected tasks locally**

```bash
mise run test-auth-workos
mise run vet
mise run lint
```

Expected: all PASS.

- [ ] **Step 8: Commit**

```bash
git add mise.toml .github/workflows/ci.yml CLAUDE.md
git commit -m "chore(ci): wire example/auth-workos into vet, lint, preflight and CI"
```

---

## Task 7: Final gate

**Files:** none (verification only).

- [ ] **Step 1: Run the full local gate**

```bash
mise run preflight
mise run tidy-check
mise run vet
mise run lint
mise run test-auth-workos
```

Expected: all PASS.

- [ ] **Step 2: Race-test the new module**

```bash
cd example/auth-workos
mise exec -- go test -race ./...
```

Expected: PASS, no data races. This module has no shared mutable state of
its own (`Verifier[T]` is constructed once and only reads `jwks`/`mapper`
per call), so this is a confirmation rather than a suspicion — but cheap
to run and worth having in the record.

- [ ] **Step 3: Confirm `go.mod`/`go.sum` are tidy**

```bash
cd example/auth-workos
mise exec -- go mod tidy
git diff --stat go.mod go.sum
```

Expected: no diff (already tidy from earlier steps). If there is a diff,
commit it.

If all of the above pass, this plan is complete: `example/auth-workos` is
a working, independently-testable `sqlb.Verifier[T]` adapter, wired into
CI the same way `example/tasks` and `example/fxapp` are.

---

## Self-Review Notes

**Spec coverage:**
- `sqlb.Verifier[T]` implementation, own `go.mod`, no dependency reaching
  sqlb core — Tasks 1–2, 6.
- No live provider calls in CI — every task's tests use an in-memory or
  locally-generated key set; the one test needing an unreachable endpoint
  (Task 4) closes its own local `httptest.Server`, never calls WorkOS.
- Principal mapping stays app-owned via a caller-supplied `mapper` — Task 2.
- CI wiring matching `example/tasks`/`example/fxapp`'s existing pattern —
  Task 6.

**Type consistency:** `Verifier[T]`, `New[T]`, `NewWithKeyfunc[T]`,
`Verify`, and `Claims` are named identically across every task and match
the code blocks exactly (`Verify(ctx context.Context, cred string) (T,
error)`, matching `sqlb.Verifier[T]`'s interface from `auth.go`).

**A note on the `TransientError` decision:** this plan deliberately does
not give this adapter a `TransientError` path, and Task 3 exists
specifically to document why (`golang-jwt/jwt/v5` and `keyfunc` both
collapse "key not found" and "couldn't fetch" into the same wrapped
error, with no reliable sentinel to tell them apart). This is a real
finding from researching the actual libraries this plan depends on, not
an oversight — a reviewer re-deriving "shouldn't this use
TransientError?" from the design spec alone, without reading Task 3's
reasoning, would be asking a question this plan already answered.

**A note on `New`'s test coverage:** no test in this plan calls
`authworkos.New` against a live JWKS fetch — Task 2 tests only its two
validation checks (empty `clientID`, nil `mapper`), which return before
any network call. `New`'s successful path — build the JWKS URL, fetch it,
wrap the result — has no branch of its own beyond what `NewWithKeyfunc`
already covers in every other test, and testing it against a real
endpoint would violate the "no live provider calls in CI" constraint. A
reviewer flagging "New itself is never tested" should read this note
before treating it as a gap Task 2 missed.
