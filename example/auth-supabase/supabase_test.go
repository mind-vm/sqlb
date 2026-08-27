package authsupabase_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/mind-vm/sqlb"
	authsupabase "github.com/mind-vm/sqlb/example/auth-supabase"
)

const (
	projectURL = "https://abcdefghijklm.supabase.co"
	issuer     = projectURL + "/auth/v1"
	secret     = "super-secret-jwt-secret-from-the-dashboard"
	testKeyID  = "test-key-1"
)

// Principal is an application's own type, which is the whole point of the
// seam: this package never sees it and never defines one.
type Principal struct {
	UserID    string
	Email     string
	Anonymous bool
	Provider  string
}

func toPrincipal(c authsupabase.Claims) Principal {
	provider, _ := c.AppMetadata["provider"].(string)
	return Principal{UserID: c.Subject, Email: c.Email, Anonymous: c.IsAnonymous, Provider: provider}
}

var _ sqlb.Verifier[Principal] = (*authsupabase.Verifier[Principal])(nil)

// userClaims is what Supabase Auth puts in an access token for a signed-in
// user, trimmed to the claims this package reads.
func userClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":          issuer,
		"sub":          "3f7c1b0e-1f0a-4a5e-9d3b-2c4e6f8a0b1d",
		"aud":          "authenticated",
		"role":         "authenticated",
		"email":        "ada@example.com",
		"session_id":   "9c2f1a44-6f6f-4a1e-9c1a-0d1e2f3a4b5c",
		"aal":          "aal1",
		"is_anonymous": false,
		"app_metadata": map[string]any{"provider": "email", "providers": []any{"email"}},
		"exp":          time.Now().Add(time.Hour).Unix(),
		"iat":          time.Now().Add(-time.Minute).Unix(),
	}
}

func signHS256(t *testing.T, claims jwt.MapClaims, key string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func secretVerifier(t *testing.T) *authsupabase.Verifier[Principal] {
	t.Helper()
	v, err := authsupabase.NewWithSecret(projectURL, secret, toPrincipal)
	if err != nil {
		t.Fatalf("NewWithSecret: %v", err)
	}
	return v
}

// The ordinary path: a token the project signed for a signed-in user reaches
// the mapper, and the mapper's own type comes back.
func TestVerifyMapsASignedInUser(t *testing.T) {
	got, err := secretVerifier(t).Verify(t.Context(), signHS256(t, userClaims(), secret))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := Principal{
		UserID:   "3f7c1b0e-1f0a-4a5e-9d3b-2c4e6f8a0b1d",
		Email:    "ada@example.com",
		Provider: "email",
	}
	if got != want {
		t.Errorf("Verify() = %+v; want %+v", got, want)
	}
}

// The headline refusal. A project's anon key is a JWT signed with the same
// secret and published in every browser bundle; a verifier that checks only
// the signature would take it as a logged-in caller. service_role is the same
// shape with more authority behind it.
func TestKeysThatAreNotASignedInUserAreRefused(t *testing.T) {
	for _, role := range []string{"anon", "service_role", ""} {
		claims := userClaims()
		claims["role"] = role
		// The key that ships in a browser bundle carries no user at all, but
		// the role is what this refusal turns on — so the subject stays, and
		// the test cannot pass by accident through the empty-sub check.
		_, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret))
		if err == nil {
			t.Fatalf("a token with role %q was accepted as a signed-in user", role)
		}
		if !strings.Contains(err.Error(), "role") {
			t.Errorf("role %q: the rejection does not say what was wrong: %v", role, err)
		}
	}
}

// An anonymous sign-in is a real user and is not the case above: it reaches
// the mapper, marked, and the application decides what it may do.
func TestAnonymousSignInReachesTheMapper(t *testing.T) {
	claims := userClaims()
	claims["is_anonymous"] = true
	delete(claims, "email")

	got, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.Anonymous {
		t.Errorf("Verify() = %+v; want the principal marked anonymous", got)
	}
}

func TestTokenSignedWithAnotherSecretIsRefused(t *testing.T) {
	if _, err := secretVerifier(t).Verify(t.Context(),
		signHS256(t, userClaims(), "not-this-project's-secret")); err == nil {
		t.Fatal("a token signed with the wrong secret was accepted")
	}
}

// One Supabase project's token is not another's, even where an operator has
// reused a secret across environments.
func TestTokenFromAnotherProjectIsRefused(t *testing.T) {
	claims := userClaims()
	claims["iss"] = "https://nopqrstuvwxyz.supabase.co/auth/v1"
	if _, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret)); err == nil {
		t.Fatal("a token issued by another project was accepted")
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	claims := userClaims()
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	if _, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret)); err == nil {
		t.Fatal("an expired token was accepted")
	}
}

// A token with no expiry is refused rather than accepted forever.
func TestTokenWithoutAnExpiryIsRefused(t *testing.T) {
	claims := userClaims()
	delete(claims, "exp")
	if _, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret)); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

func TestTokenWithNoSubjectIsRefused(t *testing.T) {
	claims := userClaims()
	claims["sub"] = ""
	_, err := secretVerifier(t).Verify(t.Context(), signHS256(t, claims, secret))
	if err == nil {
		t.Fatal("a token identifying no user was accepted")
	}
	if !strings.Contains(err.Error(), "subject") {
		t.Errorf("the rejection does not say what was wrong: %v", err)
	}
}

func TestProjectURLIsRequired(t *testing.T) {
	if _, err := authsupabase.NewWithSecret("", secret, toPrincipal); err == nil {
		t.Fatal("a verifier with no project URL was built")
	}
	if _, err := authsupabase.NewWithSecret("abcdefghijklm.supabase.co", secret, toPrincipal); err == nil {
		t.Fatal("a project URL with no scheme was accepted")
	}
}

// The dashboard shows the project URL, the issuer and the JWKS URL, and which
// one someone pastes is a coin toss. All three name the same project, and a
// 404 at startup naming a URL nobody wrote is a poor way to say so.
func TestAProjectURLPastedFromTheDashboardIsNormalised(t *testing.T) {
	for _, given := range []string{
		projectURL,
		projectURL + "/",
		issuer,
		projectURL + "/auth/v1/.well-known/jwks.json",
	} {
		v, err := authsupabase.NewWithSecret(given, secret, toPrincipal)
		if err != nil {
			t.Fatalf("NewWithSecret(%q): %v", given, err)
		}
		if _, err := v.Verify(t.Context(), signHS256(t, userClaims(), secret)); err != nil {
			t.Errorf("NewWithSecret(%q) rejects the project's own token: %v", given, err)
		}
	}
}

// ecdsaKeyfunc publishes key's public half as a one-key JWKS held in memory,
// which is the shape keyfunc builds from a live endpoint without needing one.
func ecdsaKeyfunc(t *testing.T, key *ecdsa.PrivateKey) keyfunc.Keyfunc {
	t.Helper()
	jwk, err := jwkset.NewJWKFromKey(key.Public(), jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: testKeyID},
	})
	if err != nil {
		t.Fatalf("jwkset.NewJWKFromKey: %v", err)
	}
	storage := jwkset.NewMemoryStorage()
	if err := storage.KeyWrite(t.Context(), jwk); err != nil {
		t.Fatalf("storage.KeyWrite: %v", err)
	}
	marshalled, err := storage.Marshal(t.Context())
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

func signES256(t *testing.T, claims jwt.MapClaims, key *ecdsa.PrivateKey) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = testKeyID
	s, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return s
}

func newECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return key
}

// The other regime: a project signing with asymmetric keys, verified against
// the key set its JWKS endpoint publishes. Everything past the signature is
// the same code, so this asserts the path rather than re-asserting the claims.
func TestAsymmetricallySignedTokenVerifies(t *testing.T) {
	key := newECDSAKey(t)
	v := authsupabase.NewWithKeyfunc(ecdsaKeyfunc(t, key), projectURL, toPrincipal)

	got, err := v.Verify(t.Context(), signES256(t, userClaims(), key))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.UserID != "3f7c1b0e-1f0a-4a5e-9d3b-2c4e6f8a0b1d" {
		t.Errorf("Verify() = %+v; want the token's subject", got)
	}
}

// The algorithm-confusion attack: the public key is public, so a forged token
// signed with its bytes as an HMAC secret is one anybody can mint. Two things
// refuse it — the pinned method list, and keyfunc handing back an ECDSA key
// that HMAC verification cannot use — and the pinning is the one that does not
// depend on what the key set happens to hold, which is why it is there.
func TestHMACTokenIsRefusedByAKeySetVerifier(t *testing.T) {
	key := newECDSAKey(t)
	v := authsupabase.NewWithKeyfunc(ecdsaKeyfunc(t, key), projectURL, toPrincipal)

	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, userClaims())
	forged.Header["kid"] = testKeyID
	// The bytes of the public key, which is what an attacker holds.
	pub, err := key.PublicKey.ECDH()
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	signed, err := forged.SignedString(pub.Bytes())
	if err != nil {
		t.Fatalf("signing the forgery: %v", err)
	}
	if _, err := v.Verify(t.Context(), signed); err == nil {
		t.Fatal("a token signed with the public key as an HMAC secret was accepted")
	}
}

// The claim the module makes: it is an sqlb.Verifier, so it is middleware.
func TestItPlugsIntoMiddleware(t *testing.T) {
	var seen Principal
	handler := sqlb.Middleware[Principal](secretVerifier(t), sqlb.BearerToken)(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen, _ = sqlb.PrincipalFrom[Principal](r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+signHS256(t, userClaims(), secret))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if seen.Email != "ada@example.com" {
		t.Errorf("the handler saw %+v; want the token's principal", seen)
	}

	// And the refusal is a 401 rather than a panic or a 500.
	req = httptest.NewRequest(http.MethodGet, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+signHS256(t, userClaims(), "wrong"))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", rec.Code)
	}
}
