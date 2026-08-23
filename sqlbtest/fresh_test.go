package sqlbtest

// The parts of the scratch-database helper that need no database. What it does
// against a real one is pgtest/sqlbtest_test.go's, because the engine's own
// suite stays runnable with no Postgres at all — which is the same split this
// package's two halves are.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The DSN is rewritten rather than rebuilt, and that is the whole reason this
// is a URL swap and not a struct: a rebuilt connection string silently drops
// whatever this file did not think of.
func TestTheDSNKeepsEverythingButTheDatabase(t *testing.T) {
	const base = "postgres://sqlb:secret@db.internal:15432/original" +
		"?sslmode=verify-full&application_name=suite&connect_timeout=3"

	render, err := dsnRenderer(base)
	if err != nil {
		t.Fatalf("dsnRenderer: %v", err)
	}

	got, err := url.Parse(render("t_scratch_1"))
	if err != nil {
		t.Fatalf("parsing the rendered DSN: %v", err)
	}
	if got.Path != "/t_scratch_1" {
		t.Errorf("path = %q, want the new database", got.Path)
	}
	for name, want := range map[string]string{
		"sslmode":          "verify-full",
		"application_name": "suite",
		"connect_timeout":  "3",
	} {
		if value := got.Query().Get(name); value != want {
			t.Errorf("%s = %q, want %q — a parameter was lost in the swap", name, value, want)
		}
	}
	if got.Host != "db.internal:15432" || got.User.String() != "sqlb:secret" {
		t.Errorf("the server or the credentials moved: %s", got.Redacted())
	}
}

func TestAnUnusableDSNIsRefusedWithASentence(t *testing.T) {
	for name, dsn := range map[string]string{
		"empty":     "",
		"no host":   "postgres:///justadatabase",
		"not a URL": "postgres://%zz",
	} {
		if _, err := dsnRenderer(dsn); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A password in a test log is a password in CI output.
func TestTheDSNIsRedactedBeforeItIsReported(t *testing.T) {
	got := redact("postgres://sqlb:hunter2@localhost:5432/db?sslmode=disable")

	if strings.Contains(got, "hunter2") {
		t.Errorf("the password survived: %s", got)
	}
	for _, want := range []string{"sqlb", "localhost:5432", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacting removed %q too, which is what the message is for: %s", want, got)
		}
	}
}

// The name has to be legal, lower case, short enough that Postgres does not
// truncate it, and different every time — the last because `go test ./...` runs
// package binaries concurrently, and two packages with a TestCreate each would
// otherwise fight over one database.
func TestTheDatabaseNameIsLegalAndUnique(t *testing.T) {
	first := databaseName(t)
	second := databaseName(t)

	if first == second {
		t.Errorf("two calls produced %q twice; concurrent packages would collide", first)
	}
	for _, name := range []string{first, second} {
		if len(name) > 63 {
			t.Errorf("%q is longer than Postgres keeps (63 bytes)", name)
		}
		if name != strings.ToLower(name) {
			t.Errorf("%q is not lower case, so it would need quoting to be found again", name)
		}
		if strings.ContainsAny(name, `"' /\`+"`") {
			t.Errorf("%q carries something that has no business in an identifier", name)
		}
	}
}

// A long subtest name is the case the truncation exists for: two of them
// sharing the first sixty-three bytes would be one database, and the failure
// would look like a bug in the code under test.
func TestALongTestNameStillFits(t *testing.T) {
	t.Run(strings.Repeat("a_very_long_subtest_name", 6), func(t *testing.T) {
		name := databaseName(t)
		if len(name) > 63 {
			t.Errorf("%q is %d bytes, which Postgres would truncate", name, len(name))
		}
	})
}

// The options are a single list on purpose, and the order in it is the order
// the database is built in — an extension before the schema that needs it.
func TestOptionsApplyInTheOrderTheyAreWritten(t *testing.T) {
	var cfg freshConfig
	MaxConns(9).apply(&cfg)
	Extensions("btree_gist").apply(&cfg)
	SQL("CREATE TABLE a ()").apply(&cfg)
	Configure(func(_ *pgxpool.Config) {}).apply(&cfg)

	if cfg.maxConns != 9 {
		t.Errorf("maxConns = %d, want 9", cfg.maxConns)
	}
	if len(cfg.steps) != 2 {
		t.Errorf("%d steps, want the extension and the table — Configure is not one", len(cfg.steps))
	}
	if len(cfg.configure) != 1 {
		t.Errorf("%d pool adjustments, want one", len(cfg.configure))
	}
}
