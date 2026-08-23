package pgtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/sqlbtest"
)

// `sqlb survey` end to end, which is the only place its Phase C runs at all.
//
// cmd/sqlb is package main in the engine's module, so nothing there can be
// imported and nothing in the engine's module may touch a database. The verb
// takes two DSNs and writes a document, so running the binary and reading the
// document is not a workaround here — it is the interface.
func runSurvey(t *testing.T, src, dst string, args ...string) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", append(append([]string{"run", "./cmd/sqlb", "survey"}, args...), src, dst)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlb survey: %v\n%s", err, out)
	}
	return string(out)
}

// surveyDB creates one of the two databases a survey needs, and returns a
// connection to it and the DSN to hand the command.
//
// freshDB names its database after the test and so cannot make two, and the
// survey's second database has to be empty of the shim as well as of tables —
// it is the scratch the round trip renders into.
func surveyDB(t *testing.T, suffix string) (*pgxpool.Pool, string) {
	t.Helper()
	// suffix is kept for what it says at the call sites — which of the two
	// databases this is — rather than for uniqueness: sqlbtest names each one
	// after the test and the moment, so two calls in one test are already two
	// databases.
	_ = suffix
	pool := sqlbtest.Fresh(t, serverDSN(t), sqlbtest.MaxConns(poolSize))
	return pool, pool.Config().ConnString()
}

// The report a clean schema gets. Phase C compared after one round, and a
// varchar CHECK is renormalised by Postgres on the second application — so
// every such constraint was counted as a residual on a schema that is perfectly
// stable, and the survey said so in the one phase an adopter reads when Phase B
// looks too good (issue #136).
//
// See TestVarcharCheckIsAFixpointAtTwoRounds for the property underneath this.
func TestSurveyReportsAFixpointForAVarcharCheck(t *testing.T) {
	t.Parallel()
	src, srcDSN := surveyDB(t, "src")
	_, dstDSN := surveyDB(t, "dst")
	mustExec(t, src, varcharCheckSchema)

	out := runSurvey(t, srcDSN, dstDSN)

	if !strings.Contains(out, "- fixpoint residual: 0") {
		t.Errorf("a schema that settles was reported as having a residual:\n%s", out)
	}
	// And it says how many rounds it took, so "0" is not read as "there was
	// never anything to reconcile". A construct Postgres rewrites is worth
	// knowing about even when the rewrite is stable.
	if !strings.Contains(out, "reached after 2 iterations") {
		t.Errorf("the verdict does not say how many rounds the round trip took:\n%s", out)
	}
	if !strings.Contains(out, "round trip settled after 2 iterations") {
		t.Errorf("Phase C does not say it settled:\n%s", out)
	}
	// Phase B has nothing to report about this table, which is what makes the
	// old Phase C residual a contradiction rather than a second opinion.
	if !strings.Contains(out, "| clean — imports with nothing dropped | 1 |") {
		t.Errorf("Phase B did not find the table clean, so this fixture is testing something else:\n%s", out)
	}
}

// The other half of the loop: a DDL statement the scratch database refuses is
// still reported, and reported once.
//
// This is what the iteration put at risk. Apply failures are counted from the
// first round only — every later round renders a declaration derived from a
// database rather than the adopter's own — and a loop that recounted them would
// report three times the failures on a schema that settles on the third round.
//
// The lever is the one the repository already documents: generated DDL for a
// UUIDv7 primary key emits uuid_generate_v7(), which a stock Postgres does not
// have. The source gets the shim so its table can exist; the scratch database
// is stock, which is exactly the position an adopter's scratch database is in.
func TestSurveyReportsDDLTheScratchDatabaseRefuses(t *testing.T) {
	t.Parallel()
	src, srcDSN := surveyDB(t, "src")
	_, dstDSN := surveyDB(t, "dst")
	mustExec(t, src, `CREATE FUNCTION uuid_generate_v7() RETURNS uuid
		LANGUAGE sql VOLATILE AS 'SELECT uuidv7()'`)
	mustExec(t, src, `
		CREATE TABLE widgets (
		    id   uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
		    name text NOT NULL
		);
	`)

	out := runSurvey(t, srcDSN, dstDSN)

	if !strings.Contains(out, "DDL apply failures: 1") {
		t.Errorf("the statement the scratch database refused was not reported exactly once:\n%s", out)
	}
	if !strings.Contains(out, "uuid_generate_v7") {
		t.Errorf("the failure does not name what was missing:\n%s", out)
	}
}
