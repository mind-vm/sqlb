// Command server runs the task manager.
//
//	docker run --rm -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:18
//	export TASKS_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
//	export TASKS_JWT_SECRET="$(head -c 32 /dev/urandom | base64)"
//	go run ./cmd/server
//
// Then http://localhost:8080/docs for the API, which is generated from the same
// schema the tables are.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	// The bridge back to database/sql, for goose. sqlb runs on the pool
	// directly (ADR-0040); the migration runner wants a *sql.DB, and this
	// hands it one over the same pool rather than a second one.
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/mind-vm/sqlb/example/tasks/app"
	"github.com/mind-vm/sqlb/example/tasks/migrations"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dsn := os.Getenv("TASKS_DATABASE_URL")
	if dsn == "" {
		return errors.New("TASKS_DATABASE_URL is not set")
	}

	secret := []byte(os.Getenv("TASKS_JWT_SECRET"))
	if len(secret) < 32 {
		// Not generated on the fly when missing. A server that invents a secret
		// at startup issues tokens that stop verifying the moment it restarts,
		// and the failure looks like an authentication bug rather than a
		// configuration one.
		return errors.New("TASKS_JWT_SECRET is unset or shorter than 32 bytes; " +
			`generate one with: head -c 32 /dev/urandom | base64`)
	}

	addr := os.Getenv("TASKS_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// The pool is configured here rather than in sqlb, which never opens a
	// connection of its own. Behind PgBouncer in transaction pooling mode these
	// numbers mean something different again — see ADR-0019.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("reading the database URL: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinIdleConns = 10
	poolCfg.MaxConnLifetime = time.Hour

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	// The error is deliberately dropped: this runs as the process exits, there
	// is nothing left to do about a failure, and the alternative is a log line
	// after the logger's last useful reader has gone.
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}

	// goose is a database/sql runner and stays one. It gets a handle over this
	// pool rather than a connection of its own, so the migrations and the
	// application are the same client to Postgres.
	gooseDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = gooseDB.Close() }()
	if err := migrations.Apply(ctx, gooseDB); err != nil {
		return err
	}
	log.Info("schema is up to date")

	srv, err := app.New(app.Config{DB: pool, Secret: secret, Log: log})
	if err != nil {
		return err
	}

	http := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", addr,
			"docs", "http://localhost"+lastColon(addr)+"/docs")
		errs <- http.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if errors.Is(err, os.ErrClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return http.Shutdown(shutdown)
	}
}

// lastColon renders ":8080" as ":8080" and "0.0.0.0:8080" as ":8080", so the
// logged URL is one somebody can click.
func lastColon(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return addr
}
