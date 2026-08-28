package studio

import "context"

// CredentialLogin lets an operator sign in with the application's own
// credentials instead of pasting a bearer token.
//
// Studio has no opinion on how an application authenticates, and this does not
// give it one: the hook still only ever produces the *operator's own* token,
// the same value a pasted one would be, set in the same cookie by the same
// code. What it removes is the step where the operator has to go and get that
// token outside the browser — `curl` against the sign-in route, or the
// Try-it-out button on `/docs` — before studio is usable at all (#328).
//
// That step is fine for whoever wired studio up and is real friction for anyone
// else on the team who just wants to look at data, which is the one thing
// studio exists to make easy. It is also the only piece of a generated project
// that hands the operator back to curl: the REST API, the OpenAPI document and
// the three clients all work off the schema.
//
// Leaving it unset changes nothing — the token form is the only one rendered,
// and "studio has no opinion" stays true by default.
//
//	studio.NewServer(m, apiBase, "/studio").
//	    WithCredentialLogin(studio.CredentialLogin{
//	        Label: "Email",
//	        Exchange: func(ctx context.Context, email, password string) (string, error) {
//	            return auth.SignIn(ctx, email, password)   // the app's own route
//	        },
//	    })
type CredentialLogin struct {
	// Label names what the first field holds — "Email", "Username", whatever
	// the application signs in with. Empty renders "Email".
	//
	// It exists so this does not assume email-and-password: the two fields are
	// an identifier and a secret, and only the application knows what the
	// first one is called.
	Label string

	// Exchange calls the application's own sign-in and returns the token
	// studio should hold, which is the token the operator would otherwise have
	// pasted.
	//
	// The error is *not* shown to the visitor. The login page is by definition
	// reachable without a token, so an error rendered there is readable by
	// anyone who can reach the URL, and a wrapped internal one would leak
	// through a form nobody had to authenticate to see. Studio renders a fixed
	// "those credentials were not accepted" instead — which is the message a
	// sign-in form should give anyway, since distinguishing "no such user"
	// from "wrong password" is an enumeration oracle.
	//
	// Returning a token and a nil error is the only success. An empty token
	// with no error is treated as a refusal rather than as a session with no
	// credential.
	Exchange func(ctx context.Context, identifier, secret string) (token string, err error)
}

// configured reports whether an application supplied a usable hook.
func (c CredentialLogin) configured() bool { return c.Exchange != nil }

// label is the first field's caption, defaulted.
func (c CredentialLogin) label() string {
	if c.Label == "" {
		return "Email"
	}
	return c.Label
}

// WithCredentialLogin returns s with the hook attached, so an operator can sign
// in with the application's own credentials.
//
// A method rather than an argument to [NewServer] because that signature is
// already variadic over basePath and cannot take an options struct without
// breaking every existing call. It follows sqlb.DB's WithHooks/WithoutScope
// spelling, and it is set-up-time configuration: attach it before serving.
func (s *Server) WithCredentialLogin(l CredentialLogin) *Server {
	s.credential = l
	return s
}
