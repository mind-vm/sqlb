# Pluggable Auth — Core Seam Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish `Verifier[T]`, `Middleware[T]`, `TransientError`, and
`BearerToken` in sqlb core, so applications can plug in an auth provider
(WorkOS, Clerk, Zitadel, self-hosted JWT) without hand-rolling the same
middleware skeleton each time.

**Architecture:** One new stdlib-only file, `auth.go`, at the module root
alongside `principal.go`. `Verifier[T]` is a one-method interface an app or
adapter implements; `Middleware[T]` wraps it as ordinary `net/http`
middleware that extracts a credential, verifies it, and on success calls the
existing `WithPrincipal` before continuing the chain. Failure is either 401
(bad/missing credential) or 500 (`TransientError` — the provider couldn't be
reached, distinct from the credential being rejected). Nothing about
`Hooks[T]`, `PrincipalFrom[T]`, or `rest.Resource` changes.

**Tech Stack:** Go 1.25, stdlib only (`context`, `net/http`, `encoding/json`,
`errors`, `strings`) — no new dependency, verified by the existing
`deps-check` task.

**Spec:** `docs/superpowers/specs/2026-08-15-pluggable-auth-design.md`

## Global Constraints

- Zero new dependencies. `auth.go` imports stdlib only — this is what keeps
  `mise run deps-check` green without touching its allow-list.
- `package sqlb` (module root), not a subpackage — same package as
  `principal.go`, which `Middleware[T]` calls directly (`WithPrincipal`, no
  import needed).
- Tests live in `package sqlb_test` (external test package), matching
  `db_test.go`'s convention — `import "github.com/mind-vm/sqlb"`.
- Error responses are RFC 9457 `application/problem+json`, matching the
  shape `example/tasks/auth/middleware.go` and `rest.Problem` both already
  use: `{"title", "status", "detail"}`. The underlying `Verify` error is
  never echoed in the response body — only the sentinel-checked shape
  (`TransientError` or not) decides the status code.
- This plan is **scoped to the core seam only** — `Verifier[T]`,
  `Middleware[T]`, `TransientError`, `BearerToken`, the ADR, and the
  `docs/architecture.md` update. The WorkOS/Clerk/Zitadel worked-example
  adapters and the `example/tasks/auth/jwt.go` promotion are independent
  subsystems (different external SDKs, different `go.mod`s, no shared code
  between them) and get their own follow-on plans once this seam is merged —
  each one needs its own `go.mod` and CI wiring (see `mise.toml`'s per-module
  loops for `pgtest`/`example/tasks`/`example/fxapp`), which is real
  additional surface this plan does not include.

---

## Task 1: `BearerToken` credential extractor

**Files:**
- Create: `auth.go`
- Test: `auth_test.go`

**Interfaces:**
- Produces: `type CredentialExtractor func(r *http.Request) (cred string, ok bool)`, `func BearerToken(r *http.Request) (string, bool)`

- [ ] **Step 1: Write the failing test**

Create `auth_test.go`:

```go
package sqlb_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mind-vm/sqlb"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantCred  string
		wantOK    bool
	}{
		{"present", "Bearer abc123", "abc123", true},
		{"case-insensitive scheme", "bearer abc123", "abc123", true},
		{"upper scheme", "BEARER abc123", "abc123", true},
		{"missing header", "", "", false},
		{"wrong scheme", "Basic abc123", "", false},
		{"empty token", "Bearer ", "", false},
		{"whitespace-only token", "Bearer    ", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}

			cred, ok := sqlb.BearerToken(r)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if cred != tc.wantCred {
				t.Fatalf("cred = %q, want %q", cred, tc.wantCred)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestBearerToken -v`
Expected: FAIL — `sqlb.BearerToken` undefined (auth.go does not exist yet).

- [ ] **Step 3: Write minimal implementation**

Create `auth.go`:

```go
package sqlb

import (
	"net/http"
	"strings"
)

// CredentialExtractor pulls a raw credential — a bearer token, a cookie
// value, whatever a provider issues — out of an inbound request. ok is
// false when no credential is present, which Middleware treats as a
// missing-credential rejection rather than an invalid one.
type CredentialExtractor func(r *http.Request) (cred string, ok bool)

// BearerToken extracts a credential from the Authorization: Bearer <token>
// header (RFC 6750). It is the default extractor: Zitadel's OIDC access
// tokens, self-hosted JWTs, and WorkOS/Clerk in API mode all present this
// way. A provider whose browser flow hands back a cookie instead (WorkOS's
// AuthKit, Clerk's hosted UI) needs its own CredentialExtractor — Middleware
// takes the extractor as a parameter rather than hardcoding this one so that
// substitution costs nothing.
func BearerToken(r *http.Request) (string, bool) {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestBearerToken -v`
Expected: PASS, all subtests.

- [ ] **Step 5: Commit**

```bash
git add auth.go auth_test.go
git commit -m "feat(auth): a bearer token is extracted the same way everywhere it is trusted"
```

---

## Task 2: `Verifier[T]` and `TransientError`

**Files:**
- Modify: `auth.go`
- Test: `auth_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 directly (independent types), but lives in the same file.
- Produces: `type Verifier[T any] interface { Verify(ctx context.Context, cred string) (T, error) }`, `type TransientError struct{ Err error }` with `Error() string` and `Unwrap() error`.

- [ ] **Step 1: Write the failing test**

Replace `auth_test.go`'s import block (at the top of the file) with:

```go
import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mind-vm/sqlb"
)
```

Then append below `TestBearerToken`:

```go
func TestTransientError(t *testing.T) {
	inner := errors.New("dial tcp: connection refused")
	te := sqlb.TransientError{Err: inner}

	if got := te.Error(); got != inner.Error() {
		t.Fatalf("Error() = %q, want %q", got, inner.Error())
	}
	if !errors.Is(te, inner) {
		t.Fatalf("errors.Is(te, inner) = false, want true (Unwrap must expose Err)")
	}

	var target sqlb.TransientError
	if !errors.As(error(te), &target) {
		t.Fatalf("errors.As did not recognize TransientError")
	}
}

// verifierFunc lets a test supply Verify as a closure instead of a named type.
type verifierFunc[T any] func(ctx context.Context, cred string) (T, error)

func (f verifierFunc[T]) Verify(ctx context.Context, cred string) (T, error) {
	return f(ctx, cred)
}

func TestVerifierInterface(t *testing.T) {
	// Compile-time check: verifierFunc[string] must satisfy Verifier[string].
	var _ sqlb.Verifier[string] = verifierFunc[string](func(ctx context.Context, cred string) (string, error) {
		return cred, nil
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestTransientError|TestVerifierInterface' -v`
Expected: FAIL — `sqlb.TransientError` and `sqlb.Verifier` undefined.

- [ ] **Step 3: Write minimal implementation**

Replace `auth.go`'s import block (at the top of the file) with:

```go
import (
	"context"
	"net/http"
	"strings"
)
```

Then append below `BearerToken`:

```go
// Verifier checks a credential and returns the application's own principal
// type. T is the same type the application later reads back with
// PrincipalFrom[T] — Verifier does not introduce a new principal shape, it
// produces the one the application already owns.
//
// Different providers hand back different claim shapes; Verifier stays
// generic over T rather than sqlb defining a canonical principal struct, so
// a WorkOS, Clerk, Zitadel, or self-hosted-JWT adapter maps its provider's
// claims into whatever type the application's hooks already read via
// PrincipalFrom[T].
type Verifier[T any] interface {
	Verify(ctx context.Context, cred string) (T, error)
}

// TransientError marks a Verify failure as not-a-verdict-on-the-credential —
// a network error reaching the provider, a provider 5xx, a timeout — so
// Middleware answers 500 instead of 401. "The provider is down" and "the
// token is bad" are different failures for both an operator paging on 5xx
// and a client that should not retry a rejected credential; collapsing them
// into one status code erases that distinction.
//
// This is opt-in. A Verifier with no network call to fail — local JWT
// verification, for instance — never has a transient failure mode and never
// needs to return one; every error it returns is correctly a 401.
type TransientError struct{ Err error }

func (e TransientError) Error() string { return e.Err.Error() }
func (e TransientError) Unwrap() error { return e.Err }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestTransientError|TestVerifierInterface|TestBearerToken' -v`
Expected: PASS, all subtests (Task 1's tests still pass).

- [ ] **Step 5: Commit**

```bash
git add auth.go auth_test.go
git commit -m "feat(auth): a transient failure and a rejected credential answer differently"
```

---

## Task 3: `Middleware[T]` — success path

**Files:**
- Modify: `auth.go`
- Test: `auth_test.go`

**Interfaces:**
- Consumes: `Verifier[T]` and `CredentialExtractor` from Tasks 1–2; `WithPrincipal`/`PrincipalFrom[T]` from the existing `principal.go` (same package, no import).
- Produces: `func Middleware[T any](v Verifier[T], extract CredentialExtractor) func(http.Handler) http.Handler`, plus the unexported `writeProblem(w http.ResponseWriter, status int, detail string)` helper later tasks reuse.

- [ ] **Step 1: Write the failing test**

Add to `auth_test.go`:

```go
type testPrincipal struct{ ID string }

func TestMiddleware_Success(t *testing.T) {
	v := verifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		if cred != "good-token" {
			t.Fatalf("Verify received %q, want %q", cred, "good-token")
		}
		return testPrincipal{ID: "user-1"}, nil
	})

	var gotPrincipal testPrincipal
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, gotOK = sqlb.PrincipalFrom[testPrincipal](r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !gotOK {
		t.Fatalf("PrincipalFrom[testPrincipal] found nothing in next's context")
	}
	if gotPrincipal.ID != "user-1" {
		t.Fatalf("principal.ID = %q, want %q", gotPrincipal.ID, "user-1")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestMiddleware_Success -v`
Expected: FAIL — `sqlb.Middleware` undefined.

- [ ] **Step 3: Write minimal implementation**

Replace `auth.go`'s import block (at the top of the file) with:

```go
import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)
```

Then append below `TransientError`'s methods:

```go
// Middleware wraps a Verifier[T] as net/http middleware: extract the
// credential, verify it, and on success carry the resulting principal on
// the request context via WithPrincipal before calling next. It is
// ordinary net/http middleware, not anything huma-specific, so it composes
// with whatever router or middleware chain the application already has —
// the same reason rest.Resource takes a huma.API rather than owning one.
//
// A missing or rejected credential answers 401 and never calls next.
func Middleware[T any](v Verifier[T], extract CredentialExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cred, ok := extract(r)
			if !ok {
				writeProblem(w, http.StatusUnauthorized, "the request carries no credential")
				return
			}

			principal, err := v.Verify(r.Context(), cred)
			if err != nil {
				// err is deliberately not echoed: which check a forged or
				// expired credential failed is useful to precisely one kind
				// of caller. Task 4 adds the TransientError branch here.
				writeProblem(w, http.StatusUnauthorized, "the credential is not valid")
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// writeProblem writes the same RFC 9457 problem shape example/tasks/auth
// and the rest package both use, so a client sees one error type across
// authentication failures and everything rest itself rejects.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestMiddleware_Success -v`
Expected: PASS.

Run: `go test ./...`
Expected: PASS (whole module — this is the first task where auth.go is a
complete, self-consistent file; nothing else in the module references it
yet, so a passing full run also confirms no accidental breakage elsewhere).

- [ ] **Step 5: Commit**

```bash
git add auth.go auth_test.go
git commit -m "feat(auth): a verified credential reaches the handler as a principal"
```

---

## Task 4: `Middleware[T]` — failure paths

**Files:**
- Modify: `auth.go`
- Test: `auth_test.go`

**Interfaces:**
- Consumes: `Middleware[T]`, `writeProblem` from Task 3; `TransientError` from Task 2.
- Produces: no new exported names — this task completes `Middleware[T]`'s behavior (the 401-vs-500 split promised by the spec's error-handling table).

- [ ] **Step 1: Write the failing test**

Add to `auth_test.go`:

```go
func TestMiddleware_MissingCredential(t *testing.T) {
	v := verifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		t.Fatal("Verify must not be called when no credential is present")
		return testPrincipal{}, nil
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil) // no Authorization header
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextCalled {
		t.Fatal("next was called despite a missing credential")
	}
	assertProblemJSON(t, w, http.StatusUnauthorized)
}

func TestMiddleware_InvalidCredential(t *testing.T) {
	wantErr := errors.New("signature mismatch")
	v := verifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		return testPrincipal{}, wantErr
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer bad-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	if nextCalled {
		t.Fatal("next was called despite an invalid credential")
	}
	body := assertProblemJSON(t, w, http.StatusUnauthorized)
	if strings.Contains(body["detail"].(string), wantErr.Error()) {
		t.Fatal("the underlying Verify error must not be echoed in the response body")
	}
}

func TestMiddleware_TransientError(t *testing.T) {
	v := verifierFunc[testPrincipal](func(ctx context.Context, cred string) (testPrincipal, error) {
		return testPrincipal{}, sqlb.TransientError{Err: errors.New("dial tcp: i/o timeout")}
	})
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { nextCalled = true })

	mw := sqlb.Middleware[testPrincipal](v, sqlb.BearerToken)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()

	mw(next).ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (a provider outage must not read as a rejected credential)", w.Code, http.StatusInternalServerError)
	}
	if nextCalled {
		t.Fatal("next was called despite a transient verification failure")
	}
	assertProblemJSON(t, w, http.StatusInternalServerError)
}

// assertProblemJSON checks the response is RFC 9457 problem+json shaped with
// the given status, and returns the decoded body for further assertions.
func assertProblemJSON(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("response body did not decode as JSON: %v", err)
	}
	if got, want := int(body["status"].(float64)), status; got != want {
		t.Fatalf("body[status] = %d, want %d", got, want)
	}
	if _, ok := body["detail"].(string); !ok {
		t.Fatal("body[detail] missing or not a string")
	}
	return body
}
```

Add `"encoding/json"` and `"strings"` to `auth_test.go`'s import block (both already needed above).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestMiddleware_TransientError -v`
Expected: FAIL — status is 401 (Unauthorized), not 500, because `Middleware`
does not yet check for `TransientError`.

Run: `go test ./... -run 'TestMiddleware_MissingCredential|TestMiddleware_InvalidCredential' -v`
Expected: PASS already (Task 3's implementation covers these two).

- [ ] **Step 3: Write minimal implementation**

Replace `auth.go`'s import block (at the top of the file) with:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)
```

Then, inside `Middleware`, replace the `if err != nil { ... }` block Task 3
wrote with one that branches on `TransientError` before falling through to
the 401 case:

```go
			principal, err := v.Verify(r.Context(), cred)
			if err != nil {
				var transient TransientError
				if errors.As(err, &transient) {
					writeProblem(w, http.StatusInternalServerError, "authentication could not be completed")
					return
				}
				// err is deliberately not echoed: which check a forged or
				// expired credential failed is useful to precisely one kind
				// of caller.
				writeProblem(w, http.StatusUnauthorized, "the credential is not valid")
				return
			}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestMiddleware -v`
Expected: PASS — all four `TestMiddleware_*` tests.

Run: `go test ./...`
Expected: PASS, whole module.

- [ ] **Step 5: Commit**

```bash
git add auth.go auth_test.go
git commit -m "feat(auth): a provider outage answers 500, not the 401 a bad token gets"
```

---

## Task 5: ADR-0059

**Files:**
- Create: `docs/adr/0059-a-verifier-composes-with-the-principal-seam.md`
- Modify: `docs/adr/README.md` (index table)

**Interfaces:**
- Consumes: nothing (documentation only).
- Produces: nothing other code depends on; `mise run adr-check` depends on this file's shape (status vocabulary, Confidence/Last reviewed lines, index row).

- [ ] **Step 1: Write the ADR**

Create `docs/adr/0059-a-verifier-composes-with-the-principal-seam.md`:

```markdown
# ADR-0059: A `Verifier[T]` composes with the principal seam, and adapters stay copy-paste

- **Status:** Exploring — built and unit-tested, not yet used by a second
  application
- **Confidence:** Medium — the mechanism is small and stdlib-only, so the
  risk is not whether it works but whether four real adapters (WorkOS,
  Clerk, Zitadel, self-hosted) all fit it without a signature change
- **Decided:** 2026-08-15
- **Last reviewed:** 2026-08-15

## Context

[ADR-0044](0044-the-container-is-an-adapter.md) already answered how sqlb
publishes an extension point without owning the assembly around it: a
minimal, stdlib-only context contract (`WithPrincipal`/`PrincipalFrom[T]` in
`principal.go`), with anything opinionated — which router, which provider —
kept as a copy-paste example rather than an import. That record also named
its own reconsideration trigger: *"a second author writes an sqlb-shaped
module."*

Four such authors arrived at once: applications wanting WorkOS, Clerk,
Zitadel, or a self-hosted JWT/session scheme, each of which was about to
write the same shape of middleware — extract a credential, verify it, call
`WithPrincipal` — independently. `example/tasks/auth/` and
`example/fxapp/access/` already prove the pattern works; what neither
proves is that it needs restating by hand every time.

## Decision

**`Verifier[T]`, `Middleware[T]`, `TransientError`, and `BearerToken`** are
published in `auth.go` at the module root, next to `principal.go`, with zero
new dependencies:

```go
type Verifier[T any] interface {
	Verify(ctx context.Context, cred string) (T, error)
}

func Middleware[T any](v Verifier[T], extract CredentialExtractor) func(http.Handler) http.Handler
```

`Verifier[T]` stays generic over the application's own principal type rather
than sqlb defining a canonical `Principal` struct — the same choice
`PrincipalFrom[T]` already made, extended to the thing that produces a
principal rather than just the thing that carries one.

**Credential extraction is a separate, composable piece from verification**
(`CredentialExtractor`, with `BearerToken` as the stdlib-shaped default).
WorkOS's AuthKit and Clerk's hosted UI commonly hand back session state via
a cookie for browser flows, while Zitadel and self-hosted JWT are
bearer-token-shaped; hardcoding one extraction strategy into `Middleware`
would make it wrong for half its named targets.

**A `Verify` failure is 401 unless it opts into `TransientError`, which
answers 500.** A provider outage and a rejected credential are different
failures for an operator paging on 5xx and for a client deciding whether to
retry; collapsing them loses that distinction. Opt-in rather than a required
interface method, because a `Verifier` with no network call to fail (local
JWT verification) has no transient failure mode to report.

**Provider adapters (WorkOS, Clerk, Zitadel) stay worked examples under
`example/`, each its own Go module** — not published, `go get`-able
packages. sqlb core is dependency-locked to pgx only
([ADR-0040](0040-the-driver-is-a-dependency.md)); an adapter needs its
provider's SDK, which must never reach sqlb's own `go.mod`. This is
ADR-0044's rule exercised for auth specifically, not a new one.

## Consequences

**Buys.** Four independent auth integrations share one seam instead of four
reimplementations of "extract, verify, `WithPrincipal`." The 401-vs-500
split is testable and tested once, in core, rather than once per adapter (or
not at all, in the adapters that would have skipped it).

**Costs.** `Verifier[T]` commits to a one-credential, one-call shape. An
app needing multi-factor verification, or a provider whose check genuinely
needs two round trips, does not fit `Verify(ctx, cred) (T, error)` directly
and has to compose its own `Verifier[T]` around the parts it needs — which
the spec already scoped out (multi-provider chaining within one realm is
explicitly not this seam's job).

## What would change our mind

- A second application's `Verifier[T]` implementation needs a signature
  `Verify(ctx, cred) (T, error)` cannot express — multiple credentials in
  one request, or a check that is not naturally a single call.
- Real adapters (once built) find `TransientError` insufficient — for
  instance, a provider whose rate-limit response is neither "reject the
  credential" nor cleanly "unreachable."

## Cost of change

**Widening is free** — `Middleware` gaining an optional parameter, or a
second constructor for a different failure taxonomy, is additive. **Narrowing
is cheap today**: nothing outside this file and its tests calls `Verifier[T]`
or `Middleware[T]` yet, so the honest cost of changing the signature is zero
until the first adapter (WorkOS, Clerk, or Zitadel — each its own follow-on
plan) depends on it.

## Open questions I had to answer myself

- **Whether `Middleware` should set `WWW-Authenticate`.** Not built: the
  header names a scheme (`Bearer realm="..."`), and `Middleware` is generic
  over `CredentialExtractor` — it does not know the extractor is
  bearer-shaped. `example/tasks/auth/middleware.go` sets it because that
  package knows it is bearer-only; the generic core version does not have
  that knowledge and would be wrong for a cookie-based extractor. Left as
  something an app-specific wrapper adds if it wants it.
- **Whether `Verify`'s error should carry a typed reason beyond
  transient-or-not** (invalid signature vs. expired vs. wrong audience, say).
  Not built: no consumer needed it yet, and the detail message already
  deliberately does not echo the underlying error, so a richer error type
  would have nowhere visible to surface today.

## Revisions

- 2026-08-15 — Written, after building and unit-testing `auth.go` against a
  fake `Verifier[T]`. No real adapter (WorkOS/Clerk/Zitadel) exists yet;
  those are separate follow-on plans and will be the evidence that answers
  this record's own "What would change our mind."
```

- [ ] **Step 2: Add the index row**

In `docs/adr/README.md`, add a row after the `0058` row (before the closing
`† **Deliberately not in 1.0.**` paragraph):

```markdown
| [0059](0059-a-verifier-composes-with-the-principal-seam.md) | A `Verifier[T]` composes with the principal seam, and adapters stay copy-paste | Exploring | Medium |
```

- [ ] **Step 3: Run adr-check**

Run: `mise run adr-check`
Expected: PASS — the new record's status (`Exploring`), Confidence and Last
reviewed lines, and index row all parse.

- [ ] **Step 4: Commit**

```bash
git add docs/adr/0059-a-verifier-composes-with-the-principal-seam.md docs/adr/README.md
git commit -m "docs(adr): ADR-0059 records the Verifier[T]/Middleware[T] decision"
```

---

## Task 6: `docs/architecture.md` update and final gate

**Files:**
- Modify: `docs/architecture.md`

**Interfaces:**
- Consumes: nothing (documentation only, describes Tasks 1–5's result).
- Produces: nothing — this is the last task in the plan.

- [ ] **Step 1: Add a paragraph to "Where safety lives"**

In `docs/architecture.md`, the "Where safety lives" section currently lists
four paragraphs — Bind parameters, Identifier validation, Opt-in
capabilities, Query hooks — followed by "Two smaller rails worth knowing:
...". Insert a new paragraph immediately after the **Query hooks.**
paragraph and before "Two smaller rails worth knowing:":

```markdown
**Authentication is a separate mechanism from the four above, composing
with rather than replacing them.** `Middleware[T]` (`auth.go`) verifies who
is calling and writes the result to the context via `WithPrincipal`; query
hooks then read it back with `PrincipalFrom[T]` to decide what that caller
may see. An app can swap `Middleware[T]`'s `Verifier[T]` — WorkOS, Clerk,
Zitadel, a self-hosted JWT, more than one realm at once — without touching a
hook, and can change a hook's row-scoping without touching how identity was
established. Neither seam substitutes for the other: `Middleware[T]`
answering 200 says only that the credential was valid, not what rows the
resulting principal may reach.
```

- [ ] **Step 2: Verify the doc site still builds cleanly**

Run: `mise run site-check`
Expected: PASS — no broken links, no build errors. (This does not need
`npm ci`; it is the fast check CLAUDE.md points to for exactly this kind of
edit.)

- [ ] **Step 3: Commit**

```bash
git add docs/architecture.md
git commit -m "docs(architecture): authentication and authorization are named as separate seams"
```

- [ ] **Step 4: Run the full gate**

Run: `mise run preflight`
Expected: PASS (heal, build, database-free tests — the push-path gate).

Run: `go test -race ./... -run 'TestBearerToken|TestTransientError|TestVerifierInterface|TestMiddleware'`
Expected: PASS. This plan touches no per-type cache or shared state
`Middleware[T]` itself owns (it reads `Verifier[T]`/`CredentialExtractor`
parameters and writes to a per-request context), so this is a quick
confirmation rather than a suspicion — but `test-race` is cheap and
`CLAUDE.md` calls it out as the thing to run before trusting anything that
touches the request path.

If both pass, this plan is complete: `auth.go` is a self-contained,
independently-mergeable seam that the follow-on WorkOS/Clerk/Zitadel
adapter plans will build on.

---

## Self-Review Notes

**Spec coverage:**
- `Verifier[T]`, `Middleware[T]`, `TransientError`, `BearerToken`,
  `CredentialExtractor` — Tasks 1–4.
- 401-vs-500 error handling table — Task 4.
- RFC 9457 problem shape — Task 3 (`writeProblem`).
- ADR extending ADR-0044 — Task 5.
- `docs/architecture.md` update distinguishing authn from authz — Task 6.
- Worked provider adapters (WorkOS/Clerk/Zitadel), promoting
  `example/tasks/auth/jwt.go` as the self-hosted reference, and the
  multi-realm worked example — **explicitly out of this plan's scope** (see
  Global Constraints); each is an independent follow-on plan.

**Type consistency:** `Verifier[T]`, `Middleware[T]`, `TransientError`, and
`CredentialExtractor` are named identically across all six tasks and match
the spec's code blocks exactly (`Verify(ctx context.Context, cred string)
(T, error)`, `Middleware[T any](v Verifier[T], extract CredentialExtractor)
func(http.Handler) http.Handler`).
