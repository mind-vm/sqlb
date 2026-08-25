package authworkos_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/auth-workos"
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
// published under testKeyID — the same shape keyfunc.NewDefaultOverrideCtx
// would build from a live JWKS URL, but from an in-memory JSON document
// rather than an HTTP fetch, so most tests need no network and no
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

type testPrincipal struct {
	UserID string
	OrgID  string
	Role   string
}

// newTestVerifier builds a Verifier[testPrincipal] wired directly to a
// keyfunc.Keyfunc built by the harness, via NewWithKeyfunc rather than
// New — every test in this package uses this path. New's own successful
// path (workos.GetJWKSURL plus a real keyfunc.NewDefaultOverrideCtx
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

// TestVerify_AcceptsMapsEveryClaimsField widens the mapper to capture the
// full authworkos.Claims value rather than testPrincipal's subset (Subject,
// OrgID, Role only), so SessionID, ClientID, Roles, Permissions — and the
// stringSliceClaim helper behind Roles/Permissions, including its []any
// type assertion and per-element string filter — are actually exercised.
// TestVerify_Accepts above is left alone so tests that build on
// newTestVerifier's narrower testPrincipal shape don't have to change.
func TestVerify_AcceptsMapsEveryClaimsField(t *testing.T) {
	key := newTestRSAKey(t)
	kf := newTestKeyfunc(t, key)
	v := authworkos.NewWithKeyfunc(kf, "client_test123", func(c authworkos.Claims) authworkos.Claims {
		return c
	})
	token := mintToken(t, key, validClaims())

	got, err := v.Verify(t.Context(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := authworkos.Claims{
		Subject:     "user_01HBEQKA6K4QJAS93VPE39W1JT",
		SessionID:   "session_01HQSXZGF8FHF7A9ZZFCW4387R",
		ClientID:    "client_test123",
		OrgID:       "org_01HRDMC6CM357W30QMHMQ96Q0S",
		Role:        "member",
		Roles:       []string{"member"},
		Permissions: []string{"posts:read", "posts:write"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claims = %+v, want %+v", got, want)
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

// TestVerify_RejectsEmptyConfiguredClientID proves NewWithKeyfunc, which
// skips New's up-front validation, cannot be turned into an accept-anything
// Verifier by leaving clientID empty. Without Verify's own guard, an empty
// v.clientID would make the client_id check degrade to "" == "" and accept
// a validly-signed token that carries no client_id claim at all — this
// mints exactly that token and asserts it is still rejected.
func TestVerify_RejectsEmptyConfiguredClientID(t *testing.T) {
	key := newTestRSAKey(t)
	kf := newTestKeyfunc(t, key)
	v := authworkos.NewWithKeyfunc(kf, "", func(c authworkos.Claims) testPrincipal {
		return testPrincipal{UserID: c.Subject, OrgID: c.OrgID, Role: c.Role}
	})
	claims := validClaims()
	delete(claims, "client_id")
	token := mintToken(t, key, claims)

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token with no client_id claim against a Verifier with an empty configured client_id")
	}
}

// TestVerify_RejectsNilMapper proves the other half of Verify's
// NewWithKeyfunc guard: a nil mapper is rejected with a plain error rather
// than reaching v.mapper(...) and panicking on the first otherwise-valid
// token.
func TestVerify_RejectsNilMapper(t *testing.T) {
	key := newTestRSAKey(t)
	kf := newTestKeyfunc(t, key)
	v := authworkos.NewWithKeyfunc[testPrincipal](kf, "client_test123", nil)
	token := mintToken(t, key, validClaims())

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted a token against a Verifier with a nil mapper")
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

// TestVerify_RejectsAlgorithmConfusion is the classic RS256-to-HS256
// algorithm-confusion attack: an RSA public key is public by definition —
// it is served over the JWKS endpoint — so an attacker who knows it can
// sign a forged token with HS256 using the public key's own bytes as the
// HMAC secret. A verifier that lets the token's header pick the algorithm,
// rather than pinning it itself, treats that HMAC signature as valid
// because it never checks the key was meant to be asymmetric. Verify pins
// jwt.WithValidMethods([]string{"RS256"}) specifically to close this —
// though this particular forged token is also rejected without that pin,
// since golang-jwt's HMAC verifier independently refuses a key that isn't
// []byte (keyfunc always hands back an *rsa.PublicKey here). The pin is
// what closes the general case: a Keyfunc that ever hands back raw bytes
// for some key would make this exact test pass even with the pin removed,
// which is why the pin exists as an explicit check rather than relying on
// that incidental behavior.
func TestVerify_RejectsAlgorithmConfusion(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")

	secret, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatalf("x509.MarshalPKIXPublicKey: %v", err)
	}

	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	forged.Header["kid"] = testKeyID
	token, err := forged.SignedString(secret)
	if err != nil {
		t.Fatalf("forged.SignedString: %v", err)
	}

	if _, err := v.Verify(t.Context(), token); err == nil {
		t.Fatal("Verify accepted an HS256 token forged with the RSA public key's bytes as the HMAC secret")
	}
}

func TestVerify_RejectsMalformedToken(t *testing.T) {
	key := newTestRSAKey(t)
	v := newTestVerifier(t, key, "client_test123")

	if _, err := v.Verify(t.Context(), "not-a-jwt-at-all"); err == nil {
		t.Fatal("Verify accepted a malformed token")
	}
}

// TestNewDefaultOverrideCtx_FailsWhenJWKSUnreachable proves that the exact
// mechanism New uses — keyfunc.NewDefaultOverrideCtx with
// NoErrorReturnFirstHTTPReq: false — genuinely fails on an unreachable
// endpoint. New itself cannot be tested end-to-end against a fake JWKS URL
// because workos.GetJWKSURL("", clientID) always derives the real WorkOS
// domain URL with no injectable override, and hitting the real domain is
// forbidden by the Global Constraints. This test demonstrates the mechanism
// New relies on works as documented: the initial fetch fails and is not
// swallowed.
func TestNewDefaultOverrideCtx_FailsWhenJWKSUnreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	unreachableURL := srv.URL + "/sso/jwks/client_test123"
	srv.Close() // closed before any request reaches it

	noErrorReturnFirst := false
	_, err := keyfunc.NewDefaultOverrideCtx(t.Context(), []string{unreachableURL}, keyfunc.Override{
		NoErrorReturnFirstHTTPReq: &noErrorReturnFirst,
	})
	if err == nil {
		t.Fatal("keyfunc.NewDefaultOverrideCtx succeeded against a closed server despite NoErrorReturnFirstHTTPReq: false")
	}
}

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
