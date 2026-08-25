package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/sqlb/example/tasks/auth"
)

// These tests are about the verifier, not the signer. A JWT implementation is
// not wrong in interesting ways when it signs; it is wrong when it accepts
// something it should not, and every case below is a token an attacker can
// build without the secret.

var secret = []byte("test-secret-that-is-at-least-32-bytes-long")

func signer(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner(secret, "tasks", time.Hour)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := signer(t)
	token, err := s.Sign(auth.Claims{
		Subject:   "user-1",
		Email:     "alice@example.com",
		Workspace: "workspace-1",
		Role:      auth.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Subject != "user-1" || got.Workspace != "workspace-1" || got.Role != auth.RoleAdmin {
		t.Errorf("claims round-tripped as %+v", got)
	}
	if got.Issuer != "tasks" {
		t.Errorf("issuer = %q, want %q", got.Issuer, "tasks")
	}
	if got.ExpiresAt <= got.IssuedAt {
		t.Errorf("iat %d is not before exp %d", got.IssuedAt, got.ExpiresAt)
	}

	// Base64url without padding, per RFC 7515. A padded token is rejected by
	// every other implementation, and nothing here would notice.
	if strings.Contains(token, "=") {
		t.Errorf("the token contains base64 padding: %s", token)
	}
}

// TestAlgNoneIsRejected is the oldest JWT bypass there is: strip the signature,
// say the token is unsigned, and hope the verifier believes the header.
func TestAlgNoneIsRejected(t *testing.T) {
	s := signer(t)
	token := tamper(t, s, func(h map[string]any, _ map[string]any) {
		h["alg"] = "none"
	})
	if _, err := s.Verify(token); !errors.Is(err, auth.ErrAlgorithm) {
		t.Errorf("Verify = %v, want ErrAlgorithm", err)
	}
}

// TestAlgConfusionIsRejected is the second one: claim an asymmetric algorithm so
// that a verifier which dispatches on the header treats the HMAC secret as a
// public key — which, for a service that publishes its key, is a forgery anyone
// can perform.
func TestAlgConfusionIsRejected(t *testing.T) {
	s := signer(t)
	for _, alg := range []string{"RS256", "ES256", "HS512", ""} {
		token := tamper(t, s, func(h map[string]any, _ map[string]any) {
			h["alg"] = alg
		})
		if _, err := s.Verify(token); !errors.Is(err, auth.ErrAlgorithm) {
			t.Errorf("alg=%q: Verify = %v, want ErrAlgorithm", alg, err)
		}
	}
}

// TestEditedClaimsAreRejected is the case the signature exists for: a token
// whose payload has been rewritten to point at another workspace.
func TestEditedClaimsAreRejected(t *testing.T) {
	s := signer(t)
	token := tamper(t, s, func(_ map[string]any, c map[string]any) {
		c["wsp"] = "someone-elses-workspace"
		c["role"] = auth.RoleOwner
	})
	if _, err := s.Verify(token); !errors.Is(err, auth.ErrSignature) {
		t.Errorf("Verify = %v, want ErrSignature", err)
	}
}

// TestAMissingExpiryIsRejected covers the failure that looks like nothing: a
// token with no "exp" unmarshals to zero, and a verifier that only checks the
// field when it is present has just issued an immortal token.
func TestAMissingExpiryIsRejected(t *testing.T) {
	s := signer(t)
	// Re-signed properly, so the signature is valid and only the claim is
	// missing. This is the case a signature check cannot catch.
	token := resign(t, func(c map[string]any) { delete(c, "exp") })
	if _, err := s.Verify(token); !errors.Is(err, auth.ErrMalformed) {
		t.Errorf("Verify = %v, want ErrMalformed", err)
	}
}

func TestExpiredTokensAreRejected(t *testing.T) {
	base := time.Now()
	s := signer(t).WithClock(func() time.Time { return base })

	token, err := s.Sign(auth.Claims{Subject: "user-1", Workspace: "workspace-1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Still valid a minute before expiry.
	if _, err := signer(t).WithClock(func() time.Time {
		return base.Add(59 * time.Minute)
	}).Verify(token); err != nil {
		t.Errorf("a token 59 minutes old was rejected: %v", err)
	}

	// And not a second after it.
	if _, err := signer(t).WithClock(func() time.Time {
		return base.Add(time.Hour + time.Second)
	}).Verify(token); !errors.Is(err, auth.ErrExpired) {
		t.Errorf("Verify = %v, want ErrExpired", err)
	}
}

func TestMalformedTokensAreRejected(t *testing.T) {
	s := signer(t)
	for _, token := range []string{
		"",
		"not-a-token",
		"only.two",
		"a.b.c.d",
		"!!!.???.###",
	} {
		if _, err := s.Verify(token); err == nil {
			t.Errorf("Verify(%q) accepted a malformed token", token)
		}
	}
}

func TestAShortSecretIsRefused(t *testing.T) {
	if _, err := auth.NewSigner([]byte("too-short"), "tasks", time.Hour); err == nil {
		t.Error("NewSigner accepted a 9-byte secret")
	}
}

func TestPasswordsRoundTripAndSaltDiffers(t *testing.T) {
	const password = "correct-horse-battery-staple"

	first, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// Two hashes of one password must differ, or the salt is not doing its job
	// and a precomputed table covers every account at once.
	if first == second {
		t.Error("hashing the same password twice produced the same hash")
	}

	if err := auth.CheckPassword(first, password); err != nil {
		t.Errorf("CheckPassword rejected the right password: %v", err)
	}
	if err := auth.CheckPassword(first, password+"!"); !errors.Is(err, auth.ErrPassword) {
		t.Errorf("CheckPassword = %v, want ErrPassword", err)
	}

	// The parameters travel with the digest, which is what makes the cost
	// changeable without invalidating every stored password.
	if !strings.HasPrefix(first, "pbkdf2-sha256$600000$") {
		t.Errorf("hash does not carry its parameters: %s", first)
	}
}

// tamper rewrites a valid token's header or claims and leaves the original
// signature in place — an attacker's edit, not a re-signing.
func tamper(t *testing.T, s *auth.Signer, edit func(header, claims map[string]any)) string {
	t.Helper()

	token, err := s.Sign(auth.Claims{Subject: "user-1", Workspace: "workspace-1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.Split(token, ".")

	header := decodeSegment(t, parts[0])
	claims := decodeSegment(t, parts[1])
	edit(header, claims)

	return encodeSegment(t, header) + "." + encodeSegment(t, claims) + "." + parts[2]
}

// resign builds a token that is correctly signed and has edited claims, which
// is only possible with the secret — used for the cases where the point is that
// a valid signature is not on its own sufficient.
func resign(t *testing.T, edit func(claims map[string]any)) string {
	t.Helper()

	s := signer(t)
	token, err := s.Sign(auth.Claims{Subject: "user-1", Workspace: "workspace-1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	parts := strings.Split(token, ".")

	claims := decodeSegment(t, parts[1])
	edit(claims)
	payload := encodeSegment(t, claims)

	return parts[0] + "." + payload + "." + macOf(t, parts[0]+"."+payload)
}

func decodeSegment(t *testing.T, segment string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decoding %q: %v", segment, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshalling %s: %v", raw, err)
	}
	return m
}

func encodeSegment(t *testing.T, m map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshalling %v: %v", m, err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// macOf recomputes the HS256 signature the way the package does, so a test can
// build a validly-signed token with claims the Signer would never emit.
func macOf(t *testing.T, signing string) string {
	t.Helper()
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signing))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
