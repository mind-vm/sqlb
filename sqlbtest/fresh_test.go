package sqlbtest

// The parts of the scratch-database helper that need no database. What it does
// against a real one is pgtest/sqlbtest_test.go's, because the engine's own
// suite stays runnable with no Postgres at all — which is the same split this
// package's two halves are.

import (
	"net/url"
	"strconv"
	"strings"
	"sync"
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
//
// Ten thousand calls rather than two, because two is a coin flip. The generator
// this replaced derived its suffix from the clock, and two calls differed on
// most runs and not on others; a guard that fails one run in ten is noise. On a
// host whose clock advances in microseconds this loop produced seven thousand
// duplicates.
func TestTheDatabaseNameIsLegalAndUnique(t *testing.T) {
	const calls = 10000
	seen := make(map[string]bool, calls)
	for range calls {
		name := databaseName(t)
		if seen[name] {
			t.Fatalf("%q came back twice in %d calls; concurrent packages would collide", name, calls)
		}
		seen[name] = true
		if len(name) > 63 {
			t.Fatalf("%q is longer than Postgres keeps (63 bytes)", name)
		}
		if name != strings.ToLower(name) {
			t.Fatalf("%q is not lower case, so it would need quoting to be found again", name)
		}
		if strings.ContainsAny(name, `"' /\`+"`") {
			t.Fatalf("%q carries something that has no business in an identifier", name)
		}
	}
}

// The loop above catches a clock too coarse to separate two calls, but only on
// a host whose clock is coarse; this one holds on every host, because it says
// what the suffix is made of rather than how often it happens to repeat.
//
// Consecutive names differ by exactly one, in a counter this package
// increments — the tag beside it identifies the process and does not move. The
// generator this replaced put `time.Now().UnixNano() % 1e9` there, where the
// difference between two calls is an elapsed-nanosecond count: arbitrary on a
// fine clock, zero on a coarse one, and one only by accident.
func TestTheDatabaseNameCountsRatherThanReadsAClock(t *testing.T) {
	firstTag, firstSeq := splitName(t, databaseName(t))
	secondTag, secondSeq := splitName(t, databaseName(t))

	if firstTag != secondTag {
		t.Errorf("the process tag moved between two calls (%q then %q); it identifies the process, not the call", firstTag, secondTag)
	}
	if secondSeq-firstSeq != 1 {
		t.Errorf("the counter went %d to %d; a suffix that jumps is a clock reading, and a clock can repeat", firstSeq, secondSeq)
	}
}

// Two packages are two processes, and two processes have to disagree without
// coordinating. The tag is the only part that can carry that, so it is the only
// part this asserts about — a counter that starts at one in both processes is
// exactly the situation the tag exists for.
func TestTwoProcessesGetDifferentTags(t *testing.T) {
	// newProcessTag is what runs once at start-up; calling it twice stands in
	// for the second process.
	first, second := newProcessTag(), newProcessTag()
	if first == second {
		t.Errorf("two processes would both be %q, so both would build the same database", first)
	}
}

// The counter is read and incremented from whatever goroutine calls Fresh, and
// t.Parallel means that is several at once. Run under -race, which the gate
// does.
func TestConcurrentCallersGetDistinctNames(t *testing.T) {
	const goroutines, each = 16, 500

	names := make(chan string, goroutines*each)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				names <- databaseName(t)
			}
		}()
	}
	wg.Wait()
	close(names)

	seen := make(map[string]bool, goroutines*each)
	for name := range names {
		if seen[name] {
			t.Fatalf("%q came back twice across %d goroutines", name, goroutines)
		}
		seen[name] = true
	}
}

// splitName returns the process tag and the counter off the end of a generated
// name, failing the test when the shape is not the one the guards above are
// written against.
func splitName(t *testing.T, name string) (tag string, seq uint64) {
	t.Helper()
	parts := strings.Split(name, "_")
	if len(parts) < 3 {
		t.Fatalf("%q has no _<tag>_<counter> on the end", name)
	}
	tag = parts[len(parts)-2]
	seq, err := strconv.ParseUint(parts[len(parts)-1], 10, 64)
	if err != nil {
		t.Fatalf("%q does not end in a counter: %v", name, err)
	}
	return tag, seq
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
