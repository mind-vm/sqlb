// Command server runs tasks2: a from-scratch rebuild of example/tasks with
// the auth and multi-tenancy stripped out, to measure what defining a
// schema, a declared action and a declared query cost on their own.
//
//	export TASKS2_DATABASE_URL='postgres://sqlb:sqlb@localhost:15432/tasks2?sslmode=disable'
//	go run ./cmd/server
//
// Then http://localhost:8081/docs for the API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	// The bridge back to database/sql, for goose. sqlb runs on the pool
	// directly (ADR-0040); the migration runner wants a *sql.DB, and this
	// hands it one over the same pool rather than opening a second one.
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/example/tasks2"
	"github.com/mind-vm/sqlb/example/tasks2/migrations"
	"github.com/mind-vm/sqlb/rest"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	dsn := os.Getenv("TASKS2_DATABASE_URL")
	if dsn == "" {
		log.Error("exiting", "error", "TASKS2_DATABASE_URL is not set")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := rest.Serve(ctx, rest.ServeConfig{
		DSN:    dsn,
		Addr:   ":8081",
		Server: rest.Config{Title: "tasks2", Version: "0.0.0"},
		Log:    log,
		Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
			gooseDB := stdlib.OpenDBFromPool(pool)
			defer func() { _ = gooseDB.Close() }()
			return migrations.Apply(ctx, gooseDB)
		},
	}, mount)
	if err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// mount is the seam rest.Serve leaves to the application — which resources,
// which group, which middleware. Everything above it in main is the
// boilerplate every sqlb server writes the same way.
func mount(srv *rest.Server, db *sqlb.DB) error {
	// List has nothing that needs auth, so it mounts on the bare API — the
	// same call the generated Register would make for it.
	if err := rest.Resource[tasks2.List, tasks2.ListCreate, tasks2.ListPatch](srv.API, db, rest.Options{
		Path:            "/lists",
		Name:            "list",
		Tag:             "lists",
		Ops:             rest.OpCreate | rest.OpRead | rest.OpUpdate | rest.OpDelete | rest.OpList,
		Description:     "A group of tasks.",
		DefaultPageSize: 25,
		MaxPageSize:     100,
	}); err != nil {
		return fmt.Errorf("mounting lists: %w", err)
	}

	// The group: every /tasks route — generated CRUD, the item-form action,
	// the query, the collection-form action — mounts through this instead of
	// srv.API, so RequireAuthForWrites applies to all of them at once. No prefix: the
	// Path below is already absolute, the group exists for UseMiddleware
	// alone. See auth.go for why gating on Method rather than a path or a
	// tag is what makes this agree with the schema without either naming the
	// other.
	//
	// This is also why List isn't reached through Register above: Register
	// mounts every exposed table on one shared api, and there is no
	// generated way to say "these into the group, that one plain" — so
	// scoping middleware to one table's routes means reconstructing that
	// table's rest.Resource call by hand, the cost of a codegen gap rather
	// than of huma.Group itself.
	tasksGroup := huma.NewGroup(srv.API)
	tasksGroup.UseMiddleware(tasks2.RequireAuthForWrites(tasksGroup))

	tasksOptions := rest.Options{
		Path:            "/tasks",
		Name:            "task",
		Tag:             "tasks",
		Ops:             rest.OpCreate | rest.OpRead | rest.OpUpdate | rest.OpDelete | rest.OpList,
		Description:     "A unit of work, belonging to one list.",
		DefaultPageSize: 25,
		MaxPageSize:     100,
		MaxFilters:      8,
	}
	if err := rest.Resource[tasks2.Task, tasks2.TaskCreate, tasks2.TaskPatch](tasksGroup, db, tasksOptions); err != nil {
		return fmt.Errorf("mounting tasks: %w", err)
	}
	if err := rest.CollectionAction[tasks2.ClearCompletedTaskInput](tasksGroup, db, tasksOptions, rest.ActionSpec{
		Name:        "clear-completed",
		Path:        "/tasks/clear-completed",
		Field:       "ClearCompletedTask",
		Summary:     "Clear completed tasks",
		Description: "Deletes every task whose status is done, optionally scoped to one list.",
		HasBody:     true,
	}, tasks2.ClearCompleted); err != nil {
		return fmt.Errorf("mounting clear-completed: %w", err)
	}

	// The item-form action, mounted the same way the collection form above
	// is — an application reconstructing a table's rest.Resource call by
	// hand reconstructs everything declared on it the same way, generated or
	// not.
	if err := rest.Action[tasks2.Task, tasks2.CompleteTaskInput](tasksGroup, db, tasksOptions, rest.ActionSpec{
		Name:    "complete",
		Path:    "/tasks/{id}/complete",
		Field:   "CompleteTask",
		Writes:  []string{"status", "completed_at"},
		Summary: "Marks the task done",
		HasBody: true,
	}, tasks2.CompleteTask); err != nil {
		return fmt.Errorf("mounting the complete action: %w", err)
	}

	// The prototype for a declared query — same story. GET, so
	// RequireAuthForWrites lets it through unauthenticated even though it is
	// mounted on the same group as everything above.
	if err := rest.Query[tasks2.OverdueTaskParams, []tasks2.Task](tasksGroup, db, tasksOptions, rest.QuerySpec{
		Name:      "overdue",
		Path:      "/tasks/overdue",
		Reads:     []string{"tasks"},
		Summary:   "Tasks that are not done and were due before the given time",
		HasParams: true,
	}, tasks2.OverdueTasks); err != nil {
		return fmt.Errorf("mounting the overdue query: %w", err)
	}
	return nil
}
