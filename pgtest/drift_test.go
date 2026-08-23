package pgtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb"
	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/introspect"
	"github.com/jryannel/sqlb/migrate"
	"github.com/jryannel/sqlb/schema"
	"github.com/jryannel/sqlb/shadow"
	"github.com/jryannel/sqlb/sqlbtest"
)

// The drift gate, against a database with corners the DSL cannot describe.
//
// This is the loop `sqlb check -database` runs, written out here because the
// claims are about what the database does: read it, narrow the read to what the
// schema declares, normalise the checks, diff. Every failure this file guards
// against was reported from a real adoption (issue #54): one tsvector column
// aborted the import of sixty-nine tables, and there was no way to ask for the
// five that had been declared.

// driftSchema is the database as it exists: two declared tables, one table
// nobody declared, one column the DSL cannot model, an index over that column,
// and a foreign key into a table outside the declaration.
const driftSchema = `
CREATE TABLE organizations (
    id   uuid PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE projects (
    id      uuid PRIMARY KEY,
    org_id  uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    code    text NOT NULL,
    status  text NOT NULL DEFAULT 'active',
    tags    jsonb DEFAULT '[]'::jsonb,
    -- A value-set check, which introspect reads back as an enum column
    -- (ADR-0017), and a check that is a check, which it does not.
    CONSTRAINT projects_status_check CHECK (status IN ('active', 'archived')),
    CONSTRAINT projects_code_not_empty CHECK (char_length(code) > 0)
);
CREATE INDEX idx_projects_org_id ON projects (org_id);
CREATE UNIQUE INDEX idx_projects_org_code ON projects (org_id, code);

CREATE TABLE document_chunks (
    id            uuid PRIMARY KEY,
    body          text NOT NULL,
    search_vector tsvector
);
CREATE INDEX idx_document_chunks_search_vector ON document_chunks USING gin (search_vector);

CREATE TABLE goose_db_version (
    id         bigserial PRIMARY KEY,
    version_id bigint NOT NULL
);
`

// declaredProjects is the declaration a partial adoption writes: one table of
// the four, describing the database exactly as it is — the index names it
// already has (#57), the jsonb default spelled the way the DDL spells it (#56),
// and the live foreign key into a table this declaration does not include (#55).
func declaredProjects() *schema.Registry {
	r := schema.NewRegistry()
	r.Table("projects",
		schema.UUID("id").PrimaryKey(),
		schema.ExternalRef("org", "organizations.id").Enforced().OnDelete(schema.Cascade),
		schema.Text("code"),
		// A text column with a value-set check is an enum, which is how the
		// database spells one and how introspect reads it back (ADR-0017).
		schema.Enum("status", "active", "archived").Default(schema.Value("active")),
		schema.JSON("tags").Nullable().Default(schema.Expr("'[]'::jsonb")),
	).
		Check("projects_code_not_empty", "char_length(code) > 0").
		IndexNamed("idx_projects_org_id", "org_id").
		UniqueIndexNamed("idx_projects_org_code", "org_id", "code")
	return r
}

// drift runs the gate: read the database, narrow it, normalise, diff.
func drift(t *testing.T, pool *pgxpool.Pool, declared *schema.Registry) []migrate.Change {
	t.Helper()
	ctx := context.Background()

	only := make([]string, 0, len(declared.Tables()))
	for _, tbl := range declared.Tables() {
		only = append(only, tbl.Name())
	}

	current, _, err := introspect.Registry(ctx, sqlb.New(pool), introspect.Options{Only: only})
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	if _, err := shadow.Normalize(ctx, pool, declared, shadow.Options{}); err != nil {
		t.Fatalf("normalising the declared checks: %v", err)
	}
	changes, err := migrate.Diff(current, declared)
	if err != nil {
		t.Fatalf("diffing: %v", err)
	}
	return changes
}

// A declaration that describes one table of a database exactly proposes
// nothing — which is the whole of what a drift gate needs, and what none of the
// four issues allowed before.
func TestDriftGateIsQuietWhenTheSchemaMatches(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)

	if changes := drift(t, pool, declaredProjects()); len(changes) != 0 {
		var b strings.Builder
		for _, c := range changes {
			b.WriteString("\n  " + c.Comment + "\n    " + c.Up)
		}
		t.Errorf("an accurate declaration proposed %d changes:%s", len(changes), b.String())
	}
}

// The gate is not quiet because it is blind: a column added to the database
// behind the schema's back is reported.
func TestDriftGateSeesAColumnTheSchemaDoesNotHave(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)
	mustExec(t, pool, `ALTER TABLE projects ADD COLUMN hotfix text`)

	changes := drift(t, pool, declaredProjects())
	if len(changes) != 1 {
		t.Fatalf("want one difference, got %d", len(changes))
	}
	if !strings.Contains(changes[0].Up, "hotfix") {
		t.Errorf("the drift should name the column:\n%s", changes[0].Up)
	}
}

// And a column the schema declares that the database has lost.
func TestDriftGateSeesAMissingColumn(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)
	mustExec(t, pool, `ALTER TABLE projects DROP COLUMN status`)

	changes := drift(t, pool, declaredProjects())
	var found bool
	for _, c := range changes {
		if strings.Contains(c.Up, `ADD COLUMN "status"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("a column the database lost should be reported, got %d changes", len(changes))
	}
}

// The unmodelable corner: one tsvector column and a GIN index over it used to
// abort the whole read, so no gate could be built for the other tables at all.
func TestDriftGateSurvivesAnUnmodelableColumn(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)

	ctx := context.Background()
	r, rep, err := introspect.Registry(ctx, sqlb.New(pool), introspect.Options{})
	if err != nil {
		t.Fatalf("one tsvector column should not abort the import: %v", err)
	}
	if r.Get("projects") == nil || r.Get("organizations") == nil {
		t.Fatal("the readable tables were lost with the unreadable column")
	}
	chunks := r.Get("document_chunks")
	if chunks == nil {
		t.Fatal("the table is importable; only its tsvector column is not")
	}
	if chunks.Field("search_vector") != nil || len(chunks.Indexes()) != 0 {
		t.Error("the column and the index over it should both have been skipped")
	}
	for _, want := range []string{"search_vector", "idx_document_chunks_search_vector"} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("the report should name %q:\n%s", want, rep)
		}
	}
}

// The live foreign key into a table the declaration does not include is
// preserved rather than proposed for deletion, which is the difference between
// a usable gate and one that says DROP CONSTRAINT forever (issue #55).
func TestDriftGateKeepsAForeignKeyIntoAnUndeclaredTable(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)

	ctx := context.Background()
	current, _, err := introspect.Registry(ctx, sqlb.New(pool), introspect.Options{Only: []string{"projects"}})
	if err != nil {
		t.Fatalf("reading one table of the database: %v", err)
	}
	ref := current.Get("projects").Field("org_id").Desc().Ref
	if ref == nil || !ref.Enforced {
		t.Fatalf("the constraint should survive as an enforced external reference, got %+v", ref)
	}

	for _, c := range drift(t, pool, declaredProjects()) {
		if strings.Contains(c.Up, "DROP CONSTRAINT") {
			t.Errorf("a live foreign key was proposed for deletion:\n%s", c.Up)
		}
	}
}

// The jsonb default and the index names, checked against the database rather
// than against another registry: both spellings are the same default, and the
// declared names are the ones Postgres has (issues #56 and #57).
func TestDriftGateAgreesAboutDefaultsAndIndexNames(t *testing.T) {
	t.Parallel()
	pool := freshStockDB(t)
	mustExec(t, pool, driftSchema)

	for _, c := range drift(t, pool, declaredProjects()) {
		switch {
		case strings.Contains(c.Up, "SET DEFAULT"):
			t.Errorf("the jsonb default was proposed again:\n%s", c.Up)
		case strings.Contains(c.Up, "ALTER INDEX"):
			t.Errorf("a declared index name was proposed as a rename:\n%s", c.Up)
		}
	}

	// The naive declaration is the control: without the names and with the
	// default spelled the other way, the same database is three renames — which
	// is what an adoption used to have to accept.
	naive := schema.NewRegistry()
	naive.Table("projects",
		schema.UUID("id").PrimaryKey(),
		schema.ExternalRef("org", "organizations.id").Enforced().OnDelete(schema.Cascade),
		schema.Text("code"),
		schema.Enum("status", "active", "archived").Default(schema.Value("active")),
		schema.JSON("tags").Nullable().Default(schema.Expr("'[]'::jsonb")),
	).
		Check("projects_code_not_empty", "char_length(code) > 0").
		Index("org_id").
		UniqueIndex("org_id", "code")

	var renames int
	for _, c := range drift(t, pool, naive) {
		if strings.Contains(c.Up, "ALTER INDEX") {
			renames++
		}
	}
	if renames != 2 {
		t.Errorf("the naive declaration should propose 2 index renames, got %d", renames)
	}
}

// The command, not the loop: `sqlb check -database` is the gate the issue asked
// for, so that every consumer does not reimplement the hundred lines above.
func TestCheckDatabaseCommandReportsDrift(t *testing.T) {
	pool := sqlbtest.Fresh(t, serverDSN(t), sqlbtest.SQL(driftSchema))
	target := pool.Config().ConnString()

	// A Project's paths resolve against the module root, so the generated
	// files go into a temporary working directory rather than an absolute one.
	t.Chdir(t.TempDir())
	opts := codegen.Options{Registry: declaredProjects(), Dir: ".", Package: "gen"}
	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("generating: %v", err)
	}
	project := codegen.Project{Options: opts}

	run := func() (int, string) {
		var stdout, stderr strings.Builder
		code := codegen.Run(project, []string{"check", "-database", target}, &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}

	code, out := run()
	if code != 0 {
		t.Fatalf("a matching database should pass, got exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "the database matches the schema") {
		t.Errorf("output does not say the database matches:\n%s", out)
	}

	// The database moves, the declaration does not, and the gate fails with the
	// difference in it.
	mustExec(t, pool, `ALTER TABLE projects ADD COLUMN hotfix text`)

	code, out = run()
	if code == 0 {
		t.Fatalf("drift should fail the gate:\n%s", out)
	}
	for _, want := range []string{"does not match", "hotfix", "sqlb migrate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// Without -database the command never opens one, which is what it did before
// and what keeps it usable in a CI job that has no database at all.
func TestCheckWithoutADatabaseAsksNothingOfOne(t *testing.T) {
	t.Chdir(t.TempDir())
	opts := codegen.Options{Registry: declaredProjects(), Dir: ".", Package: "gen"}
	if _, err := codegen.Generate(opts); err != nil {
		t.Fatalf("generating: %v", err)
	}

	var stdout, stderr strings.Builder
	code := codegen.Run(codegen.Project{Options: opts}, []string{"check"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("check should pass with no database involved, got %d:\n%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "database") {
		t.Errorf("check said something about a database it never opened:\n%s", stderr.String())
	}
}
