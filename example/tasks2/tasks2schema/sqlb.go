package tasks2schema

//go:generate go run github.com/mind-vm/sqlb/cmd/sqlb generate .

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/codegen"
)

// shadowDSNEnv names the scratch database `sqlb migrate` replays the history
// into, the same arrangement example/tasks uses.
const shadowDSNEnv = "TASKS2_SHADOW_DSN"

// SqlbProject tells `sqlb generate` what this example emits and where.
//
// example/tasks2 is a module of its own, so Dir is left empty the same way
// example/tasks leaves it empty. Go only — no TS/Dart/CLI/skill emitters —
// because this example exists to measure the schema-to-server path, not to
// repeat what example/tasks already demonstrates about the client emitters.
func SqlbProject() codegen.Project {
	return codegen.Project{
		Options: codegen.Options{
			Package: "tasks2",
		},
		MigrationsDir: "migrations",
		MinPostgres:   18,
		ShadowDB:      shadowDB,
	}
}

// shadowDB opens the scratch database `sqlb migrate` replays the committed
// migration history into.
func shadowDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv(shadowDSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf("%s is not set, and replaying the migration history needs a scratch database", shadowDSNEnv)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("emptying the shadow database at %s: %w", shadowDSNEnv, err)
	}
	return pool, nil
}
