package studio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Signing in with the application's own credentials (#328).
//
// Studio's login took a pasted bearer token and nothing else, so a first-time
// operator had to mint one outside the browser — curl against the sign-in
// route, or Try-it-out on /docs — before studio was usable at all. Fine for
// whoever wired it up; real friction for anyone else who wants to look at data,
// which is the one thing studio exists to make easy.
//
// The workaround was worse than the friction: an application setting the cookie
// itself couples to three unexported details of a module it does not own — the
// cookie's name, its path shape, and the fact that the value is the raw token
// — none of which is a documented contract, so a future release could change
// any of them without it counting as a break.

func credentialServer(t *testing.T, l CredentialLogin) *Server {
	t.Helper()
	s, err := NewServer(testManifest(), "http://api.test", "/studio")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s.WithCredentialLogin(l)
}

// post submits the login form and returns the response.
func post(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/studio/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestCredentialsAreExchangedForATokenAndHeldLikeAPastedOne(t *testing.T) {
	var gotID, gotSecret string
	s := credentialServer(t, CredentialLogin{
		Label: "Email",
		Exchange: func(_ context.Context, id, secret string) (string, error) {
			gotID, gotSecret = id, secret
			return "token-from-the-app", nil
		},
	})

	w := post(t, s, url.Values{"identifier": {"ada@example.com"}, "secret": {"hunter2"}})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", w.Code, w.Body)
	}
	if gotID != "ada@example.com" || gotSecret != "hunter2" {
		t.Errorf("the hook got (%q, %q)", gotID, gotSecret)
	}
	// The same cookie a pasted token produces — same name, same path, same
	// flags — because both paths go through setTokenCookie. That is the
	// coupling the issue is about: an application no longer has to reproduce
	// any of it.
	c := cookieNamed(t, w, tokenCookie)
	if c.Value != "token-from-the-app" {
		t.Errorf("cookie value = %q, want the token the hook returned", c.Value)
	}
	if c.Path != "/studio/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie shape differs from a pasted token's: %+v", c)
	}
}

// The hook's own error is never rendered. This page is reachable without a
// token, so anything shown on it is readable by anyone who can reach the URL,
// and a wrapped internal error would leak through a form nobody authenticated
// to see.
func TestAFailedExchangeLeaksNothingFromTheHooksError(t *testing.T) {
	s := credentialServer(t, CredentialLogin{
		Exchange: func(context.Context, string, string) (string, error) {
			return "", errors.New("pq: relation \"users\" does not exist at 10.0.0.4:5432")
		},
	})

	w := post(t, s, url.Values{"identifier": {"ada@example.com"}, "secret": {"wrong"}})
	body := w.Body.String()
	for _, leak := range []string{"pq:", "relation", "10.0.0.4"} {
		if strings.Contains(body, leak) {
			t.Errorf("the hook's error reached the page (%q):\n%s", leak, body)
		}
	}
	if !strings.Contains(body, "were not accepted") {
		t.Errorf("the page should say the credentials were refused:\n%s", body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a failed exchange set a cookie")
	}
}

// A hook returning an empty token and no error is a refusal, not a session with
// no credential — which would put studio in a state where every API call is
// unauthenticated and the operator believes they are signed in.
func TestAnEmptyTokenIsARefusal(t *testing.T) {
	s := credentialServer(t, CredentialLogin{
		Exchange: func(context.Context, string, string) (string, error) { return "", nil },
	})

	w := post(t, s, url.Values{"identifier": {"ada@example.com"}, "secret": {"hunter2"}})
	if w.Code == http.StatusFound {
		t.Fatal("an empty token was accepted as a sign-in")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("an empty token set a cookie")
	}
}

// Both forms post to the same handler, so a pasted token has to keep working
// when a hook is configured — otherwise adding the hook takes away the escape
// hatch for anyone whose credentials studio cannot exchange.
func TestAPastedTokenStillWorksAlongsideAConfiguredHook(t *testing.T) {
	s := credentialServer(t, CredentialLogin{
		Exchange: func(context.Context, string, string) (string, error) {
			return "", errors.New("should not be called")
		},
	})

	w := post(t, s, url.Values{"token": {"pasted"}})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302: %s", w.Code, w.Body)
	}
	if c := cookieNamed(t, w, tokenCookie); c.Value != "pasted" {
		t.Errorf("cookie value = %q, want the pasted token", c.Value)
	}
}

// With no hook, studio is exactly what it was: one form, and credentials posted
// to it are refused rather than silently ignored.
func TestWithNoHookOnlyTheTokenFormIsOffered(t *testing.T) {
	s, err := NewServer(testManifest(), "http://api.test", "/studio")
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/studio/login", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if body := w.Body.String(); strings.Contains(body, `name="identifier"`) {
		t.Errorf("the credential form was rendered with no hook configured:\n%s", body)
	}

	if got := post(t, s, url.Values{"identifier": {"a"}, "secret": {"b"}}); got.Code == http.StatusFound {
		t.Error("credentials were accepted with no hook to answer them")
	}
}

// The label is the application's, because only it knows what it signs in with.
func TestTheCredentialLabelIsTheApplicationsToChoose(t *testing.T) {
	s := credentialServer(t, CredentialLogin{
		Label:    "Staff number",
		Exchange: func(context.Context, string, string) (string, error) { return "t", nil },
	})

	r := httptest.NewRequest(http.MethodGet, "/studio/login", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "Staff number") {
		t.Errorf("the declared label is not on the form:\n%s", w.Body)
	}
}

func cookieNamed(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie was set; got %v", name, w.Result().Cookies())
	return nil
}
