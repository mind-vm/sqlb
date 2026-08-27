package shadow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// DB is what a replay needs: statements, and a transaction to group each file's
// statements in. *pgxpool.Pool and *pgx.Conn both satisfy it.
type DB interface {
	sqlb.Executor
	sqlb.Beginner
}

// Options configures a replay.
type Options struct {
	// Dir is the migration directory. Required.
	Dir string

	// Format is the migration format the directory is written in. Defaults to
	// migrate.Goose.
	//
	// A custom Format is not supported here: rendering a file and reading one
	// back are different problems, and this package only knows how to read the
	// three that ship.
	Format migrate.Format

	// Schema and Module are passed through to introspect when the replayed
	// database is read back.
	Schema string
	Module string

	// Only and Exclude are passed through too, and narrow what is read back
	// rather than what is replayed. The whole history always runs — replaying a
	// subset of it would build a schema no file describes, which is the failure
	// this package exists to catch — so these narrow the reading only.
	//
	// A module adopting a few tables at a time needs both halves: the history
	// builds sixty-nine tables and the declaration covers five, and without this
	// the report is about the sixty-four nobody asked about.
	Only    []string
	Exclude []string
}

// Result reports what was replayed, so a failure or a surprise can be traced to
// a file rather than to "the migrations".
type Result struct {
	// Files are the migration filenames applied, in the order they ran.
	Files []string
	// Statements is how many statements were executed in total.
	Statements int
}

// Build applies every migration in the directory to db and returns the schema
// they produce.
//
// db must be connected to an **empty** database. Replaying a history onto a
// schema that already has tables in it produces a registry describing neither
// one, and the migration generated from it would be wrong in a way nothing
// downstream could detect — so a non-empty database is refused rather than
// worked around.
//
// The introspect.Report is the same one introspect.Registry returns: a
// non-empty one means the replayed schema uses constructs the DSL cannot
// express, so the registry does not describe it completely.
func Build(ctx context.Context, db DB, opts Options) (*schema.Registry, *introspect.Report, *Result, error) {
	if db == nil {
		return nil, nil, nil, fmt.Errorf("shadow: Build needs a database connection")
	}
	if opts.Dir == "" {
		return nil, nil, nil, fmt.Errorf("shadow: Build needs a migration directory")
	}
	format := opts.Format
	if format == nil {
		format = migrate.Goose
	}

	if err := requireEmpty(ctx, db, opts.Schema); err != nil {
		return nil, nil, nil, err
	}

	files, err := collect(opts.Dir, format.Name())
	if err != nil {
		return nil, nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, nil, fmt.Errorf(
			"shadow: no migrations found in %s. An empty history would replay to an "+
				"empty schema, and a diff against that proposes creating every table you "+
				"already have — so this is refused rather than answered", opts.Dir)
	}

	res := &Result{}
	for _, f := range files {
		if err := apply(ctx, db, f); err != nil {
			return nil, nil, nil, err
		}
		res.Files = append(res.Files, f.Name)
		res.Statements += len(f.Statements)
	}

	reg, report, err := introspect.Registry(ctx, db, introspect.Options{
		Schema:  opts.Schema,
		Module:  opts.Module,
		Only:    opts.Only,
		Exclude: opts.Exclude,
	})
	if err != nil {
		return nil, nil, res, fmt.Errorf("shadow: reading back the replayed schema: %w", err)
	}
	return reg, report, res, nil
}

// apply runs one migration's forward statements.
//
// In a transaction unless the file said not to, which mirrors what a runner
// does and is what makes a failure leave the shadow database at a file
// boundary rather than halfway through one.
func apply(ctx context.Context, db DB, f file) error {
	if f.NoTransaction {
		for i, stmt := range f.Statements {
			if _, err := db.Exec(ctx, stmt); err != nil {
				return statementError(f, i, stmt, err)
			}
		}
		return nil
	}

	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("shadow: %s: beginning a transaction: %w", f.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for i, stmt := range f.Statements {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return statementError(f, i, stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("shadow: %s: committing: %w", f.Name, err)
	}
	return nil
}

func statementError(f file, i int, stmt string, err error) error {
	return fmt.Errorf("shadow: %s: statement %d of %d failed: %w\n%s%s",
		f.Name, i+1, len(f.Statements), err, strings.TrimSpace(stmt), hint(err))
}

// hint adds the one line a replay failure cannot carry on its own.
//
// A migration may name a schema this project does not own and does not create
// — a foreign key into a platform's tables, which
// docs/architecture.md's "a foreign key may name another schema" decision
// allows and obliges nothing to provision. The database it was written against
// has that schema; a scratch database created for the replay does not, and
// Postgres answers 3F000 with the schema's name and no idea why anyone
// expected it to be there. Naming the arrangement here is the difference
// between a puzzling failure and a one-line fix.
func hint(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "3F000" {
		return ""
	}
	return "\n\nThe history names a schema this database does not have. Nothing in sqlb " +
		"creates one:\ncreate it in the shadow database before the replay, or point the " +
		"shadow DSN at a database\nthat already has it — docs/supabase.md's \"The shadow " +
		"database\" is the worked case."
}

// requireEmpty refuses a database that already has tables in it.
func requireEmpty(ctx context.Context, db DB, schemaName string) error {
	if schemaName == "" {
		schemaName = "public"
	}

	rows, err := db.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		ORDER BY c.relname
		LIMIT 6
	`, schemaName)
	if err != nil {
		return fmt.Errorf("shadow: checking that the target database is empty: %w", err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("shadow: checking that the target database is empty: %w", err)
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("shadow: checking that the target database is empty: %w", err)
	}
	if len(found) == 0 {
		return nil
	}

	list := strings.Join(found, ", ")
	if len(found) > 5 {
		list = strings.Join(found[:5], ", ") + ", …"
	}
	return fmt.Errorf(
		"shadow: schema %q already contains tables (%s), and a history replayed on top "+
			"of them describes neither the history nor the database. Point Build at an "+
			"empty database — it will not drop these, because it cannot tell a scratch "+
			"database from a real one", schemaName, list)
}
