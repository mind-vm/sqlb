// Package app assembles the task manager: a chi router, the application's own
// middleware, a Huma API, the generated resources, the hand-written endpoints,
// and the hooks that hold the workspace boundary.
//
// It is a library rather than a main so that the tests can build the same
// server the binary builds. A demo whose tests exercise a different assembly
// than the one that ships is testing the tests.
package app

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks"
	"github.com/mind-vm/sqlb/example/tasks/auth"
	"github.com/mind-vm/sqlb/rest"
)

// Config is what New needs.
type Config struct {
	// DB is the database. Required.
	DB *pgxpool.Pool

	// Secret signs and verifies tokens. Required, at least 32 bytes.
	Secret []byte

	// TokenTTL defaults to 24 hours.
	TokenTTL time.Duration

	// Issuer is the "iss" claim. Defaults to "tasks".
	Issuer string

	// Log defaults to slog.Default().
	Log *slog.Logger
}

// Server is the assembled application.
type Server struct {
	// Handler is the router. Serve it.
	Handler http.Handler
	// API is the Huma API, exposed so a test can read the OpenAPI document
	// without going over HTTP.
	API huma.API
	// Signer is exposed so that a test can mint a token without logging in.
	Signer *auth.Signer

	// broker is the change feed's in-process source. It is not exported: the
	// only supported ways to reach it are subscribing over HTTP and writing
	// through the handlers, and an exported Publish would be a way for
	// application code to announce a change it did not make.
	broker *rest.Broker
}

// Close releases what the server holds. It disconnects every subscriber to the
// change feed; it does not close the connection pool, which the caller owns.
func (s *Server) Close() {
	if s.broker != nil {
		s.broker.Close()
	}
}

// New assembles the server.
func New(cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("app: Config.DB is required")
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = 24 * time.Hour
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "tasks"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	signer, err := auth.NewSigner(cfg.Secret, cfg.Issuer, cfg.TokenTTL)
	if err != nil {
		return nil, err
	}

	// Two handles over one connection pool.
	//
	// sys resolves against an empty hook registry and is used by exactly two
	// endpoints — register and login — which have to read and write before
	// there is an identity to scope by. Everything else uses hooked, where the
	// workspace boundary applies.
	//
	// They are separate handles rather than one handle and a "skip the hooks"
	// flag, because a flag is a thing that gets passed in from a caller, and the
	// set of callers that may pass it is the whole point. Two values, one of
	// which never leaves this file's neighbours, is harder to misuse.
	sys := sqlb.New(cfg.DB).WithHooks(sqlb.NewRegistry())

	// The registry is bound rather than inlined because two things register on
	// it: the workspace boundary, and the change feed's publisher. Both are
	// hooks on the same models, and both have to reach writes that never go
	// through a handler.
	reg := Register(cfg.Log)
	broker := rest.NewBroker(rest.BrokerOptions{})
	if err := publishChanges(reg, broker); err != nil {
		return nil, fmt.Errorf("app: wiring the change feed: %w", err)
	}
	hooked := sys.WithHooks(reg)

	router := chi.NewRouter()
	router.Use(
		middleware.RequestID,
		// No middleware.RealIP. It rewrites r.RemoteAddr from X-Forwarded-For or
		// X-Real-IP whether or not anything in front of this server sets them,
		// so a client can choose its own address — which is worth avoiding in
		// general and doubly so in an example somebody may deploy. A service
		// behind a proxy it actually controls should read the header itself and
		// trust it only from that proxy.
		middleware.Recoverer,
		// Everything is authenticated except what is listed here. See the note
		// on auth.Middleware for why the list is of exceptions rather than of
		// protected routes.
		auth.Middleware(signer,
			"/auth/register",
			"/auth/login",
			"/openapi.json",
			"/openapi.yaml",
			"/docs",
			"/docs/",
			"/health",
		),
		// A second, narrower gate on top of the first: every /admin/* request
		// already carries valid claims by the time this runs, and this is the
		// one place that checks whether those claims say PlatformAdmin. The
		// row-visibility half of the boundary is app/admin.go's Unscoped
		// release; this is the route half, and neither is the boundary alone
		// — see RequireAdmin's doc comment.
		auth.RequireAdmin("/admin/"),
	)

	router.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	api := humachi.New(router, openAPIConfig(cfg.Issuer))

	// The generated resources. One call, six tables, with filtering, sorting,
	// search, pagination and an OpenAPI operation per exposed table — and no
	// mention anywhere in it of workspaces, tokens or roles, because the hooks
	// already cover those for every read the handlers issue.
	if err := tasks.Register(api, hooked, tasks.Actions{
		CompleteTask: completeTask,
	}); err != nil {
		return nil, fmt.Errorf("app: mounting the generated resources: %w", err)
	}

	registerAuthRoutes(api, &authAPI{sys: sys, hooks: hooked.Hooks(), signer: signer})
	registerSoftDeleteRoutes(api, hooked)

	// The hand-written admin half — see app/admin.go's doc comment for why it
	// is hand-written rather than generated, and RequireAdmin above for the
	// route guard that keeps it from being reachable by an ordinary token.
	if err := registerAdminRoutes(api, hooked); err != nil {
		return nil, fmt.Errorf("app: mounting the admin resources: %w", err)
	}

	if err := registerEvents(api, broker); err != nil {
		return nil, fmt.Errorf("app: mounting the change feed: %w", err)
	}

	return &Server{Handler: router, API: api, Signer: signer, broker: broker}, nil
}

// openAPIConfig declares bearer authentication once, for the document as a
// whole.
//
// Enforcement is the middleware's job, not this document's — an OpenAPI
// security requirement describes an API, it does not protect one. What it buys
// is that a generated client knows to send the header, and that the two public
// endpoints stand out by overriding it.
func openAPIConfig(issuer string) huma.Config {
	cfg := huma.DefaultConfig("Tasks", "1.0.0")
	cfg.Info.Description = "A multi-tenant task manager built on sqlb: " +
		"generated CRUD over a declared schema, a workspace boundary held by " +
		"query hooks, and JWT bearer authentication."
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description:  "A token from POST /auth/login, issued by " + issuer,
		},
	}
	cfg.Security = []map[string][]string{{"bearer": {}}}
	return cfg
}
