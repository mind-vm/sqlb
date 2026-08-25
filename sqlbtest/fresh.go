package sqlbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// The other half of testing on sqlb: a real database, and still no container.
//
// [DB] answers the questions a scripted Executor can — which statement was
// issued, what it bound, whether the write was wrapped in a transaction. What
// it cannot answer is whether the SQL is *valid*: whether the column exists,
// whether the cast is legal, whether the constraint fires. That needs Postgres,
// and every suite that wanted one used to write the same eighty lines.
//
// Nine copies of them are in this repository alone — pgtest, fxapp, vault,
// catalog, rooms, meter, outbox, tasks-evolved and the task manager's migration
// tests — and example/vault's says out loud why: "copied rather than imported:
// fxapp is a separate module and this one is too, so there is nothing to import
// it from". This is the thing to import it from.
//
// # It takes a DSN and starts nothing
//
// Deliberately, and the reversal is recorded in pgtest/doc.go: these suites
// used to start a container each through testcontainers, which cost a full CI
// run six servers, put docker/docker and forty modules in a go.mod, and shipped
// a reaper that reaps by label — and therefore removed long-lived containers
// belonging to unrelated work on the same machine. A DSN has none of those
// properties, and every way of providing one already exists: a compose file, a
// CI service container, a database somebody left running, or testcontainers in
// the caller's own module if that is what they want. What this package will not
// do is choose for them.
//
// There is also no skip-when-absent path. A suite that passes quietly when it
// cannot reach a database reports coverage it does not have, which is worse
// than one that fails: [DSN] fails the test naming the variable that was unset.

// DSN reads a Postgres URL out of the environment, failing the test when it is
// not there.
//
// hint is what the caller should do about it — `mise run pg-up`, `docker
// compose up`, whatever provides the database in this project — and it is a
// parameter rather than a sentence this package invents, because only the
// caller knows.
//
//	dsn := sqlbtest.DSN(t, "SQLB_TEST_POSTGRES", "run `mise run pg-up` first")
func DSN(t testing.TB, env, hint string) string {
	t.Helper()
	value := envLookup(env)
	if value == "" {
		if hint == "" {
			t.Fatalf("sqlbtest: %s is not set, so there is no database to run against", env)
		}
		t.Fatalf("sqlbtest: %s is not set, so there is no database to run against; %s", env, hint)
	}
	return value
}

// Fresh creates a database of its own on the server dsn names, applies each
// option in order, and returns a pool for it.
//
// The database is dropped when the test ends, so tests are independent without
// truncating anything, and they may run in parallel: a database per test costs
// milliseconds, where a server per test costs seconds and a shared one costs
// the isolation.
//
//	db := sqlbtest.Fresh(t, dsn, sqlbtest.Declared(schema.DefaultRegistry()))
//	handle := sqlb.New(db).WithHooks(hooks)
//
// dsn may name any database on the server — the path is replaced with the
// maintenance database to create the new one, and with the new one to connect.
// A user that may not CREATE DATABASE is the one requirement this has beyond a
// connection.
func Fresh(t testing.TB, dsn string, opts ...Option) *pgxpool.Pool {
	t.Helper()
	pool, _ := fresh(t, dsn, opts)
	return pool
}

// FreshDSN is [Fresh] for a caller that opens its own connection: an
// application booting from a URL, a pool with settings this package does not
// know about, a test that hands the string to something else entirely.
//
// The options are applied the same way, through a pool that is closed before
// this returns — so what the caller gets is a database already built, and the
// only connection to it is the one they open.
func FreshDSN(t testing.TB, dsn string, opts ...Option) string {
	t.Helper()
	pool, target := fresh(t, dsn, opts)
	pool.Close()
	return target
}

// fresh does the work for both, returning the pool and the DSN that reaches it.
func fresh(t testing.TB, dsn string, opts []Option) (*pgxpool.Pool, string) {
	t.Helper()

	cfg := freshConfig{maxConns: 4}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	renderer, err := dsnRenderer(dsn)
	if err != nil {
		t.Fatalf("sqlbtest: %v", err)
	}

	ctx := context.Background()
	name := databaseName(t)

	// Created and dropped through a single short-lived connection rather than a
	// pool held for the run: a pool would be one more thing for a caller to own
	// and close, and creating a database is two statements at the start of a
	// test.
	admin, err := pgx.Connect(ctx, renderer("postgres"))
	if err != nil {
		t.Fatalf("sqlbtest: opening the maintenance connection: %v\n"+
			"the DSN reached %s; is the server up?", err, redact(dsn))
	}
	// Dropped first, so a crashed run leaves nothing that makes the next one
	// fail with "already exists" instead of with its real problem.
	adminExec(ctx, t, admin, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`)
	adminExec(ctx, t, admin, `CREATE DATABASE `+quoteIdent(name))
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("sqlbtest: closing the maintenance connection: %v", err)
	}

	poolCfg, err := pgxpool.ParseConfig(renderer(name))
	if err != nil {
		t.Fatalf("sqlbtest: parsing the connection string for %s: %v", name, err)
	}
	poolCfg.MaxConns = cfg.maxConns
	for _, fn := range cfg.configure {
		fn(poolCfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("sqlbtest: opening %s: %v", name, err)
	}

	t.Cleanup(func() {
		// The pool first: a database with open connections cannot be dropped,
		// and WITH (FORCE) is the belt to that braces.
		pool.Close()
		drop, err := pgx.Connect(context.Background(), renderer("postgres"))
		if err != nil {
			t.Logf("sqlbtest: %s was left behind: %v", name, err)
			return
		}
		defer func() { _ = drop.Close(context.Background()) }()
		if _, err := drop.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			t.Logf("sqlbtest: %s was left behind: %v", name, err)
		}
	})

	for _, step := range cfg.steps {
		if err := step(ctx, pool); err != nil {
			t.Fatalf("sqlbtest: preparing %s: %v", name, err)
		}
	}
	return pool, renderer(name)
}

// Option is what [Fresh] does after creating the database, or how it connects.
//
// One list rather than two, because the order a caller writes them in is the
// order the database is built in, and separating "configuration" from "content"
// would put the pool size somewhere other than beside the extension the schema
// needs.
type Option interface{ apply(*freshConfig) }

type freshConfig struct {
	maxConns  int32
	configure []func(*pgxpool.Config)
	steps     []step
}

// step is one thing done to the new database, in the order given.
type step func(context.Context, *pgxpool.Pool) error

type optionFunc func(*freshConfig)

func (f optionFunc) apply(cfg *freshConfig) { f(cfg) }

func addStep(s step) Option {
	return optionFunc(func(cfg *freshConfig) { cfg.steps = append(cfg.steps, s) })
}

// MaxConns caps the pool. The default is four.
//
// It matters more than it looks: the ceiling a suite reaches is this number
// times the number of tests running in parallel, and a stock server allows a
// hundred connections in total. A pool sized to the machine rather than to the
// test is how a suite passes on a laptop and exhausts a CI runner.
func MaxConns(n int32) Option {
	return optionFunc(func(cfg *freshConfig) { cfg.maxConns = n })
}

// Configure adjusts the pool before it is opened, for the settings this package
// does not name.
//
// The one that keeps coming up is the query mode: pgx prepares and caches a
// plan per connection keyed on the statement text, so a suite measuring the
// same SQL under different session settings measures the first plan every time.
// pgtest's vector tests were doing exactly that, and reported a perfect result
// for the query they existed to show failing.
//
//	sqlbtest.Configure(func(c *pgxpool.Config) {
//	    c.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
//	})
func Configure(fn func(*pgxpool.Config)) Option {
	return optionFunc(func(cfg *freshConfig) { cfg.configure = append(cfg.configure, fn) })
}

// SQL runs statements against the new database, in order.
//
// For the DDL a suite writes by hand — the table three tests share, the trigger
// under test, the shim the generated DDL assumes exists.
func SQL(statements ...string) Option {
	return addStep(func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, statement := range statements {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := pool.Exec(ctx, statement); err != nil {
				return fmt.Errorf("%w\n%s", err, strings.TrimSpace(statement))
			}
		}
		return nil
	})
}

// Extensions creates each named extension if it is not already there.
//
// Separate from [SQL] because the failure is worth naming: an extension that is
// not installed on the server cannot be created by a test, and the error says
// so rather than looking like a syntax problem.
func Extensions(names ...string) Option {
	return addStep(func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, name := range names {
			if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS `+quoteIdent(name)); err != nil {
				return fmt.Errorf("creating extension %s: %w\n"+
					"it has to be available on the server; a test cannot install one", name, err)
			}
		}
		return nil
	})
}

// Declared builds the schema a registry declares: [migrate.Diff] from nothing
// to that registry, applied statement by statement.
//
// This is the baseline every example builds on, and it is deliberately not a
// migration history — what it proves is that the DDL sqlb renders *now* applies
// to Postgres. A suite testing a history should replay the history, which is
// [SQL] over the checked-in files.
func Declared(reg *schema.Registry, opts ...migrate.Option) Option {
	return addStep(func(ctx context.Context, pool *pgxpool.Pool) error {
		changes, err := migrate.Diff(nil, reg, opts...)
		if err != nil {
			return fmt.Errorf("diffing the declared schema: %w", err)
		}
		for _, change := range changes {
			if strings.TrimSpace(change.Up) == "" {
				continue
			}
			if _, err := pool.Exec(ctx, change.Up); err != nil {
				return fmt.Errorf("applying %q: %w\n%s", change.Comment, err, strings.TrimSpace(change.Up))
			}
		}
		return nil
	})
}

// Changes applies a set of migration changes, which is what a suite that owns a
// history replays.
func Changes(changes []migrate.Change) Option {
	return addStep(func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, change := range changes {
			if strings.TrimSpace(change.Up) == "" {
				continue
			}
			if _, err := pool.Exec(ctx, change.Up); err != nil {
				return fmt.Errorf("applying %q: %w\n%s", change.Comment, err, strings.TrimSpace(change.Up))
			}
		}
		return nil
	})
}

// Do runs a caller's own function against the new database, for the preparation
// that is neither DDL nor a schema: seeding rows, installing a fixture, calling
// a package's own bootstrap.
func Do(fn func(context.Context, *pgxpool.Pool) error) Option { return addStep(fn) }

func adminExec(ctx context.Context, t testing.TB, conn *pgx.Conn, statement string) {
	t.Helper()
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("sqlbtest: %v\n%s", err, statement)
	}
}

// Two things distinguish one scratch database from another, and neither is a
// clock.
//
// processTag distinguishes processes, which is what `go test ./...` needs:
// package binaries run concurrently, and two packages with a TestCreate each
// would otherwise fight over one database and fail in a way that looks like a
// bug in the code under test. nameSeq distinguishes calls inside one process,
// which is what a package with two tests needs.
//
// This used to be `time.Now().UnixNano() % 1e9` doing both jobs, and it did
// neither reliably. A clock is only as fine as the host's resolution — on a
// machine whose reading advances in microseconds, four calls to [Fresh] land
// on the same tick and produce the same name — and two processes started
// together read times that are close, not different. A counter cannot repeat
// and a random tag does not depend on when the process started.
var (
	processTag = newProcessTag()
	nameSeq    atomic.Uint64
)

// newProcessTag returns forty bits of randomness as hex: short enough to leave
// the test's own name room inside an identifier, wide enough that the several
// dozen package binaries of a `go test ./...` do not collide.
func newProcessTag() string {
	var b [5]byte
	// crypto/rand.Read does not fail. Since Go 1.24 it either fills the buffer
	// or crashes the program, and the error it returns is always nil.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// databaseName derives a legal, unique name from the test's.
//
// Unique across packages as well as within one, and without consulting a
// clock — see [processTag] for why that is the whole point.
func databaseName(t testing.TB) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, t.Name())

	// Postgres truncates an identifier at 63 bytes, which would collide two
	// long subtests into one database. The test's name is what gets cut,
	// because it is the part that is decoration: the tag and the counter are
	// what make the name unique, so the budget is whatever they leave.
	suffix := fmt.Sprintf("_%s_%d", processTag, nameSeq.Add(1))
	const maxIdent = 63
	if budget := maxIdent - len("t_") - len(suffix); len(name) > budget {
		name = name[:budget]
	}
	return "t_" + name + suffix
}

// dsnRenderer returns a function producing the DSN for a named database on the
// same server.
func dsnRenderer(dsn string) (func(database string) string, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("no DSN; these suites take one and start nothing (see DSN)")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid URL: %w", redact(dsn), err)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%q names no host; a DSN looks like postgres://user:pass@host:5432/db", redact(dsn))
	}
	return func(database string) string {
		on := *parsed
		on.Path = "/" + database
		return on.String()
	}, nil
}

// redact removes the password from a DSN before it reaches a test log.
func redact(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		return dsn
	}
	if _, ok := parsed.User.Password(); !ok {
		return dsn
	}
	parsed.User = url.UserPassword(parsed.User.Username(), "…")
	return parsed.String()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// envLookup is os.Getenv, named so this file's one process-wide read is
// visible in the import list rather than buried.
func envLookup(name string) string { return os.Getenv(name) }
