package pgtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/mind-vm/sqlb/codegen"
	"github.com/mind-vm/sqlb/introspect"
	"github.com/mind-vm/sqlb/migrate"
	"github.com/mind-vm/sqlb/schema"
)

// The round trip is a fixpoint, and this is the test that says so.
//
// introspect, RenderSchema and migrate.Diff are each tested. Nothing asserted
// that they agree with each other about one schema, and running the loop by
// hand over a real 69-table database turned up three disagreements — a type
// introspect could read and RenderSchema could not write, an import that failed
// sqlb's own validation, and DDL Postgres rejected (issue #53). Every one of
// them lives *between* two packages that are individually well tested, which is
// why none of their own tests could see it.
//
// The invariant:
//
//	apply(fixture)              → a database
//	introspect(db)              → registry
//	RenderSchema(registry)      → source that compiles
//	apply(Diff(∅, registry))    → a second database
//	introspect(db')             → registry'
//	Diff(registry, registry')   → empty
//
// The last line is the one a consumer needs to be able to trust, because "sqlb
// can own your schema" is exactly this property.

// awkwardSchema is deliberately the schema that has historically broken this
// loop: the types that were skipped, an index whose operator class is its
// meaning, storage parameters, a partial index, a composite unique, a check
// that is not an enum and one that is, arrays, nullable jsonb.
const awkwardSchema = `
CREATE TABLE orgs (
    id   uuid PRIMARY KEY,
    name text NOT NULL,
    plan text NOT NULL DEFAULT 'free',
    CONSTRAINT chk_org_plan CHECK (plan IN ('free', 'pro', 'enterprise'))
);

CREATE TABLE document_chunks (
    id         uuid PRIMARY KEY,
    org_id     uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    title      varchar(200),
    body       text NOT NULL,
    score      numeric,
    weight     double precision,
    confidence real,
    rating     smallint,
    weekdays   smallint[],
    revision   bigint NOT NULL DEFAULT 0,
    tags       text[],
    meta       jsonb DEFAULT '{}'::jsonb,
    embedding  vector(1536),
    archived   boolean NOT NULL DEFAULT false,
    published  date,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT document_chunks_body_not_empty CHECK (char_length(body) > 0),
    -- Over a smallint column on purpose. An unsupported column type does not
    -- cost one column: everything defined over it goes with it, and three of
    -- the eight distinct skips in the survey behind #120 were that cascade
    -- rather than independent gaps. If smallint ever regresses to a skip, this
    -- constraint and the index below fall with it and say so.
    CONSTRAINT document_chunks_rating_range CHECK (rating >= 1 AND rating <= 5)
);

CREATE INDEX idx_chunks_org ON document_chunks (org_id);
CREATE INDEX idx_chunks_tags ON document_chunks USING gin (tags);
CREATE INDEX idx_chunks_live ON document_chunks (org_id, created_at) WHERE NOT archived;
CREATE UNIQUE INDEX idx_chunks_org_title ON document_chunks (org_id, title);
CREATE INDEX idx_chunks_rating ON document_chunks (rating, created_at DESC);
CREATE INDEX idx_chunks_embedding ON document_chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);

COMMENT ON TABLE document_chunks IS 'Chunks of a document, with their embeddings.';
COMMENT ON COLUMN document_chunks.embedding IS 'The embedding, 1536 wide.';

-- The three shapes an adoption sweep found next, each of which used to leave
-- the loop above with something to reconcile by hand.
--
-- members.supervisor_id is a self-referential foreign key (#82): declarable
-- only as ExternalRef, since Ref inside members' own definition is a Go
-- initialisation cycle, and reported by the import as undeclarable until it
-- was taught the same spelling.
--
-- members ↔ images is a foreign-key cycle (#80). The import used to drop every
-- table on it, with advice a consumer could not follow.
--
-- contracted_hours_per_week is a numeric with a precision (#81), which is a
-- different type from an unbounded numeric and was skipped outright.
--
-- The composite UNIQUE below is #108, and it is here in both spellings: one
-- named the way Postgres names it, which must come back as the Unique()
-- shorthand, and one named otherwise, which must keep its name through
-- UniqueNamed. Approximating either as a unique index would diff as a drop and
-- a rebuild, so this is the assertion that the constraint stays a constraint.
CREATE TABLE members (
    id                        uuid PRIMARY KEY,
    org_id                    uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name                      text NOT NULL,
    supervisor_id             uuid,
    profile_image_id          uuid,
    contracted_hours_per_week numeric(5,2),
    hired_on                  date,
    CONSTRAINT members_supervisor_id_fkey FOREIGN KEY (supervisor_id)
        REFERENCES members (id) ON DELETE SET NULL,
    CONSTRAINT members_org_id_name_key UNIQUE (org_id, name),
    CONSTRAINT members_roster_slot UNIQUE (org_id, hired_on)
);

-- A composite PRIMARY KEY (issue #109): a natural-key cache, keyed by what it
-- describes and referenced by nothing. The workaround was a surrogate UUID plus
-- a unique index — 16 bytes and an index per row identifying something nothing
-- points at — so the round trip has to reproduce the key the database has, not
-- one the DSL found easier.
CREATE TABLE llmcatalog_models (
    provider     text NOT NULL,
    model_id     text NOT NULL,
    display_name text NOT NULL,
    context_size integer,
    PRIMARY KEY (provider, model_id)
);

-- An EXCLUDE constraint (issue #121): the one construct with no near miss.
-- A composite UNIQUE has a unique index and a composite key has a surrogate;
-- dropping this has no equivalent, only "enforce it in Go", where two concurrent
-- requests interleave between the check and the insert. It needs a gist index, a
-- range expression over two columns, per-element operators and a partial
-- predicate at once, which is why it is the last of the declaration gaps.
CREATE TABLE bookings (
    id        uuid PRIMARY KEY,
    coach_id  uuid NOT NULL,
    status    text NOT NULL DEFAULT 'confirmed',
    starts_at timestamptz NOT NULL,
    ends_at   timestamptz NOT NULL,
    CONSTRAINT bookings_no_double_booking
        EXCLUDE USING gist (coach_id WITH =, tstzrange(starts_at, ends_at) WITH &&)
        WHERE (status = 'confirmed')
);

-- A composite UNIQUE *constraint*, which is a different object from the unique
-- index above: only a constraint can be the target of REFERENCES t (a, b) or be
-- named in ON CONFLICT ON CONSTRAINT, so declaring the index instead is not a
-- spelling difference (issue #108). Here it is the write path's conflict
-- target, which is why the shape is worth reproducing rather than simplifying.
CREATE TABLE secrets (
    id          uuid PRIMARY KEY,
    tenant_kind text NOT NULL,
    tenant_id   uuid NOT NULL,
    name        text NOT NULL,
    ciphertext  bytea NOT NULL,
    CONSTRAINT secrets_tenant_kind_tenant_id_name_key UNIQUE (tenant_kind, tenant_id, name)
);

-- A deferred UNIQUE (issue #154). A variant is identified by the combination of
-- its option values, which live in a child table, so the combination is carried
-- as a denormalised signature and the constraint has to hold over the committed
-- state: a variant is inserted before the option values that identify it, so
-- every new one passes through a state where its signature is still the default
-- and two variants of a product collide on it.
--
-- It is here because deferrability used to be invisible to *both* sides of this
-- loop, which meant the fixpoint held for the wrong reason — the declaration and
-- the database were blind to the same property, which is the failure ADR-0016
-- names. A migration that recreated this constraint without its clause would
-- break every multi-variant write and the drift gate would stay green.
CREATE TABLE product_variants (
    id               uuid PRIMARY KEY,
    product_id       uuid NOT NULL,
    option_signature text NOT NULL DEFAULT '',
    sku              text NOT NULL,
    CONSTRAINT product_variants_product_id_option_signature_key
        UNIQUE (product_id, option_signature) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT product_variants_sku_key UNIQUE (sku) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE images (
    id         uuid PRIMARY KEY,
    creator_id uuid NOT NULL REFERENCES members (id) ON DELETE CASCADE,
    url        text NOT NULL
);

ALTER TABLE members ADD CONSTRAINT members_profile_image_id_fkey
    FOREIGN KEY (profile_image_id) REFERENCES images (id) ON DELETE SET NULL;

-- An ExternalRef wants an index on the column it joins on, so the two sides of
-- the loop only agree if the database has one. That is the same index anybody
-- would create here anyway.
CREATE INDEX members_supervisor_id_idx ON members (supervisor_id);
CREATE INDEX members_profile_image_id_idx ON members (profile_image_id);

-- An ordered index (#64), whose ordering is the index: this one backs
-- ORDER BY hired_on DESC, name ASC NULLS FIRST.
CREATE INDEX idx_members_roster ON members (org_id, hired_on DESC, name NULLS FIRST);

-- An auto-incrementing integer key, in both of Postgres's spellings (issue
-- #132). It is here rather than only in serial_test.go because it is the shape
-- that breaks *this* loop in three places at once: introspect refused it, so
-- the column was dropped and the index over it fell with it; RenderSchema had
-- no constructor to write it back as; and Diff renders a serial as a type name
-- rather than as a column plus a sequence, which is the one construct whose
-- DDL has to be a macro to apply at all.
--
-- The serial is on the append-only log and the identity on the step table,
-- which is how the report found them — and coprocess_steps_session_idx is the
-- index that used to fall with the column it covers.
CREATE TABLE activity_log (
    id          BIGSERIAL PRIMARY KEY,
    actor_id    uuid NOT NULL,
    action      text NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE coprocess_steps (
    seq        bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    session_id uuid NOT NULL,
    attempt    integer GENERATED ALWAYS AS IDENTITY,
    bucket     SMALLSERIAL NOT NULL
);

CREATE INDEX coprocess_steps_session_idx ON coprocess_steps (session_id, seq);
`

// readBack introspects a database and fails the test on anything the DSL could
// not describe: this fixture is chosen so that nothing should be skipped, so a
// report entry is a finding rather than a note.
func readBack(t *testing.T, pool *pgxpool.Pool) *schema.Registry {
	t.Helper()
	reg, rep, err := introspect.Registry(context.Background(), sqlb.New(pool), introspect.Options{})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if !rep.Empty() {
		t.Fatalf("the fixture is meant to be fully describable, and this was skipped:\n%s", rep)
	}
	// The fixture has a vector column, so pgvector is installed and the report
	// must say so. Diff renders no CREATE EXTENSION, so this list is the only
	// thing standing between an adopter and 228 identical "function does not
	// exist" errors on the first bootstrap into an empty database (issue #115).
	//
	// Asserted here rather than in a unit test because the unit test cannot see
	// whether the pg_extension query is right — a query returning nothing looks
	// exactly like a database with no extensions.
	var found bool
	for _, e := range rep.Extensions {
		if e == "vector" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pgvector is installed and the report does not name it: %v", rep.Extensions)
	}
	return reg
}

// applyRegistry renders a registry to DDL and applies it, failing on the first
// statement Postgres refuses.
func applyRegistry(t *testing.T, pool *pgxpool.Pool, reg *schema.Registry) {
	t.Helper()
	changes, err := migrate.Diff(nil, reg)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, c := range changes {
		if strings.TrimSpace(c.Up) == "" {
			continue
		}
		if _, err := pool.Exec(context.Background(), c.Up); err != nil {
			t.Fatalf("Postgres refused generated DDL: %v\n%s", err, c.Up)
		}
	}
}

// The whole loop.
func TestRoundTripIsAFixpoint(t *testing.T) {
	t.Parallel()
	// The pgvector image, because the fixture's whole point is the types and
	// the index that were breaking the loop.
	source := vectorDB(t)
	mustExec(t, source, awkwardSchema)

	// 1. Read the database.
	reg := readBack(t, source)

	// 2. Write it back out as the schema package a project would then own. This
	//    is the bootstrap that turns sixty-nine tables into sixty-nine
	//    declarations to review rather than sixty-nine to write, and it used to
	//    stop at the first vector column.
	src, err := codegen.RenderSchema(reg, codegen.SchemaOptions{Package: "ragschema"})
	if err != nil {
		t.Fatalf("RenderSchema over an introspected registry: %v", err)
	}
	if err := buildsAgainstSqlb(t, string(src)); err != nil {
		t.Fatalf("the rendered schema does not compile: %v\n%s", err, src)
	}

	// 3. Build a second database from what was read.
	rebuilt := vectorDB(t)
	applyRegistry(t, rebuilt, reg)

	// 4. Read that one, and the two must agree in both directions. Diff is not
	//    symmetric — one side proposes what the other lacks — so both are
	//    checked rather than assuming.
	reread := readBack(t, rebuilt)
	forward, err := migrate.Diff(reg, reread)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	backward, err := migrate.Diff(reread, reg)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(forward) != 0 || len(backward) != 0 {
		t.Errorf("the round trip is not a fixpoint:\n%s%s",
			renderChanges("read → rebuilt", forward), renderChanges("rebuilt → read", backward))
	}
}

// The vector index specifically: its operator class is not decoration, it is
// the distance function, and pgvector has no default — so an index emitted
// without one is rejected outright.
func TestVectorIndexKeepsItsOperatorClassAndParameters(t *testing.T) {
	t.Parallel()
	source := vectorDB(t)
	mustExec(t, source, awkwardSchema)

	reg := readBack(t, source)
	var found bool
	for _, idx := range reg.Get("document_chunks").Indexes() {
		if idx.Name != "idx_chunks_embedding" {
			continue
		}
		found = true
		if got := idx.Opclasses["embedding"]; got != "vector_cosine_ops" {
			t.Errorf("operator class = %q, want vector_cosine_ops", got)
		}
		if idx.With["m"] != "16" || idx.With["ef_construction"] != "64" {
			t.Errorf("storage parameters = %v, want m=16 ef_construction=64", idx.With)
		}
	}
	if !found {
		t.Fatal("the vector index was not imported at all")
	}

	// And the DDL it produces is DDL Postgres accepts, which is the half that
	// was failing: `USING hnsw (embedding)` is an error, not a lesser index.
	rebuilt := vectorDB(t)
	applyRegistry(t, rebuilt, reg)

	var def string
	if err := rebuilt.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_chunks_embedding'`).Scan(&def); err != nil {
		t.Fatalf("reading the rebuilt index: %v", err)
	}
	for _, want := range []string{"hnsw", "vector_cosine_ops", "m='16'", "ef_construction='64'"} {
		if !strings.Contains(def, want) {
			t.Errorf("the rebuilt index is missing %q:\n%s", want, def)
		}
	}
}

// A schema rendered from a database has to compile, which is what the bootstrap
// depends on and what a string comparison would not check.
func buildsAgainstSqlb(t *testing.T, src string) error {
	t.Helper()

	// pgtest is a module of its own beside the engine, so the checkout is the
	// parent directory.
	root, err := filepath.Abs("..")
	if err != nil {
		return err
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod": "module fixpointcheck\n\ngo 1.25.0\n\n" +
			"require github.com/mind-vm/sqlb v0.0.0\n\n" +
			"replace github.com/mind-vm/sqlb => " + root + "\n",
		"schema.go": src,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return err
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func renderChanges(label string, changes []migrate.Change) string {
	if len(changes) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s (%d):\n", label, len(changes))
	for _, c := range changes {
		fmt.Fprintf(&b, "  %s\n    %s\n", c.Comment, strings.TrimSpace(c.Up))
	}
	return b.String()
}

// The stronger claim, and the one a consumer actually depends on: the rebuilt
// database *is* the first database.
//
// Comparing the two registries is not enough, and the gap is not academic —
// anything both sides drop is invisible to that comparison. A constraint name
// introspect does not record is dropped identically on each pass, so the
// registries agree while the databases do not, and the project's next diff
// against production proposes dropping and re-adding a constraint forever
// (issue #53's fourth finding).
func TestRebuiltDatabaseMatchesTheOriginal(t *testing.T) {
	t.Parallel()
	source := vectorDB(t)
	mustExec(t, source, awkwardSchema)

	rebuilt := vectorDB(t)
	applyRegistry(t, rebuilt, readBack(t, source))

	before, after := catalogDigest(t, source), catalogDigest(t, rebuilt)
	if before != after {
		t.Errorf("the rebuilt database is not the one that was read:\n%s", diffLines(before, after))
	}
}

// catalogDigest is what the database says about itself, in a form two of them
// can be compared by: every column with its type, nullability and default,
// every constraint by name and definition, every index by definition.
func catalogDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()

	var lines []string
	collect := func(query string) {
		rows, err := pool.Query(ctx, query)
		if err != nil {
			t.Fatalf("reading the catalog: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scanning the catalog: %v", err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading the catalog: %v", err)
		}
	}

	collect(`
		SELECT format('column %s.%s %s null=%s default=%s',
		              c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
		              NOT a.attnotnull, COALESCE(pg_get_expr(d.adbin, d.adrelid), '-'))
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1`)
	collect(`
		SELECT format('constraint %s.%s %s', c.relname, con.conname, pg_get_constraintdef(con.oid))
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND con.contype <> 'n'
		ORDER BY 1`)
	// The indexes, with their storage parameters sorted. reloptions is a set,
	// Postgres hands it back in declaration order, and sqlb renders it sorted so
	// that a generated migration does not reorder itself between runs —
	// comparing the definition text as-is would report that choice as a
	// difference. Normalised here rather than in SQL, where a missing WITH
	// clause turns the whole expression null.
	rows, err := pool.Query(ctx, `
		SELECT pg_get_indexdef(i.oid), COALESCE(i.reloptions, '{}')
		FROM pg_class i
		JOIN pg_index x ON x.indexrelid = i.oid
		JOIN pg_class c ON c.oid = x.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'`)
	if err != nil {
		t.Fatalf("reading the indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var def string
		var options []string
		if err := rows.Scan(&def, &options); err != nil {
			t.Fatalf("scanning the indexes: %v", err)
		}
		if len(options) > 0 {
			sort.Strings(options)
			if cut := strings.Index(def, " WITH ("); cut >= 0 {
				def = def[:cut]
			}
			def += " WITH (" + strings.Join(options, ", ") + ")"
		}
		lines = append(lines, "index "+def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the indexes: %v", err)
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// diffLines reports the lines that differ, because a wall of identical catalog
// text with one changed row in it is not a reviewable failure message.
func diffLines(before, after string) string {
	have := map[string]bool{}
	for _, line := range strings.Split(after, "\n") {
		have[line] = true
	}
	var b strings.Builder
	for _, line := range strings.Split(before, "\n") {
		if !have[line] {
			fmt.Fprintf(&b, "  only in the original: %s\n", line)
		}
	}
	had := map[string]bool{}
	for _, line := range strings.Split(before, "\n") {
		had[line] = true
	}
	for _, line := range strings.Split(after, "\n") {
		if !had[line] {
			fmt.Fprintf(&b, "  only in the rebuild:  %s\n", line)
		}
	}
	return b.String()
}
