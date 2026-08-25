package rest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/sqlb"
)

// ServeConfig configures [Serve].
//
// What is here is the part every application's main.go writes identically:
// open a pool, ping it, maybe migrate, listen, shut down gracefully on
// signal. What is not here — which resources mount, whether they need a
// group, what a group's middleware does — cannot be, because none of it is
// inferable from a DSN and a schema. Mount is where that goes; see [Serve].
type ServeConfig struct {
	// DSN is the Postgres connection string. Required.
	DSN string

	// Addr is the listen address. Defaults to ":8080".
	Addr string

	// Server configures the huma.API [Serve] builds with [NewServer].
	Server Config

	// Middleware, if set, wraps the handler [Serve] listens on, applied once
	// mount has returned. It is the supported place to establish a
	// principal — authentication, request logging, panic recovery —
	// upstream of every guarantee a [sqlb.Scoped] hook's guard makes, since
	// a scoping hook constrains rows once a principal exists rather than
	// establishing one. Without it, wrapping means assigning [Server.Handler]
	// from inside mount and relying on Serve reading the field afterwards —
	// correct, but load-bearing on an ordering nothing states
	// ([#301](https://github.com/mind-vm/sqlb/issues/301)).
	//
	// Compose more than one middleware with a single function:
	//
	//	Middleware: func(next http.Handler) http.Handler {
	//	    return outer(inner(next))
	//	},
	Middleware func(http.Handler) http.Handler

	// Migrate runs against the pool before Server is built and before mount
	// is called, so a schema mount can rely on it having already run. Nil
	// means no migration step.
	//
	// It takes the pool rather than Serve owning a migration runner, on
	// purpose: goose, atlas, a hand-rolled one — which to use is exactly the
	// kind of decision [Options] a mount makes and Serve does not, and
	// wiring one in would make it a dependency of every application that
	// calls Serve, including the ones that migrate as a separate deploy
	// step and want nothing running at boot. See
	// [github.com/mind-vm/sqlb/example/tasks2/migrations] for a goose one.
	Migrate func(ctx context.Context, pool *pgxpool.Pool) error

	// ShutdownTimeout bounds how long Serve waits for in-flight requests to
	// finish after ctx is cancelled. Defaults to 5 seconds.
	ShutdownTimeout time.Duration

	// Log receives startup and shutdown messages. Defaults to slog.Default().
	Log *slog.Logger
}

// Serve opens the pool, migrates if configured to, builds the server, hands
// it to mount so the application can register its own resources — CRUD,
// actions, mutations, queries, groups, whatever the schema needs — and then
// runs until ctx is cancelled or the server errors, shutting down gracefully
// either way.
//
// mount is the seam. Everything before it is boilerplate every sqlb
// application writes the same way; everything mount does is the reason the
// application exists, and Serve does not try to guess it. A minimal server:
//
//	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
//	defer stop()
//	err := rest.Serve(ctx, rest.ServeConfig{
//	    DSN:     os.Getenv("DATABASE_URL"),
//	    Migrate: migrations.Apply,
//	}, func(srv *rest.Server, db *sqlb.DB) error {
//	    return myapp.Register(srv.API, db.WithHooks(myapp.Hooks()), myapp.Actions{})
//	})
//
// mount receives the handle at the type Serve built it as, *sqlb.DB, rather
// than the sqlb.Executor interface. Serve constructs it a frame up, so it
// knows the concrete type, and the thing a mount most often does first is
// attach a hook registry — [sqlb.DB.WithHooks], which lives on *sqlb.DB and
// not on Executor. Handing out the interface made every real mount open with
// a type assertion to recover what the caller already held
// ([#277](https://github.com/mind-vm/sqlb/issues/277)). Passing db straight
// on to a Register func still works: *sqlb.DB satisfies Executor.
func Serve(ctx context.Context, cfg ServeConfig, mount func(*Server, *sqlb.DB) error) error {
	if cfg.DSN == "" {
		return errors.New("rest: ServeConfig.DSN is required")
	}
	if mount == nil {
		return errors.New("rest: Serve called with a nil mount func")
	}
	addr := cfg.Addr
	if addr == "" {
		addr = ":8080"
	}
	timeout := cfg.ShutdownTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("rest: opening the database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("rest: connecting to the database: %w", err)
	}

	if cfg.Migrate != nil {
		if err := cfg.Migrate(ctx, pool); err != nil {
			return fmt.Errorf("rest: migrating: %w", err)
		}
		log.Info("schema is up to date")
	}

	db := sqlb.New(pool)
	srv := NewServer(cfg.Server)
	if err := mount(srv, db); err != nil {
		return fmt.Errorf("rest: mounting resources: %w", err)
	}

	httpServer := &http.Server{Addr: addr, Handler: wrapHandler(srv, cfg)}

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "docs", "http://localhost"+addr+"/docs")
		errs <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// wrapHandler applies cfg.Middleware, if set, to whatever mount left on
// srv.Handler. Its own function so that ordering — outside everything mount
// did, rather than a value read before mount ran — is testable without the
// live database the rest of Serve needs.
func wrapHandler(srv *Server, cfg ServeConfig) http.Handler {
	if cfg.Middleware == nil {
		return srv.Handler
	}
	return cfg.Middleware(srv.Handler)
}
